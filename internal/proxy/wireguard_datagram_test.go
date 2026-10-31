package proxy

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	wgconn "golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/conn/bindtest"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func TestWireGuardConnectedDatagramsPreservePacketsAndTunnelDNS(t *testing.T) {
	runtime, serverIPs, queries := wireGuardDatagramPeer(t, nil)
	for _, network := range []string{"udp", "udp4", "udp6"} {
		t.Run(network, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := runtime.DialContext(ctx, network, "datagram.synthetic.test:443")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			remote, err := netip.ParseAddrPort(c.RemoteAddr().String())
			if err != nil || (network == "udp4" && remote.Addr() != serverIPs[0]) || (network == "udp6" && remote.Addr() != serverIPs[1]) {
				t.Fatalf("wrong datagram family: %s, %v", c.RemoteAddr(), err)
			}
			// A connected flow cannot be retargeted through PacketConn.WriteTo.
			if _, ok := c.(net.PacketConn); ok {
				t.Fatal("connected datagram exposed an unrestricted destination API")
			}
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			packets := [][]byte{[]byte("first"), bytes.Repeat([]byte{0x5a}, 1000), {}, bytes.Repeat([]byte{0xa5}, 65507)}
			for _, packet := range packets {
				if n, err := c.Write(packet); err != nil || n != len(packet) {
					t.Fatalf("datagram write = %d, %v", n, err)
				}
			}
			for _, want := range packets {
				buffer := make([]byte, 65507)
				n, err := c.Read(buffer)
				if err != nil || !bytes.Equal(buffer[:n], want) {
					t.Fatalf("datagram boundaries changed: got %d bytes, want %d, error %v", n, len(want), err)
				}
			}
		})
	}
	if queries.Load() == 0 {
		t.Fatal("destination hostname bypassed tunnel DNS")
	}
}

func TestWireGuardDatagramValidationDoesNotActivateTunnel(t *testing.T) {
	p := &wireGuardProfile{addresses: []netip.Addr{netip.MustParseAddr("10.17.0.2")}, peers: []wireGuardPeer{{allowedIPs: []netip.Prefix{netip.MustParsePrefix("10.17.0.0/24")}}}}
	var starts atomic.Int32
	runtime := newWireGuardRuntime(p, func() wgconn.Bind { starts.Add(1); panic("unexpected activation") })
	defer runtime.Close()
	for _, tc := range []struct{ network, address string }{
		{"udp", "203.0.113.1:443"},
		{"udp", "[2001:db8::1]:443"},
		{"udp", "10.17.0.1:0"},
		{"udp", "10.17.0.1:65536"},
		{"udp", "bad address"},
		{"udp6", "10.17.0.1:443"},
		{"unixgram", "10.17.0.1:443"},
	} {
		if c, err := runtime.DialContext(context.Background(), tc.network, tc.address); err == nil {
			c.Close()
			t.Fatalf("invalid datagram accepted: %s %s", tc.network, tc.address)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.DialContext(ctx, "udp", "10.17.0.1:443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled UDP dial: %v", err)
	}
	_ = runtime.Close()
	if _, err := runtime.DialContext(context.Background(), "udp", "10.17.0.1:443"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed UDP runtime: %v", err)
	}
	if starts.Load() != 0 {
		t.Fatal("invalid or closed datagram activated tunnel")
	}
}

func TestWireGuardDatagramRuntimeCloseUnblocksRead(t *testing.T) {
	runtime, serverIPs, _ := wireGuardDatagramPeer(t, nil)
	c, err := runtime.DialContext(context.Background(), "udp", netip.AddrPortFrom(serverIPs[0], 443).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("datagram read panicked")
			}
		}()
		_, err := c.Read(make([]byte, 1))
		done <- err
	}()
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("UDP read succeeded after runtime closed")
		}
	case <-time.After(time.Second):
		t.Fatal("runtime close left UDP read blocked")
	}
}

