package proxy

import (
	"errors"
	"net"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// newSOCKSEgress uses SOCKS CONNECT for hostnames in both supported schemes.
// Resolution is performed by the proxy rather than the local resolver.
func newSOCKSEgress(u *url.URL) (*CodexEgress, error) {
	if u == nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("configure Codex SOCKS egress: invalid proxy origin")
	}
	port := u.Port()
	if port == "" {
		port = "1080"
	}
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(u.Hostname(), port), nil, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
	if err != nil {
		return nil, errors.New("configure Codex SOCKS egress: create dialer")
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("configure Codex SOCKS egress: context dialer unavailable")
	}
	return codexEgressWithDialer(contextDialer.DialContext, nil)
}
