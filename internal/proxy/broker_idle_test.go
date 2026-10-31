package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNativeBrokerIdleClosesBlockedConsumer(t *testing.T) {
	_, peer, done, cancel := nativeBrokerIdleFixture(t, 30*time.Millisecond)
	defer cancel()
	// The response has been consumed, but no datagram frame will be read. This
	// leaves the broker blocked in its downstream write rather than UDP.Read.
	if _, err := peer.Write([]byte("blocked consumer")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("UDP idle expiry left downstream write blocked")
	}
}

func TestNativeBrokerIdleStopCancelsAndJoinsExpiry(t *testing.T) {
	closed := make(chan struct{})
	idle := newEgressBrokerIdle(20*time.Millisecond, func() { close(closed) })
	idle.stop()
	select {
	case <-closed:
		t.Fatal("stopped idle timer closed a flow")
	case <-time.After(50 * time.Millisecond):
	}
	started, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	idle = newEgressBrokerIdle(time.Millisecond, func() { close(started); <-release })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("idle expiry did not run")
	}
	go func() {
		defer close(stopped)
		defer func() { _ = recover() }()
		idle.stop()
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while expiry still owned the connection")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not join committed expiry")
	}
}

func TestNativeBrokerIdleRenewsOnDuplexActivityAndCancels(t *testing.T) {
	client, peer, done, cancel := nativeBrokerIdleFixture(t, 100*time.Millisecond)
	defer cancel()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		defer func() { _ = recover() }()
		buffer := make([]byte, 100)
		for {
			n, err := peer.Read(buffer)
			if err != nil {
				return
			}
			if _, err := peer.Write(buffer[:n]); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { peer.Close(); <-echoDone })
	frame := []byte{0, 6, 'a', 'c', 't', 'i', 'v', 'e'}
	deadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := client.Write(frame); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(frame))
		if _, err := io.ReadFull(client, got); err != nil || !bytes.Equal(got, frame) {
			t.Fatalf("active UDP flow closed or changed: %q, %v", got, err)
		}
		select {
		case <-done:
			t.Fatal("active duplex flow expired")
		case <-time.After(25 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation left active UDP flow running")
	}
}

func TestNativeBrokerIdleRenewsOnOutgoingDatagrams(t *testing.T) {
	client, peer, done, cancel := nativeBrokerIdleFixture(t, 100*time.Millisecond)
	defer cancel()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer func() { _ = recover() }()
		_, _ = io.Copy(io.Discard, peer)
	}()
	t.Cleanup(func() { peer.Close(); <-readDone })
	frame := []byte{0, 6, 'a', 'c', 't', 'i', 'v', 'e'}
	deadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := client.Write(frame); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			t.Fatal("outgoing datagrams did not preserve flow")
		case <-time.After(25 * time.Millisecond):
		}
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("quiet outgoing UDP flow did not expire")
	}
}

func TestNativeBrokerIdleRenewsOnIncomingDatagrams(t *testing.T) {
	client, peer, done, cancel := nativeBrokerIdleFixture(t, 100*time.Millisecond)
	defer cancel()
	deadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := peer.Write([]byte("incoming")); err != nil {
			t.Fatal(err)
		}
		frame := make([]byte, 10)
		if _, err := io.ReadFull(client, frame); err != nil || binary.BigEndian.Uint16(frame[:2]) != 8 {
			t.Fatalf("incoming datagram flow closed: %v", err)
		}
		select {
		case <-done:
			t.Fatal("incoming datagrams did not preserve flow")
		case <-time.After(25 * time.Millisecond):
		}
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("quiet UDP flow did not expire")
	}
}

type nativeIdleResponse struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *nativeIdleResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func nativeBrokerIdleFixture(t *testing.T, idle time.Duration) (net.Conn, net.Conn, <-chan struct{}, context.CancelFunc) {
	t.Helper()
	downstream, client := net.Pipe()
	upstream, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	handler := newEgressBrokerWithIdle(nativeTestDialer(func(context.Context, string, string) (net.Conn, error) { return upstream, nil }), "native-capability", nativeEgressTarget, func(*http.Request) (string, bool) { return "udp", true }, idle)
	r := httptest.NewRequest(http.MethodConnect, "http://1.1.1.1:443", nil).WithContext(ctx)
	r.RequestURI, r.Host = "1.1.1.1:443", "1.1.1.1:443"
	r.Header.Set("Authorization", "Bearer native-capability")
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		handler.ServeHTTP(&nativeIdleResponse{ResponseRecorder: httptest.NewRecorder(), conn: downstream}, r)
	}()
	t.Cleanup(func() {
		cancel()
		client.Close()
		peer.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("broker handler leaked during cleanup")
		}
	})
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response: %v", err)
	}
	return client, peer, done, cancel
}
