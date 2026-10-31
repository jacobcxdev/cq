package proxy

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
	"golang.org/x/net/dns/dnsmessage"
	wgconn "golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/conn/bindtest"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// This is a real WireGuard handshake and encrypted packet path, but even the
// outer transport is a pair of channels. No OS listener, tunnel, or route exists.
func TestWireGuardInMemoryEgressStreamsAndDNSFailover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clientKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var clientPrivate, clientPublic, serverPrivate, serverPublic [32]byte
	copy(clientPrivate[:], clientKey.Bytes())
	copy(clientPublic[:], clientKey.PublicKey().Bytes())
	copy(serverPrivate[:], serverKey.Bytes())
	copy(serverPublic[:], serverKey.PublicKey().Bytes())
	serverIP := netip.MustParseAddr("10.13.0.1")
	clientIP := netip.MustParseAddr("10.13.0.2")
	binds := bindtest.NewChannelBinds()
	tun, stack, err := netstack.CreateNetTUN([]netip.Addr{serverIP}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	peer := device.NewDevice(tun, binds[1], device.NewLogger(device.LogLevelSilent, ""))
	defer closeWireGuardDevice(peer)
	serverProfile := &wireGuardProfile{privateKey: serverPrivate, peers: []wireGuardPeer{{publicKey: clientPublic, allowedIPs: []netip.Prefix{netip.PrefixFrom(clientIP, 32)}}}}
	if err := peer.IpcSet(serverProfile.ipcConfig([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:2")})); err != nil {
		t.Fatal(err)
	}
	if err := peer.Up(); err != nil {
		t.Fatal(err)
	}
	dns, err := stack.ListenUDPAddrPort(netip.AddrPortFrom(serverIP, 53))
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	start := func(fn func()) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("synthetic peer panic: %v", p)
				}
			}()
			fn()
		}()
	}
	var queries atomic.Int32
	start(func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := dns.ReadFrom(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if err := q.Unpack(buf[:n]); err != nil {
				t.Errorf("DNS query: %v", err)
				return
			}
			reply := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RecursionAvailable: true}, Questions: q.Questions}
			for _, question := range q.Questions {
				if question.Name.String() != "egress.synthetic.test." {
					t.Errorf("unexpected DNS name %q", question.Name.String())
					continue
				}
				queries.Add(1)
				if question.Type == dnsmessage.TypeA {
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: serverIP.As4()}})
				}
			}
			packet, err := reply.Pack()
			if err != nil {
				t.Errorf("DNS response: %v", err)
				return
			}
			if _, err = dns.WriteTo(packet, addr); err != nil {
				return
			}
		}
	})
	cert, roots := wireGuardSyntheticCertificate(t)
	listener, err := stack.ListenTCPAddrPort(netip.AddrPortFrom(serverIP, 443))
	if err != nil {
		dns.Close()
		workers.Wait()
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
	var authenticated atomic.Int32
	var handlers sync.WaitGroup
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("synthetic HTTP handler panic: %v", p)
			}
		}()
		if r.Header.Get("Authorization") != "Bearer synthetic-test-only" {
			http.Error(w, "unauthorized", 401)
			return
		}
		authenticated.Add(1)
		if r.URL.Path == "/ws" {
			c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			kind, data, err := c.ReadMessage()
			if err != nil {
				t.Errorf("websocket read: %v", err)
				return
			}
			if err := c.WriteMessage(kind, append([]byte("echo:"), data...)); err != nil {
				t.Errorf("websocket write: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-ctx.Done():
			return
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	start(func() {
		if err := server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})); err != nil && err != http.ErrServerClosed {
			t.Errorf("serve: %v", err)
		}
	})
	// The first DNS address is routed into the tunnel but has no responder.
	// A successful UDP dial must not prevent query-level fallback to serverIP.
	runtime := newWireGuardRuntime(&wireGuardProfile{addresses: []netip.Addr{clientIP}, dnsServers: []netip.Addr{netip.MustParseAddr("10.13.0.3"), serverIP}, privateKey: clientPrivate, mtu: 1420, peers: []wireGuardPeer{{publicKey: serverPublic, endpoint: "127.0.0.1:1", allowedIPs: []netip.Prefix{netip.MustParsePrefix("10.13.0.0/24")}}}}, func() wgconn.Bind { return binds[0] })
	defer func() {
		releaseStream()
		shutdown, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
		_ = dns.Close()
		// Shutdown does not join hijacked WebSocket handlers. Finish all
		// fixture I/O before closing the client's userspace network.
		workers.Wait()
		handlers.Wait()
		_ = runtime.Close()
	}()
	egress, err := codexEgressWithDialer(runtime.DialContext, runtime.Close)
	if err != nil {
		t.Fatal(err)
	}
	transport := egress.HTTP.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	defer transport.CloseIdleConnections()
	egress.WebSocket.TLSClientConfig = transport.TLSClientConfig.Clone()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://egress.synthetic.test/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer synthetic-test-only")
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatalf("HTTPS through DNS failover: %v (healthy DNS queries: %d)", err, queries.Load())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatalf("first streaming chunk = %q, %v", first, err)
	}
	// The handler cannot finish until this release: receiving first proves streaming.
	releaseStream()
	rest, err := httputil.ReadBody(reader)
	if err != nil || !strings.Contains(string(rest), "data: last\n\n") {
		t.Fatalf("remaining SSE = %q, %v", rest, err)
	}
	ws, _, err := egress.WebSocket.DialContext(ctx, "wss://egress.synthetic.test/ws", http.Header{"Authorization": []string{"Bearer synthetic-test-only"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, []byte("stream")); err != nil {
		t.Fatal(err)
	}
	_, message, err := ws.ReadMessage()
	if err != nil || string(message) != "echo:stream" {
		t.Fatalf("WebSocket = %q, %v", message, err)
	}
	if queries.Load() == 0 {
		t.Fatal("hostname never reached in-tunnel DNS")
	}
	if authenticated.Load() != 2 {
		t.Fatalf("authenticated requests = %d", authenticated.Load())
	}
}

func wireGuardSyntheticCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "egress.synthetic.test"}, DNSNames: []string{"egress.synthetic.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}

// wireGuardMemoryBind is a multi-endpoint encrypted datagram bus. Every packet
// remains in memory, including handshakes and packets sent to an offline peer.
type wireGuardMemoryPacket struct {
	data   []byte
	source uint16
}
type wireGuardMemoryBind struct {
	id   uint16
	bus  map[uint16]*wireGuardMemoryBind
	rx   chan wireGuardMemoryPacket
	done chan struct{}
}

func (b *wireGuardMemoryBind) Open(uint16) ([]wgconn.ReceiveFunc, uint16, error) {
	b.done = make(chan struct{})
	receive := func(bufs [][]byte, sizes []int, endpoints []wgconn.Endpoint) (int, error) {
		select {
		case <-b.done:
			return 0, net.ErrClosed
		case p := <-b.rx:
			sizes[0] = copy(bufs[0], p.data)
			endpoints[0] = bindtest.ChannelEndpoint(p.source)
			return 1, nil
		}
	}
	return []wgconn.ReceiveFunc{receive}, b.id, nil
}
func (b *wireGuardMemoryBind) Close() error {
	if b.done != nil {
		select {
		case <-b.done:
		default:
			close(b.done)
		}
	}
	return nil
}
func (*wireGuardMemoryBind) SetMark(uint32) error { return nil }
func (*wireGuardMemoryBind) BatchSize() int       { return 1 }
func (*wireGuardMemoryBind) ParseEndpoint(s string) (wgconn.Endpoint, error) {
	a, e := netip.ParseAddrPort(s)
	return bindtest.ChannelEndpoint(a.Port()), e
}
func (b *wireGuardMemoryBind) Send(bufs [][]byte, endpoint wgconn.Endpoint) error {
	target := b.bus[uint16(endpoint.(bindtest.ChannelEndpoint))]
	if target == nil {
		return nil
	}
	for _, data := range bufs {
		p := wireGuardMemoryPacket{data: append([]byte(nil), data...), source: b.id}
		select {
		case <-b.done:
			return net.ErrClosed
		case target.rx <- p:
		}
	}
	return nil
}

func TestWireGuardInMemoryMultiplePeersLongestPrefixAndNoFallback(t *testing.T) {
	bus := map[uint16]*wireGuardMemoryBind{}
	for _, id := range []uint16{1, 2, 3} {
		bus[id] = &wireGuardMemoryBind{id: id, bus: bus, rx: make(chan wireGuardMemoryPacket, 8192)}
	}
	key := func() ([32]byte, [32]byte) {
		t.Helper()
		k, e := ecdh.X25519().GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		var private, public [32]byte
		copy(private[:], k.Bytes())
		copy(public[:], k.PublicKey().Bytes())
		return private, public
	}
	clientPrivate, clientPublic := key()
	broadPrivate, broadPublic := key()
	narrowPrivate, narrowPublic := key()
	clientIP := netip.MustParseAddr("10.14.0.2")
	broadIP := netip.MustParseAddr("10.14.0.1")
	narrowIP := netip.MustParseAddr("10.14.0.129")
	isolatedIP := netip.MustParseAddr("10.15.0.1")
	var broadCalls, narrowCalls atomic.Int32
	var workers sync.WaitGroup
	t.Cleanup(workers.Wait)
	server := func(id uint16, private [32]byte, ips []netip.Addr, label string, calls *atomic.Int32) *device.Device {
		t.Helper()
		tun, stack, e := netstack.CreateNetTUN(ips, nil, 1420)
		if e != nil {
			t.Fatal(e)
		}
		dev := device.NewDevice(tun, bus[id], device.NewLogger(device.LogLevelSilent, ""))
		t.Cleanup(func() { closeWireGuardDevice(dev) })
		p := &wireGuardProfile{privateKey: private, peers: []wireGuardPeer{{publicKey: clientPublic, allowedIPs: []netip.Prefix{netip.PrefixFrom(clientIP, 32)}}}}
		if e := dev.IpcSet(p.ipcConfig([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")})); e != nil {
			t.Fatal(e)
		}
		if e := dev.Up(); e != nil {
			t.Fatal(e)
		}
		for _, ip := range ips {
			listener, e := stack.ListenTCPAddrPort(netip.AddrPortFrom(ip, 443))
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { listener.Close() })
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() {
					if recover() != nil {
						t.Error("synthetic multiple-peer listener panic")
					}
				}()
				for {
					c, e := listener.Accept()
					if e != nil {
						return
					}
					calls.Add(1)
					_ = c.SetWriteDeadline(time.Now().Add(time.Second))
					_, _ = io.WriteString(c, label)
					_ = c.Close()
				}
			}()
		}
		return dev
	}
	// Both servers can answer the narrow address: observing the returned label
	// proves cryptographic route selection, rather than mere destination reachability.
	_ = server(2, broadPrivate, []netip.Addr{broadIP, narrowIP, isolatedIP}, "broad", &broadCalls)
	narrow := server(3, narrowPrivate, []netip.Addr{narrowIP, isolatedIP}, "narrow", &narrowCalls)
	profile := &wireGuardProfile{addresses: []netip.Addr{clientIP}, mtu: 1420, privateKey: clientPrivate, peers: []wireGuardPeer{
		// Insert the broader peer last to prove longest-prefix lookup rather
		// than the most recently configured peer determines the destination.
		{publicKey: narrowPublic, endpoint: "127.0.0.1:3", allowedIPs: []netip.Prefix{netip.PrefixFrom(narrowIP, 32), netip.MustParsePrefix("10.15.0.0/24")}},
		{publicKey: broadPublic, endpoint: "127.0.0.1:2", allowedIPs: []netip.Prefix{netip.MustParsePrefix("10.14.0.0/24")}},
	}}
	runtime := newWireGuardRuntime(profile, func() wgconn.Bind { return bus[1] })
	t.Cleanup(func() { runtime.Close() })
	var resolutions atomic.Int32
	runtime.resolveEndpoint = func(ctx context.Context, s string) (netip.AddrPort, error) {
		resolutions.Add(1)
		return resolveWireGuardEndpoint(ctx, s)
	}
	dial := func(ip netip.Addr, want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, e := runtime.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, 443).String())
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		body := make([]byte, len(want))
		if _, e := io.ReadFull(c, body); e != nil || string(body) != want {
			t.Fatal("wrong peer handled destination")
		}
	}
	dial(broadIP, "broad")
	dial(narrowIP, "narrow")
	dial(isolatedIP, "narrow")
	if resolutions.Load() != 2 {
		t.Fatal("peer endpoints were not resolved exactly once")
	}
	closeWireGuardDevice(narrow)
	for _, ip := range []netip.Addr{narrowIP, isolatedIP} {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		c, e := runtime.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, 443).String())
		cancel()
		if c != nil {
			c.Close()
		}
		if e == nil {
			t.Fatal("offline peer fell back to another peer")
		}
	}
	dial(broadIP, "broad")
	if broadCalls.Load() != 2 || narrowCalls.Load() != 2 {
		t.Fatal("unexpected cross-peer fallback")
	}
}
