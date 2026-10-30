package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"

	wgconn "golang.zx2c4.com/wireguard/conn"
)

type interfaceSocketControl func(network, address string, raw syscall.RawConn) error

type interfaceBindDependencies struct {
	lookup  func(string) (*net.Interface, error)
	listen  func(network, address string, control interfaceSocketControl) (net.PacketConn, error)
	control func(index int) interfaceSocketControl
}

// interfaceWireGuardBind only controls WireGuard's outer UDP sockets. Interface
// scoping is subject to host network policy, including Network Extensions and
// VPN kill switches; it cannot guarantee that traffic bypasses a VPN.
//
// This small, single-packet Bind is needed because the pinned WireGuard
// StdNetBind has no Darwin socket-control or descriptor-access hook. It never
// emits ancillary data, which can override IP_BOUND_IF on Darwin.
type interfaceWireGuardBind struct {
	mu     sync.Mutex
	iface  net.Interface
	deps   interfaceBindDependencies
	active *interfaceBindSockets
	failed error
}

type interfaceBindSockets struct {
	v4, v6 net.PacketConn
	closed atomic.Bool
}

var _ wgconn.Bind = (*interfaceWireGuardBind)(nil)

func newInterfaceWireGuardBindWithDependencies(name string, deps interfaceBindDependencies) (wgconn.Bind, error) {
	iface, err := deps.lookup(name)
	if err != nil || !usableEgressInterface(iface) || iface.Name != name {
		return nil, errors.New("codex_egress_interface: selected interface is unavailable or unsuitable")
	}
	snapshot := *iface
	snapshot.HardwareAddr = bytes.Clone(iface.HardwareAddr)
	return &interfaceWireGuardBind{iface: snapshot, deps: deps}, nil
}

func usableEgressInterface(iface *net.Interface) bool {
	return iface != nil && iface.Index > 0 && iface.Name != "" &&
		iface.Flags&(net.FlagUp|net.FlagRunning) == net.FlagUp|net.FlagRunning &&
		iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) == 0 &&
		len(iface.HardwareAddr) != 0
}

func (b *interfaceWireGuardBind) checkInterface() error {
	iface, err := b.deps.lookup(b.iface.Name)
	if err != nil || !usableEgressInterface(iface) || iface.Name != b.iface.Name ||
		iface.Index != b.iface.Index || !bytes.Equal(iface.HardwareAddr, b.iface.HardwareAddr) {
		return errors.New("codex_egress_interface: selected interface was lost or changed; check the selected interface")
	}
	return nil
}

func listenInterfacePacket(network, address string, control interfaceSocketControl) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: control}
	return lc.ListenPacket(context.Background(), network, address)
}

func (b *interfaceWireGuardBind) Open(port uint16) ([]wgconn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed != nil {
		return nil, 0, b.failed
	}
	if b.active != nil {
		return nil, 0, wgconn.ErrBindAlreadyOpen
	}
	if err := b.checkInterface(); err != nil {
		b.failed = err
		return nil, 0, err
	}
	control := b.deps.control(b.iface.Index)
	// Match WireGuard's dual-stack port selection. A failed second listener
	// always closes the first; no unscoped socket is ever returned or reused.
	for attempt := 0; attempt < 100; attempt++ {
		sockets := &interfaceBindSockets{}
		selected := port
		var fns []wgconn.ReceiveFunc
		for _, network := range []string{"udp4", "udp6"} {
			pc, err := b.deps.listen(network, ":"+strconv.Itoa(int(selected)), control)
			if errors.Is(err, syscall.EAFNOSUPPORT) {
				continue
			}
			if err != nil {
				_ = sockets.close()
				if port == 0 && network == "udp6" && errors.Is(err, syscall.EADDRINUSE) {
					break
				}
				return nil, 0, errors.New("codex_egress_interface: could not open interface-bound WireGuard UDP socket")
			}
			if network == "udp4" {
				sockets.v4 = pc
			} else {
				sockets.v6 = pc
			}
			addr, ok := pc.LocalAddr().(*net.UDPAddr)
			if !ok || addr.Port <= 0 || addr.Port > 65535 || (selected != 0 && selected != uint16(addr.Port)) {
				_ = sockets.close()
				return nil, 0, errors.New("codex_egress_interface: invalid WireGuard UDP listener")
			}
			selected = uint16(addr.Port)
			fns = append(fns, sockets.receive(pc))
		}
		if sockets.closed.Load() {
			continue
		}
		if len(fns) == 0 {
			return nil, 0, syscall.EAFNOSUPPORT
		}
		if err := b.checkInterface(); err != nil {
			_ = sockets.close()
			b.failed = err
			return nil, 0, err
		}
		b.active = sockets
		return fns, selected, nil
	}
	return nil, 0, errors.New("codex_egress_interface: could not select a shared WireGuard UDP port")
}