func TestWireGuardDatagramCancellationDuringTunnelDNS(t *testing.T) {
	runtime, _, queries := wireGuardDatagramPeer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("datagram dial panicked")
			}
		}()
		c, err := runtime.DialContext(ctx, "udp", "cancel.synthetic.test:443")
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for queries.Load() == 0 {
		select {
		case <-timeout.C:
			t.Fatal("datagram DNS query never reached tunnel peer")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled tunnel DNS dial = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled UDP dial left tunnel DNS blocked")
	}
}

func TestWireGuardNativeDestinationPolicy(t *testing.T) {
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:1.1.1.1"} {
		if !isNativePublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("public destination rejected: %s", address)
		}
	}
	for _, address := range []string{
		"0.0.0.0", "0.1.2.3", "10.17.0.1", "100.64.0.1", "127.0.0.1", "169.254.1.1", "172.16.0.1", "192.0.0.1", "192.0.2.1", "192.88.99.1", "192.168.0.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "100::1", "2001::1", "2001:2::1", "2001:db8::1", "2002:7f00:1::1", "3fff::1", "fc00::1", "fe80::1", "ff02::1", "2606:4700::1111%en0",
	} {
		if isNativePublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("non-public destination accepted: %s", address)
		}
	}
	if isNativePublicAddress(netip.Addr{}) {
		t.Error("invalid destination accepted")
	}
	var starts atomic.Int32
	profile := &wireGuardProfile{addresses: []netip.Addr{netip.MustParseAddr("10.17.0.2"), netip.MustParseAddr("fd17::2")}, peers: []wireGuardPeer{{allowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}}}
	runtime := newWireGuardRuntime(profile, func() wgconn.Bind { starts.Add(1); panic("unexpected activation") })
	defer runtime.Close()
	for _, network := range []string{"tcp", "udp", "udp4", "udp6"} {
		for _, address := range []string{"10.17.0.1:443", "[fd17::1]:443", "127.0.0.1:443", "203.0.113.1:443", "[2001:db8::1]:443"} {
			if c, err := runtime.DialNativeContext(context.Background(), network, address); err == nil {
				c.Close()
				t.Errorf("native private destination accepted: %s %s", network, address)
			}
		}
	}
	if starts.Load() != 0 {
		t.Fatal("private native destination activated tunnel")
	}
}

func TestWireGuardNativeDatagramsResolveInsideTunnel(t *testing.T) {
	publicIPs := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2606:4700:4700::1111")}
	runtime, _, queries := wireGuardDatagramPeer(t, publicIPs)
	for _, network := range []string{"udp4", "udp6"} {
		c, err := runtime.DialNativeContext(context.Background(), network, "datagram.synthetic.test:443")
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		packet := []byte("native datagram")
		if _, err := c.Write(packet); err != nil {
			c.Close()
			t.Fatal(err)
		}
		buffer := make([]byte, 100)
		n, err := c.Read(buffer)
		c.Close()
		if err != nil || !bytes.Equal(buffer[:n], packet) {
			t.Fatalf("native datagram roundtrip = %q, %v", buffer[:n], err)
		}
	}
	if queries.Load() == 0 {
		t.Fatal("native destination bypassed tunnel DNS")
	}
}

