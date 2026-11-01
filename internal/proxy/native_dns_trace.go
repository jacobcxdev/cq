package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
)

// Native DNS diagnostics deliberately exclude names, records, payloads, tokens
// and raw error strings. A flow identifier joins both packet directions.
type nativeDNSEvent struct {
	Event     string    `json:"event"`
	Time      time.Time `json:"time"`
	Flow      string    `json:"flow"`
	Target    string    `json:"target"`
	Stage     string    `json:"stage"`
	Route     string    `json:"route"`
	Network   string    `json:"network"`
	ID        uint16    `json:"dns_id"`
	Response  bool      `json:"response"`
	RCode     uint8     `json:"rcode"`
	Questions uint16    `json:"questions"`
	Answers   uint16    `json:"answers"`
	Packet    bool      `json:"packet"`
	Failed    bool      `json:"failed"`
	ErrorCode string    `json:"error_code,omitempty"`
}

type nativeDNSTraceDialer struct {
	EgressDialer
	emit         func(nativeDNSEvent)
	localAddress func(netip.Addr) (netip.Addr, bool)
	dialLocal    func(context.Context, string, string, netip.Addr) (net.Conn, error)
}

func (d nativeDNSTraceDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	host, port, _ := net.SplitHostPort(target)
	ip, err := netip.ParseAddr(host)
	if (network != "udp" && network != "tcp") || port != "53" || err != nil {
		return d.EgressDialer.DialContext(ctx, network, target)
	}
	trace := &nativeDNSConn{target: target, flow: newCodexTraceID("dns"), route: "wireguard", network: network, emit: d.emit, window: time.Now()}
	var conn net.Conn
	if source, local := d.localDNS(ip); local {
		trace.route = "lan"
		conn, err = d.dialLocal(ctx, network, target, source)
	} else {
		conn, err = d.EgressDialer.DialContext(ctx, network, target)
	}
	trace.record("dial", nil, err)
	if err != nil || conn == nil {
		return conn, err
	}
	trace.Conn = conn
	return trace, nil
}

func (d nativeDNSTraceDialer) localDNS(ip netip.Addr) (netip.Addr, bool) {
	if d.localAddress == nil || d.dialLocal == nil {
		return netip.Addr{}, false
	}
	return d.localAddress(ip)
}

type nativeDNSConn struct {
	net.Conn
	mu                           sync.Mutex
	target, flow, route, network string
	window                       time.Time
	count                        int
	emit                         func(nativeDNSEvent)
}

func (c *nativeDNSConn) record(stage string, packet []byte, err error) {
	if c.emit == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.window) >= time.Minute {
		c.window, c.count = now, 0
	}
	if c.count >= 65 {
		return
	}
	c.count++
	event := nativeDNSEvent{Event: "native_dns", Time: now.UTC(), Flow: c.flow, Target: c.target, Route: c.route, Network: c.network, Stage: stage, Failed: err != nil}
	if err != nil {
		event.ErrorCode = nativeDNSErrorCode(err)
	}
	if c.count == 65 {
		event.Stage = "rate_limited"
	} else if c.network == "udp" && len(packet) >= 12 {
		event.Packet = true
		event.ID = binary.BigEndian.Uint16(packet)
		event.Response = packet[2]&0x80 != 0
		event.RCode = packet[3] & 15
		event.Questions = binary.BigEndian.Uint16(packet[4:])
		event.Answers = binary.BigEndian.Uint16(packet[6:])
	}
	c.emit(event)
}

func nativeDNSErrorCode(err error) string {
	switch {
	case errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "unreachable"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "address_unavailable"
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return "denied"
	case errors.Is(err, io.EOF):
		return "eof"
	default:
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return fmt.Sprintf("errno_%d", uint64(errno))
		}
		if errors.Is(err, io.ErrShortWrite) {
			return "short_write"
		}
		return "transport"
	}
}

func (c *nativeDNSConn) Write(packet []byte) (int, error) {
	n, err := c.Conn.Write(packet)
	c.record("send", packet, err)
	return n, err
}

func (c *nativeDNSConn) Read(packet []byte) (int, error) {
	n, err := c.Conn.Read(packet)
	c.record("receive", packet[:n], err)
	return n, err
}

func (c *nativeDNSConn) Close() error {
	err := c.Conn.Close()
	c.record("close", nil, err)
	return err
}

func writeNativeDNSEvent(event nativeDNSEvent) {
	if data, err := json.Marshal(event); err == nil {
		fmt.Fprintf(os.Stderr, "%s\n", data)
	}
}
