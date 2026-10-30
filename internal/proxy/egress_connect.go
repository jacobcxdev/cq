package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func directEgressDial(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
}

func ambientEgressDial(ctx context.Context, network, address string) (net.Conn, error) {
	return ambientEgressDialForScheme(ctx, "https", network, address)
}

func ambientOriginEgressDial(origin string) func(context.Context, string, string) (net.Conn, error) {
	if origin == "" {
		origin = DefaultCodexUpstream
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("invalid ambient Codex origin")
		}
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	target := net.JoinHostPort(u.Hostname(), port)
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		scheme := "https"
		if address == target {
			scheme = u.Scheme
		}
		return ambientEgressDialForScheme(ctx, scheme, network, address)
	}
}

func ambientEgressDialForScheme(ctx context.Context, scheme, network, address string) (net.Conn, error) {
	u, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: scheme, Host: address}})
	if err != nil {
		return nil, errors.New("ambient Codex proxy unavailable")
	}
	if u == nil {
		return directEgressDial(ctx, network, address)
	}
	if u.Scheme == "socks5" || u.Scheme == "socks5h" {
		v := *u
		v.Scheme = "socks5"
		if v.Port() == "" {
			v.Host = net.JoinHostPort(v.Hostname(), "1080")
		}
		dialer, err := xproxy.FromURL(&v, &net.Dialer{Timeout: 30 * time.Second})
		if err != nil {
			return nil, errors.New("ambient SOCKS proxy unavailable")
		}
		contextual, ok := dialer.(xproxy.ContextDialer)
		if !ok {
			return nil, errors.New("ambient SOCKS proxy cannot honor cancellation")
		}
		return contextual.DialContext(ctx, network, address)
	}
	return connectEgressProxy(ctx, u, nil, network, address)
}

func connectEgressProxy(parent context.Context, u *url.URL, trust *tls.Config, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("Codex proxy requires TCP")
	}
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid Codex proxy scheme")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	connection, err := directEgressDial(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, errors.New("connect Codex proxy failed")
	}
	succeeded := false
	defer func() {
		if !succeeded {
			connection.Close()
		}
	}()
	raw := connection
	var cleanup sync.WaitGroup
	cleanup.Add(1)
	stop := context.AfterFunc(ctx, func() { defer cleanup.Done(); defer func() { _ = recover() }(); raw.Close() })
	defer func() {
		if stop() {
			cleanup.Done()
		}
		cleanup.Wait()
	}()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	if u.Scheme == "https" {
		config := &tls.Config{}
		if trust != nil {
			config = trust.Clone()
		}
		config.ServerName = u.Hostname()
		config.NextProtos = []string{"http/1.1"}
		secure := tls.Client(connection, config)
		if secure.HandshakeContext(ctx) != nil {
			return nil, errors.New("verify Codex proxy TLS failed")
		}
		connection = secure
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: address}, Host: address, Header: make(http.Header)}
	if u.User != nil {
		password, _ := u.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password)))
	}
	if request.Write(connection) != nil {
		return nil, errors.New("Codex proxy CONNECT failed")
	}
	limited := &egressBrokerHeaderReader{reader: connection, remaining: 32 * 1024}
	reader := bufio.NewReader(limited)
	response, err := http.ReadResponse(reader, request)
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, errors.New("Codex proxy CONNECT rejected")
	}
	limited.remaining = -1
	if stop() {
		cleanup.Done()
	}
	cleanup.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if connection.SetDeadline(time.Time{}) != nil {
		return nil, errors.New("Codex proxy tunnel unavailable")
	}
	succeeded = true
	return &egressBrokerBufferedConn{Conn: connection, reader: reader}, nil
}
