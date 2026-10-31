package proxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EgressDialer opens an origin connection without choosing an ambient route.
type EgressDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// NewEgressBroker serves authenticated CONNECT requests on a caller-owned private
// Unix listener. The token and destination policy must both be configured.
func NewEgressBroker(dialer EgressDialer, token string, allowed func(string) bool) http.Handler {
	return newEgressBroker(dialer, token, allowed, func(*http.Request) (string, bool) { return "tcp", true })
}

func newEgressBroker(dialer EgressDialer, token string, allowed func(string) bool, selectNetwork func(*http.Request) (string, bool)) http.Handler {
	return newEgressBrokerWithIdle(dialer, token, allowed, selectNetwork, nativeDatagramIdle)
}

func newEgressBrokerWithIdle(dialer EgressDialer, token string, allowed func(string) bool, selectNetwork func(*http.Request) (string, bool), idleTimeout time.Duration) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacked := false
		defer func() {
			if recover() != nil && !hijacked {
				http.Error(w, "egress connection failed", http.StatusBadGateway)
			}
		}()
		presented := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if !validEgressBrokerToken(token) || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(expected[:], presented[:]) != 1 {
			http.Error(w, "egress broker authentication failed", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		target := r.RequestURI
		if !canonicalEgressBrokerTarget(target) || r.Host != target || r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "invalid egress target", http.StatusBadRequest)
			return
		}
		if allowed == nil || !allowed(target) || dialer == nil {
			http.Error(w, "egress target denied", http.StatusForbidden)
			return
		}
		network, valid := selectNetwork(r)
		if !valid {
			http.Error(w, "invalid egress network", http.StatusBadRequest)
			return
		}
		upstream, err := dialer.DialContext(r.Context(), network, target)
		if err != nil || upstream == nil {
			if upstream != nil {
				_ = upstream.Close()
			}
			http.Error(w, "egress connection failed", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		if network == "udp" {
			upstream = newNativeDatagramStream(upstream)
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "egress tunnel unavailable", http.StatusInternalServerError)
			return
		}
		downstream, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		hijacked = true
		defer downstream.Close()
		if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err = buffered.Flush(); err != nil {
			return
		}
		upstreamToClose := upstream
		closeBoth := func() { closeEgressBrokerConn(downstream); closeEgressBrokerConn(upstreamToClose) }
		if network == "udp" {
			idle := newEgressBrokerIdle(idleTimeout, closeBoth)
			defer idle.stop()
			upstream = &egressBrokerIdleConn{Conn: upstream, idle: idle}
		}
		var cancellation sync.WaitGroup
		cancellation.Add(1)
		stop := context.AfterFunc(r.Context(), func() { defer cancellation.Done(); closeBoth() })
		defer func() {
			if stop() {
				cancellation.Done()
			}
			cancellation.Wait()
		}()
		var workers sync.WaitGroup
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer closeBoth()
			defer func() { _ = recover() }()
			_, _ = io.Copy(upstream, buffered)
		}()
		func() {
			defer closeBoth()
			defer func() { _ = recover() }()
			_, _ = io.Copy(downstream, upstream)
		}()
		workers.Wait()
	})
}

// A UDP deadline alone cannot wake a blocked write to the companion's stream.
// Own both ends independently and renew only when a framed transfer progresses.
type egressBrokerIdle struct {
	mu       sync.Mutex
	timer    *time.Timer
	timeout  time.Duration
	deadline time.Time
	close    func()
	closing  chan struct{}
	stopped  bool
}

func newEgressBrokerIdle(timeout time.Duration, closeBoth func()) *egressBrokerIdle {
	idle := &egressBrokerIdle{timeout: timeout, deadline: time.Now().Add(timeout), close: closeBoth}
	idle.mu.Lock()
	idle.timer = time.AfterFunc(timeout, idle.expire)
	idle.mu.Unlock()
	return idle
}

func (i *egressBrokerIdle) activity() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.stopped {
		i.deadline = time.Now().Add(i.timeout)
		i.timer.Reset(i.timeout)
	}
}

