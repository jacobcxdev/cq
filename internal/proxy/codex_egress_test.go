package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// These tests catch transport wiring that ignores explicit egress, buffers SSE,
// falls back directly, or discloses rejected proxy credentials.
func TestCodexEgressValidationAndConfig(t *testing.T) {
	for _, value := range []string{"", "direct", "http://127.0.0.1:8080", "https://proxy.example:8443", "socks5://proxy:1080", "socks5h://proxy"} {
		cfg := Config{CodexEgressProxy: value}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var reopened Config
		if err := json.Unmarshal(data, &reopened); err != nil {
			t.Fatal(err)
		}
		if reopened.CodexEgressProxy != value {
			t.Fatalf("egress not retained: %q", reopened.CodexEgressProxy)
		}
		if err := ValidateCodexEgressProxy(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"socks4://proxy:1080", "http://", "http://proxy:bad", "http://proxy:0", "http://proxy:65536", "http://proxy/path", "http://proxy?secret=value", "http://proxy#secret", "http://alice:secret@proxy:8080", "http://alice:%zz@proxy", " direct "} {
		if err := ValidateCodexEgressProxy(value); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "alice") {
			t.Fatalf("unsafe validation for rejected URL: %v", err)
		}
		if _, err := json.Marshal(Config{CodexEgressProxy: value}); err == nil {
			t.Fatal("invalid proxy serialized")
		}
		if _, err := NewCodexEgress(value); err == nil {
			t.Fatal("invalid proxy accepted")
		}
	}
}

func TestCodexEgressDefaultsAndDirect(t *testing.T) {
	egress, err := NewCodexEgress("")
	if err != nil {
		t.Fatal(err)
	}
	if egress.HTTP != http.DefaultTransport || egress.WebSocket.Proxy == nil {
		t.Fatal("ambient defaults changed")
	}
	direct, err := NewCodexEgress("direct")
	if err != nil {
		t.Fatal(err)
	}
	if direct.HTTP.(*http.Transport).Proxy != nil || direct.WebSocket.Proxy != nil {
		t.Fatal("direct still uses environment proxy")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "direct") }))
	defer origin.Close()
	response, err := (&http.Client{Transport: direct.HTTP}).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil || string(body) != "direct" {
		t.Fatalf("direct response %q: %v", body, err)
	}
}

func TestCodexEgressRescuePersistence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &Config{LocalToken: "synthetic-local", ProxyResilienceStateDir: filepath.Join(t.TempDir(), "state"), CodexEgressProxy: "http://127.0.0.1:8080"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExistingConfig()
	if err != nil || loaded.CodexEgressProxy != cfg.CodexEgressProxy {
		t.Fatalf("normal config: %v", err)
	}
	bootstrap, err := LoadProxyRescueBootstrapConfig()
	if err != nil || bootstrap.CodexEgressProxy != cfg.CodexEgressProxy {
		t.Fatalf("rescue config: %v", err)
	}
	paths, err := ResolveDefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	// A manual edit followed by restart must not retain stale direct egress.
	cfg.CodexEgressProxy = "http://127.0.0.1:8081"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrap, err = LoadProxyRescueBootstrapConfig()
	if err != nil || bootstrap.CodexEgressProxy != cfg.CodexEgressProxy {
		t.Fatalf("manual egress edit ignored: %v", err)
	}
	data = []byte(strings.Replace(string(data), "codex_egress_proxy", "CODEX_EGRESS_PROXY", 1))
	if err := os.WriteFile(paths.ConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrap, err = LoadProxyRescueBootstrapConfig()
	if err != nil || bootstrap.CodexEgressProxy != cfg.CodexEgressProxy {
		t.Fatalf("mixed-case egress edit ignored: %v", err)
	}
	if err := os.Remove(paths.RescueBootstrap); err != nil {
		t.Fatal(err)
	}
	bootstrap, err = LoadProxyRescueBootstrapConfig()
	if err != nil || bootstrap.CodexEgressProxy != cfg.CodexEgressProxy {
		t.Fatalf("bootstrap fallback: %v", err)
	}
}

func TestCodexEgressRescueRejectsInvalidManualEdit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SaveConfig(&Config{LocalToken: "synthetic", ProxyResilienceStateDir: filepath.Join(t.TempDir(), "state"), CodexEgressProxy: "direct"}); err != nil {
		t.Fatal(err)
	}
	paths, err := ResolveDefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`123`, `null`, `"http://alice:secret@proxy"`, `"socks4://proxy:1080"`} {
		if err := os.WriteFile(paths.ConfigFile, []byte(`{"codex_egress_proxy":`+value+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProxyRescueBootstrapConfig(); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid edit allowed rescue or leaked secret: %v", err)
		}
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`null`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProxyRescueBootstrapConfig(); err == nil {
		t.Fatal("null normal configuration cleared the saved rescue egress policy")
	}
}

func newCodexEgressTestProxy(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("origin credential leaked to proxy")
		}
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "unreachable", http.StatusBadGateway)
			return
		}
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("copy panic: %v", recovered)
				}
			}()
			_, _ = io.Copy(upstream, buffered)
			_ = upstream.Close()
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
}

