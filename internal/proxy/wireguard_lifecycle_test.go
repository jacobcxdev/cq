package proxy

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wgconn "golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/conn/bindtest"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// A real encrypted TCP SYN is held inside the peer's sequential receiver.
// Closing the runtime must stop the bind and join that receiver before closing
// its TUN. The gate makes the otherwise timing-dependent shutdown race explicit.
func TestWireGuardCloseQuiescesIncomingPacketsBeforeTUN(t *testing.T) {
	t.Run("incoming packet", func(t *testing.T) { testWireGuardCloseBeforeTUN(t, false) })
	t.Run("queued initial up event", func(t *testing.T) { testWireGuardCloseBeforeTUN(t, true) })
}

func testWireGuardCloseBeforeTUN(t *testing.T, queuedUp bool) {
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
	clientIP := netip.MustParseAddr("10.16.0.2")
	serverIP := netip.MustParseAddr("10.16.0.1")
	binds := bindtest.NewChannelBinds()
	serverBind := &wireGuardObservedCloseBind{Bind: binds[1], closed: make(chan struct{})}
	serverTUN, _, err := netstack.CreateNetTUN([]netip.Addr{serverIP}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	gate := &wireGuardIncomingGateTUN{Device: serverTUN, entered: make(chan struct{}), release: make(chan struct{}), closing: make(chan struct{})}
	if queuedUp {
		// The library enqueues EventUp before NewDevice starts its event reader.
		// Hold that event until teardown is already inside Device.Down.
		initialUp := <-serverTUN.Events()
		gate.events = make(chan tun.Event, 1)
		serverBind.beforeClose = func() {
			gate.eventsMu.Lock()
			defer gate.eventsMu.Unlock()
			if !gate.eventsClosed {
				gate.events <- initialUp
			}
		}
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	server := device.NewDevice(gate, serverBind, device.NewLogger(device.LogLevelSilent, ""))
	if queuedUp {
		gate.checkClose = func() {
			// A delayed Up can acquire the device state lock between Down and
			// Close. Remove the configured peer so it cannot restart a receiver,
			// irrespective of which goroutine wins that state-lock race.
			if server.LookupPeer(device.NoisePublicKey(clientPublic)) != nil {
				t.Error("runtime left a peer available for queued EventUp to restart before TUN closure")
			}
		}
	}
	runtime := newWireGuardRuntime(nil, nil)
	runtime.device = server
	defer func() {
		release()
		_ = runtime.Close()
	}()
	serverProfile := &wireGuardProfile{privateKey: serverPrivate, peers: []wireGuardPeer{{publicKey: clientPublic, allowedIPs: []netip.Prefix{netip.PrefixFrom(clientIP, 32)}}}}
	if err := server.IpcSet(serverProfile.ipcConfig([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:2")})); err != nil {
		t.Fatal(err)
	}
	if err := server.Up(); err != nil {
		t.Fatal(err)
	}
	client := newWireGuardRuntime(&wireGuardProfile{addresses: []netip.Addr{clientIP}, mtu: 1420, privateKey: clientPrivate, peers: []wireGuardPeer{{publicKey: serverPublic, endpoint: "127.0.0.1:1", allowedIPs: []netip.Prefix{netip.PrefixFrom(serverIP, 32)}}}}, func() wgconn.Bind { return binds[0] })
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("synthetic dial panic: %v", p)
			}
		}()
		c, _ := client.DialContext(ctx, "tcp", net.JoinHostPort(serverIP.String(), "443"))
		if c != nil {
			_ = c.Close()
		}
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("encrypted TCP SYN did not enter peer receiver")
	}
	serverBind.observe.Store(true)
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("runtime close panic: %v", p)
			}
		}()
		_ = runtime.Close()
	}()
	select {
	case <-gate.closing:
		t.Error("runtime closed TUN while the peer receiver could still write packets")
	case <-serverBind.closed:
	case <-ctx.Done():
		t.Error("runtime did not stop the encrypted packet bind")
	}
	select {
	case <-gate.closing:
		t.Error("runtime did not wait for the held peer receiver before closing TUN")
	default:
	}
	release()
	select {
	case <-closeDone:
	case <-ctx.Done():
		t.Fatal("runtime did not join its packet workers")
	}
	select {
	case <-server.Wait():
	default:
		t.Error("runtime left the WireGuard device open")
	}
	cancel()
	<-dialDone
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

type wireGuardIncomingGateTUN struct {
	tun.Device
	mu           sync.Mutex
	closed       bool
	once         sync.Once
	entered      chan struct{}
	release      chan struct{}
	closing      chan struct{}
	checkClose   func()
	eventsMu     sync.Mutex
	events       chan tun.Event
	eventsClosed bool
}

func (t *wireGuardIncomingGateTUN) Events() <-chan tun.Event {
	if t.events != nil {
		return t.events
	}
	return t.Device.Events()
}

func (t *wireGuardIncomingGateTUN) Write(bufs [][]byte, offset int) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, net.ErrClosed
	}
	t.once.Do(func() {
		close(t.entered)
		<-t.release
	})
	return t.Device.Write(bufs, offset)
}

func (t *wireGuardIncomingGateTUN) Close() error {
	close(t.closing)
	if t.checkClose != nil {
		t.checkClose()
	}
	// Keep the regression failure safe: the underlying netstack must never be
	// closed until the deliberately held receiver is allowed to finish.
	<-t.release
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.events != nil {
		t.eventsMu.Lock()
		t.eventsClosed = true
		close(t.events)
		t.eventsMu.Unlock()
	}
	return t.Device.Close()
}

type wireGuardObservedCloseBind struct {
	wgconn.Bind
	observe     atomic.Bool
	once        sync.Once
	closed      chan struct{}
	beforeClose func()
}

func (b *wireGuardObservedCloseBind) Close() error {
	if b.observe.Load() {
		b.once.Do(func() {
			if b.beforeClose != nil {
				b.beforeClose()
			}
			close(b.closed)
		})
	}
	return b.Bind.Close()
}
