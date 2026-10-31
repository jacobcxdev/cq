package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type nativeTestDialer func(context.Context, string, string) (net.Conn, error)

func (f nativeTestDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	return f(ctx, network, target)
}

func TestNativeEgressRejectedAuthorityAndDestinations(t *testing.T) {
	var dials atomic.Int32
	handler := NewNativeEgressBroker(nativeTestDialer(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, net.ErrClosed
	}), "native-capability")
	for _, test := range []struct {
		name, target, token, marker, network string
		status                               int
	}{
		{"worker authority", "1.1.1.1:443", "worker-capability", "1", "tcp", 401},
		{"missing marker", "1.1.1.1:443", "native-capability", "", "tcp", 400},
		{"wrong network", "1.1.1.1:443", "native-capability", "1", "unix", 400},
		{"private v4", "10.0.0.1:443", "native-capability", "1", "tcp", 403},
		{"private v6", "[fd00::1]:443", "native-capability", "1", "udp", 403},
		{"localhost", "localhost:443", "native-capability", "1", "tcp", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodConnect, "http://"+test.target, nil)
			r.RequestURI, r.Host = test.target, test.target
			r.Header.Set("Authorization", "Bearer "+test.token)
			if test.marker != "" {
				r.Header.Set("X-CQ-Native-Flow", test.marker)
			}
			r.Header.Set("X-CQ-Native-Network", test.network)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d", w.Code, test.status)
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatal("rejected request reached dialler")
	}
}

func TestNativeEgressFramedUDPOverConnect(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		buffer := make([]byte, 65535)
		for {
			n, peer, err := echo.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if _, err := echo.WriteToUDP(buffer[:n], peer); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = echo.Close(); <-done })
	broker := httptest.NewServer(NewNativeEgressBroker(nativeTestDialer(func(_ context.Context, network, target string) (net.Conn, error) {
		if network != "udp" || target != "1.1.1.1:443" {
			t.Error("native request lost network or target")
		}
		return net.DialUDP("udp4", nil, echo.LocalAddr().(*net.UDPAddr))
	}), "native-capability"))
	t.Cleanup(broker.Close)
	c, err := net.Dial("tcp", strings.TrimPrefix(broker.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = io.WriteString(c, "CONNECT 1.1.1.1:443 HTTP/1.1\r\nHost: 1.1.1.1:443\r\nAuthorization: Bearer native-capability\r\nX-CQ-Native-Flow: 1\r\nX-CQ-Native-Network: udp\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT failed: %v", err)
	}
	// macOS's default host UDP limit is smaller than WireGuard's packet limit.
	// The in-memory WireGuard tests qualify the full 65,507-byte payload.
	for _, payload := range [][]byte{nil, []byte("first"), []byte("second"), bytes.Repeat([]byte("x"), 8192)} {
		frame := make([]byte, len(payload)+2)
		binary.BigEndian.PutUint16(frame, uint16(len(payload)))
		copy(frame[2:], payload)
		// Deliberately split the length header across stream writes.
		if _, err := c.Write(frame[:1]); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(frame[1:]); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(frame))
		if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, frame) {
			t.Fatalf("datagram length %d did not round-trip: %v", len(payload), err)
		}
	}
	if _, err := c.Write([]byte{0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("oversized datagram did not close stream")
	}
}

