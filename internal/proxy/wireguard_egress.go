package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	wgconn "golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
)

func readWireGuardProfile(fs fsutil.FileSystem, path string) (*wireGuardProfile, error) {
	if !fsutil.IsCleanAbsoluteNonRootPath(path) {
		return nil, errors.New("invalid codex_wireguard_profile: require a clean absolute file path")
	}
	data, err := fsutil.ReadSecureFile(fs, path, 64*1024)
	if err != nil {
		return nil, errors.New("read WireGuard profile: require an owner-only regular file in a private directory, at most 64 KiB")
	}
	defer clear(data)
	return parseWireGuardProfile(data)
}

// wireGuardRuntime owns one lazy device. No host TUN, route, DNS, firewall or
// child process is created. Only the encrypted UDP bind uses the host network.
// Failed initialization is cleaned up and may be retried by a later request;
// it never selects a direct fallback.
type wireGuardRuntime struct {
	mu              sync.Mutex
	initializing    chan struct{}
	profile         *wireGuardProfile
	bindFactory     func() wgconn.Bind
	resolveEndpoint func(context.Context, string) (netip.AddrPort, error)
	device          *device.Device
	network         *wireGuardNetstack
	closed          bool
	life            context.Context
	cancel          context.CancelFunc
}

func newWireGuardRuntime(p *wireGuardProfile, bindFactory func() wgconn.Bind) *wireGuardRuntime {
	life, cancel := context.WithCancel(context.Background())
	return &wireGuardRuntime{profile: p, bindFactory: bindFactory, resolveEndpoint: resolveWireGuardEndpoint, life: life, cancel: cancel, initializing: make(chan struct{}, 1)}
}

func resolveWireGuardEndpoint(ctx context.Context, endpoint string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return netip.AddrPort{}, errors.New("invalid WireGuard endpoint")
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return netip.AddrPort{}, errors.New("invalid WireGuard endpoint")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(ip, uint16(number)), nil
	}
	// Bootstrap lookup is only for the outer peer. Destination DNS uses the
	// isolated resolver below. A literal endpoint IP avoids bootstrap DNS.
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return netip.AddrPort{}, errors.New("resolve WireGuard peer endpoint failed")
	}
	return netip.AddrPortFrom(addresses[0].Unmap(), uint16(number)), nil
}

func (r *wireGuardRuntime) start(ctx context.Context) (*wireGuardNetstack, error) {
	// Serialize setup with a context-selectable gate. Waiting requests must not
	// inherit another request's endpoint-resolution timeout through a mutex.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.life.Done():
		return nil, net.ErrClosed
	case r.initializing <- struct{}{}:
		defer func() { <-r.initializing }()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, net.ErrClosed
	}
	if r.network != nil {
		return r.network, nil
	}
	endpoints := make([]netip.AddrPort, len(r.profile.peers))
	for i, peer := range r.profile.peers {
		endpoint, err := r.resolveEndpoint(ctx, peer.endpoint)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("initialise WireGuard egress: peer endpoint unavailable")
		}
		endpoints[i] = endpoint
	}
	tun, network, err := createWireGuardNetTUN(r.profile.addresses, r.profile.mtu)
	if err != nil {
		return nil, errors.New("initialise WireGuard userspace network failed")
	}
	dev := device.NewDevice(tun, r.bindFactory(), device.NewLogger(device.LogLevelSilent, ""))
	if err = dev.IpcSet(r.profile.ipcConfig(endpoints)); err == nil {
		err = dev.Up()
	}
	if err != nil {
		closeWireGuardDevice(dev)
		return nil, errors.New("initialise WireGuard peer failed")
	}
	r.device, r.network = dev, network
	return network, nil
}

