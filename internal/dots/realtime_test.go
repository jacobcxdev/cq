//go:build darwin || linux

package dots

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

func realtimeTestTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Realtime test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"realtime.chatgpt.com", "ws.chatgpt.com"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}, MinVersion: tls.VersionTLS12}, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func realtimeTestProxy(t *testing.T, handler http.Handler) (*realtimeProxy, *http.Client, *httptest.Server) {
	t.Helper()
	serverTLS, publicTLS := realtimeTestTLS(t)
	upstream := httptest.NewUnstartedServer(handler)
	upstream.TLS = serverTLS
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "realtime.chatgpt.com:443" && address != "ws.chatgpt.com:443" {
			return nil, errors.New("Unexpected test dial")
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	s := &Settings{RuntimeRoot: t.TempDir(), SystemAlias: "primary", IntegrationMode: "native", Dots: map[string]Dot{"secondary": {OwnerAlias: "secondary", ProfileID: "secondary-profile", RootThreadID: "secondary-root"}}}
	accounts := Inventory{System: Account{Alias: "primary", AccountID: "primary-id", Token: "primary-token"}, Accounts: []Account{{Alias: "primary", AccountID: "primary-id", Token: "primary-token"}, {Alias: "secondary", AccountID: "secondary-id", Token: "secondary-token"}}}
	httpClient := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: dial, TLSClientConfig: publicTLS}, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	proxy, err := newRealtimeProxy(s, realtimeDependencies{inventory: func() (Inventory, error) { return accounts, nil }, require: func(_ context.Context, h http.Header) error {
		if h.Get("Authorization") != "Bearer primary-token" || h.Get("ChatGPT-Account-Id") != "primary-id" {
			return errors.New("Native mismatch")
		}
		return nil
	}, roots: func(context.Context, map[string]Account, http.Header) (map[string]RootOwner, error) {
		return nil, errors.New("Unexpected roots refresh")
	}, dial: dial, http: httpClient, publicTLS: publicTLS, signallingTLS: serverTLS, event: func(map[string]any) {}})
	if err != nil {
		t.Fatal(err)
	}
	connect := httptest.NewServer(proxy)
	t.Cleanup(connect.Close)
	proxyURL, _ := url.Parse(connect.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: publicTLS}, Timeout: 4 * time.Second}
	t.Cleanup(func() { client.CloseIdleConnections(); httpClient.CloseIdleConnections() })
	return proxy, client, connect
}

func realtimeTestRequest(t *testing.T, client *http.Client, path string, headers http.Header, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://realtime.chatgpt.com"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header = headers
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func realtimeNativeHeaders() http.Header {
	return http.Header{"Authorization": []string{"Bearer primary-token"}, "Chatgpt-Account-Id": []string{"primary-id"}, "Content-Type": []string{"application/sdp"}}
}

func TestRealtimeSignallingUsesVerifiedOwnerAndStripsIdentity(t *testing.T) {
	seen := make(chan *http.Request, 1)
	_, client, _ := realtimeTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := httputil.ReadBody(r.Body)
		if string(data) != "v=0 offer" {
			t.Error("signalling body changed")
		}
		seen <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Set-Cookie", "not-forwarded")
		_, _ = io.WriteString(w, "v=0 answer")
	}))
	headers := realtimeNativeHeaders()
	headers.Set("Cookie", "primary-cookie")
	headers.Set("X-OpenAI-Account-Routing-Override", "primary-id")
	headers.Set("X-OpenAI-Expected-Account-Id", "primary-id")
	status, body := realtimeTestRequest(t, client, "/v1/cua?source=aeon&thread_id=secondary-root&aeon_id=secondary-profile", headers, "v=0 offer")
	if status != 200 || body != "v=0 answer" {
		t.Fatalf("status %d body %q", status, body)
	}
	request := <-seen
	if request.Header.Get("Authorization") != "Bearer secondary-token" || request.Header.Get("ChatGPT-Account-Id") != "secondary-id" {
		t.Fatal("owner identity not substituted")
	}
	for _, name := range []string{"Cookie", "X-OpenAI-Account-Routing-Override", "X-OpenAI-Expected-Account-Id"} {
		if request.Header.Get(name) != "" {
			t.Errorf("retained %s", name)
		}
	}
}

func TestRealtimeRejectsUnknownConflictingAndMalformedDotResources(t *testing.T) {
	var upstream atomic.Int32
	_, client, _ := realtimeTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstream.Add(1); w.WriteHeader(500) }))
	for _, query := range []string{
		"source=aeon&thread_id=unknown&aeon_id=secondary-profile",
		"source=aeon&thread_id=secondary-root&aeon_id=wrong-profile",
		"source=aeon&thread_id=secondary-root&aeon_id=secondary-profile&extra=x",
		"source=aeon&thread_id=secondary-root&thread_id=other&aeon_id=secondary-profile",
	} {
		status, _ := realtimeTestRequest(t, client, "/v1/cua?"+query, realtimeNativeHeaders(), "v=0")
		if status != 403 {
			t.Fatalf("query returned %d", status)
		}
	}
	headers := realtimeNativeHeaders()
	headers.Add("Authorization", "Bearer other")
	status, _ := realtimeTestRequest(t, client, "/v1/cua?source=aeon&thread_id=secondary-root&aeon_id=secondary-profile", headers, "v=0")
	if status != 403 {
		t.Fatalf("duplicate bearer returned %d", status)
	}
	headers = realtimeNativeHeaders()
	headers.Set("Authorization", "Bearer invalid-native-token")
	status, _ = realtimeTestRequest(t, client, "/v1/cua?source=aeon&thread_id=secondary-root&aeon_id=secondary-profile", headers, "v=0")
	if status != 403 {
		t.Fatalf("invalid native identity returned %d", status)
	}
	if upstream.Load() != 0 {
		t.Fatalf("upstream called %d times", upstream.Load())
	}
}

