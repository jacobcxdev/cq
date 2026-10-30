package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type egressBrokerTestDialer func(context.Context, string, string) (net.Conn, error)

func (f egressBrokerTestDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func egressBrokerTestSocket(t *testing.T) (string, net.Listener) {
	t.Helper()
	// Keep below the macOS Unix socket path limit even when TMPDIR is long.
	dir, err := os.MkdirTemp("/tmp", "cq-broker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "broker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return path, listener
}

func egressBrokerTestServe(t *testing.T, handler http.Handler) (string, context.CancelFunc) {
	t.Helper()
	path, listener := egressBrokerTestSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	var handlers sync.WaitGroup
	server := &http.Server{ReadHeaderTimeout: time.Second, BaseContext: func(net.Listener) context.Context { return ctx }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handler.ServeHTTP(w, r)
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("broker server panic: %v", p)
			}
		}()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); _ = server.Close(); <-done; handlers.Wait() })
	return path, cancel
}

func TestEgressBrokerTunnelLargeDuplex(t *testing.T) {
	const token = "synthetic-broker-secret"
	var dispatched atomic.Int32
	var peers sync.WaitGroup
	// Registered before the server cleanup, so the broker closes first.
	t.Cleanup(peers.Wait)
	dialer := egressBrokerTestDialer(func(_ context.Context, network, target string) (net.Conn, error) {
		if network != "tcp" || target != "origin.synthetic.test:443" {
			t.Errorf("dispatch = %q %q", network, target)
		}
		dispatched.Add(1)
		local, remote := net.Pipe()
		peers.Add(1)
		go func() {
			defer peers.Done()
			defer remote.Close()
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("echo panic: %v", p)
				}
			}()
			_, _ = io.Copy(remote, remote)
		}()
		return local, nil
	})
	path, _ := egressBrokerTestServe(t, NewEgressBroker(dialer, token, func(target string) bool { return target == "origin.synthetic.test:443" }))
	for _, network := range []string{"tcp", "tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := NewEgressBrokerClient(path, token).DialContext(ctx, network, "origin.synthetic.test:443")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			payload := bytes.Repeat([]byte("encrypted-origin-bytes\x00"), 16384)
			sent := make(chan error, 1)
			go func() {
				defer func() {
					if recover() != nil {
						sent <- errors.New("writer panic")
					}
				}()
				_, err := conn.Write(payload)
				sent <- err
			}()
			received := make([]byte, len(payload))
			_, readErr := io.ReadFull(conn, received)
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			if readErr != nil || !bytes.Equal(received, payload) {
				t.Fatalf("duplex payload mismatch: %v", readErr)
			}
		})
	}
	if dispatched.Load() != 3 {
		t.Fatalf("dispatches = %d", dispatched.Load())
	}
}

func TestEgressBrokerRejectsBeforeDispatch(t *testing.T) {
	const token = "synthetic-broker-secret"
	var dispatched atomic.Int32
	dialer := egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) {
		dispatched.Add(1)
		return nil, errors.New("unexpected dispatch")
	})
	path, _ := egressBrokerTestServe(t, NewEgressBroker(dialer, token, func(target string) bool { return target == "origin.synthetic.test:443" }))
	for _, tc := range []struct {
		name, credential, method, target string
		status                           int
	}{
		{"wrong auth", "wrong", http.MethodConnect, "origin.synthetic.test:443", 401},
		{"missing auth", "", http.MethodConnect, "origin.synthetic.test:443", 401},
		{"wrong method", token, http.MethodGet, "origin.synthetic.test:443", 405},
		{"denied", token, http.MethodConnect, "denied.synthetic.test:443", 403},
		{"noncanonical port", token, http.MethodConnect, "origin.synthetic.test:0443", 400},
		{"uppercase", token, http.MethodConnect, "Origin.synthetic.test:443", 400},
		{"zero port", token, http.MethodConnect, "origin.synthetic.test:0", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			}}
			defer transport.CloseIdleConnections()
			req, err := http.NewRequest(tc.method, "http://"+tc.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.credential != "" {
				req.Header.Set("Authorization", "Bearer "+tc.credential)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req = req.WithContext(ctx)
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.status)
			}
		})
	}
	if dispatched.Load() != 0 {
		t.Fatalf("denied requests dispatched %d times", dispatched.Load())
	}
	for _, allowed := range []func(string) bool{nil, func(string) bool { return false }} {
		handler := NewEgressBroker(dialer, token, allowed)
		deniedPath, _ := egressBrokerTestServe(t, handler)
		_, err := NewEgressBrokerClient(deniedPath, token).DialContext(context.Background(), "tcp", "origin.synthetic.test:443")
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe denial: %v", err)
		}
	}
}