func (r *wireGuardRuntime) allows(ip netip.Addr) bool {
	ip = ip.Unmap()
	family := false
	for _, a := range r.profile.addresses {
		family = family || a.Is4() == ip.Is4()
	}
	if !family {
		return false
	}
	// The WireGuard device routes across peers by longest matching prefix.
	for _, peer := range r.profile.peers {
		for _, prefix := range peer.allowedIPs {
			if prefix.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func (r *wireGuardRuntime) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	return r.dialContext(parent, network, address, false)
}

// DialNativeContext preserves the shared tunnel and its resolver, but prevents
// a captured application flow from reaching private or reserved destinations.
func (r *wireGuardRuntime) DialNativeContext(parent context.Context, network, address string) (net.Conn, error) {
	return r.dialContext(parent, network, address, true)
}

func (r *wireGuardRuntime) dialContext(parent context.Context, network, address string, publicOnly bool) (net.Conn, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	switch network {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
	default:
		return nil, errors.New("WireGuard egress supports TCP and connected UDP requests only")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid WireGuard destination")
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 || host == "" {
		return nil, errors.New("invalid WireGuard destination")
	}
	ip, literalErr := netip.ParseAddr(host)
	if literalErr == nil && (!r.allows(ip) || !wireGuardNetworkFamily(network, ip) || (publicOnly && !isNativePublicAddress(ip))) {
		return nil, errors.New("WireGuard destination is outside AllowedIPs")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(r.life, cancel)
	defer stop()
	stack, err := r.start(ctx)
	if err != nil {
		return nil, r.dialError(parent, err)
	}
	var ips []netip.Addr
	if literalErr == nil {
		ips = []netip.Addr{ip.Unmap()}
	} else {
		ips, err = r.lookupHost(ctx, stack, host)
		if err != nil {
			return nil, r.dialError(parent, err)
		}
	}
	for _, candidate := range ips {
		if !r.allows(candidate) || !wireGuardNetworkFamily(network, candidate) || (publicOnly && !isNativePublicAddress(candidate)) {
			continue
		}
		connection, dialErr := stack.DialContext(ctx, network, netip.AddrPortFrom(candidate.Unmap(), uint16(number)).String())
		if dialErr == nil {
			if ctx.Err() != nil {
				_ = connection.Close()
				return nil, r.dialError(parent, ctx.Err())
			}
			if strings.HasPrefix(network, "udp") {
				// Expose only connected Read/Write. The underlying UDPConn's
				// WriteTo could otherwise select an unvalidated destination.
				return &wireGuardDatagramConn{Conn: connection}, nil
			}
			return connection, nil
		}
		if ctx.Err() != nil {
			return nil, r.dialError(parent, ctx.Err())
		}
	}
	return nil, r.dialError(parent, errors.New("WireGuard destination unavailable or outside AllowedIPs"))
}

func wireGuardNetworkFamily(network string, ip netip.Addr) bool {
	ip = ip.Unmap()
	return (!strings.HasSuffix(network, "4") || ip.Is4()) && (!strings.HasSuffix(network, "6") || ip.Is6())
}

type wireGuardDatagramConn struct{ net.Conn }

// GlobalUnicast includes private, documentation and benchmarking allocations.
// Native flows exclude those as well as special IPv6 transition allocations,
// which could otherwise translate a public-looking address to a local target.
func isNativePublicAddress(ip netip.Addr) bool {
	if ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if ip.Is4() {
		for _, prefix := range nativeExcludedIPv4 {
			if prefix.Contains(ip) {
				return false
			}
		}
		return true
	}
	if !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range nativeExcludedIPv6 {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var nativeExcludedIPv4 = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

var nativeExcludedIPv6 = [...]netip.Prefix{
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func (r *wireGuardRuntime) lookupHost(ctx context.Context, stack *wireGuardNetstack, host string) ([]netip.Addr, error) {
	// A successful UDP dial proves nothing about DNS reachability. Fail over
	// whole lookups, with a bounded budget per configured server and the outer
	// 30-second dial deadline bounding the complete sequence.
	for _, dns := range r.profile.dnsServers {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resolver := &net.Resolver{PreferGo: true, StrictErrors: true, Dial: func(queryCtx context.Context, protocol, _ string) (net.Conn, error) {
			// Never use the system resolver's supplied server address. TCP retry
			// after a truncated UDP answer also stays inside this same tunnel.
			c, err := stack.DialContext(queryCtx, protocol, netip.AddrPortFrom(dns, 53).String())
			if err != nil {
				return nil, err
			}
			return newWireGuardDNSConn(queryCtx, c), nil
		}}
		// Absolute names prevent host search-domain suffixes changing queries.
		ips, err := resolver.LookupNetIP(attemptCtx, "ip", strings.TrimSuffix(host, ".")+".")
		cancel()
		if err == nil && len(ips) != 0 {
			return ips, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("WireGuard DNS unavailable")
}

func (r *wireGuardRuntime) dialError(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if r.life.Err() != nil {
		return net.ErrClosed
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("WireGuard egress unavailable; no direct fallback")
}

// Resolver UDP reads must unblock immediately on cancellation, not just their
// socket deadline. The stop callback is removed when the resolver closes it.
type wireGuardDNSConn struct {
	net.Conn
	stop func() bool
}

func newWireGuardDNSConn(ctx context.Context, c net.Conn) net.Conn {
	wrapped := &wireGuardDNSConn{Conn: c, stop: context.AfterFunc(ctx, func() { defer func() { _ = recover() }(); _ = c.Close() })}
	if packet, ok := c.(net.PacketConn); ok {
		return &wireGuardDNSPacketConn{wireGuardDNSConn: wrapped, packet: packet}
	}
	return wrapped
}

// net.Resolver distinguishes datagram DNS from length-prefixed stream DNS by
// net.PacketConn. Preserve that capability when wrapping a UDP connection.
type wireGuardDNSPacketConn struct {
	*wireGuardDNSConn
	packet net.PacketConn
}

func (c *wireGuardDNSPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	return c.packet.ReadFrom(b)
}
func (c *wireGuardDNSPacketConn) WriteTo(b []byte, a net.Addr) (int, error) {
	return c.packet.WriteTo(b, a)
}
func (c *wireGuardDNSConn) Close() error { c.stop(); return c.Conn.Close() }

func (r *wireGuardRuntime) Close() error {
	r.cancel()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	dev := r.device
	r.device = nil
	r.mu.Unlock()
	if dev != nil {
		closeWireGuardDevice(dev)
	}
	return nil
}

// Device.Close closes its TUN before stopping the peers. With a userspace TUN,
// an in-flight receive can synchronously emit a reply into that TUN's outgoing
// packet channel. Down closes the bind and joins the peer workers first, while
// the TUN reader is still available to drain those replies. Removing all peers
// also fences a delayed initial TUN EventUp from restarting them between Down
// and Close, which acquire the device state lock separately.
func closeWireGuardDevice(dev *device.Device) {
	_ = dev.Down()
	dev.RemoveAllPeers()
	dev.Close()
}

var _ io.Closer = (*wireGuardRuntime)(nil)
