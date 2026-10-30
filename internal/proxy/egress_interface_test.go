package proxy

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	wgconn "golang.zx2c4.com/wireguard/conn"
)

type syntheticInterfacePacketConn struct {
	port   int
	closed bool
	writes int
	to     net.Addr
}

func (c *syntheticInterfacePacketConn) ReadFrom(buf []byte) (int, net.Addr, error) {
	if c.closed {
		return 0, nil, net.ErrClosed
	}
	return copy(buf, "packet"), net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.1:51820")), nil
}
func (c *syntheticInterfacePacketConn) WriteTo(buf []byte, to net.Addr) (int, error) {
	if c.closed {
		return 0, net.ErrClosed
	}
	c.writes++
	c.to = to
	return len(buf), nil
}
func (c *syntheticInterfacePacketConn) Close() error                   { c.closed = true; return nil }
func (c *syntheticInterfacePacketConn) LocalAddr() net.Addr            { return &net.UDPAddr{Port: c.port} }
func (*syntheticInterfacePacketConn) SetDeadline(time.Time) error      { return nil }
func (*syntheticInterfacePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (*syntheticInterfacePacketConn) SetWriteDeadline(time.Time) error { return nil }

type syntheticInterfaceRawConn struct{ err error }

func (r syntheticInterfaceRawConn) Control(fn func(uintptr)) error {
	if r.err != nil {
		return r.err
	}
	fn(42)
	return nil
}
func (syntheticInterfaceRawConn) Read(func(uintptr) bool) error  { panic("unexpected raw read") }
func (syntheticInterfaceRawConn) Write(func(uintptr) bool) error { panic("unexpected raw write") }

func syntheticEgressInterface() *net.Interface {
	return &net.Interface{Name: "en0", Index: 4, Flags: net.FlagUp | net.FlagRunning | net.FlagBroadcast, HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 1}}
}

func syntheticInterfaceDependencies(iface **net.Interface) interfaceBindDependencies {
	return interfaceBindDependencies{
		lookup: func(string) (*net.Interface, error) { return *iface, nil },
		control: func(index int) interfaceSocketControl {
			return func(string, string, syscall.RawConn) error {
				if index != 4 {
					return errors.New("wrong index")
				}
				return nil
			}
		},
		listen: func(network, address string, control interfaceSocketControl) (net.PacketConn, error) {
			if err := control(network, address, syntheticInterfaceRawConn{}); err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(strings.TrimPrefix(address, ":"))
			if port == 0 {
				port = 40000
			}
			return &syntheticInterfacePacketConn{port: port}, nil
		},
	}
}

func TestInterfaceWireGuardBindConstructionIsSocketFree(t *testing.T) {
	iface := syntheticEgressInterface()
	deps := syntheticInterfaceDependencies(&iface)
	deps.listen = func(string, string, interfaceSocketControl) (net.PacketConn, error) {
		t.Fatal("construction opened a socket")
		return nil, nil
	}
	bind, err := newInterfaceWireGuardBindWithDependencies("en0", deps)
	if err != nil || bind == nil {
		t.Fatalf("construct: %v", err)
	}
	if bind.BatchSize() != 1 || bind.SetMark(0) != nil || bind.SetMark(1) == nil {
		t.Fatal("unexpected batching or mark support")
	}
}

func TestInterfaceWireGuardBindRejectsUnsuitableInterfaces(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*net.Interface)
	}{
		{"zero index", func(i *net.Interface) { i.Index = 0 }},
		{"wrong name", func(i *net.Interface) { i.Name = "en1" }},
		{"down", func(i *net.Interface) { i.Flags &^= net.FlagUp }},
		{"not running", func(i *net.Interface) { i.Flags &^= net.FlagRunning }},
		{"loopback", func(i *net.Interface) { i.Flags |= net.FlagLoopback }},
		{"tunnel", func(i *net.Interface) { i.Flags |= net.FlagPointToPoint }},
		{"no link address", func(i *net.Interface) { i.HardwareAddr = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			iface := syntheticEgressInterface()
			test.edit(iface)
			if bind, err := newInterfaceWireGuardBindWithDependencies("en0", syntheticInterfaceDependencies(&iface)); err == nil || bind != nil {
				t.Fatal("accepted unsuitable interface")
			}
		})
	}
	iface := (*net.Interface)(nil)
	if _, err := newInterfaceWireGuardBindWithDependencies("en0", syntheticInterfaceDependencies(&iface)); err == nil {
		t.Fatal("accepted missing interface")
	}
	deps := syntheticInterfaceDependencies(&iface)
	deps.lookup = func(string) (*net.Interface, error) { return nil, errors.New("sensitive lookup detail") }
	if _, err := newInterfaceWireGuardBindWithDependencies("en0", deps); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe lookup failure: %v", err)
	}
}

