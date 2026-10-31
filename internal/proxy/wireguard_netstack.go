// SPDX-License-Identifier: MIT
//
// Network setup adapted from WireGuard's tun/netstack/tun.go at
// b3bc704ef0ee, Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// wireGuardNetstack reads the link's bounded packet queue directly. In
// particular, no notification callback can outlive a separate output channel.
// CQ owns name resolution; this network accepts literal TCP/UDP addresses only.
type wireGuardNetstack struct {
	stack  *stack.Stack
	link   *channel.Endpoint
	mtu    int
	events chan tun.Event
	life   context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	calls  sync.WaitGroup
	once   sync.Once
}

func createWireGuardNetTUN(addresses []netip.Addr, mtu int) (tun.Device, *wireGuardNetstack, error) {
	life, cancel := context.WithCancel(context.Background())
	n := &wireGuardNetstack{
		stack: stack.New(stack.Options{
			NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
			TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
			HandleLocal:        true,
		}),
		link: channel.New(1024, uint32(mtu), ""), mtu: mtu,
		events: make(chan tun.Event, 1), life: life, cancel: cancel,
	}
	fail := func(err tcpip.Error) (tun.Device, *wireGuardNetstack, error) {
		_ = n.Close()
		return nil, nil, errors.New("initialise WireGuard userspace network: " + err.String())
	}
	sack := tcpip.TCPSACKEnabled(true)
	if err := n.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		return fail(err)
	}
	if err := n.stack.CreateNIC(1, n.link); err != nil {
		return fail(err)
	}
	for _, ip := range addresses {
		if !ip.IsValid() || ip.Zone() != "" {
			_ = n.Close()
			return nil, nil, errors.New("invalid WireGuard userspace address")
		}
		protocol := ipv6.ProtocolNumber
		if ip.Is4() {
			protocol = ipv4.ProtocolNumber
		}
		if err := n.stack.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: protocol, AddressWithPrefix: tcpip.AddrFromSlice(ip.AsSlice()).WithPrefix()}, stack.AddressProperties{}); err != nil {
			return fail(err)
		}
	}
	// Runtime policy checks the profile's families and AllowedIPs before dial.
	n.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}, {Destination: header.IPv6EmptySubnet, NIC: 1}})
	n.events <- tun.EventUp
	return n, n, nil
}

func (n *wireGuardNetstack) begin() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return net.ErrClosed
	}
	n.calls.Add(1)
	return nil
}

func (n *wireGuardNetstack) File() *os.File           { return nil }
func (n *wireGuardNetstack) Name() (string, error)    { return "cq-wireguard", nil }
func (n *wireGuardNetstack) MTU() (int, error)        { return n.mtu, nil }
func (n *wireGuardNetstack) BatchSize() int           { return 1 }
func (n *wireGuardNetstack) Events() <-chan tun.Event { return n.events }

func (n *wireGuardNetstack) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if len(bufs) == 0 || len(sizes) == 0 || offset < 0 || offset > len(bufs[0]) {
		return 0, syscall.EINVAL
	}
	if err := n.begin(); err != nil {
		return 0, err
	}
	defer n.calls.Done()
	packet := n.link.ReadContext(n.life)
	if packet == nil {
		return 0, net.ErrClosed
	}
	defer packet.DecRef()
	if n.life.Err() != nil {
		return 0, net.ErrClosed
	}
	view := packet.ToView()
	defer view.Release()
	sizes[0] = copy(bufs[0][offset:], view.AsSlice())
	if sizes[0] != view.Size() {
		return 0, io.ErrShortBuffer
	}
	return 1, nil
}

func (n *wireGuardNetstack) Write(bufs [][]byte, offset int) (int, error) {
	if err := n.begin(); err != nil {
		return 0, err
	}
	defer n.calls.Done()
	for _, data := range bufs {
		if offset < 0 || offset > len(data) {
			return 0, syscall.EINVAL
		}
		data = data[offset:]
		if len(data) == 0 {
			continue
		}
		protocol := ipv4.ProtocolNumber
		switch data[0] >> 4 {
		case 4:
		case 6:
			protocol = ipv6.ProtocolNumber
		default:
			return 0, syscall.EAFNOSUPPORT
		}
		packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
		n.link.InjectInbound(protocol, packet)
		packet.DecRef()
	}
	return len(bufs), nil
}

func (n *wireGuardNetstack) Close() error {
	n.once.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.cancel()
		n.mu.Unlock()
		// Fencing admission and joining dial creation prevents Stack.Close from
		// missing an endpoint created concurrently with its endpoint snapshot.
		n.calls.Wait()
		n.stack.Close()
		n.stack.Wait()
		n.link.Close()
		close(n.events)
	})
	return nil
}

func wireGuardFullAddress(address netip.AddrPort) (tcpip.FullAddress, tcpip.NetworkProtocolNumber) {
	ip := address.Addr().Unmap()
	protocol := ipv6.ProtocolNumber
	if ip.Is4() {
		protocol = ipv4.ProtocolNumber
	}
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ip.AsSlice()), Port: address.Port()}, protocol
}

func (n *wireGuardNetstack) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	remote, err := netip.ParseAddrPort(address)
	if err != nil || remote.Addr().Zone() != "" || !wireGuardNetworkFamily(network, remote.Addr()) {
		return nil, errors.New("WireGuard userspace network requires a literal address of the requested family")
	}
	switch network {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
	default:
		return nil, net.UnknownNetworkError(network)
	}
	if err := n.begin(); err != nil {
		return nil, err
	}
	defer n.calls.Done()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(n.life, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, protocol := wireGuardFullAddress(remote)
	var c net.Conn
	if strings.HasPrefix(network, "tcp") {
		c, err = gonet.DialContextTCP(ctx, n.stack, full, protocol)
	} else {
		c, err = gonet.DialUDP(n.stack, nil, &full, protocol)
	}
	if err != nil {
		if n.life.Err() != nil {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	if n.life.Err() != nil || ctx.Err() != nil {
		_ = c.Close()
		if n.life.Err() != nil {
			return nil, net.ErrClosed
		}
		return nil, ctx.Err()
	}
	return c, nil
}

var _ tun.Device = (*wireGuardNetstack)(nil)