func TestNativeEgressRequiresWireGuardAndPreservesWorkerPolicy(t *testing.T) {
	m, err := NewCodexEgressManager(&Config{CodexEgressProxy: "direct"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.DialNativeContext(context.Background(), "tcp", "1.1.1.1:443"); err == nil || !strings.Contains(err.Error(), "requires WireGuard") {
		t.Fatal("native routing fell back to direct networking")
	}
	if m.AllowsTarget("1.1.1.1:443") {
		t.Fatal("ordinary worker policy gained native-app destinations")
	}
}

func TestNativeEgressReloadClosesTCPAndUDPAndRejectsRemovedProfile(t *testing.T) {
	peers := make(chan net.Conn, 4)
	var ambientDials atomic.Int32
	opener := func(cfg *Config) (*CodexEgress, [32]byte, error) {
		return &CodexEgress{
			dial: func(context.Context, string, string) (net.Conn, error) {
				ambientDials.Add(1)
				return nil, net.ErrClosed
			},
			nativeDial: func(_ context.Context, network, _ string) (net.Conn, error) {
				if network != "tcp" && network != "udp" {
					t.Errorf("unexpected native network: %s", network)
				}
				a, b := net.Pipe()
				peers <- b
				return a, nil
			},
		}, sha256.Sum256([]byte(cfg.CodexWireGuardProfile)), nil
	}
	m, err := newEgressManager(&Config{CodexWireGuardProfile: "first"}, opener)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for _, network := range []string{"tcp", "udp"} {
		connection, err := m.DialNativeContext(context.Background(), network, "1.1.1.1:443")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
	}
	if err := m.Reload(&Config{CodexWireGuardProfile: "second"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		peer := <-peers
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		_, readErr := peer.Read(make([]byte, 1))
		_ = peer.Close()
		if readErr != io.EOF {
			t.Fatalf("old native flow survived replacement: %v", readErr)
		}
	}
	if !m.NativeReady() {
		t.Fatal("replacement native generation unavailable")
	}
	if err := m.Reload(&Config{CodexEgressProxy: "direct"}); err != nil {
		t.Fatal(err)
	}
	if m.NativeReady() {
		t.Fatal("removed profile still reports native routing ready")
	}
	if _, err := m.DialNativeContext(context.Background(), "tcp", "1.1.1.1:443"); err == nil {
		t.Fatal("removed profile permitted native flow")
	}
	if ambientDials.Load() != 0 {
		t.Fatal("native flow fell back to ambient networking")
	}
}

func TestNativeEgressReloadCancelsPendingDial(t *testing.T) {
	started := make(chan struct{})
	done := make(chan error, 1)
	m, err := newEgressManager(&Config{CodexWireGuardProfile: "first"}, func(cfg *Config) (*CodexEgress, [32]byte, error) {
		return &CodexEgress{nativeDial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}}, sha256.Sum256([]byte(cfg.CodexWireGuardProfile)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	go func() {
		defer func() { _ = recover() }()
		_, err := m.DialNativeContext(context.Background(), "udp", "1.1.1.1:443")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native dial did not start")
	}
	if err := m.Reload(&Config{CodexWireGuardProfile: "second"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("old native dial was not cancelled: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old native dial survived replacement")
	}
}

type nativeReadyTestDialer struct {
	EgressDialer
	ready bool
}

func (d nativeReadyTestDialer) NativeReady() bool { return d.ready }

func TestNativeEgressReadinessDoesNotDial(t *testing.T) {
	var dials atomic.Int32
	dialer := nativeTestDialer(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, net.ErrClosed
	})
	for _, test := range []struct {
		name, target, token, network string
		ready                        bool
		status                       int
	}{
		{"ready", "cq-native-probe:0", "native-capability", "tcp", true, 200},
		{"unavailable", "cq-native-probe:0", "native-capability", "tcp", false, 503},
		{"wrong authority", "cq-native-probe:0", "worker-capability", "tcp", true, 401},
		{"wrong target", "1.1.1.1:443", "native-capability", "tcp", true, 400},
		{"wrong network", "cq-native-probe:0", "native-capability", "udp", true, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodConnect, "http://"+test.target, nil)
			r.RequestURI, r.Host = test.target, test.target
			r.Header.Set("Authorization", "Bearer "+test.token)
			r.Header.Set("X-CQ-Native-Flow", "1")
			r.Header.Set("X-CQ-Native-Network", test.network)
			r.Header.Set("X-CQ-Native-Probe", "1")
			w := httptest.NewRecorder()
			NewNativeEgressBroker(nativeReadyTestDialer{dialer, test.ready}, "native-capability").ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d", w.Code, test.status)
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatal("readiness probe opened an origin connection")
	}
}

type nativePacketTestConn struct {
	net.Conn
	packet, written []byte
}

func (c *nativePacketTestConn) Read(p []byte) (int, error) { return copy(p, c.packet), nil }
func (c *nativePacketTestConn) Write(p []byte) (int, error) {
	c.written = append(c.written, p...)
	return len(p), nil
}
func (*nativePacketTestConn) SetDeadline(time.Time) error { return nil }

func TestNativeEgressMaximumDatagramCodec(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), nativeDatagramLimit)
	raw := &nativePacketTestConn{packet: payload}
	stream := newNativeDatagramStream(raw)
	frame := make([]byte, len(payload)+2)
	if _, err := io.ReadFull(stream, frame); err != nil {
		t.Fatal(err)
	}
	if int(binary.BigEndian.Uint16(frame)) != len(payload) || !bytes.Equal(frame[2:], payload) {
		t.Fatal("maximum datagram was truncated")
	}
	for offset := 0; offset < len(frame); {
		end := min(offset+8192, len(frame))
		if n, err := stream.Write(frame[offset:end]); err != nil || n != end-offset {
			t.Fatalf("fragmented datagram failed: %v", err)
		}
		offset = end
	}
	if !bytes.Equal(raw.written, payload) {
		t.Fatal("maximum packet changed during reassembly")
	}
	raw.packet = append(payload, 0)
	if _, err := stream.Read(make([]byte, 1)); err == nil {
		t.Fatal("oversized inbound packet accepted")
	}
}

type runtimeNativeTestManager struct{ runtimeEgressFakeManager }

func (*runtimeNativeTestManager) NativeReady() bool { return true }

func (m *runtimeNativeTestManager) DialNativeContext(ctx context.Context, network, target string) (net.Conn, error) {
	return m.DialContext(ctx, network, target)
}
func (*runtimeNativeTestManager) AllowsTarget(target string) bool { return target == "chatgpt.com:443" }

func TestNativeEgressCapabilitiesAndLegacyDescriptor(t *testing.T) {
	dir := runtimeEgressTestDirectory(t)
	fs := fsutil.OSFileSystem{}
	manager := &runtimeNativeTestManager{}
	deps := runtimeEgressDependencies{newManager: func(*Config, fsutil.FileSystem) (runtimeEgressManager, error) { return manager, nil }, load: func() (*Config, error) { return &Config{}, nil }, interval: time.Hour}
	owner, err := startRuntimeEgress(context.Background(), &Config{Port: 4567}, fs, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	data, err := fsutil.ReadSecureFile(fs, runtimeEgressDescriptorPath(dir, 4567), 8192)
	if err != nil {
		t.Fatal(err)
	}
	// Exact strict schema used by already-running released workers.
	var legacy struct{ Socket, Token string }
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		t.Fatal("service upgrade broke old worker descriptor parsing")
	}
	native, err := readEgressDescriptor(fs, nativeEgressDescriptorPath(dir, 4567), dir, 4567, true)
	if err != nil || native.NativeToken == legacy.Token || native.Token != "" || native.Socket != legacy.Socket {
		t.Fatal("native capability was not separately published")
	}
	for _, test := range []struct {
		token, marker, target string
		status                int
	}{
		{legacy.Token, "1", "1.1.1.1:443", 401},
		{native.NativeToken, "", "chatgpt.com:443", 401},
		{legacy.Token, "", "1.1.1.1:443", 403},
		{native.NativeToken, "1", "1.1.1.1:443", 200},
		{legacy.Token, "", "chatgpt.com:443", 200},
	} {
		c, err := net.Dial("unix", legacy.Socket)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		marker := ""
		if test.marker != "" {
			marker = "X-CQ-Native-Flow: 1\r\nX-CQ-Native-Network: tcp\r\n"
		}
		_, err = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n%s\r\n", test.target, test.target, test.token, marker)
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		reader := bufio.NewReader(c)
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err != nil || response.StatusCode != test.status {
			c.Close()
			t.Fatalf("capability separation failed: %v", err)
		}
		if test.status == 200 {
			var body [9]byte
			if _, err := io.ReadFull(reader, body[:]); err != nil || string(body[:]) != "synthetic" {
				c.Close()
				t.Fatal("authorised connection did not reach shared manager")
			}
		}
		c.Close()
	}
	if manager.dials.Load() != 2 {
		t.Fatal("denied capability reached manager")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nativeEgressDescriptorPath(dir, 4567)); !os.IsNotExist(err) {
		t.Fatal("native capability survived owner shutdown")
	}
}
