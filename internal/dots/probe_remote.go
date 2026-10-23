package dots

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// remoteControl relays opaque WebSocket bytes. The native and public peers retain
// frame masking, close and ping policy; no owner credential substitution occurs.
func (p *Probe) remoteControl(w http.ResponseWriter, r *http.Request) {
	origin, err := url.Parse(p.options.Origin)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	address := origin.Host
	if !strings.Contains(address, ":") {
		address = net.JoinHostPort(address, "443")
	}
	dialer := &net.Dialer{Timeout: 25 * time.Second}
	var upstream net.Conn
	if origin.Scheme == "https" {
		publicTLS, rootErr := cloudPublicTLS()
		if rootErr != nil {
			w.WriteHeader(502)
			return
		}
		publicTLS.ServerName = origin.Hostname()
		publicTLS.NextProtos = []string{"http/1.1"}
		upstream, err = (&tls.Dialer{NetDialer: dialer, Config: publicTLS}).DialContext(r.Context(), "tcp", address)
	} else {
		upstream, err = dialer.DialContext(r.Context(), "tcp", address)
	}
	if err != nil {
		w.WriteHeader(502)
		return
	}
	defer upstream.Close()
	_ = upstream.SetDeadline(time.Now().Add(25 * time.Second))
	request := r.Clone(r.Context())
	request.URL = &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath}
	request.RequestURI = ""
	request.Host = origin.Host
	request.Body = nil
	request.ContentLength = 0
	if err := request.Write(upstream); err != nil {
		w.WriteHeader(502)
		return
	}
	reader := bufio.NewReaderSize(upstream, 65536)
	var header []byte
	for len(header) < 65536 {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			w.WriteHeader(502)
			return
		}
		header = append(header, line...)
		if len(header) > 65536 {
			w.WriteHeader(502)
			return
		}
		if string(line) == "\r\n" {
			break
		}
	}
	response, err := http.ReadResponse(bufio.NewReader(strings.NewReader(string(header))), r)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(502)
		return
	}
	client, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := buffer.Write(header); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	p.event(map[string]any{"operation": "native_remote_control_websocket", "status": response.StatusCode})
	if response.StatusCode != 101 {
		return
	}
	_ = upstream.SetDeadline(time.Time{})
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	copyStream := func(destination net.Conn, source io.Reader) {
		defer func() { _ = recover(); _ = destination.Close(); done <- struct{}{} }()
		_, _ = io.CopyBuffer(destination, source, make([]byte, 65536))
	}
	go copyStream(upstream, buffer.Reader)
	go copyStream(client, reader)
	select {
	case <-r.Context().Done():
	case <-done:
	}
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

// A context deadline bounds handshakes; streaming has no whole-response timeout.
func probeHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// A fresh connection prevents Transport's reused-connection retry path for
	// owner mutations, including callers carrying an idempotency header.
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.DisableCompression = true
	transport.DialContext = (&net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 25 * time.Second
	transport.ResponseHeaderTimeout = 25 * time.Second
	publicTLS, err := cloudPublicTLS()
	// An unavailable public bundle leaves an empty pool and fails closed. Never
	// inherit the native application's process-only local interception trust.
	if err != nil {
		publicTLS = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig = publicTLS
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