func TestRealtimeRefreshesRootOnlyForConfiguredProfile(t *testing.T) {
	p, _, _ := realtimeTestProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	accounts, _ := p.accounts()
	var refresh int
	p.dependencies.roots = func(context.Context, map[string]Account, http.Header) (map[string]RootOwner, error) {
		refresh++
		return map[string]RootOwner{"new-root": {OwnerAlias: "secondary", ProfileID: "secondary-profile"}}, nil
	}
	request, _ := http.NewRequest(http.MethodPost, "/v1/cua?source=aeon&thread_id=new-root&aeon_id=secondary-profile", nil)
	request.Header.Set("Content-Type", "application/sdp")
	owner, err := p.signallingOwner(context.Background(), request, accounts)
	if err != nil || owner != "secondary" || refresh != 1 {
		t.Fatalf("owner %q refresh %d error %v", owner, refresh, err)
	}
	request.URL.RawQuery = "source=aeon&thread_id=unknown&aeon_id=unknown"
	if _, err := p.signallingOwner(context.Background(), request, accounts); err == nil || refresh != 1 {
		t.Fatal("unconfigured profile refreshed or accepted")
	}
	p.dependencies.roots = func(context.Context, map[string]Account, http.Header) (map[string]RootOwner, error) {
		return map[string]RootOwner{"secondary-root": {OwnerAlias: "primary", ProfileID: "secondary-profile"}, "later-root": {OwnerAlias: "secondary", ProfileID: "secondary-profile"}}, nil
	}
	request.URL.RawQuery = "source=aeon&thread_id=later-root&aeon_id=secondary-profile"
	if _, err := p.signallingOwner(context.Background(), request, accounts); err == nil {
		t.Fatal("changed root owner accepted")
	}
	if _, ok := p.resources["later-root"]; ok {
		t.Fatal("partial conflicting root refresh persisted")
	}
}

func TestRealtimeNativeUnknownHTTPStreamsUnchanged(t *testing.T) {
	release := make(chan struct{})
	seen := make(chan *http.Request, 1)
	_, client, _ := realtimeTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second")
	}))
	request, _ := http.NewRequest(http.MethodGet, "https://realtime.chatgpt.com/ordinary", nil)
	request.Header.Set("Authorization", "Bearer opaque-native-token")
	request.Header.Set("Cookie", "native-cookie")
	response, err := client.Do(request)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, 5)
	_, err = io.ReadFull(response.Body, first)
	close(release)
	if err != nil || string(first) != "first" {
		t.Fatalf("first stream %q %v", first, err)
	}
	remaining, err := httputil.ReadBody(response.Body)
	if err != nil || string(remaining) != "second" {
		t.Fatalf("rest %q %v", remaining, err)
	}
	got := <-seen
	if got.Header.Get("Authorization") != "Bearer opaque-native-token" || got.Header.Get("Cookie") != "native-cookie" || got.Header.Get("Connection") != "close" {
		t.Fatal("ordinary identity or connection changed")
	}
}

