package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newNativeDNSLifecycleManager(t *testing.T) *EgressManager {
	t.Helper()
	m, err := newEgressManager(&Config{CodexWireGuardProfile: "first"}, func(cfg *Config) (*CodexEgress, [32]byte, error) {
		return &CodexEgress{nativeDial: func(context.Context, string, string) (net.Conn, error) {
			t.Error("on-link DNS used WireGuard")
			return nil, net.ErrClosed
		}}, sha256.Sum256([]byte(cfg.CodexWireGuardProfile)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.dnsLocalAddress = func(netip.Addr) (netip.Addr, bool) { return netip.MustParseAddr("2a00:23a8:67a:d900::232"), true }
	return m
}

func TestNativeDNSLANRetainsGenerationOwnership(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		for _, change := range []string{"replace", "block", "remove_profile"} {
			t.Run(network+"/"+change, func(t *testing.T) {
				m := newNativeDNSLifecycleManager(t)
				var calls atomic.Int32
				client, peer := net.Pipe()
				defer peer.Close()
				m.dnsDialLocal = func(context.Context, string, string, netip.Addr) (net.Conn, error) {
					calls.Add(1)
					return client, nil
				}
				conn, err := m.DialNativeContext(context.Background(), network, "[2a00:23a8:67a:d900::1]:53")
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				switch change {
				case "replace":
					err = m.Reload(&Config{CodexWireGuardProfile: "second"})
				case "block":
					m.Block()
				case "remove_profile":
					err = m.Reload(&Config{})
				}
				if err != nil {
					t.Fatal(err)
				}
				_ = peer.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("old LAN socket survived egress change: %v", err)
				}
				if change == "replace" {
					m.Block()
				}
				if conn, err := m.DialNativeContext(context.Background(), network, "[2a00:23a8:67a:d900::1]:53"); conn != nil || err == nil || calls.Load() != 1 {
					t.Fatalf("unavailable egress permitted LAN DNS: %v, %v, calls %d", conn, err, calls.Load())
				}
			})
		}
	}
}

func TestNativeDNSLANPendingDialCancelledOnReload(t *testing.T) {
	m := newNativeDNSLifecycleManager(t)
	started, done := make(chan struct{}), make(chan error, 1)
	m.dnsDialLocal = func(ctx context.Context, _, _ string, _ netip.Addr) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("LAN dial panicked")
			}
		}()
		_, err := m.DialNativeContext(context.Background(), "udp", "[2a00:23a8:67a:d900::1]:53")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("LAN dial did not start")
	}
	if err := m.Reload(&Config{CodexWireGuardProfile: "second"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LAN dial survived reload: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("LAN dial was not cancelled")
	}
}

func TestNativeDNSRoutesOnLinkResolverWithoutPublicFallback(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			source := netip.MustParseAddr("2a00:23a8:67a:d900::232")
			want := errors.New("LAN failure must not fall back")
			var events []nativeDNSEvent
			d := nativeDNSTraceDialer{
				EgressDialer: egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("on-link DNS used public egress")
					return nil, nil
				}),
				localAddress: func(ip netip.Addr) (netip.Addr, bool) { return nativeDNSSubnetAddress(ip, source.String()+"/64") },
				dialLocal: func(_ context.Context, gotNetwork, target string, gotSource netip.Addr) (net.Conn, error) {
					if gotNetwork != network || target != "[2a00:23a8:67a:d900::1]:53" || gotSource != source {
						t.Fatal("LAN binding changed")
					}
					return nil, want
				},
				emit: func(e nativeDNSEvent) { events = append(events, e) },
			}
			if _, err := d.DialContext(context.Background(), network, "[2a00:23a8:67a:d900::1]:53"); err != want {
				t.Fatalf("error = %v", err)
			}
			if len(events) != 1 || events[0].Route != "lan" || events[0].Network != network || !events[0].Failed {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

func TestNativeDNSPublicResolverKeepsConfiguredEgress(t *testing.T) {
	want := errors.New("configured egress failure")
	called := false
	d := nativeDNSTraceDialer{
		EgressDialer: egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) { called = true; return nil, want }),
		localAddress: func(ip netip.Addr) (netip.Addr, bool) {
			return nativeDNSSubnetAddress(ip, "2a00:23a8:67a:d900::232/64")
		},
		dialLocal: func(context.Context, string, string, netip.Addr) (net.Conn, error) {
			t.Fatal("public DNS used LAN fallback")
			return nil, nil
		},
	}
	if _, err := d.DialContext(context.Background(), "udp", "[2001:4860:4860::8888]:53"); err != want || !called {
		t.Fatalf("error = %v, configured egress = %v", err, called)
	}
}

