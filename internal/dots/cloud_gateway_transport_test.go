package dots

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCloudGatewayPeerRequiresExactUnambiguousConnection(t *testing.T) {
	for _, test := range []struct {
		name, remote, output string
		pid                  int
	}{
		{"matching", "127.0.0.1:50123", "p42\nn127.0.0.1:50123->127.0.0.1:8000\np99\nn127.0.0.1:8000->127.0.0.1:50123\n", 42},
		{"same-pid-repeated", "127.0.0.1:50123", "p42\nn127.0.0.1:50123->127.0.0.1:8000\nn127.0.0.1:50123->127.0.0.1:8000\n", 42},
		{"different-loopback-bind", "127.0.0.1:50123", "p42\nn127.0.0.1:50123->127.0.0.2:8000\n", 0},
		{"ipv6-loopback-bind", "[::1]:50123", "p42\nn[::1]:50123->[::1]:8000\n", 0},
		{"peer-port-mismatch", "127.0.0.1:50124", "p42\nn127.0.0.1:50123->127.0.0.1:8000\n", 0},
		{"server-port-mismatch", "127.0.0.1:50123", "p42\nn127.0.0.1:50123->127.0.0.1:8001\n", 0},
		{"public-server", "127.0.0.1:50123", "p42\nn127.0.0.1:50123->198.51.100.1:8000\n", 0},
		{"public-peer", "198.51.100.1:50123", "p42\nn198.51.100.1:50123->127.0.0.1:8000\n", 0},
		{"ambiguous", "127.0.0.1:50123", "p42\nn127.0.0.1:50123->127.0.0.1:8000\np43\nn127.0.0.1:50123->127.0.0.1:8000\n", 0},
		{"missing-pid", "127.0.0.1:50123", "n127.0.0.1:50123->127.0.0.1:8000\n", 0},
		{"invalid-pid", "127.0.0.1:50123", "p42\npinvalid\nn127.0.0.1:50123->127.0.0.1:8000\n", 0},
		{"invalid-peer", "localhost:50123", "p42\nn127.0.0.1:50123->127.0.0.1:8000\n", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			pid, ok := cloudGatewayPeerPID(test.remote, []byte(test.output))
			if pid != test.pid || ok != (test.pid != 0) {
				t.Fatalf("peer identity = %d, %v; want %d", pid, ok, test.pid)
			}
		})
	}
}

func TestCloudGatewayRequiresNativeAppServer(t *testing.T) {
	cfg := &Settings{AppPath: "/fixture/ChatGPT.app"}
	executable, _, _, _, _ := cloudInvocation(cfg, nil, nil)
	for _, test := range []struct {
		name string
		uid  int
		args []string
		want bool
	}{
		{"native", os.Getuid(), []string{executable, "app-server", "-c", cloudAppMarker + "true"}, true},
		{"native-plugin-disabled", os.Getuid(), []string{executable, "app-server", "-c", cloudAppMarker + "false"}, true},
		{"foreign-user", os.Getuid() + 1, []string{executable, "app-server", "-c", cloudAppMarker + "true"}, false},
		{"different-executable", os.Getuid(), []string{"/usr/local/bin/codex", "app-server", "-c", cloudAppMarker + "true"}, false},
		{"ordinary-cli", os.Getuid(), []string{executable, "exec", "-c", cloudAppMarker + "true"}, false},
		{"unmarked-server", os.Getuid(), []string{executable, "app-server"}, false},
		{"unknown-plugin-value", os.Getuid(), []string{executable, "app-server", "-c", cloudAppMarker + "unknown"}, false},
		{"missing-arguments", os.Getuid(), nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cloudGatewayNativeProcess(cfg, test.uid, test.args); got != test.want {
				t.Fatalf("native qualification = %v; want %v", got, test.want)
			}
		})
	}
}

func gatewayTestListener(t *testing.T, pid int) (*cloudGatewayListener, string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native gateway listener requires Unix peer credentials")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := newCloudGatewayListener(root, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(filepath.Dir(listener.path)) })
	return listener, root
}