func TestRealtimeCelsiusHandshakeAndFramesRemainVerbatim(t *testing.T) {
	p, _, connect := realtimeTestProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("Celsius reached public upstream") }))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	p.dependencies.celsiusAddress = func() (string, error) { return listener.Addr().String(), nil }
	baseDial := p.dependencies.dial
	p.dependencies.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == listener.Addr().String() {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		return baseDial(ctx, network, address)
	}
	path := "/cq-owner-events/" + strings.Repeat("a", 43)
	raw := "GET " + path + " HTTP/1.1\r\nhOsT: ws.chatgpt.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Proof: preserve-header-order\r\n\r\n"
	frames := []byte{0x82, 3, 1, 2, 3}
	captured := make(chan []byte, 1)
	go func() {
		defer func() { _ = recover() }()
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(connection)
		headers, _, err := realtimeRequest(reader)
		if err != nil {
			return
		}
		body := make([]byte, len(frames))
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		captured <- append(headers, body...)
		_, _ = io.WriteString(connection, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = connection.Write(frames)
	}()
	proxyURL, _ := url.Parse(connect.URL)
	connection, err := net.DialTimeout("tcp", proxyURL.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(4 * time.Second))
	_, _ = io.WriteString(connection, "CONNECT ws.chatgpt.com:443 HTTP/1.1\r\nHost: ws.chatgpt.com:443\r\n\r\n")
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT response %v %v", response, err)
	}
	tlsClientConfig := p.dependencies.publicTLS.Clone()
	tlsClientConfig.ServerName = "ws.chatgpt.com"
	tlsClient := tls.Client(&realtimeBufferedConn{connection, reader}, tlsClientConfig)
	if err := tlsClient.Handshake(); err != nil {
		t.Fatal(err)
	}
	_, _ = tlsClient.Write(append([]byte(raw), frames...))
	upgraded := bufio.NewReader(tlsClient)
	response, err = http.ReadResponse(upgraded, &http.Request{Method: http.MethodGet})
	if err != nil || response.StatusCode != 101 {
		t.Fatalf("upgrade %v %v", response, err)
	}
	gotFrames := make([]byte, len(frames))
	if _, err := io.ReadFull(upgraded, gotFrames); err != nil || !bytes.Equal(gotFrames, frames) {
		t.Fatal("WebSocket frames changed")
	}
	select {
	case got := <-captured:
		if !bytes.Equal(got, append([]byte(raw), frames...)) {
			t.Fatal("WebSocket handshake or frames rewritten")
		}
	case <-time.After(time.Second):
		t.Fatal("Missing handshake capture")
	}
}

func TestRealtimeOpaqueConnectPreservesTraffic(t *testing.T) {
	p, _, connect := realtimeTestProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	p.dependencies.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "example.com:443" {
			return nil, errors.New("Unexpected destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}
	go func() {
		defer func() { _ = recover() }()
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		data := make([]byte, 5)
		if _, err := io.ReadFull(peer, data); err == nil {
			_, _ = peer.Write(data)
		}
	}()
	proxyURL, _ := url.Parse(connect.URL)
	peer, err := net.DialTimeout("tcp", proxyURL.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(peer, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	reader := bufio.NewReader(peer)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT %v %v", response, err)
	}
	_, _ = io.WriteString(peer, "proof")
	data := make([]byte, 5)
	if _, err := io.ReadFull(reader, data); err != nil || string(data) != "proof" {
		t.Fatalf("opaque tunnel %q %v", data, err)
	}
}

func TestRealtimeRejectsInvalidAuthoritiesAndHandshakeBounds(t *testing.T) {
	for _, authority := range []string{"localhost:443", "127.0.0.1:443", "127.0.0.2:443", "[::1]:443", "example.com:80", "user@example.com:443", "example.com:443/path"} {
		if _, err := realtimeAuthority(authority); err == nil {
			t.Fatalf("accepted authority %q", authority)
		}
	}
	for _, raw := range []string{"GET / HTTP/1.1\r\n", "GET / HTTP/1.1\r\nX: " + strings.Repeat("a", 65536) + "\r\n\r\n"} {
		if _, _, err := realtimeRequest(bufio.NewReaderSize(strings.NewReader(raw), 65536)); err == nil {
			t.Fatal("unbounded or truncated handshake accepted")
		}
	}
}

func TestRealtimePortAndMetadataStayPrivate(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "realtime-port")
	if port, err := realtimePort(path, root, true); err != nil || port != 0 {
		t.Fatalf("fresh port %d %v", port, err)
	}
	for _, data := range []string{"0", "65536", "notaport"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := realtimePort(path, root, false); err == nil {
			t.Fatal("invalid port accepted")
		}
	}
	if err := os.WriteFile(path, []byte("54321"), 0o600); err != nil {
		t.Fatal(err)
	}
	if port, err := realtimePort(path, root, false); err != nil || port != 54321 {
		t.Fatalf("saved port %d %v", port, err)
	}
	event := realtimeEventSink(root)
	event(map[string]any{"signalling_rejected": true})
	file := filepath.Join(root, "realtime-events.jsonl")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata permissions %v %v", info, err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	event(map[string]any{"upgraded": true})
	data, err := os.ReadFile(file)
	info, statErr := os.Stat(file)
	if err != nil || statErr != nil || info.Mode().Perm() != 0o600 || !bytes.HasPrefix(data, []byte("legacy\n")) || !bytes.Contains(data, []byte(`"upgraded":true`)) {
		t.Fatalf("legacy diagnostic log not migrated: %q, %v, %v", data, err, statErr)
	}
	if err := os.Chmod(file, 0o666); err != nil {
		t.Fatal(err)
	}
	event(map[string]any{"unsafe_permissions": true})
	data, err = os.ReadFile(file)
	if err != nil || bytes.Contains(data, []byte("unsafe_permissions")) {
		t.Fatal("unsafe diagnostic log accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "protected")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	event(map[string]any{"should_not_write": true})
	data, _ = os.ReadFile(target)
	if string(data) != "untouched" {
		t.Fatal("log symlink followed")
	}
}

func TestRealtimeServeClosesOnContextCancellation(t *testing.T) {
	p, _, _ := realtimeTestProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- fmt.Errorf("serve panic")
			}
		}()
		done <- p.serve(ctx, listener)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy stayed alive after cancellation")
	}
}