func TestNativeDNSSubnetRequiresActualInterfacePrefix(t *testing.T) {
	for _, tc := range []struct {
		target, local string
		want          bool
	}{
		{"2a00:23a8:67a:d900::1", "2a00:23a8:67a:d900::232/64", true},
		{"2a00:23a8:67a:d901::1", "2a00:23a8:67a:d900::232/64", false},
		{"2a00:23a8:67a:d900::1", "::/0", false},
		{"2a00:23a8:67a:d900::1", "10.0.1.232/24", false},
		{"10.0.1.1", "10.0.1.232/24", true},
		{"8.8.8.8", "10.0.1.232/24", false},
		{"8.8.8.8", "0.0.0.0/0", false},
		{"8.8.8.8", "invalid", false},
		{"fe80::1", "fe80::232/64", false},
	} {
		t.Run(tc.target+tc.local, func(t *testing.T) {
			source, ok := nativeDNSSubnetAddress(netip.MustParseAddr(tc.target), tc.local)
			if ok != tc.want || (ok && source.String() != strings.Split(tc.local, "/")[0]) {
				t.Fatalf("source = %v, local = %v", source, ok)
			}
		})
	}
}

func TestNativeDNSLocalInterfaceExcludesTunnelsAndInactiveLinks(t *testing.T) {
	addresses := []net.Addr{&net.IPNet{IP: net.ParseIP("2a00:23a8:67a:d900::232"), Mask: net.CIDRMask(64, 128)}}
	for _, flags := range []net.Flags{0, net.FlagUp | net.FlagPointToPoint, net.FlagUp | net.FlagLoopback, net.FlagUp | net.FlagBroadcast} {
		_, ok := nativeDNSInterfaceAddress(netip.MustParseAddr("2a00:23a8:67a:d900::1"), flags, addresses)
		if ok != (flags == net.FlagUp|net.FlagBroadcast) {
			t.Fatalf("flags %v allowed = %v", flags, ok)
		}
	}
}

func TestNativeDNSDatagramKeepsFixedPeer(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := dialNativeLocalDNS(context.Background(), "udp", server.LocalAddr().String(), netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := conn.(net.PacketConn); ok {
		t.Fatal("caller can change datagram destination")
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_ = server.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64)
	n, client, err := server.ReadFromUDP(data)
	if err != nil || string(data[:n]) != "query" {
		t.Fatalf("server read = %d, %v", n, err)
	}
	if _, err := other.WriteToUDP([]byte("foreign"), client); err != nil {
		t.Fatal(err)
	}
	if _, err := server.WriteToUDP([]byte("answer"), client); err != nil {
		t.Fatal(err)
	}
	n, err = conn.Read(data)
	if err != nil || string(data[:n]) != "answer" {
		t.Fatalf("client read = %q, %v", data[:n], err)
	}
	if conn.RemoteAddr().String() != server.LocalAddr().String() {
		t.Fatal("remote peer changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := dialNativeLocalDNS(ctx, "udp", server.LocalAddr().String(), netip.MustParseAddr("127.0.0.1")); conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled dial = %v, %v", conn, err)
	}
}
