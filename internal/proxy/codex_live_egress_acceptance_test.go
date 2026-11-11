//go:build !windows

package proxy

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

const codexLiveEgressPortEnvironment = "CQ_CODEX_LIVE_EGRESS_PORT"

type codexLiveAcceptanceEgress struct {
	http              http.RoundTripper
	websocket         websocket.Dialer
	proxyURL          string
	proxyEndpoint     string
	candidateProxyURL string
}

// Live release checks borrow the installed runtime's selected route. They never
// read a WireGuard profile, start another tunnel, or fall back after broker failure.
func newCodexLiveAcceptanceEgress(t *testing.T) codexLiveAcceptanceEgress {
	t.Helper()
	value := os.Getenv(codexLiveEgressPortEnvironment)
	egress, err := codexLiveAcceptanceRuntime(value, func(selectedPort int) (*CodexEgress, error) {
		return OpenRuntimeEgressClient(selectedPort, fsutil.OSFileSystem{})
	})
	if err != nil {
		t.Fatal("configured live acceptance egress unavailable")
	}
	token, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		egress.Close()
		t.Fatal("create live acceptance relay authority")
	}
	t.Cleanup(func() { egress.Close() })
	relay := newCodexLiveAcceptanceRelay(t, egress, token, true)
	candidateRelay := newCodexLiveAcceptanceRelay(t, egress, token, false)
	u, _ := url.Parse(relay.URL)
	u.User = url.UserPassword("cq-live-token", token)
	return codexLiveAcceptanceEgress{http: egress.HTTP, websocket: egress.WebSocket, proxyURL: u.String(), proxyEndpoint: relay.URL, candidateProxyURL: candidateRelay.URL}
}

func codexLiveAcceptanceRelay(dialer EgressDialer, token string, authenticated bool) http.Handler {
	broker := NewEgressBroker(dialer, token, IsCodexServiceTarget)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		clone.Header.Set("Authorization", r.Header.Get("Proxy-Authorization"))
		user, password, ok := clone.BasicAuth()
		if authenticated && (!ok || user != "cq-live-token" || subtle.ConstantTimeCompare([]byte(password), []byte(token)) != 1) {
			http.Error(w, "live acceptance relay authentication failed", http.StatusProxyAuthRequired)
			return
		}
		clone.Header = make(http.Header)
		clone.Header.Set("Authorization", "Bearer "+token)
		broker.ServeHTTP(w, clone)
	})
}

func (e codexLiveAcceptanceEgress) environment(environment []string) []string {
	if e.proxyURL == "" {
		return environment
	}
	return append(environment, "HTTP_PROXY="+e.proxyURL, "HTTPS_PROXY="+e.proxyURL, "ALL_PROXY="+e.proxyURL,
		"http_proxy="+e.proxyURL, "https_proxy="+e.proxyURL, "all_proxy="+e.proxyURL,
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
}

type codexLiveFailingEgress struct{ calls atomic.Int32 }

func (d *codexLiveFailingEgress) DialContext(context.Context, string, string) (net.Conn, error) {
	d.calls.Add(1)
	return nil, errors.New("selected route unavailable")
}

func TestCodexLiveAcceptanceRelayFailedClosed(t *testing.T) {
	token := "synthetic-live-relay-authority-123456789"
	selected := &codexLiveFailingEgress{}
	relay := httptest.NewServer(codexLiveAcceptanceRelay(selected, token, true))
	defer relay.Close()
	for _, tt := range []struct {
		name, target, password string
		status                 int
		calls                  int
	}{
		{"missing auth", "chatgpt.com:443", "", 407, 0},
		{"foreign target", "other.invalid:443", token, 403, 0},
		{"selected route failure", "chatgpt.com:443", token, 502, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodConnect, relay.URL, nil)
			req.URL.Opaque = tt.target
			req.Host = tt.target
			if tt.password != "" {
				req.SetBasicAuth("cq-live-token", tt.password)
				req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
				req.Header.Del("Authorization")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.status || selected.calls.Load() != int32(tt.calls) {
				t.Fatalf("status=%d selected calls=%d", resp.StatusCode, selected.calls.Load())
			}
		})
	}
}

func codexLiveAcceptanceRuntime(value string, open func(int) (*CodexEgress, error)) (*CodexEgress, error) {
	if value == "" {
		return nil, errors.New("live acceptance requires an explicit runtime egress port")
	}
	port, err := codexLiveAcceptancePort(value)
	if err != nil {
		return nil, err
	}
	return open(port)
}

func codexLiveAcceptancePort(value string) (int, error) {
	if value == "" {
		return DefaultPort, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("invalid live acceptance egress port")
	}
	return port, nil
}

func TestCodexLiveAcceptanceRuntimeFailedClosed(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "65536", "bad", "4567"} {
		t.Run(value, func(t *testing.T) {
			calls := 0
			result, err := codexLiveAcceptanceRuntime(value, func(port int) (*CodexEgress, error) {
				calls++
				if port != 4567 {
					t.Fatal("wrong runtime selected")
				}
				return nil, errors.New("broker unavailable")
			})
			if err == nil || result != nil || (value == "4567" && calls != 1) || (value != "4567" && calls != 0) {
				t.Fatalf("unavailable configured route accepted: calls=%d", calls)
			}
		})
	}
}

