package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	wgconn "golang.zx2c4.com/wireguard/conn"
)

// CodexEgress supplies matching HTTP and WebSocket upstream policies. It never
// changes process environment or the shared default transport.
type CodexEgress struct {
	HTTP       http.RoundTripper
	WebSocket  websocket.Dialer
	close      func() error
	closeOnce  sync.Once
	closeErr   error
	dial       func(context.Context, string, string) (net.Conn, error)
	nativeDial func(context.Context, string, string) (net.Conn, error)
}

func (e *CodexEgress) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if e == nil || e.dial == nil {
		return nil, errors.New("Codex egress dialer unavailable")
	}
	return e.dial(ctx, network, address)
}

// ValidateCodexEgressSettings is non-creating: configuration inspection never
// reads a profile or starts a tunnel.
func ValidateCodexEgressSettings(proxyURL, profilePath string) error {
	if err := ValidateCodexEgressProxy(proxyURL); err != nil {
		return err
	}
	if profilePath != "" {
		if proxyURL != "" {
			return errors.New("codex_egress_proxy and codex_wireguard_profile are mutually exclusive")
		}
		if !fsutil.IsCleanAbsoluteNonRootPath(profilePath) {
			return errors.New("invalid codex_wireguard_profile: require a clean absolute file path")
		}
	}
	return nil
}

// OpenCodexEgress snapshots a private profile but activates its userspace
// device only on the first request. One instance must be shared per runtime.
func OpenCodexEgress(cfg *Config, fs fsutil.FileSystem) (*CodexEgress, error) {
	if cfg == nil {
		return nil, errors.New("Codex egress configuration unavailable")
	}
	if err := validateCodexEgressInterface(cfg.CodexWireGuardProfile, cfg.CodexEgressInterface); err != nil {
		return nil, err
	}
	if err := ValidateCodexEgressSettings(cfg.CodexEgressProxy, cfg.CodexWireGuardProfile); err != nil {
		return nil, err
	}
	if cfg.CodexWireGuardProfile == "" {
		egress, err := NewCodexEgress(cfg.CodexEgressProxy)
		if err == nil && cfg.CodexEgressProxy == "" {
			egress.dial = ambientOriginEgressDial(cfg.CodexUpstream)
		}
		return egress, err
	}
	p, err := readWireGuardProfile(fs, cfg.CodexWireGuardProfile)
	if err != nil {
		return nil, err
	}
	return openCodexWireGuardEgress(cfg, p)
}

func openCodexWireGuardEgress(cfg *Config, p *wireGuardProfile) (*CodexEgress, error) {
	bindFactory := wgconn.NewDefaultBind
	if cfg.CodexEgressInterface != "" {
		bind, err := newInterfaceWireGuardBind(cfg.CodexEgressInterface)
		if err != nil {
			return nil, err
		}
		bindFactory = func() wgconn.Bind { return bind }
	}
	runtime := newWireGuardRuntime(p, bindFactory)
	egress, err := codexEgressWithDialer(runtime.DialContext, runtime.Close)
	if err != nil {
		_ = runtime.Close()
		return nil, err
	}
	egress.nativeDial = runtime.DialNativeContext
	return egress, nil
}

func validateCodexEgressInterface(profile, name string) error {
	if name == "" {
		return nil
	}
	if profile == "" || len(name) > 64 || strings.ContainsAny(name, "/\\\x00\r\n\t ") {
		return errors.New("codex_egress_interface requires a WireGuard profile and a valid interface name")
	}
	return nil
}

// Close releases an owned transport and userspace network. It is idempotent.
func (e *CodexEgress) Close() error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		if e.close != nil {
			e.closeErr = e.close()
		}
	})
	return e.closeErr
}

func codexEgressWithDialer(dial func(context.Context, string, string) (net.Conn, error), closeNetwork func() error) (*CodexEgress, error) {
	egress, err := NewCodexEgress("direct")
	if err != nil {
		return nil, err
	}
	transport := egress.HTTP.(*http.Transport)
	transport.DialContext = dial
	egress.dial = dial
	transport.DialTLSContext = nil
	transport.DialTLS = nil
	egress.WebSocket.NetDialContext = dial
	egress.close = func() error {
		transport.CloseIdleConnections()
		if closeNetwork != nil {
			return closeNetwork()
		}
		return nil
	}
	return egress, nil
}

// ValidateCodexEgressProxy accepts ambient defaults, explicit direct egress, or
// an HTTP(S) proxy origin. Credentials are deliberately unsupported: use a
// local forwarding proxy rather than storing secrets in this setting.
func ValidateCodexEgressProxy(value string) error {
	_, err := parseCodexEgressProxy(value)
	return err
}

func parseCodexEgressProxy(value string) (*url.URL, error) {
	if value == "" || value == "direct" {
		return nil, nil
	}
	invalid := errors.New("invalid codex_egress_proxy: use direct or an HTTP(S)/SOCKS5 proxy origin without credentials, path, query, or fragment")
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, invalid
		}
	} else if u.Host[len(u.Host)-1] == ':' {
		return nil, invalid
	}
	return u, nil
}

// NewCodexEgress preserves ambient proxy behavior when unset. An explicit proxy
// is used even for loopback origins and never falls back to a direct connection.
func NewCodexEgress(value string) (*CodexEgress, error) {
	return newCodexEgress(value, nil)
}

// proxyTLSConfig permits isolated certificate roots for the proxy hop. Origin
// TLS remains owned by the HTTP transport and WebSocket dialer separately.
func newCodexEgress(value string, proxyTLSConfig *tls.Config) (*CodexEgress, error) {
	u, err := parseCodexEgressProxy(value)
	if err != nil {
		return nil, err
	}
	if u != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h") {
		return newSOCKSEgress(u)
	}
	egress := &CodexEgress{
		HTTP:      http.DefaultTransport,
		WebSocket: websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 30 * time.Second, EnableCompression: true},
		dial:      ambientEgressDial,
	}
	if value == "" {
		return egress, nil
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("configure Codex egress: default HTTP transport is unavailable")
	}
	transport := base.Clone()
	transport.Proxy = nil
	egress.dial = directEgressDial
	egress.WebSocket.Proxy = nil
	if u != nil {
		egress.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return connectEgressProxy(ctx, u, proxyTLSConfig, network, address)
		}
		transport.Proxy = http.ProxyURL(u)
		egress.WebSocket.Proxy = http.ProxyURL(u)
		if u.Scheme == "https" {
			// Gorilla registers only plain HTTP CONNECT. Give it a verified
			// TLS connection to the proxy, independently of origin TLS.
			connectProxy := *u
			connectProxy.Scheme = "http"
			if u.Port() == "" {
				connectProxy.Host = net.JoinHostPort(u.Hostname(), "443")
			}
			egress.WebSocket.Proxy = http.ProxyURL(&connectProxy)
			config := &tls.Config{}
			if proxyTLSConfig != nil {
				config = proxyTLSConfig.Clone()
			} else if base.TLSClientConfig != nil {
				config = base.TLSClientConfig.Clone()
			}
			config.ServerName = u.Hostname()
			config.NextProtos = []string{"http/1.1"}
			egress.WebSocket.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&tls.Dialer{NetDialer: &net.Dialer{}, Config: config}).DialContext(ctx, network, address)
			}
		}
	}
	egress.HTTP = transport
	egress.close = func() error { transport.CloseIdleConnections(); return nil }
	return egress, nil
}