func TestInterfaceWireGuardBindLifecycle(t *testing.T) {
	iface := syntheticEgressInterface()
	deps := syntheticInterfaceDependencies(&iface)
	var sockets []*syntheticInterfacePacketConn
	listen := deps.listen
	deps.listen = func(network, address string, control interfaceSocketControl) (net.PacketConn, error) {
		pc, err := listen(network, address, control)
		sockets = append(sockets, pc.(*syntheticInterfacePacketConn))
		return pc, err
	}
	bind, err := newInterfaceWireGuardBindWithDependencies("en0", deps)
	if err != nil {
		t.Fatal(err)
	}
	fns, port, err := bind.Open(0)
	if err != nil || port != 40000 || len(fns) != 2 {
		t.Fatalf("open: port=%d fns=%d err=%v", port, len(fns), err)
	}
	if _, _, err := bind.Open(0); !errors.Is(err, wgconn.ErrBindAlreadyOpen) {
		t.Fatalf("second open: %v", err)
	}
	for i, address := range []string{"192.0.2.1:51820", "[2001:db8::1]:51820"} {
		ep, err := bind.ParseEndpoint(address)
		if err != nil {
			t.Fatal(err)
		}
		if err := bind.Send([][]byte{[]byte("encrypted")}, ep); err != nil || sockets[i].writes != 1 || sockets[i].to.String() != address {
			t.Fatalf("send family %d: %v", i, err)
		}
	}
	packets, sizes, eps := [][]byte{make([]byte, 32)}, make([]int, 1), make([]wgconn.Endpoint, 1)
	if n, err := fns[0](packets, sizes, eps); err != nil || n != 1 || sizes[0] != 6 || eps[0].DstToString() != "192.0.2.1:51820" {
		t.Fatalf("receive: n=%d size=%d err=%v", n, sizes[0], err)
	}
	if err := bind.Close(); err != nil {
		t.Fatal(err)
	}
	if !sockets[0].closed || !sockets[1].closed {
		t.Fatal("close did not release both sockets")
	}
	for _, receive := range fns {
		if _, err := receive(packets, sizes, eps); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("receive after close: %v", err)
		}
	}
	if err := bind.Send(nil, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("send after close: %v", err)
	}
	if _, _, err := bind.Open(51820); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = bind.Close()
}

func TestInterfaceWireGuardBindLatchesInterfaceLoss(t *testing.T) {
	for _, change := range []string{"missing", "down", "index", "link address"} {
		t.Run(change, func(t *testing.T) {
			iface := syntheticEgressInterface()
			bind, err := newInterfaceWireGuardBindWithDependencies("en0", syntheticInterfaceDependencies(&iface))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := bind.Open(0); err != nil {
				t.Fatal(err)
			}
			sockets := bind.(*interfaceWireGuardBind).active
			switch change {
			case "missing":
				iface = nil
			case "down":
				iface.Flags &^= net.FlagUp
			case "index":
				iface.Index++
			case "link address":
				iface.HardwareAddr[5]++
			}
			ep, _ := bind.ParseEndpoint("192.0.2.1:51820")
			if err := bind.Send([][]byte{[]byte("must not send")}, ep); err == nil {
				t.Fatal("sent after interface loss")
			}
			if !sockets.v4.(*syntheticInterfacePacketConn).closed || sockets.v4.(*syntheticInterfacePacketConn).writes != 0 || !sockets.v6.(*syntheticInterfacePacketConn).closed {
				t.Fatal("interface loss did not close before sending")
			}
			iface = syntheticEgressInterface()
			_ = bind.Close()
			if _, _, err := bind.Open(0); err == nil {
				t.Fatal("interface loss was not terminal")
			}
		})
	}
}

func TestInterfaceWireGuardBindOpenFailureClosesOtherFamily(t *testing.T) {
	iface := syntheticEgressInterface()
	deps := syntheticInterfaceDependencies(&iface)
	first := &syntheticInterfacePacketConn{port: 51820}
	deps.listen = func(network, _ string, _ interfaceSocketControl) (net.PacketConn, error) {
		if network == "udp4" {
			return first, nil
		}
		return nil, errors.New("sensitive socket error")
	}
	bind, _ := newInterfaceWireGuardBindWithDependencies("en0", deps)
	fns, port, err := bind.Open(51820)
	if err == nil || strings.Contains(err.Error(), "sensitive") || len(fns) != 0 || port != 0 || !first.closed {
		t.Fatalf("partial open: port=%d fns=%d closed=%v err=%v", port, len(fns), first.closed, err)
	}
}

