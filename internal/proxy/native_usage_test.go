package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	cqhttp "github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/nativeusage"
)

const nativeUsageTestBody = `{"credits":{"balance":"0","has_credits":false},"unknown":{"sequence":9007199254740993}}`

func TestNativeUsageFailureOmitsPrivateDetails(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"closed", io.EOF, "connection closed"},
		{"cancelled", fmt.Errorf("private detail: %w", context.Canceled), "connection closed"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"upstream trust", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "upstream certificate rejected"},
		{"peer trust", errors.New("remote error: tls: unknown certificate authority"), "client certificate rejected"},
		{"peer certificate", errors.New("remote error: tls: bad certificate"), "client certificate rejected"},
		{"protocol", errors.New(`tls: client requested unsupported application protocols (["private-value"])`), "application protocol mismatch"},
		{"unknown", errors.New("request https://private.invalid/?token=private-value failed"), "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeUsageFailure(test.err); got != test.want {
				t.Fatalf("classification = %q; want %q", got, test.want)
			}
		})
	}
}

type nativeUsageTestRoute struct {
	EgressDialer
	ready bool
}

func (r nativeUsageTestRoute) NativeReady() bool { return r.ready }

type nativeUsageTestFixture struct {
	root   string
	client *tls.Config
	dial   func(context.Context, string, string) (net.Conn, error)
	cancel context.CancelFunc
	dials  atomic.Int32
}

