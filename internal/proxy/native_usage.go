package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/codexusage"
	"github.com/jacobcxdev/cq/internal/fsutil"
	cqhttp "github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/nativeusage"
)

// NewNativeUsageDialer retains the existing native route. Only the two usage
// service hosts gain local HTTPS handling, and only after explicit activation.
func NewNativeUsageDialer(upstream EgressDialer, root string, fs fsutil.FileSystem) EgressDialer {
	return &nativeUsageDialer{upstream: upstream, root: root, fs: fs}
}

type nativeUsageDialer struct {
	upstream  EgressDialer
	root      string
	fs        fsutil.FileSystem
	event     func(string)
	publicTLS *tls.Config
}

func (d *nativeUsageDialer) NativeReady() bool {
	ready, ok := d.upstream.(interface{ NativeReady() bool })
	return ok && ready.NativeReady()
}

func (d *nativeUsageDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	if d.upstream == nil {
		return nil, errors.New("native usage route unavailable")
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || network != "tcp" || port != "443" || !nativeusage.IsHost(host) && net.ParseIP(host) == nil {
		return d.upstream.DialContext(ctx, network, target)
	}
	config, err := nativeusage.LoadTLS(d.root, d.fs)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return d.upstream.DialContext(ctx, network, target)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if nativeusage.IsHost(host) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			defer func() { _ = recover() }()
			stop := context.AfterFunc(ctx, func() { _ = server.Close() })
			defer stop()
			hello := inspectNativeUsageHello(ctx, server)
			if ctx.Err() != nil || len(hello.prefix) == 0 {
				return
			}
			if nativeusage.IsHost(hello.host) {
				d.serve(ctx, &nativeUsageReplayConn{Conn: server, reader: io.MultiReader(bytes.NewReader(hello.prefix), server)}, net.JoinHostPort(hello.host, "443"), config)
				return
			}
			original, err := d.upstream.DialContext(ctx, network, target)
			if err != nil || original == nil {
				return
			}
			defer original.Close()
			forwardNativeUsageOriginal(server, original, &hello, &nativeUsageFirstRead{}, nil, nil)
		}()
		return client, nil
	}
	original, err := d.upstream.DialContext(ctx, network, target)
	if err != nil {
		return nil, err
	}
	if original == nil {
		return nil, errors.New("native usage route unavailable")
	}
	return routeNativeUsage(ctx, original, config, func(ctx context.Context, conn net.Conn, service string, config *tls.Config) {
		d.note("HTTPS usage service selected: numeric destination")
		d.serve(ctx, conn, service, config)
	}), nil
}

func (d *nativeUsageDialer) note(kind string) {
	if d.event != nil {
		d.event(kind)
		return
	}
	// Never record URL queries, headers, credentials or response contents.
	fmt.Fprintf(os.Stderr, "cq native usage: %s\n", kind)
}