func TestWireGuardNativeDatagramsRejectPrivateDNSResults(t *testing.T) {
	runtime, _, queries := wireGuardDatagramPeer(t, nil)
	if c, err := runtime.DialNativeContext(context.Background(), "udp", "datagram.synthetic.test:443"); err == nil {
		c.Close()
		t.Fatal("native DNS result escaped public-address policy")
	}
	if queries.Load() == 0 {
		t.Fatal("native destination was not resolved in tunnel")
	}
	// Existing configured private routes remain usable through the generic API.
	c, err := runtime.DialContext(context.Background(), "udp", "datagram.synthetic.test:443")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// Real encrypted WireGuard packets and DNS stay in channel-backed binds. No
// host socket, profile, TUN interface, route or resolver is accessed.
func wireGuardDatagramPeer(t *testing.T, serverIPs []netip.Addr) (*wireGuardRuntime, []netip.Addr, *atomic.Int32) {
	t.Helper()
	key := func() (private, public [32]byte) {
		t.Helper()
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		copy(private[:], k.Bytes())
		copy(public[:], k.PublicKey().Bytes())
		return
	}
	clientPrivate, clientPublic := key()
	serverPrivate, serverPublic := key()
	if serverIPs == nil {
		serverIPs = []netip.Addr{netip.MustParseAddr("10.17.0.1"), netip.MustParseAddr("fd17::1")}
	}
	clientIPs := []netip.Addr{netip.MustParseAddr("10.17.0.2"), netip.MustParseAddr("fd17::2")}
	binds := bindtest.NewChannelBinds()
	tun, stack, err := netstack.CreateNetTUN(serverIPs, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	server := device.NewDevice(tun, binds[1], device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(func() { closeWireGuardDevice(server) })
	serverProfile := &wireGuardProfile{privateKey: serverPrivate, peers: []wireGuardPeer{{publicKey: clientPublic, allowedIPs: []netip.Prefix{netip.PrefixFrom(clientIPs[0], 32), netip.PrefixFrom(clientIPs[1], 128)}}}}
	if err := server.IpcSet(serverProfile.ipcConfig([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:2")})); err != nil {
		t.Fatal(err)
	}
	if err := server.Up(); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	t.Cleanup(workers.Wait)
	start := func(fn func()) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("synthetic datagram peer panic: %v", p)
				}
			}()
			fn()
		}()
	}
	for _, ip := range serverIPs {
		listener, err := stack.ListenUDPAddrPort(netip.AddrPortFrom(ip, 443))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		start(func() {
			buffer := make([]byte, 65535)
			for {
				n, remote, err := listener.ReadFrom(buffer)
				if err != nil {
					return
				}
				if _, err := listener.WriteTo(buffer[:n], remote); err != nil {
					return
				}
			}
		})
	}
	dns, err := stack.ListenUDPAddrPort(netip.AddrPortFrom(serverIPs[0], 53))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dns.Close() })
	queries := &atomic.Int32{}
	start(func() {
		buffer := make([]byte, 1500)
		for {
			n, remote, err := dns.ReadFrom(buffer)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if err := q.Unpack(buffer[:n]); err != nil {
				t.Errorf("datagram DNS query: %v", err)
				return
			}
			if len(q.Questions) == 1 && q.Questions[0].Name.String() == "cancel.synthetic.test." {
				queries.Add(1)
				continue // Exercise cancellation while an actual tunnel lookup waits.
			}
			reply := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RecursionAvailable: true}, Questions: q.Questions}
			for _, question := range q.Questions {
				if question.Name.String() != "datagram.synthetic.test." {
					t.Errorf("unexpected datagram DNS name %q", question.Name.String())
					return
				}
				queries.Add(1)
				header := dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60}
				switch question.Type {
				case dnsmessage.TypeA:
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AResource{A: serverIPs[0].As4()}})
				case dnsmessage.TypeAAAA:
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AAAAResource{AAAA: serverIPs[1].As16()}})
				}
			}
			packet, err := reply.Pack()
			if err != nil {
				t.Errorf("datagram DNS response: %v", err)
				return
			}
			if _, err := dns.WriteTo(packet, remote); err != nil {
				return
			}
		}
	})
	runtime := newWireGuardRuntime(&wireGuardProfile{addresses: clientIPs, dnsServers: serverIPs[:1], privateKey: clientPrivate, mtu: 1420, peers: []wireGuardPeer{{publicKey: serverPublic, endpoint: "127.0.0.1:1", allowedIPs: []netip.Prefix{netip.PrefixFrom(serverIPs[0], 32), netip.PrefixFrom(serverIPs[1], 128)}}}}, func() wgconn.Bind { return binds[0] })
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime, serverIPs, queries
}