func TestCodexEgressHTTPStreamsThroughProxy(t *testing.T) {
	release := make(chan struct{})
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("origin auth changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer origin.Close()
	defer close(release)
	var calls atomic.Int32
	proxy := newCodexEgressTestProxy(t, &calls)
	defer proxy.Close()
	egress, err := NewCodexEgress(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := egress.HTTP.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Synthetic TLS origin only.
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL, nil)
	request.Header.Set("Authorization", "Bearer synthetic")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "data: first\n\n" || calls.Load() != 1 {
		t.Fatalf("stream %q, proxy calls %d", first, calls.Load())
	}
}

func TestCodexEgressWebSocketThroughProxy(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("origin auth changed")
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteMessage(websocket.TextMessage, []byte("proxied"))
	}))
	defer origin.Close()
	var calls atomic.Int32
	proxy := newCodexEgressTestProxy(t, &calls)
	defer proxy.Close()
	egress, err := NewCodexEgress(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	egress.WebSocket.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Synthetic TLS origin only.
	connection, response, err := egress.WebSocket.Dial("wss"+strings.TrimPrefix(origin.URL, "https"), http.Header{"Authorization": {"Bearer synthetic"}})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, body, err := connection.ReadMessage()
	if err != nil || string(body) != "proxied" || calls.Load() != 1 {
		t.Fatalf("websocket %q, calls %d: %v", body, calls.Load(), err)
	}
}

func TestCodexEgressHTTPSProxyWebSocket(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			_ = conn.WriteMessage(websocket.TextMessage, []byte("tls proxy"))
			_ = conn.Close()
		}
	}))
	defer upstream.Close()
	var calls atomic.Int32
	plainProxy := newCodexEgressTestProxy(t, &calls)
	defer plainProxy.Close()
	tlsProxy := httptest.NewUnstartedServer(plainProxy.Config.Handler)
	tlsProxy.EnableHTTP2 = true
	tlsProxy.StartTLS()
	defer tlsProxy.Close()
	egress, err := NewCodexEgress(tlsProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	egress.WebSocket.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	wsURL := "wss" + strings.TrimPrefix(upstream.URL, "https")
	conn, response, err := egress.WebSocket.Dial(wsURL, nil)
	if conn != nil {
		conn.Close()
	}
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("accepted untrusted proxy certificate")
	}
	proxyTLS := tlsProxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	proxyTLS.NextProtos = []string{"h2", "http/1.1"}
	egress, err = newCodexEgress(tlsProxy.URL, proxyTLS)
	if err != nil {
		t.Fatal(err)
	}
	egress.WebSocket.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	conn, response, err = egress.WebSocket.Dial(wsURL, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, body, err := conn.ReadMessage()
	if err != nil || string(body) != "tls proxy" || calls.Load() != 1 {
		t.Fatalf("TLS proxy %q calls %d: %v", body, calls.Load(), err)
	}
}

func TestCodexEgressProxyFailureNeverFallsBack(t *testing.T) {
	var originCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { originCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer origin.Close()
	for _, status := range []int{http.StatusBadGateway, http.StatusProxyAuthRequired} {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		egress, err := NewCodexEgress(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		egress.HTTP.(*http.Transport).TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		egress.WebSocket.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		request, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
		response, err := egress.HTTP.RoundTrip(request)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("HTTP accepted failed CONNECT")
		}
		connection, response, err := egress.WebSocket.Dial("wss"+strings.TrimPrefix(origin.URL, "https"), nil)
		if connection != nil {
			connection.Close()
		}
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("WebSocket accepted failed CONNECT")
		}
		proxy.Close()
		// The same explicit configuration must also fail closed when unreachable.
		_, err = egress.HTTP.RoundTrip(request)
		if err == nil {
			t.Fatal("unreachable proxy accepted")
		}
		connection, response, err = egress.WebSocket.Dial("wss"+strings.TrimPrefix(origin.URL, "https"), nil)
		if connection != nil {
			connection.Close()
		}
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("unreachable WebSocket proxy accepted")
		}
	}
	if originCalls.Load() != 0 {
		t.Fatalf("direct origin requests: %d", originCalls.Load())
	}
}