type codexLiveEchoEgress struct {
	calls  atomic.Int32
	target atomic.Value
}

func (d *codexLiveEchoEgress) DialContext(_ context.Context, network, target string) (net.Conn, error) {
	d.calls.Add(1)
	d.target.Store(target)
	if network != "tcp" {
		return nil, errors.New("unexpected network")
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		defer func() { _ = recover() }()
		_, _ = io.WriteString(server, "selected-route")
	}()
	return client, nil
}

func TestCodexLiveAcceptanceRelayUsedSelectedRoute(t *testing.T) {
	token := "synthetic-live-relay-authority-123456789"
	selected := &codexLiveEchoEgress{}
	relay := httptest.NewServer(codexLiveAcceptanceRelay(selected, token, true))
	defer relay.Close()
	u, _ := url.Parse(relay.URL)
	u.User = url.UserPassword("cq-live-token", token)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := connectEgressProxy(ctx, u, nil, "tcp", "chatgpt.com:443")
	if err != nil {
		t.Fatal("selected relay connection failed")
	}
	defer conn.Close()
	data := make([]byte, len("selected-route"))
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "selected-route" {
		t.Fatal("relay bypassed selected route")
	}
	if selected.calls.Load() != 1 || selected.target.Load() != "chatgpt.com:443" {
		t.Fatal("relay changed selected destination")
	}
}

func newCodexLiveAcceptanceRelay(t *testing.T, dialer EgressDialer, token string, authenticated bool) *httptest.Server {
	t.Helper()
	life, cancel := context.WithCancel(context.Background())
	handler := codexLiveAcceptanceRelay(dialer, token, authenticated)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancelRequest := context.WithCancel(r.Context())
		defer cancelRequest()
		stop := context.AfterFunc(life, cancelRequest)
		defer stop()
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(func() { cancel(); relay.Close() })
	return relay
}

func TestCodexLiveCandidateRelayFailedClosed(t *testing.T) {
	token := "synthetic-live-relay-authority-123456789"
	selected := &codexLiveFailingEgress{}
	relay := newCodexLiveAcceptanceRelay(t, selected, token, false)
	if err := ValidateCodexEgressProxy(relay.URL); err != nil {
		t.Fatal("candidate relay configuration rejected")
	}
	for _, tt := range []struct {
		name, method, target string
		status, calls        int
	}{
		{"ordinary HTTP", http.MethodGet, "chatgpt.com:443", 405, 0},
		{"foreign target", http.MethodConnect, "other.invalid:443", 403, 0},
		{"wrong port", http.MethodConnect, "chatgpt.com:80", 403, 0},
		{"selected route failure", http.MethodConnect, "chatgpt.com:443", 502, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, relay.URL, nil)
			if tt.method == http.MethodConnect {
				req.URL.Opaque = tt.target
				req.Host = tt.target
			}
			req.Header.Set("Authorization", "Bearer must-not-forward")
			req.Header.Set("Proxy-Authorization", "must-not-forward")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.status || selected.calls.Load() != int32(tt.calls) {
				t.Fatalf("status=%d calls=%d", resp.StatusCode, selected.calls.Load())
			}
		})
	}
}

type codexLiveHeldEgress struct{ peers chan net.Conn }

func (d *codexLiveHeldEgress) DialContext(context.Context, string, string) (net.Conn, error) {
	client, peer := net.Pipe()
	d.peers <- peer
	return client, nil
}

func TestCodexLiveCandidateRelayClosedTunnels(t *testing.T) {
	selected := &codexLiveHeldEgress{peers: make(chan net.Conn, 1)}
	var conn net.Conn
	t.Run("fixture", func(t *testing.T) {
		relay := newCodexLiveAcceptanceRelay(t, selected, "synthetic-live-relay-authority-123456789", false)
		egress, err := NewCodexEgress(relay.URL)
		if err != nil {
			t.Fatal("candidate relay rejected")
		}
		t.Cleanup(func() { egress.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn, err = egress.DialContext(ctx, "tcp", "chatgpt.com:443")
		if err != nil {
			t.Fatal("candidate relay connection failed")
		}
	})
	if conn == nil {
		t.Fatal("missing connected tunnel")
	}
	var peer net.Conn
	select {
	case peer = <-selected.peers:
	case <-time.After(time.Second):
		t.Fatal("missing selected route peer")
	}
	defer conn.Close()
	defer peer.Close()
	for _, side := range []net.Conn{conn, peer} {
		_ = side.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := side.Read(make([]byte, 1)); err == nil {
			t.Fatal("fixture left tunnel open")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("fixture cleanup did not close tunnel")
		}
	}
}

func TestCodexLiveAcceptanceSelectedPort(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  int
	}{{"", DefaultPort}, {"4567", 4567}, {"1", 1}, {"65535", 65535}} {
		got, err := codexLiveAcceptancePort(tt.value)
		if err != nil || got != tt.want {
			t.Fatalf("selected port=%d err=%v", got, err)
		}
	}
	for _, value := range []string{"false", "0", "-1", "65536", "4567.5"} {
		if _, err := codexLiveAcceptancePort(value); err == nil {
			t.Fatal("invalid selected port accepted")
		}
	}
}
