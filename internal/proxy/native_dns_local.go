package proxy

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// LAN resolvers can use globally allocated addresses. Only an address on an
// active, non-tunnel interface may take this route; Internet DNS stays on the
// configured egress. Binding the interface address prevents an ambient fallback.
func nativeDNSLocalAddress(target netip.Addr) (netip.Addr, bool) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		if source, ok := nativeDNSInterfaceAddress(target, iface.Flags, addresses); ok {
			return source, true
		}
	}
	return netip.Addr{}, false
}

func nativeDNSInterfaceAddress(target netip.Addr, flags net.Flags, addresses []net.Addr) (netip.Addr, bool) {
	if flags&net.FlagUp == 0 || flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
		return netip.Addr{}, false
	}
	for _, address := range addresses {
		if source, ok := nativeDNSSubnetAddress(target, address.String()); ok {
			return source, true
		}
	}
	return netip.Addr{}, false
}

func nativeDNSSubnetAddress(target netip.Addr, address string) (netip.Addr, bool) {
	prefix, err := netip.ParsePrefix(address)
	if err != nil || prefix.Bits() == 0 || !target.IsGlobalUnicast() || !prefix.Addr().IsGlobalUnicast() ||
		target.Is4() != prefix.Addr().Is4() || !prefix.Contains(target) {
		return netip.Addr{}, false
	}
	return prefix.Addr(), true
}

func dialNativeLocalDNS(ctx context.Context, network, target string, source netip.Addr) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network == "udp" {
		peer, err := netip.ParseAddrPort(target)
		if err != nil {
			return nil, err
		}
		socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(source.AsSlice())})
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			socket.Close()
			return nil, err
		}
		// Source-bound, connected UDP intermittently returned ENOTCONN and
		// EDESTADDRREQ in live Darwin transparent-proxy checks.
		// Use explicit fixed-peer sends and reject all other receive peers.
		return &nativeDNSDatagramConn{Conn: socket, peer: peer}, nil
	}
	return (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(source.AsSlice())}, Timeout: 30 * time.Second}).DialContext(ctx, network, target)
}

type nativeDNSDatagramConn struct {
	net.Conn
	peer netip.AddrPort
}

func (c *nativeDNSDatagramConn) Write(packet []byte) (int, error) {
	return c.Conn.(*net.UDPConn).WriteToUDPAddrPort(packet, c.peer)
}

func (c *nativeDNSDatagramConn) Read(packet []byte) (int, error) {
	for {
		n, peer, err := c.Conn.(*net.UDPConn).ReadFromUDPAddrPort(packet)
		if err != nil || (peer.Addr().Unmap() == c.peer.Addr().Unmap() && peer.Port() == c.peer.Port()) {
			return n, err
		}
	}
}

func (c *nativeDNSDatagramConn) RemoteAddr() net.Addr { return net.UDPAddrFromAddrPort(c.peer) }