func TestEgressBrokerClientHandshakeCancellation(t *testing.T) {
	path, listener := egressBrokerTestSocket(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("handshake server panic: %v", p)
			}
		}()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := NewEgressBrokerClient(path, "synthetic-broker-secret").DialContext(ctx, "tcp", "origin.synthetic.test:443")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled handshake leaked connection")
	}
}

func TestEgressBrokerPumpCancellation(t *testing.T) {
	remoteDone := make(chan struct{})
	dialer := egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) {
		local, remote := net.Pipe()
		go func() {
			defer close(remoteDone)
			defer remote.Close()
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("peer panic: %v", p)
				}
			}()
			_, _ = io.Copy(io.Discard, remote)
		}()
		return local, nil
	})
	path, cancelPump := egressBrokerTestServe(t, NewEgressBroker(dialer, "synthetic-broker-secret", func(string) bool { return true }))
	conn, err := NewEgressBrokerClient(path, "synthetic-broker-secret").DialContext(context.Background(), "tcp", "origin.synthetic.test:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancelPump()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("canceled tunnel remained readable")
	}
	select {
	case <-remoteDone:
	case <-time.After(time.Second):
		t.Fatal("canceled pump leaked upstream")
	}
}

func TestEgressBrokerClientResponseBoundsAndBufferedBytes(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		wantError      bool
	}{
		{"buffered tunnel", "HTTP/1.1 200 Connection Established\r\n\r\nearly-origin-bytes", false},
		{"oversized header", "HTTP/1.1 200 OK\r\nX-Large: " + strings.Repeat("x", 40*1024) + "\r\n\r\n", true},
		{"untrusted response", "HTTP/1.1 403 synthetic-broker-secret\r\nX-Secret: synthetic-broker-secret\r\n\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, listener := egressBrokerTestSocket(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("response server panic: %v", p)
					}
				}()
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				buffer := make([]byte, 4096)
				_, _ = conn.Read(buffer)
				_, _ = io.WriteString(conn, tc.response)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := NewEgressBrokerClient(path, "synthetic-broker-secret").DialContext(ctx, "tcp", "origin.synthetic.test:443")
			if tc.wantError {
				if err == nil {
					conn.Close()
					t.Fatal("response accepted")
				}
				if strings.Contains(err.Error(), "synthetic-broker-secret") {
					t.Fatal("response secret leaked")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				payload := make([]byte, len("early-origin-bytes"))
				_, err = io.ReadFull(conn, payload)
				if err != nil || string(payload) != "early-origin-bytes" {
					t.Fatalf("buffered payload = %q, %v", payload, err)
				}
			}
			<-done
		})
	}
}

func TestEgressBrokerMalformedTargetsAndSafeFailures(t *testing.T) {
	const token = "synthetic-broker-secret"
	var dispatched atomic.Int32
	dialer := egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) {
		dispatched.Add(1)
		return nil, errors.New("sensitive provider failure " + token)
	})
	handler := NewEgressBroker(dialer, token, func(string) bool { return true })
	for _, target := range []string{"", "host", ":443", "host:0", "host:65536", "host:0443", "Host:443", "host.:443", "host:443/path", "[fe80::1%lo0]:443", "[::ffff:127.0.0.1]:443"} {
		t.Run(target, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodConnect, "http://placeholder.test", nil)
			request.RequestURI, request.Host = target, target
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
	if dispatched.Load() != 0 {
		t.Fatal("malformed target dispatched")
	}
	request := httptest.NewRequest(http.MethodConnect, "http://origin.synthetic.test:443", nil)
	request.RequestURI, request.Host = "origin.synthetic.test:443", "origin.synthetic.test:443"
	request.Header.Set("Authorization", "Bearer "+token)
	for _, handler := range []http.Handler{
		handler,
		NewEgressBroker(dialer, token, func(string) bool { panic(token) }),
		NewEgressBroker(dialer, "", func(string) bool { return true }),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || strings.Contains(response.Body.String(), token) {
			t.Fatalf("unsafe error response: status %d", response.Code)
		}
	}
}

type egressBrokerLateDeadlineContext struct{ context.Context }

func (egressBrokerLateDeadlineContext) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Second), true
}
func TestEgressBrokerDeadlineBeforeContextNotification(t *testing.T) {
	ctx := egressBrokerLateDeadlineContext{Context: context.Background()}
	if err := egressBrokerClientError(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("elapsed socket deadline misclassified: %v", err)
	}
}
