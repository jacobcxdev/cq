package dots

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

var realtimeHost = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
var realtimeCelsiusPath = regexp.MustCompile(`^/cq-owner-events/[A-Za-z0-9_-]{43}$`)

type realtimeDependencies struct {
	inventory                func() (Inventory, error)
	require                  func(context.Context, http.Header) error
	roots                    func(context.Context, map[string]Account, http.Header) (map[string]RootOwner, error)
	dial                     func(context.Context, string, string) (net.Conn, error)
	http                     httputil.Doer
	publicTLS, signallingTLS *tls.Config
	celsiusAddress           func() (string, error)
	event                    func(map[string]any)
}

type realtimeProxy struct {
	settings     *Settings
	dependencies realtimeDependencies
	mu           sync.Mutex
	resources    map[string]RootOwner
}

func realtimeEventSink(root string) func(map[string]any) {
	var lock sync.Mutex
	return func(value map[string]any) {
		lock.Lock()
		defer lock.Unlock()
		if privateDirectory(root) != nil {
			return
		}
		file, err := openDotsPrivateLog(filepath.Join(root, "realtime-events.jsonl"))
		if err != nil {
			return
		}
		defer file.Close()
		row := make(map[string]any, len(value)+1)
		for key, field := range value {
			row[key] = field
		}
		row["at"] = float64(time.Now().UnixNano()) / 1e9
		_ = json.NewEncoder(file).Encode(row)
	}
}

func realtimeCertificate(s *Settings) (*tls.Config, error) {
	cert, err := readPrivate(filepath.Join(s.RuntimeRoot, "realtime-cert.pem"), s.RuntimeRoot, true)
	if err != nil {
		return nil, err
	}
	key, err := readPrivate(filepath.Join(s.RuntimeRoot, "leaf-key.pem"), s.RuntimeRoot, true)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, errors.New("Realtime interception certificate invalid")
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

func newRealtimeProxy(s *Settings, d realtimeDependencies) (*realtimeProxy, error) {
	if s == nil {
		return nil, errors.New("Dots settings unavailable")
	}
	if d.publicTLS == nil {
		var err error
		d.publicTLS, err = cloudPublicTLS()
		if err != nil {
			return nil, err
		}
	}
	if d.signallingTLS == nil {
		var err error
		d.signallingTLS, err = realtimeCertificate(s)
		if err != nil {
			return nil, err
		}
	}
	if d.http == nil {
		d.http = &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: dotsDialContext, TLSClientConfig: d.publicTLS, DisableCompression: true}, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	catalogue := &CatalogueClient{Settings: s, HTTP: d.http}
	if d.inventory == nil {
		d.inventory = catalogue.Inventory
	}
	if d.require == nil {
		d.require = NewNativeGate(s, catalogue).Require
	}
	if d.roots == nil {
		d.roots = catalogue.PrimaryRoots
	}
	if d.dial == nil {
		d.dial = dotsDialContext
	}
	if d.celsiusAddress == nil {
		d.celsiusAddress = func() (string, error) {
			port, err := realtimePort(filepath.Join(s.RuntimeRoot, "celsius-port"), s.RuntimeRoot, false)
			if err != nil {
				return "", err
			}
			return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
		}
	}
	if d.event == nil {
		d.event = realtimeEventSink(s.RuntimeRoot)
	}
	p := &realtimeProxy{settings: s, dependencies: d, resources: map[string]RootOwner{}}
	for _, dot := range s.Dots {
		p.resources[dot.RootThreadID] = RootOwner{OwnerAlias: dot.OwnerAlias, ProfileID: dot.ProfileID}
	}
	if _, err := p.accounts(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *realtimeProxy) accounts() (map[string]Account, error) {
	inventory, err := p.dependencies.inventory()
	if err != nil {
		return nil, err
	}
	expected := map[string]bool{p.settings.SystemAlias: true}
	for _, dot := range p.settings.Dots {
		expected[dot.OwnerAlias] = true
	}
	accounts := map[string]Account{}
	for _, account := range inventory.Accounts {
		if expected[account.Alias] {
			if previous, ok := accounts[account.Alias]; ok && previous.AccountID != account.AccountID {
				return nil, errors.New("Signalling account identity collision")
			}
			accounts[account.Alias] = account
		}
	}
	if len(accounts) != len(expected) {
		return nil, errors.New("Configured signalling principals unavailable")
	}
	return accounts, nil
}

func realtimePort(path, root string, allowMissing bool) (int, error) {
	data, err := readPrivate(path, root, true)
	if allowMissing && os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(data) > 8 {
		return 0, errors.New("Realtime proxy port invalid")
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("Realtime proxy port invalid")
	}
	return port, nil
}

// RunRealtimeProxy serves only loopback CONNECT traffic. Owner credentials are
// substituted exclusively for verified Dot computer signalling resources.
func RunRealtimeProxy(ctx context.Context, cfg *Settings) error {
	if cfg == nil {
		return errors.New("Dots settings unavailable")
	}
	if err := ValidateRuntimePaths(cfg.RuntimeRoot); err != nil {
		return err
	}
	p, err := newRealtimeProxy(cfg, realtimeDependencies{})
	if err != nil {
		return err
	}
	port, err := realtimePort(filepath.Join(cfg.RuntimeRoot, "realtime-port"), cfg.RuntimeRoot, true)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return errors.New("Realtime proxy listener unavailable")
	}
	defer listener.Close()
	_, actualPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	if err := launchWrite(filepath.Join(cfg.RuntimeRoot, "realtime-port"), []byte(actualPort), 0o600); err != nil {
		return err
	}
	p.dependencies.event(map[string]any{"listening_port": actualPort})
	return p.serve(ctx, listener)
}

func (p *realtimeProxy) serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{Handler: p, ReadHeaderTimeout: 20 * time.Second, MaxHeaderBytes: 16 * 1024, BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { defer func() { _ = recover() }(); _ = server.Close() })
	defer stop()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

type realtimeBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *realtimeBufferedConn) Read(data []byte) (int, error) { return c.reader.Read(data) }