// Classify failures without exposing request URLs, headers or peer-supplied data.
func nativeUsageFailure(err error) string {
	if errors.Is(err, context.Canceled) {
		return "request cancelled"
	}
	if errors.Is(err, io.EOF) {
		return "peer EOF"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "incomplete TLS message"
	}
	if errors.Is(err, net.ErrClosed) {
		return "connection closed"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	var certificate *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	if errors.As(err, &certificate) || errors.As(err, &authority) {
		return "upstream certificate rejected"
	}
	for _, code := range []tls.AlertError{42, 43, 44, 45, 46, 48, 49} {
		if err.Error() == "remote error: "+code.Error() {
			return "client certificate rejected"
		}
	}
	if strings.Contains(err.Error(), "tls: client requested unsupported application protocols") {
		return "application protocol mismatch"
	}
	return "other"
}

func (d *nativeUsageDialer) upstreamTLS() *tls.Config {
	if d.publicTLS != nil {
		return d.publicTLS.Clone()
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

func (d *nativeUsageDialer) enabled() bool {
	config, err := nativeusage.LoadTLS(d.root, d.fs)
	return err == nil && config != nil
}

func (d *nativeUsageDialer) serve(ctx context.Context, raw net.Conn, target string, config *tls.Config) {
	tracked := &nativeUsageConn{Conn: raw, done: make(chan struct{})}
	defer tracked.Close()
	stop := context.AfterFunc(ctx, func() { _ = tracked.Close() })
	defer stop()
	config = config.Clone()
	helloScope := "no client hello"
	config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		helloScope = "other hostname"
		if nativeusage.IsHost(hello.ServerName) {
			helloScope = "usage hostname"
		}
		return nil, nil
	}
	conn := tls.Server(tracked, config)
	defer conn.Close()
	handshake, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := conn.HandshakeContext(handshake)
	cancel()
	if err != nil {
		d.note("HTTPS connection failed: " + nativeUsageFailure(err) + " (" + helloScope + ")")
		return
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: d.upstream.DialContext,
		TLSClientConfig:     d.upstreamTLS(),
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	host, _, _ := net.SplitHostPort(target)
	origin := &url.URL{Scheme: "https", Host: target}
	forward := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(origin)
			request.Out.Host = request.In.Host
			if nativeUsageRequest(request.Out) {
				request.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: func(response *http.Response) error {
			if !d.enabled() {
				return nil
			}
			changed, err := nativeUsageResponse(response)
			if changed {
				d.note("HTTP usage adapted")
			}
			return err
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			d.note("HTTP upstream unavailable: " + nativeUsageFailure(err))
			http.Error(w, "usage service unavailable", http.StatusBadGateway)
		},
	}
	server := &http.Server{
		ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
		BaseContext:    func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != host && r.Host != target || r.URL.IsAbs() || r.Method == http.MethodConnect {
				http.Error(w, "unexpected usage service host", http.StatusMisdirectedRequest)
				return
			}
			if host == "ws.chatgpt.com" && websocket.IsWebSocketUpgrade(r) {
				d.websocket(w, r, target)
				return
			}
			if host == "chatgpt.com" && r.Method == http.MethodGet && r.URL.EscapedPath() == "/backend-api/wham/usage" {
				kind := "other client"
				if r.Header.Get("OAI-App-Brand") == "chatgpt" || strings.Contains(r.UserAgent(), "Chrome/") {
					kind = "browser client"
				}
				d.note("HTTP usage requested: " + kind)
			}
			forward.ServeHTTP(w, r)
		}),
	}
	listener := &nativeUsageListener{conn: conn, done: tracked.done}
	defer server.Close()
	_ = server.Serve(listener)
}

func nativeUsageRequest(request *http.Request) bool {
	return request != nil && request.URL.Hostname() == "chatgpt.com" && request.Method == http.MethodGet && request.URL.EscapedPath() == "/backend-api/wham/usage"
}

func nativeUsageResponse(response *http.Response) (bool, error) {
	if !nativeUsageRequest(response.Request) || response.StatusCode != http.StatusOK || response.Body == nil ||
		response.ContentLength > cqhttp.MaxResponseBody {
		return false, nil
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return false, nil
	}
	// Retain a bounded prefix so an oversized, unknown-length response can still
	// be forwarded intact instead of truncating an unrelated upstream result.
	var prefix bytes.Buffer
	original := response.Body
	data, err := cqhttp.ReadBody(io.TeeReader(original, &prefix))
	if errors.Is(err, cqhttp.ErrBodyTooLarge) {
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix.Bytes()), original), original}
		return false, nil
	}
	_ = original.Close()
	if err != nil {
		return false, errors.New("read usage response failed")
	}
	projected := codexusage.FloorCreditResponse(data)
	response.Body = io.NopCloser(bytes.NewReader(projected))
	if bytes.Equal(projected, data) {
		return false, nil
	}
	response.ContentLength = int64(len(projected))
	response.Header.Set("Content-Length", strconv.Itoa(len(projected)))
	for _, header := range []string{"ETag", "Content-MD5", "Digest", "Content-Digest"} {
		response.Header.Del(header)
	}
	return true, nil
}