func TestInterfaceWireGuardBindSingleFamilyAndPortCollision(t *testing.T) {
	for _, missingFamily := range []string{"udp4", "udp6", "both", "collision"} {
		t.Run(missingFamily, func(t *testing.T) {
			iface := syntheticEgressInterface()
			deps := syntheticInterfaceDependencies(&iface)
			listen := deps.listen
			var first *syntheticInterfacePacketConn
			collision := true
			deps.listen = func(network, address string, control interfaceSocketControl) (net.PacketConn, error) {
				if network == missingFamily || missingFamily == "both" {
					return nil, syscall.EAFNOSUPPORT
				}
				if missingFamily == "collision" && network == "udp6" && collision {
					collision = false
					return nil, syscall.EADDRINUSE
				}
				pc, err := listen(network, address, control)
				if first == nil {
					first = pc.(*syntheticInterfacePacketConn)
				}
				return pc, err
			}
			bind, _ := newInterfaceWireGuardBindWithDependencies("en0", deps)
			fns, port, err := bind.Open(0)
			if missingFamily == "both" {
				if !errors.Is(err, syscall.EAFNOSUPPORT) || len(fns) != 0 || port != 0 {
					t.Fatalf("no families: %v", err)
				}
				return
			}
			want := 1
			if missingFamily == "collision" {
				want = 2
				if !first.closed {
					t.Fatal("port collision leaked first socket")
				}
			}
			if err != nil || len(fns) != want || port != 40000 {
				t.Fatalf("open: fns=%d port=%d err=%v", len(fns), port, err)
			}
			_ = bind.Close()
		})
	}
}

func TestInterfaceWireGuardBindRejectsEndpointOverrides(t *testing.T) {
	iface := syntheticEgressInterface()
	bind, _ := newInterfaceWireGuardBindWithDependencies("en0", syntheticInterfaceDependencies(&iface))
	for _, address := range []string{"not an endpoint", "192.0.2.1:0", "0.0.0.0:51820", "[ff02::1]:51820", "[fe80::1%utun3]:51820"} {
		if _, err := bind.ParseEndpoint(address); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
	for _, address := range []string{"[fe80::1%en0]:51820", "[fe80::1%4]:51820"} {
		if _, err := bind.ParseEndpoint(address); err != nil {
			t.Fatalf("rejected matching zone: %v", err)
		}
	}
	_, _, _ = bind.Open(0)
	defer bind.Close()
	ep := &wgconn.StdNetEndpoint{AddrPort: netip.MustParseAddrPort("[fe80::1%utun3]:51820")}
	if err := bind.Send([][]byte{[]byte("packet")}, ep); err == nil {
		t.Fatal("Send accepted manually constructed mismatched zone")
	}
	if err := bind.Send(nil, nil); !errors.Is(err, wgconn.ErrWrongEndpointType) {
		t.Fatalf("nil endpoint: %v", err)
	}
}

type blockingInterfacePacketConn struct {
	net.PacketConn
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *blockingInterfacePacketConn) WriteTo([]byte, net.Addr) (int, error) {
	close(c.started)
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockingInterfacePacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func TestInterfaceWireGuardBindCloseInterruptsWrite(t *testing.T) {
	iface := syntheticEgressInterface()
	deps := syntheticInterfaceDependencies(&iface)
	blocked := &blockingInterfacePacketConn{
		PacketConn: &syntheticInterfacePacketConn{port: 40000},
		started:    make(chan struct{}),
		closed:     make(chan struct{}),
	}
	deps.listen = func(network, _ string, _ interfaceSocketControl) (net.PacketConn, error) {
		if network == "udp6" {
			return nil, syscall.EAFNOSUPPORT
		}
		return blocked, nil
	}
	bind, _ := newInterfaceWireGuardBindWithDependencies("en0", deps)
	if _, _, err := bind.Open(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocked.Close() })
	ep, _ := bind.ParseEndpoint("192.0.2.1:51820")
	sent, closed := make(chan error, 1), make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				sent <- errors.New("send panicked")
			}
		}()
		sent <- bind.Send([][]byte{[]byte("packet")}, ep)
	}()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	go func() {
		defer func() {
			if recover() != nil {
				closed <- errors.New("close panicked")
			}
		}()
		closed <- bind.Close()
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close was blocked by a write")
	}
	select {
	case err := <-sent:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("blocked send: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not interrupt write")
	}
}
