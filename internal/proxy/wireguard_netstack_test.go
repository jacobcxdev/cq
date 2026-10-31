package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

func TestWireGuardNetstackQueuesOutputWithoutTUNReader(t *testing.T) {
	tun, network, err := createWireGuardNetTUN([]netip.Addr{netip.MustParseAddr("10.18.0.2")}, 1420)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	c, err := network.DialContext(context.Background(), "udp4", "10.18.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	written := make(chan error, 1)
	go func() {
		_, err := c.Write([]byte("queued before teardown"))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		// Safely release the old synchronous WriteNotify before teardown.
		_, _ = tun.Read([][]byte{make([]byte, 1500)}, make([]int, 1), 0)
		<-written
		t.Fatal("socket output blocked on a TUN reader; teardown cannot join that writer")
	}
}

func TestWireGuardNetstackCloseWithQueuedSYNAndNoReader(t *testing.T) {
	tun, network, err := createWireGuardNetTUN([]netip.Addr{netip.MustParseAddr("10.18.0.2")}, 1420)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		c, err := network.DialContext(ctx, "tcp4", "10.18.0.1:443")
		if c != nil {
			c.Close()
		}
		dialed <- err
	}()
	waitWireGuardQueuedPacket(t, ctx, network)
	// The TCP SYN is queued, no reader can consume it, and the dial is waiting
	// for an answer. Close must cancel it and join all protocol workers.
	closed := make(chan struct{})
	go func() {
		tun.Close()
		close(closed)
	}()
	select {
	case err := <-dialed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("closing TCP dial: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("close did not cancel the queued TCP dial")
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("close did not join queued TCP workers")
	}
	for range tun.Events() {
	}
	if network.link.NumQueued() != 0 {
		t.Fatal("closed network retained queued packets")
	}
}

func waitWireGuardQueuedPacket(t *testing.T, ctx context.Context, network *wireGuardNetstack) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for network.link.NumQueued() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("TCP SYN never reached userspace queue")
		case <-ticker.C:
		}
	}
}

func TestWireGuardNetstackCloseUnblocksReaders(t *testing.T) {
	tun, network, err := createWireGuardNetTUN([]netip.Addr{netip.MustParseAddr("10.18.0.2")}, 1420)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	c, err := network.DialContext(context.Background(), "udp4", "10.18.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reads := make(chan error, 2)
	go func() {
		_, err := c.Read(make([]byte, 1))
		reads <- err
	}()
	go func() {
		_, err := tun.Read([][]byte{make([]byte, 1500)}, make([]int, 1), 0)
		reads <- err
	}()
	if err := tun.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-reads:
			if err == nil {
				t.Fatal("read succeeded after teardown")
			}
		case <-time.After(time.Second):
			t.Fatal("close left a TUN or UDP reader blocked")
		}
	}
}

func TestWireGuardNetstackConcurrentDialWriteAndClose(t *testing.T) {
	for range 10 {
		tun, network, err := createWireGuardNetTUN([]netip.Addr{netip.MustParseAddr("10.18.0.2")}, 1420)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var calls sync.WaitGroup
		start := make(chan struct{})
		wrote := make(chan struct{}, 2)
		continueWriting := make(chan struct{})
		for _, kind := range []string{"tcp4", "udp4", "udp4"} {
			calls.Add(1)
			go func() {
				defer calls.Done()
				<-start
				c, err := network.DialContext(ctx, kind, "10.18.0.1:443")
				if err != nil {
					return
				}
				defer c.Close()
				for index := range 32 {
					if _, err := c.Write([]byte("concurrent close")); err != nil {
						return
					}
					if index == 0 {
						wrote <- struct{}{}
						<-continueWriting
					}
				}
			}()
		}
		close(start)
		select {
		case <-wrote:
		case <-ctx.Done():
			close(continueWriting)
			tun.Close()
			calls.Wait()
			cancel()
			t.Fatal("no admitted UDP writer before teardown")
		}
		close(continueWriting)
		if err := tun.Close(); err != nil {
			t.Fatal(err)
		}
		calls.Wait()
		cancel()
		for _, kind := range []string{"tcp4", "udp4"} {
			if c, err := network.DialContext(context.Background(), kind, "10.18.0.1:443"); c != nil || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("dial admitted after close: %v, %v", c, err)
			}
		}
	}
}

// Only synthetic peers need listeners; production egress exposes dialling.
func (n *wireGuardNetstack) ListenTCPAddrPort(address netip.AddrPort) (*gonet.TCPListener, error) {
	if err := n.begin(); err != nil {
		return nil, err
	}
	defer n.calls.Done()
	full, protocol := wireGuardFullAddress(address)
	return gonet.ListenTCP(n.stack, full, protocol)
}

func (n *wireGuardNetstack) ListenUDPAddrPort(address netip.AddrPort) (*gonet.UDPConn, error) {
	if err := n.begin(); err != nil {
		return nil, err
	}
	defer n.calls.Done()
	full, protocol := wireGuardFullAddress(address)
	return gonet.DialUDP(n.stack, &full, nil, protocol)
}

func TestWireGuardNetstackRejectsInvalidDialAndPacketBuffers(t *testing.T) {
	tun, network, err := createWireGuardNetTUN([]netip.Addr{netip.MustParseAddr("10.18.0.2")}, 1420)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	for _, tc := range []struct{ network, address string }{{"udp", "dns.example:443"}, {"udp6", "10.18.0.1:443"}, {"tcp4", "[fd18::1]:443"}, {"ping", "10.18.0.1:443"}, {"tcp", "[fe80::1%en0]:443"}} {
		if c, err := network.DialContext(context.Background(), tc.network, tc.address); c != nil || err == nil {
			t.Fatalf("invalid dial accepted: %v, %v", tc, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := network.DialContext(ctx, "tcp4", "10.18.0.1:443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled dial: %v", err)
	}
	if _, err := tun.Read(nil, nil, 0); err == nil {
		t.Fatal("empty read buffers accepted")
	}
	if _, err := tun.Write([][]byte{{4}}, -1); err == nil {
		t.Fatal("invalid packet offset accepted")
	}
	if _, err := tun.Write([][]byte{{0x10}}, 0); err == nil {
		t.Fatal("unknown IP protocol accepted")
	}
}