func (d *nativeUsageDialer) websocket(w http.ResponseWriter, r *http.Request, target string) {
	endpoint := url.URL{Scheme: "wss", Host: target, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	headers := r.Header.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol"} {
		headers.Del(name)
	}
	dialer := websocket.Dialer{NetDialContext: d.upstream.DialContext, HandshakeTimeout: 15 * time.Second,
		TLSClientConfig: d.upstreamTLS(), Subprotocols: websocket.Subprotocols(r)}
	var wire *nativeUsageHandshakeConn
	dialer.NetDialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		raw, err := d.upstream.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		config := d.upstreamTLS()
		if config.ServerName == "" {
			config.ServerName, _, _ = net.SplitHostPort(address)
		}
		conn := tls.Client(raw, config)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		wire = &nativeUsageHandshakeConn{Conn: conn}
		wire.retain.Store(true)
		wire.stopCancel = context.AfterFunc(r.Context(), func() { _ = conn.Close() })
		return wire, nil
	}
	defer func() {
		if wire != nil {
			wire.retain.Store(false)
			wire.stopCancel()
			_ = wire.Conn.Close()
		}
	}()
	upstream, response, err := dialer.DialContext(r.Context(), endpoint.String(), headers)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		if response != nil && wire != nil && errors.Is(err, websocket.ErrBadHandshake) && response.StatusCode != http.StatusSwitchingProtocols {
			// Gorilla retains only a 1 KiB diagnostic body on rejection. Read the
			// original verified response instead, including bytes it prefetched.
			original, readErr := http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(wire.prefix.Bytes()), wire.Conn)), r)
			if readErr != nil {
				http.Error(w, "live usage unavailable", http.StatusBadGateway)
				return
			}
			defer original.Body.Close()
			for name, values := range original.Header {
				switch strings.ToLower(name) {
				case "connection", "upgrade", "keep-alive", "transfer-encoding", "trailer", "te":
					continue
				}
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(original.StatusCode)
			_, _ = io.Copy(w, original.Body)
			return
		}
		d.note("live usage upstream unavailable")
		http.Error(w, "live usage unavailable", http.StatusBadGateway)
		return
	}
	wire.retain.Store(false)
	wire.prefix = bytes.Buffer{}
	defer upstream.Close()
	upgrader := websocket.Upgrader{HandshakeTimeout: 15 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	if protocol := upstream.Subprotocol(); protocol != "" {
		upgrader.Subprotocols = []string{protocol}
	}
	responseHeaders := http.Header{}
	if response != nil {
		for _, cookie := range response.Header.Values("Set-Cookie") {
			responseHeaders.Add("Set-Cookie", cookie)
		}
	}
	client, err := upgrader.Upgrade(w, r, responseHeaders)
	if err != nil {
		return
	}
	d.note("live usage connected")
	defer client.Close()
	closeBoth := func() { _ = client.Close(); _ = upstream.Close() }
	stop := context.AfterFunc(r.Context(), closeBoth)
	defer stop()
	var sender sync.WaitGroup
	sender.Add(1)
	go func() {
		defer sender.Done()
		defer closeBoth()
		defer func() { _ = recover() }()
		for {
			kind, reader, err := client.NextReader()
			if err != nil {
				forwardNativeUsageClose(upstream, err)
				return
			}
			if writeNativeUsageMessage(upstream, kind, nil, reader) != nil {
				return
			}
		}
	}()
	func() {
		defer closeBoth()
		defer func() { _ = recover() }()
		for {
			kind, reader, err := upstream.NextReader()
			if err != nil {
				forwardNativeUsageClose(client, err)
				return
			}
			var prefix []byte
			var remainder io.Reader = reader
			if kind == websocket.TextMessage {
				var captured bytes.Buffer
				data, readErr := cqhttp.ReadBodyLimit(io.TeeReader(reader, &captured), 2<<20)
				if errors.Is(readErr, cqhttp.ErrBodyTooLarge) {
					prefix = captured.Bytes()
				} else if readErr != nil {
					forwardNativeUsageClose(client, readErr)
					return
				} else {
					prefix, remainder = data, nil
					projected := codexusage.FloorCreditMessage(data)
					if !bytes.Equal(projected, data) && d.enabled() {
						d.note("live usage adapted")
						prefix = projected
					}
				}
			}
			if writeNativeUsageMessage(client, kind, prefix, remainder) != nil {
				return
			}
		}
	}()
	sender.Wait()
}

// Keep the verified handshake stream until the upgrade outcome is known. This
// avoids losing a rejected response when the WebSocket library closes it.
type nativeUsageHandshakeConn struct {
	net.Conn
	prefix     bytes.Buffer
	retain     atomic.Bool
	stopCancel func() bool
}

func (c *nativeUsageHandshakeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.retain.Load() {
		if c.prefix.Len()+n > 128<<10 {
			return 0, errors.New("live usage response headers exceed limit")
		}
		_, _ = c.prefix.Write(p[:n])
	}
	return n, err
}

func (c *nativeUsageHandshakeConn) Close() error {
	if c.retain.Load() {
		return nil
	}
	return c.Conn.Close()
}

func writeNativeUsageMessage(socket *websocket.Conn, kind int, prefix []byte, remainder io.Reader) error {
	writer, err := socket.NextWriter(kind)
	if err != nil {
		return err
	}
	if len(prefix) != 0 {
		_, err = writer.Write(prefix)
	}
	if err == nil && remainder != nil {
		_, err = io.Copy(writer, remainder)
	}
	closeErr := writer.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func forwardNativeUsageClose(socket *websocket.Conn, err error) {
	var closed *websocket.CloseError
	if errors.As(err, &closed) && closed.Code != websocket.CloseNoStatusReceived && closed.Code != websocket.CloseAbnormalClosure && closed.Code != websocket.CloseTLSHandshake {
		_ = socket.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(closed.Code, closed.Text), time.Now().Add(time.Second))
	}
}

type nativeUsageConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *nativeUsageConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

type nativeUsageListener struct {
	conn net.Conn
	done <-chan struct{}
	used bool
}

func (l *nativeUsageListener) Accept() (net.Conn, error) {
	if !l.used {
		l.used = true
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *nativeUsageListener) Close() error   { return l.conn.Close() }
func (l *nativeUsageListener) Addr() net.Addr { return l.conn.LocalAddr() }