func (i *egressBrokerIdle) expire() {
	i.mu.Lock()
	if i.stopped {
		i.mu.Unlock()
		return
	}
	// Reset may race an already queued callback. Check the current deadline
	// before closing a flow that has since made progress.
	if remaining := time.Until(i.deadline); remaining > 0 {
		i.timer.Reset(remaining)
		i.mu.Unlock()
		return
	}
	i.stopped = true
	done := make(chan struct{})
	i.closing = done
	i.mu.Unlock()
	defer close(done)
	defer func() { _ = recover() }()
	i.close()
}

func (i *egressBrokerIdle) stop() {
	i.mu.Lock()
	i.stopped = true
	i.timer.Stop()
	closing := i.closing
	i.mu.Unlock()
	if closing != nil {
		<-closing
	}
}

type egressBrokerIdleConn struct {
	net.Conn
	idle *egressBrokerIdle
}

func (c *egressBrokerIdleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.idle.activity()
	}
	return n, err
}

func (c *egressBrokerIdleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.idle.activity()
	}
	return n, err
}

func validEgressBrokerToken(token string) bool {
	if token == "" {
		return false
	}
	for _, c := range token {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}

func canonicalEgressBrokerTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return false
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != port {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || ip.Is4In6() || ip.String() != host {
			return false
		}
	} else {
		if len(host) > 253 {
			return false
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return false
				}
			}
		}
	}
	return target == net.JoinHostPort(host, port)
}

type egressBrokerClient struct{ socketPath, token string }

// NewEgressBrokerClient connects only to the supplied local broker. It never
// falls back to a host-network origin connection or consults proxy environment.
func NewEgressBrokerClient(socketPath, token string) EgressDialer {
	return &egressBrokerClient{socketPath: socketPath, token: token}
}

func (c *egressBrokerClient) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if (network != "tcp" && network != "tcp4" && network != "tcp6") || !canonicalEgressBrokerTarget(target) || !validEgressBrokerToken(c.token) || c.socketPath == "" {
		return nil, errors.New("invalid egress broker request")
	}
	handshake, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(handshake, "unix", c.socketPath)
	if err != nil {
		return nil, egressBrokerClientError(handshake)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = conn.Close()
		}
	}()
	deadline, _ := handshake.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, egressBrokerClientError(handshake)
	}
	var cancellation sync.WaitGroup
	cancellation.Add(1)
	stop := context.AfterFunc(handshake, func() { defer cancellation.Done(); defer func() { _ = recover() }(); _ = conn.Close() })
	defer func() {
		if stop() {
			cancellation.Done()
		}
		cancellation.Wait()
	}()
	request := &http.Request{Method: http.MethodConnect, Host: target, Header: make(http.Header)}
	// Request.Write requires a URL, but CONNECT's request-target is its authority.
	request.URL = &url.URL{Host: target}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if err := request.Write(conn); err != nil {
		return nil, egressBrokerClientError(handshake)
	}
	limited := &egressBrokerHeaderReader{reader: conn, remaining: 32 * 1024}
	buffered := bufio.NewReader(limited)
	response, err := http.ReadResponse(buffered, request)
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, egressBrokerClientError(handshake)
	}
	// Keep the same buffered reader: it may already contain origin TLS bytes.
	limited.remaining = -1
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, egressBrokerClientError(handshake)
	}
	if handshake.Err() != nil {
		return nil, egressBrokerClientError(handshake)
	}
	// End cancellation ownership before returning a long-lived tunnel.
	if stop() {
		cancellation.Done()
	}
	cancellation.Wait()
	if handshake.Err() != nil {
		return nil, egressBrokerClientError(handshake)
	}
	succeeded = true
	return &egressBrokerBufferedConn{Conn: conn, reader: buffered}, nil
}

func egressBrokerClientError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Socket deadlines can fire before the context timer goroutine publishes Err.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return errors.New("egress broker connection failed; no direct fallback")
}

type egressBrokerHeaderReader struct {
	reader    io.Reader
	remaining int
}

func (r *egressBrokerHeaderReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("egress broker response headers too large")
	}
	if r.remaining > 0 && len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	if r.remaining > 0 {
		r.remaining -= n
	}
	return n, err
}

type egressBrokerBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *egressBrokerBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// A caller-provided transport must not turn cleanup into a goroutine panic.
func closeEgressBrokerConn(conn net.Conn) {
	defer func() { _ = recover() }()
	_ = conn.Close()
}