func (s *interfaceBindSockets) close() error {
	s.closed.Store(true)
	var errs []error
	for _, pc := range []net.PacketConn{s.v4, s.v6} {
		if pc != nil {
			errs = append(errs, pc.Close())
		}
	}
	return errors.Join(errs...)
}

func (s *interfaceBindSockets) receive(pc net.PacketConn) wgconn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []wgconn.Endpoint) (int, error) {
		if s.closed.Load() {
			return 0, net.ErrClosed
		}
		if len(packets) != 1 || len(sizes) < 1 || len(eps) < 1 {
			return 0, errors.New("invalid WireGuard receive batch")
		}
		n, addr, err := pc.ReadFrom(packets[0])
		if s.closed.Load() {
			return 0, net.ErrClosed
		}
		if err != nil {
			return 0, err
		}
		udp, ok := addr.(*net.UDPAddr)
		if !ok || !udp.AddrPort().IsValid() {
			return 0, errors.New("invalid WireGuard packet address")
		}
		sizes[0] = n
		eps[0] = &wgconn.StdNetEndpoint{AddrPort: udp.AddrPort()}
		return 1, nil
	}
}

func (b *interfaceWireGuardBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == nil {
		return nil
	}
	err := b.active.close()
	b.active = nil
	return err
}

func (*interfaceWireGuardBind) BatchSize() int { return 1 }

func (*interfaceWireGuardBind) SetMark(mark uint32) error {
	if mark != 0 {
		return errors.New("packet marks are unsupported with codex_egress_interface")
	}
	return nil
}

func (b *interfaceWireGuardBind) ParseEndpoint(value string) (wgconn.Endpoint, error) {
	addr, err := netip.ParseAddrPort(value)
	if err != nil || !addr.IsValid() || addr.Port() == 0 || addr.Addr().IsUnspecified() || addr.Addr().IsMulticast() {
		return nil, errors.New("invalid interface-bound WireGuard endpoint")
	}
	// An explicit IPv6 zone must not select a different interface.
	zone := addr.Addr().Zone()
	if zone != "" && zone != b.iface.Name && zone != strconv.Itoa(b.iface.Index) {
		return nil, errors.New("WireGuard endpoint zone differs from codex_egress_interface")
	}
	return &wgconn.StdNetEndpoint{AddrPort: addr}, nil
}

func (b *interfaceWireGuardBind) Send(bufs [][]byte, endpoint wgconn.Endpoint) error {
	b.mu.Lock()
	if b.failed != nil {
		err := b.failed
		b.mu.Unlock()
		return err
	}
	if b.active == nil {
		b.mu.Unlock()
		return net.ErrClosed
	}
	if err := b.checkInterface(); err != nil {
		_ = b.active.close()
		b.active = nil
		b.failed = err
		b.mu.Unlock()
		return err
	}
	ep, ok := endpoint.(*wgconn.StdNetEndpoint)
	if !ok || ep == nil {
		b.mu.Unlock()
		return wgconn.ErrWrongEndpointType
	}
	// Validate again because Bind.Send is an exported interface and callers
	// can construct endpoints without passing through ParseEndpoint.
	if _, err := b.ParseEndpoint(ep.AddrPort.String()); err != nil {
		b.mu.Unlock()
		return err
	}
	if len(bufs) > b.BatchSize() {
		b.mu.Unlock()
		return errors.New("invalid WireGuard send batch")
	}
	pc := b.active.v4
	if ep.Addr().Is6() {
		pc = b.active.v6
	}
	// UDPConn permits concurrent writes and Close. Do not hold the lifecycle
	// lock across a write: Close must be able to interrupt a blocked writer.
	b.mu.Unlock()
	if pc == nil {
		return syscall.EAFNOSUPPORT
	}
	for _, buf := range bufs {
		if _, err := pc.WriteTo(buf, net.UDPAddrFromAddrPort(ep.AddrPort)); err != nil {
			return err
		}
	}
	return nil
}