func newNativeUsageTestFixture(t *testing.T, handler http.Handler) *nativeUsageTestFixture {
	t.Helper()
	f := &nativeUsageTestFixture{root: filepath.Join(t.TempDir(), "usage")}
	fs := fsutil.OSFileSystem{}
	info, err := nativeusage.Prepare(f.root, fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeusage.SetEnabled(f.root, fs, true); err != nil {
		t.Fatal(err)
	}
	serverTLS, err := nativeusage.LoadTLS(f.root, fs)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := os.ReadFile(info.CertificatePath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		t.Fatal("test certificate could not be loaded")
	}
	f.client = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	origin := httptest.NewUnstartedServer(handler)
	origin.TLS = serverTLS.Clone()
	origin.StartTLS()
	t.Cleanup(origin.Close)
	base := nativeUsageTestRoute{ready: true, EgressDialer: egressBrokerTestDialer(func(ctx context.Context, network, target string) (net.Conn, error) {
		if network != "tcp" || target != "chatgpt.com:443" && target != "ws.chatgpt.com:443" {
			return nil, fmt.Errorf("unexpected native route: %s %s", network, target)
		}
		f.dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	})}
	adapter := NewNativeUsageDialer(base, f.root, fs).(*nativeUsageDialer)
	adapter.publicTLS = f.client
	adapter.event = func(string) {}
	socket, cancel := egressBrokerTestServe(t, NewNativeEgressBroker(adapter, "native-usage-test-token"))
	f.cancel = cancel
	f.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
		if network != "tcp" {
			return nil, fmt.Errorf("unexpected client network %s", network)
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\nAuthorization: Bearer native-usage-test-token\r\nX-CQ-Native-Flow: 1\r\nX-CQ-Native-Network: tcp\r\n\r\n")
		reader := bufio.NewReader(conn)
		if err == nil {
			var response *http.Response
			response, err = http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
			if err == nil && response.StatusCode != http.StatusOK {
				err = fmt.Errorf("native CONNECT status %d", response.StatusCode)
			}
		}
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return &nativeUsageTestBufferedConn{Conn: conn, reader: reader}, nil
	}
	return f
}

type nativeUsageTestBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *nativeUsageTestBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func TestNativeUsageHTTPSOverAuthenticatedConnect(t *testing.T) {
	var identityEncoding atomic.Int32
	f := newNativeUsageTestFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-user" || r.Host != "chatgpt.com" {
			t.Error("request identity or host was not preserved")
		}
		if r.Method == http.MethodGet && r.URL.Path == "/backend-api/wham/usage" && r.Header.Get("Accept-Encoding") == "identity" {
			identityEncoding.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"origin-etag"`)
		w.Header().Set("Content-Digest", "origin-digest")
		w.Header().Set("Content-Length", strconv.Itoa(len(nativeUsageTestBody)))
		if r.URL.Query().Get("status") == "429" {
			w.WriteHeader(http.StatusTooManyRequests)
		}
		_, _ = io.WriteString(w, nativeUsageTestBody)
	}))
	transport := &http.Transport{Proxy: nil, DialContext: f.dial, TLSClientConfig: f.client, DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, test := range []struct {
		name, method, path string
		status             int
		changed            bool
		disable            bool
	}{
		{"usage", "GET", "/backend-api/wham/usage?refresh=1", 200, true, false},
		{"other path", "GET", "/backend-api/wham/other", 200, false, false},
		{"other method", "POST", "/backend-api/wham/usage", 200, false, false},
		{"upstream error", "GET", "/backend-api/wham/usage?status=429", 429, false, false},
		{"disabled existing connection", "GET", "/backend-api/wham/usage", 200, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.disable {
				if err := nativeusage.SetEnabled(f.root, fsutil.OSFileSystem{}, false); err != nil {
					t.Fatal(err)
				}
			}
			req, err := http.NewRequest(test.method, "https://chatgpt.com"+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer test-user")
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := cqhttp.ReadBody(response.Body)
			if err != nil || response.StatusCode != test.status {
				t.Fatalf("response status=%d error=%v", response.StatusCode, err)
			}
			if !test.changed {
				if string(body) != nativeUsageTestBody || response.Header.Get("ETag") != `"origin-etag"` {
					t.Fatal("unrelated upstream response changed")
				}
				return
			}
			want := strings.Replace(nativeUsageTestBody, `"balance":"0","has_credits":false`, `"balance":"1","has_credits":true`, 1)
			nativeUsageAssertJSON(t, body, []byte(want))
			if response.ContentLength != int64(len(body)) || response.Header.Get("ETag") != "" || response.Header.Get("Content-Digest") != "" {
				t.Fatal("adapted response retained stale length or validators")
			}
		})
	}
	if identityEncoding.Load() != 3 || f.dials.Load() == 0 {
		t.Fatal("usage request did not traverse authenticated native route with identity encoding")
	}
}

func TestNativeUsageWebSocketPreservesUpstreamRejection(t *testing.T) {
	f := newNativeUsageTestFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("WWW-Authenticate", `Bearer realm="usage"`)
		http.Error(w, "session expired", http.StatusUnauthorized)
	}))
	dialer := websocket.Dialer{NetDialContext: f.dial, TLSClientConfig: f.client, HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.DialContext(context.Background(), "wss://ws.chatgpt.com/usage", nil)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("rejected upstream was accepted as websocket")
	}
	if !errors.Is(err, websocket.ErrBadHandshake) || response == nil {
		t.Fatalf("missing upstream rejection: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("Retry-After") != "30" || response.Header.Get("WWW-Authenticate") != `Bearer realm="usage"` {
		t.Fatalf("upstream rejection changed: status=%d retry=%q authentication=%q", response.StatusCode, response.Header.Get("Retry-After"), response.Header.Get("WWW-Authenticate"))
	}
}

func TestNativeUsageWebSocketPreservesLargeRejectionBodies(t *testing.T) {
	var plain bytes.Buffer
	for i := range 512 {
		fmt.Fprintf(&plain, "session expired %04d %08x; refresh required\n", i, uint32(i)*2654435761)
	}
	for _, encoding := range []string{"", "gzip"} {
		name := encoding
		if name == "" {
			name = "plain"
		}
		t.Run(name, func(t *testing.T) {
			body := plain.Bytes()
			if encoding == "gzip" {
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(body); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				body = compressed.Bytes()
			}
			if len(body) <= 1024 {
				t.Fatal("rejection fixture must exceed the websocket dialer's diagnostic prefix")
			}
			f := newNativeUsageTestFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "30")
				w.Header().Set("WWW-Authenticate", `Bearer realm="usage"`)
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				if encoding != "" {
					w.Header().Set("Content-Encoding", encoding)
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write(body)
			}))
			transport := &http.Transport{Proxy: nil, DialContext: f.dial, TLSClientConfig: f.client, DisableCompression: true}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			request, _ := http.NewRequest(http.MethodGet, "https://ws.chatgpt.com/usage", nil)
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			request.Header.Set("Sec-WebSocket-Version", "13")
			request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			got, err := cqhttp.ReadBody(response.Body)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("rejection body changed: got=%d want=%d error=%v", len(got), len(body), err)
			}
			if response.StatusCode != http.StatusUnauthorized || response.ContentLength != int64(len(body)) || response.Header.Get("Content-Encoding") != encoding || response.Uncompressed || response.Header.Get("Retry-After") != "30" || response.Header.Get("WWW-Authenticate") != `Bearer realm="usage"` {
				t.Fatalf("rejection metadata changed: status=%d length=%d encoding=%q retry=%q authentication=%q", response.StatusCode, response.ContentLength, response.Header.Get("Content-Encoding"), response.Header.Get("Retry-After"), response.Header.Get("WWW-Authenticate"))
			}
		})
	}
}

func TestNativeUsageWebSocketCancellationClosesStalledRejection(t *testing.T) {
	started := make(chan struct{})
	closed := make(chan struct{})
	release := make(chan struct{})
	f := newNativeUsageTestFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusUnauthorized)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(closed)
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })
	transport := &http.Transport{Proxy: nil, DialContext: f.dial, TLSClientConfig: f.client, DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://ws.chatgpt.com/usage", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	done := make(chan error, 1)
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				done <- fmt.Errorf("HTTP client panic: %v", failure)
			}
		}()
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream rejection headers were not received")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled rejection retained upstream until handshake timeout")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client cancellation changed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled HTTP client remained blocked")
	}
}

func TestNativeUsageRejectsMismatchedHostAndUntrustedCertificate(t *testing.T) {
	f := newNativeUsageTestFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("rejected request reached origin")
	}))
	transport := &http.Transport{Proxy: nil, DialContext: f.dial, TLSClientConfig: f.client}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	request.Host = "ws.chatgpt.com"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("mismatched Host status=%d", response.StatusCode)
	}
	raw, err := f.dial(context.Background(), "tcp", "chatgpt.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	conn := tls.Client(raw, &tls.Config{ServerName: "chatgpt.com", RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12})
	var unknown x509.UnknownAuthorityError
	if err := conn.Handshake(); !errors.As(err, &unknown) {
		t.Fatalf("untrusted certificate was not rejected: %v", err)
	}
	_ = conn.Close()
	if f.dials.Load() != 0 {
		t.Fatal("rejected TLS or Host request reached upstream route")
	}
}

func TestNativeUsageWebSocketPreservedTrafficAndDisable(t *testing.T) {
	type frame struct {
		kind int
		data []byte
	}
	received := make(chan frame, 1)
	frames := make(chan frame)
	upstreamErrors := make(chan error, 1)
	f := newNativeUsageTestFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, Subprotocols: []string{"usage.v1"}}
		conn, err := upgrader.Upgrade(w, r, http.Header{"Set-Cookie": {"usage-test=1"}})
		if err != nil {
			upstreamErrors <- err
			return
		}
		defer conn.Close()
		kind, body, err := conn.ReadMessage()
		if err != nil {
			upstreamErrors <- err
			return
		}
		received <- frame{kind, body}
		for next := range frames {
			if err := conn.WriteMessage(next.kind, next.data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(func() { close(frames) })
	dialer := websocket.Dialer{NetDialContext: f.dial, TLSClientConfig: f.client, Subprotocols: []string{"usage.v1"}, HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.DialContext(context.Background(), "wss://ws.chatgpt.com/usage?ticket=synthetic", http.Header{"Origin": {"https://chatgpt.com"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.Subprotocol() != "usage.v1" || response.Header.Get("Set-Cookie") != "usage-test=1" {
		t.Fatal("websocket negotiation metadata changed")
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	clientBody := []byte{0, 1, 255, 3}
	if err := conn.WriteMessage(websocket.BinaryMessage, clientBody); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got.kind != websocket.BinaryMessage || !bytes.Equal(got.data, clientBody) {
			t.Fatal("client websocket frame changed")
		}
	case err := <-upstreamErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("client frame did not reach upstream")
	}
	usage := `{"type":"message","topic_id":"codex-work-usage-push-v1","payload":{"type":"usage.snapshot","payload":{"sequence":9007199254740993,"usage_by_client":{"electron":` + nativeUsageTestBody + `,"browser":` + nativeUsageTestBody + `}}}}`
	unrelated := ` {"type":"message","topic_id":"other","payload":{"value":9007199254740993}} `
	for _, test := range []struct {
		name, body, want string
		disable          bool
	}{
		{"unrelated", unrelated, unrelated, false},
		{"usage batch", "[" + unrelated + "," + usage + "]", "[" + unrelated + "," + strings.Replace(usage, `"balance":"0","has_credits":false`, `"balance":"1","has_credits":true`, 1) + "]", false},
		{"disabled existing connection", usage, usage, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.disable {
				if err := nativeusage.SetEnabled(f.root, fsutil.OSFileSystem{}, false); err != nil {
					t.Fatal(err)
				}
			}
			frames <- frame{websocket.TextMessage, []byte(test.body)}
			kind, got, err := conn.ReadMessage()
			if err != nil || kind != websocket.TextMessage {
				t.Fatalf("websocket message kind=%d error=%v", kind, err)
			}
			if test.want == test.body {
				if string(got) != test.body {
					t.Fatal("unchanged websocket frame was re-encoded")
				}
			} else {
				nativeUsageAssertJSON(t, got, []byte(test.want))
			}
		})
	}
}

func TestNativeUsageWebSocketLargeMessagesAndUpstreamClose(t *testing.T) {
	for _, kind := range []int{websocket.TextMessage, websocket.BinaryMessage} {
		t.Run(map[int]string{websocket.TextMessage: "text", websocket.BinaryMessage: "binary"}[kind], func(t *testing.T) {
			type message struct {
				kind int
				body []byte
				err  error
			}
			received := make(chan message, 1)
			f := newNativeUsageTestFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					received <- message{err: err}
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				gotKind, body, err := conn.ReadMessage()
				received <- message{gotKind, body, err}
				if err == nil {
					_ = conn.WriteMessage(gotKind, body)
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4401, "session renewed"), time.Now().Add(time.Second))
				}
			}))
			dialer := websocket.Dialer{NetDialContext: f.dial, TLSClientConfig: f.client, HandshakeTimeout: 5 * time.Second}
			conn, _, err := dialer.DialContext(context.Background(), "wss://ws.chatgpt.com/other-events", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			body := bytes.Repeat([]byte("unrelated message "), (2<<20)/len("unrelated message ")+1024)
			if kind == websocket.BinaryMessage {
				body[0], body[len(body)-1] = 0, 255
			}
			if err := conn.WriteMessage(kind, body); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-received:
				if got.err != nil || got.kind != kind || !bytes.Equal(got.body, body) {
					t.Fatalf("large client message changed: kind=%d length=%d error=%v", got.kind, len(got.body), got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("large client message did not reach upstream")
			}
			gotKind, got, err := conn.ReadMessage()
			if err != nil || gotKind != kind || !bytes.Equal(got, body) {
				t.Fatalf("large upstream message changed: kind=%d length=%d error=%v", gotKind, len(got), err)
			}
			_, _, err = conn.ReadMessage()
			var closed *websocket.CloseError
			if !errors.As(err, &closed) || closed.Code != 4401 || closed.Text != "session renewed" {
				t.Fatalf("upstream close changed: %v", err)
			}
		})
	}
}

func TestNativeUsageWebSocketPreservesClientClose(t *testing.T) {
	received := make(chan error, 1)
	f := newNativeUsageTestFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			received <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err = conn.ReadMessage()
		received <- err
	}))
	dialer := websocket.Dialer{NetDialContext: f.dial, TLSClientConfig: f.client, HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(context.Background(), "wss://ws.chatgpt.com/other-events", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4410, "client complete"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-received:
		var closed *websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != 4410 || closed.Text != "client complete" {
			t.Fatalf("client close changed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client close did not reach upstream")
	}
}

func TestNativeUsageDialerOptInAndScope(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	fs := fsutil.OSFileSystem{}
	if _, err := nativeusage.Prepare(root, fs); err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var calls int
	base := nativeUsageTestRoute{ready: true, EgressDialer: egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) { calls++; return left, nil })}
	adapter := NewNativeUsageDialer(base, root, fs).(*nativeUsageDialer)
	if !adapter.NativeReady() {
		t.Fatal("native readiness was not delegated")
	}
	conn, err := adapter.DialContext(context.Background(), "tcp", "chatgpt.com:443")
	if err != nil || conn != left || calls != 1 {
		t.Fatal("preparation changed native routing before activation")
	}
	if err := nativeusage.SetEnabled(root, fs, true); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ network, target string }{{"tcp", "assets.example:443"}, {"tcp", "chatgpt.com:80"}, {"udp", "chatgpt.com:443"}, {"tcp", "chatgpt.com.example:443"}, {"tcp", "1.1.1.1:443"}} {
		conn, err := adapter.DialContext(context.Background(), test.network, test.target)
		if err != nil || conn != left {
			t.Fatalf("unrelated native route changed: %s %s", test.network, test.target)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls
	if conn, err := adapter.DialContext(ctx, "tcp", "chatgpt.com:443"); conn != nil || err != context.Canceled || calls != before {
		t.Fatal("cancelled adapted request reached upstream")
	}
}

func TestNativeUsageCancellationClosesTLSConnect(t *testing.T) {
	f := newNativeUsageTestFixture(t, http.NotFoundHandler())
	raw, err := f.dial(context.Background(), "tcp", "chatgpt.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	config := f.client.Clone()
	config.ServerName = "chatgpt.com"
	conn := tls.Client(raw, config)
	if err := conn.HandshakeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.cancel()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("cancelled native connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("native cancellation waited for read deadline")
	}
	if f.dials.Load() != 0 {
		t.Fatal("TLS connection without HTTP request opened an upstream")
	}
}

func TestNativeUsageResponsePreservesOpaqueAndOversizedBodies(t *testing.T) {
	for _, test := range []struct {
		name, body, encoding string
		length               int64
	}{
		{"malformed JSON", `{"credits":`, "", -1},
		{"other encoding", nativeUsageTestBody, "br", -1},
		{"known oversized", nativeUsageTestBody + strings.Repeat(" ", cqhttp.MaxResponseBody), "", int64(len(nativeUsageTestBody) + cqhttp.MaxResponseBody)},
		{"unknown oversized", nativeUsageTestBody + strings.Repeat(" ", cqhttp.MaxResponseBody), "", -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
			response := &http.Response{Request: request, StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(test.body)), ContentLength: test.length}
			response.Header.Set("ETag", "original")
			response.Header.Set("Content-Encoding", test.encoding)
			changed, err := nativeUsageResponse(response)
			if err != nil || changed {
				t.Fatalf("opaque response adaptation changed=%v error=%v", changed, err)
			}
			defer response.Body.Close()
			body, err := cqhttp.ReadBodyLimit(response.Body, 2*cqhttp.MaxResponseBody)
			if err != nil || string(body) != test.body || response.Header.Get("ETag") != "original" {
				t.Fatal("opaque response body or validators were lost")
			}
		})
	}
}

func nativeUsageAssertJSON(t *testing.T, got, want []byte) {
	t.Helper()
	decode := func(body []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(got), decode(want)) {
		t.Fatalf("usage JSON differed:\ngot: %s\nwant: %s", got, want)
	}
}