func TestCloudGatewayListenerHandoffAndCleanup(t *testing.T) {
	listener, root := gatewayTestListener(t, 42)
	if len(listener.path) > 103 {
		t.Fatalf("unbounded socket path: %s", listener.path)
	}
	if _, err := cloudGatewaySocket(listener.path); err != nil {
		t.Fatal(err)
	}
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := newCloudGatewayListener(root, 42); err == nil {
		t.Fatal("existing listener was displaced")
	}
	if _, err := importCloudGatewayListener(root, 43, file); err == nil {
		t.Fatal("listener accepted a different native PID")
	}
	imported, err := importCloudGatewayListener(root, 42, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.UnixListener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(listener.path); err != nil {
		t.Fatalf("parent closed inherited socket pathname: %v", err)
	}
	accepted := make(chan error, 1)
	go func() {
		defer func() { _ = recover() }()
		connection, err := imported.Accept()
		if err == nil {
			defer connection.Close()
			_, err = io.WriteString(connection, "inherited-listener")
		}
		accepted <- err
	}()
	client, err := net.DialTimeout("unix", listener.path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(time.Second))
	data, err := io.ReadAll(client)
	_ = client.Close()
	if err != nil || string(data) != "inherited-listener" {
		t.Fatalf("inherited listener did not accept owned peer: %q, %v", data, err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	if err := imported.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(listener.path); !os.IsNotExist(err) {
		t.Fatalf("broker left its socket: %v", err)
	}
}

func TestCloudGatewayShortPathIgnoredProcessTempDirectory(t *testing.T) {
	listener, root := gatewayTestListener(t, 49)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "different-native-environment"))
	path, err := cloudGatewayPath(root, 49, false)
	if err != nil || path != listener.path {
		t.Fatalf("process environment changed gateway rendezvous: %q, %v", path, err)
	}
}

func TestCloudGatewayRejectedInvalidInheritedDescriptor(t *testing.T) {
	listener, root := gatewayTestListener(t, 47)
	file, err := os.CreateTemp(t.TempDir(), "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, candidate := range []*os.File{nil, file} {
		if _, err := importCloudGatewayListener(root, 47, candidate); err == nil {
			t.Fatal("invalid descriptor acquired gateway ownership")
		}
	}
	other, otherRoot := gatewayTestListener(t, 48)
	otherFile, err := other.File()
	if err != nil {
		t.Fatal(err)
	}
	defer otherFile.Close()
	if _, err := importCloudGatewayListener(root, 47, otherFile); err == nil {
		t.Fatal("another listener acquired gateway ownership")
	}
	for _, path := range []string{listener.path, other.path} {
		if _, err := cloudGatewaySocket(path); err != nil {
			t.Fatalf("rejected descriptor changed a socket: %v", err)
		}
	}
	if imported, err := importCloudGatewayListener(otherRoot, 48, otherFile); err != nil {
		t.Fatalf("rejection closed the source descriptor: %v", err)
	} else {
		_ = imported.Close()
	}
}

func TestCloudGatewayListenerPreservedExistingAndReplacedPaths(t *testing.T) {
	listener, _ := gatewayTestListener(t, 44)
	if err := os.Remove(listener.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listener.path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(listener.path); err != nil || string(data) != "replacement" {
		t.Fatalf("cleanup changed replacement: %q, %v", data, err)
	}
	t.Cleanup(func() { _ = os.Remove(listener.path) })
	if _, err := cloudGatewaySocket(listener.path); err == nil {
		t.Fatal("ordinary file qualified as a gateway")
	}
}

func TestCloudGatewayRejectedUnsafeRootAndSocket(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newCloudGatewayListener(root, 45); err == nil {
		t.Fatal("unsafe runtime root accepted")
	}
	listener, _ := gatewayTestListener(t, 46)
	if err := os.Chmod(listener.path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudGatewaySocket(listener.path); err == nil {
		t.Fatal("unsafe socket permissions accepted")
	}
}

func TestCloudGatewayRelayPreservedUpgradeAndBufferedBytes(t *testing.T) {
	upstream, public := net.Pipe()
	defer upstream.Close()
	defer public.Close()
	finished := make(chan error, 1)
	go func() {
		defer func() { _ = recover() }()
		reader := bufio.NewReader(public)
		request, err := http.ReadRequest(reader)
		if err != nil {
			finished <- err
			return
		}
		if request.Header.Get("Authorization") != "Bearer synthetic-native" || request.URL.Path != "/backend-api/wham/remote/control/server" || request.Host != "localhost:8000" {
			finished <- fmt.Errorf("gateway request changed")
			return
		}
		_, err = io.WriteString(public, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nX-Private-Header: kept\r\n\r\nserver-buffered")
		if err == nil {
			data := make([]byte, len("client-buffered"))
			_, err = io.ReadFull(reader, data)
			if err == nil && string(data) != "client-buffered" {
				err = fmt.Errorf("client buffered bytes changed: %q", data)
			}
		}
		finished <- err
		_ = public.Close()
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCloudGateway(w, r, upstream, nil)
	}))
	defer server.Close()
	client, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = io.WriteString(client, "GET /backend-api/wham/remote/control/server HTTP/1.1\r\nHost: localhost:8000\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer synthetic-native\r\n\r\nclient-buffered")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("X-Private-Header") != "kept" {
		t.Fatalf("gateway upgrade changed: %v, %v", response, err)
	}
	data := make([]byte, len("server-buffered"))
	if _, err := io.ReadFull(reader, data); err != nil || string(data) != "server-buffered" {
		t.Fatalf("server buffered bytes changed: %q, %v", data, err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway relay stalled")
	}
}

func TestCloudGatewayRelayPreservedRejectionResponse(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := `{"error":"synthetic gateway rejection"}`
			response := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nWWW-Authenticate: Bearer error=\"invalid_token\"\r\nRetry-After: 7\r\n\r\n%s", status, http.StatusText(status), len(body), body)
			writer := cloudGatewayRejectionFixture(t, response)
			if writer.Code != status || writer.Body.String() != body {
				t.Fatalf("gateway rejection changed: status=%d body=%q", writer.Code, writer.Body.String())
			}
			if writer.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` || writer.Header().Get("Retry-After") != "7" || writer.Header().Get("Content-Type") != "application/json" || writer.Header().Get("Content-Length") != fmt.Sprint(len(body)) {
				t.Fatalf("gateway auth/retry headers changed: %v", writer.Header())
			}
		})
	}
}

func TestCloudGatewayRelayRejectedOversizedAndMalformedBodies(t *testing.T) {
	for _, test := range []struct{ name, framing, body string }{
		{"oversized", "Content-Length: 1048577\r\n", strings.Repeat("x", 1048577)},
		{"truncated", "Content-Length: 9\r\n", "short"},
		{"malformed-chunk", "Transfer-Encoding: chunked\r\n", "invalid\r\nsynthetic-private-body\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Bearer synthetic-private\r\nRetry-After: 7\r\n" + test.framing + "\r\n" + test.body
			writer := cloudGatewayRejectionFixture(t, response)
			if writer.Code != http.StatusBadGateway || writer.Body.Len() != 0 || writer.Header().Get("WWW-Authenticate") != "" || writer.Header().Get("Retry-After") != "" {
				t.Fatalf("invalid gateway body escaped before validation: status=%d body_bytes=%d headers=%v", writer.Code, writer.Body.Len(), writer.Header())
			}
		})
	}
}

func cloudGatewayRejectionFixture(t *testing.T, response string) *httptest.ResponseRecorder {
	t.Helper()
	upstream, broker := net.Pipe()
	defer upstream.Close()
	defer broker.Close()
	finished := make(chan struct{})
	go func() {
		defer func() { _ = recover(); close(finished) }()
		defer broker.Close()
		if _, err := http.ReadRequest(bufio.NewReader(broker)); err == nil {
			_, _ = io.WriteString(broker, response)
		}
	}()
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/backend-api/wham/remote/control/server", nil)
	relayCloudGateway(writer, request, upstream, nil)
	_ = upstream.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("gateway rejection fixture did not stop")
	}
	return writer
}

func TestCloudGatewayPeerMismatchKeptOpaqueFallback(t *testing.T) {
	probe, err := NewProbe(Settings{}, ProbeOptions{Event: func(map[string]any) {}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/backend-api/wham/remote/control/server", nil)
	request.RemoteAddr = "198.51.100.1:1234"
	ctx, cancel := context.WithTimeout(request.Context(), time.Second)
	defer cancel()
	writer := httptest.NewRecorder()
	if probe.nativeCloudGateway(writer, request.WithContext(ctx)) || writer.Body.Len() != 0 || writer.Code != http.StatusOK {
		t.Fatal("unqualified request acquired a gateway or wrote a response")
	}
}