func realtimeAuthority(authority string) (string, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port != "443" || !realtimeHost.MatchString(host) || strings.EqualFold(host, "localhost") {
		return "", errors.New("CONNECT destination rejected")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return "", errors.New("CONNECT destination rejected")
	}
	return strings.ToLower(host), nil
}

func (p *realtimeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	host, err := realtimeAuthority(r.Host)
	if err != nil {
		p.dependencies.event(map[string]any{"connect_rejected": true})
		http.Error(w, "CONNECT destination rejected", http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	client, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	defer func() {
		if recover() != nil {
			p.dependencies.event(map[string]any{"connect_failure_type": "panic"})
		}
	}()
	stop := context.AfterFunc(r.Context(), func() { defer func() { _ = recover() }(); _ = client.Close() })
	defer stop()
	if host != "ws.chatgpt.com" && host != "realtime.chatgpt.com" {
		upstream, err := p.dependencies.dial(r.Context(), "tcp", net.JoinHostPort(host, "443"))
		if err != nil {
			_, _ = buffer.WriteString("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
			_ = buffer.Flush()
			return
		}
		defer upstream.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if buffer.Flush() != nil {
			return
		}
		label := "other"
		if host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com") {
			label = host
		}
		p.dependencies.event(map[string]any{"opaque_tunnel_host": label})
		realtimeTunnel(r.Context(), client, buffer.Reader, upstream)
		return
	}
	_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	if buffer.Flush() != nil {
		return
	}
	wrapped := tls.Server(&realtimeBufferedConn{client, buffer.Reader}, p.dependencies.signallingTLS)
	defer wrapped.Close()
	handshake, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	err = wrapped.HandshakeContext(handshake)
	cancel()
	if err != nil {
		return
	}
	reader := bufio.NewReaderSize(wrapped, 65536)
	_ = wrapped.SetReadDeadline(time.Now().Add(25 * time.Second))
	raw, request, err := realtimeRequest(reader)
	if err != nil {
		realtimeFailure(wrapped, http.StatusBadRequest)
		return
	}
	_ = wrapped.SetReadDeadline(time.Time{})
	if host == "ws.chatgpt.com" {
		p.websocket(r.Context(), wrapped, reader, raw, request)
		return
	}
	p.realtime(r.Context(), wrapped, reader, raw, request)
}

func realtimeRequest(reader *bufio.Reader) ([]byte, *http.Request, error) {
	var raw []byte
	for {
		line, err := reader.ReadSlice('\n')
		if len(raw)+len(line) > 65536 {
			return nil, nil, errors.New("Realtime handshake exceeds bound")
		}
		raw = append(raw, line...)
		if err != nil {
			return nil, nil, errors.New("Realtime handshake truncated")
		}
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, nil, errors.New("Realtime handshake invalid")
	}
	return raw, request, nil
}

func realtimeFailure(w io.Writer, status int) {
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status))
}

func realtimeTunnel(ctx context.Context, client net.Conn, reader io.Reader, upstream net.Conn) {
	stop := context.AfterFunc(ctx, func() { defer func() { _ = recover() }(); _ = client.Close(); _ = upstream.Close() })
	defer stop()
	done := make(chan struct{})
	go func() {
		defer func() {
			if recover() != nil {
				_ = upstream.Close()
				_ = client.Close()
			}
			close(done)
		}()
		_, _ = io.Copy(upstream, reader)
		if connection, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = connection.CloseWrite()
		} else {
			_ = upstream.Close()
		}
	}()
	_, _ = io.Copy(client, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

func (p *realtimeProxy) secureUpstream(ctx context.Context, host string) (net.Conn, error) {
	raw, err := p.dependencies.dial(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return nil, err
	}
	config := p.dependencies.publicTLS.Clone()
	config.ServerName = host
	wrapped := tls.Client(raw, config)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := wrapped.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return wrapped, nil
}

func (p *realtimeProxy) websocket(ctx context.Context, client net.Conn, reader *bufio.Reader, raw []byte, r *http.Request) {
	var upstream net.Conn
	var err error
	if r.Method == http.MethodGet && realtimeCelsiusPath.MatchString(r.RequestURI) {
		address, addressErr := p.dependencies.celsiusAddress()
		if addressErr != nil {
			realtimeFailure(client, http.StatusBadGateway)
			return
		}
		host, port, splitErr := net.SplitHostPort(address)
		portNumber, portErr := strconv.Atoi(port)
		if splitErr != nil || host != "127.0.0.1" || portErr != nil || portNumber < 1 || portNumber > 65535 {
			realtimeFailure(client, http.StatusBadGateway)
			return
		}
		upstream, err = p.dependencies.dial(ctx, "tcp", address)
		if err == nil {
			p.dependencies.event(map[string]any{"celsius_native_proxy_connection": true})
		}
	} else {
		upstream, err = p.secureUpstream(ctx, "ws.chatgpt.com")
	}
	if err != nil {
		realtimeFailure(client, http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	if _, err := upstream.Write(raw); err != nil {
		return
	}
	realtimeTunnel(ctx, client, reader, upstream)
}

func (p *realtimeProxy) dotSignalling(r *http.Request) bool {
	if r.URL.Path != "/v1/cua" {
		return false
	}
	query := r.URL.Query()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, thread := range query["thread_id"] {
		if _, ok := p.resources[thread]; ok {
			return true
		}
	}
	for _, profile := range query["aeon_id"] {
		for _, dot := range p.settings.Dots {
			if profile == dot.ProfileID {
				return true
			}
		}
	}
	return false
}

func (p *realtimeProxy) signallingOwner(ctx context.Context, r *http.Request, accounts map[string]Account) (string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || r.Method != http.MethodPost || r.URL.Path != "/v1/cua" || r.URL.IsAbs() || r.URL.Host != "" || r.Header.Get("Content-Type") != "application/sdp" || len(query) != 3 || len(query["source"]) != 1 || query.Get("source") != "aeon" || len(query["thread_id"]) != 1 || len(query["aeon_id"]) != 1 {
		return "", errors.New("Invalid signalling resource scope")
	}
	thread, profile := query.Get("thread_id"), query.Get("aeon_id")
	p.mu.Lock()
	owner, exists := p.resources[thread]
	p.mu.Unlock()
	knownProfile := false
	for _, dot := range p.settings.Dots {
		if dot.ProfileID == profile {
			knownProfile = true
		}
	}
	if !exists && knownProfile {
		headers := http.Header{"User-Agent": []string{"Codex Desktop/" + p.settings.AppVersion + " (Mac OS; arm64)"}, "Originator": []string{"codex_desktop"}}
		roots, err := p.dependencies.roots(ctx, accounts, headers)
		if err != nil {
			return "", errors.New("Signalling root refresh unavailable")
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		for root, pair := range roots {
			if old, ok := p.resources[root]; ok && old != pair {
				return "", errors.New("Signalling resource owner changed")
			}
		}
		for root, pair := range roots {
			p.resources[root] = pair
		}
		owner, exists = p.resources[thread]
	}
	if !exists || owner.ProfileID != profile || !knownProfile {
		return "", errors.New("Unknown or conflicting signalling resource owner")
	}
	return owner.OwnerAlias, nil
}

func (p *realtimeProxy) realtime(ctx context.Context, client net.Conn, reader *bufio.Reader, raw []byte, r *http.Request) {
	if !p.dotSignalling(r) && p.settings.IntegrationMode == "native" {
		p.passthrough(ctx, client, reader, raw, r)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 || len(r.Header.Values("ChatGPT-Account-Id")) != 1 || p.dependencies.require(ctx, r.Header) != nil {
		p.dependencies.event(map[string]any{"signalling_rejected": true, "error_type": "native_authentication"})
		realtimeFailure(client, http.StatusForbidden)
		return
	}
	accounts, err := p.accounts()
	if err != nil {
		realtimeFailure(client, http.StatusForbidden)
		return
	}
	alias, err := p.signallingOwner(ctx, r, accounts)
	if err != nil || r.ContentLength < 1 || r.ContentLength > 1<<20 || len(r.TransferEncoding) != 0 {
		p.dependencies.event(map[string]any{"signalling_rejected": true, "error_type": "resource_scope"})
		realtimeFailure(client, http.StatusForbidden)
		return
	}
	body := make([]byte, int(r.ContentLength))
	_ = client.SetReadDeadline(time.Now().Add(25 * time.Second))
	_, err = io.ReadFull(reader, body)
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		p.dependencies.event(map[string]any{"signalling_truncated": true})
		realtimeFailure(client, http.StatusForbidden)
		return
	}
	owner, exists := accounts[alias]
	if !exists {
		realtimeFailure(client, http.StatusForbidden)
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://realtime.chatgpt.com"+r.RequestURI, bytes.NewReader(body))
	if err != nil {
		realtimeFailure(client, http.StatusForbidden)
		return
	}
	request.Header = r.Header.Clone()
	for _, name := range []string{"Host", "Connection", "Content-Length", "Accept-Encoding", "Authorization", "Cookie", "ChatGPT-Account-Id", "X-OpenAI-Account-Routing-Override", "X-OpenAI-Expected-Account-Id", "Proxy-Authorization", "Proxy-Connection"} {
		request.Header.Del(name)
	}
	request.Header.Set("Authorization", "Bearer "+owner.Token)
	request.Header.Set("ChatGPT-Account-Id", owner.AccountID)
	request.Header.Set("Accept-Encoding", "identity")
	response, err := p.dependencies.http.Do(request)
	if err != nil {
		p.dependencies.event(map[string]any{"owner_alias": alias, "signalling_failure_type": "upstream"})
		realtimeFailure(client, http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	data, err := httputil.ReadBodyLimit(response.Body, 2<<20)
	if err != nil {
		realtimeFailure(client, http.StatusBadGateway)
		return
	}
	p.dependencies.event(map[string]any{"owner_alias": alias, "signalling_status": response.StatusCode, "content_type": response.Header.Get("Content-Type"), "sdp_answer": response.StatusCode >= 200 && response.StatusCode < 300 && bytes.HasPrefix(data, []byte("v=0"))})
	header := response.Header.Clone()
	for _, name := range []string{"Content-Length", "Transfer-Encoding", "Content-Encoding", "Connection", "Set-Cookie"} {
		header.Del(name)
	}
	header.Set("Content-Length", strconv.Itoa(len(data)))
	header.Set("Connection", "close")
	_, _ = fmt.Fprintf(client, "HTTP/1.1 %d %s\r\n", response.StatusCode, http.StatusText(response.StatusCode))
	_ = header.Write(client)
	_, _ = io.WriteString(client, "\r\n")
	_, _ = client.Write(data)
}

func realtimeCloseHeader(raw []byte) []byte {
	lines := bytes.Split(raw, []byte("\r\n"))
	var value bytes.Buffer
	value.Write(lines[0])
	value.WriteString("\r\n")
	drop := false
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			colon := bytes.IndexByte(line, ':')
			drop = colon > 0 && strings.EqualFold(string(line[:colon]), "Connection")
		}
		if !drop {
			value.Write(line)
			value.WriteString("\r\n")
		}
	}
	value.WriteString("Connection: close\r\n\r\n")
	return value.Bytes()
}

func (p *realtimeProxy) passthrough(ctx context.Context, client net.Conn, reader *bufio.Reader, raw []byte, r *http.Request) {
	upstream, err := p.secureUpstream(ctx, "realtime.chatgpt.com")
	if err != nil {
		realtimeFailure(client, http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	if r.Header.Get("Upgrade") == "" {
		raw = realtimeCloseHeader(raw)
	}
	if _, err := upstream.Write(raw); err != nil {
		return
	}
	realtimeTunnel(ctx, client, reader, upstream)
}
