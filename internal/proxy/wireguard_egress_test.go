package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	wgconn "golang.zx2c4.com/wireguard/conn"
)

func TestWireGuardRuntimeRejectsWithoutActivation(t *testing.T) {
	p := &wireGuardProfile{addresses: []netip.Addr{netip.MustParseAddr("10.0.0.2")}, dnsServers: []netip.Addr{netip.MustParseAddr("10.0.0.1")}, peers: []wireGuardPeer{{allowedIPs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}}}}
	var starts atomic.Int32
	runtime := newWireGuardRuntime(p, func() wgconn.Bind { starts.Add(1); panic("unexpected activation") })
	defer runtime.Close()
	for _, destination := range []string{"203.0.113.1:443", "[2001:db8::1]:443", "bad address"} {
		if connection, err := runtime.DialContext(context.Background(), "tcp", destination); err == nil {
			connection.Close()
			t.Fatal("unrouted destination accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.DialContext(ctx, "tcp", "10.0.0.1:443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DialContext(context.Background(), "tcp", "10.0.0.1:443"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed runtime: %v", err)
	}
	if starts.Load() != 0 {
		t.Fatal("invalid or closed request activated tunnel")
	}
}

func TestWireGuardProfileReadIsPrivateAndSafe(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := filepath.Join("/private-profiles", "synthetic.conf")
	if err := fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []uint32{0o644, 0o600} {
		if err := fs.WriteFile(path, []byte("PrivateKey = secret-must-not-escape"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fs.Chmod(path, os.FileMode(mode)); err != nil {
			t.Fatal(err)
		}
		_, err := readWireGuardProfile(fs, path)
		if err == nil || strings.Contains(err.Error(), "secret-must-not-escape") || strings.Contains(err.Error(), path) {
			t.Fatalf("unsafe profile read error: %v", err)
		}
	}
	if _, err := readWireGuardProfile(fs, "relative.conf"); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestWireGuardEgressConfigAndPrivateProfile(t *testing.T) {
	fs := fsutil.NewMemFS()
	if err := fs.MkdirAll("/private-profiles", 0o700); err != nil {
		t.Fatal(err)
	}
	const path = "/private-profiles/synthetic.conf"
	if err := fs.WriteFile(path, []byte(wgTestProfile()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardProfile(fs, path); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{CodexWireGuardProfile: path}
	egress, err := OpenCodexEgress(cfg, fs)
	if err != nil {
		t.Fatal(err)
	}
	// Construction and shutdown must not activate or resolve the synthetic peer.
	if err := egress.Close(); err != nil {
		t.Fatal(err)
	}
	if err := egress.Close(); err != nil {
		t.Fatal(err)
	}
	for _, proxy := range []string{"direct", "http://127.0.0.1:8080", "socks5://127.0.0.1:1080"} {
		if _, err := OpenCodexEgress(&Config{CodexWireGuardProfile: path, CodexEgressProxy: proxy}, fs); err == nil {
			t.Fatal("conflicting egress accepted")
		}
	}
	if _, err := OpenCodexEgress(&Config{CodexWireGuardProfile: "relative.conf"}, fs); err == nil {
		t.Fatal("relative profile accepted")
	}
}

func TestWireGuardDNSCancellationClosesQuery(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	ctx, cancel := context.WithCancel(context.Background())
	wrapped := newWireGuardDNSConn(ctx, left)
	defer wrapped.Close()
	cancel()
	if err := wrapped.SetReadDeadline(time.Now().Add(time.Second)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	_, err := wrapped.Read(make([]byte, 1))
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("cancelled DNS read: %v", err)
	}
}

func TestWireGuardInitializationWaitHonorsCancellation(t *testing.T) {
	runtime := newWireGuardRuntime(&wireGuardProfile{peers: []wireGuardPeer{{}}}, func() wgconn.Bind { panic("unexpected activation") })
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	defer runtime.Close()
	entered := make(chan struct{})
	runtime.resolveEndpoint = func(ctx context.Context, _ string) (netip.AddrPort, error) {
		close(entered)
		<-ctx.Done()
		return netip.AddrPort{}, ctx.Err()
	}
	firstDone := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				firstDone <- errors.New("initialization panic")
			}
		}()
		_, err := runtime.start(firstCtx)
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first initializer did not start")
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	waitDone := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				waitDone <- errors.New("waiter panic")
			}
		}()
		_, err := runtime.start(waitCtx)
		waitDone <- err
	}()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled initializer wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("cancelled waiter remained blocked behind another initialization")
	}
	cancelFirst()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("first initializer cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first initializer did not stop")
	}
}

func TestWireGuardMultipleEndpointsResolveBeforeActivation(t *testing.T) {
	p := &wireGuardProfile{peers: []wireGuardPeer{{endpoint: "first.synthetic:123"}, {endpoint: "second.synthetic:123"}}}
	var starts, resolutions atomic.Int32
	runtime := newWireGuardRuntime(p, func() wgconn.Bind { starts.Add(1); panic("unexpected activation") })
	defer runtime.Close()
	runtime.resolveEndpoint = func(context.Context, string) (netip.AddrPort, error) {
		if resolutions.Add(1)%2 == 0 {
			return netip.AddrPort{}, errors.New("secret-sentinel")
		}
		return netip.MustParseAddrPort("127.0.0.1:123"), nil
	}
	for i := 0; i < 2; i++ {
		_, err := runtime.start(context.Background())
		if err == nil || strings.Contains(err.Error(), "secret-sentinel") {
			t.Fatal("unsafe endpoint failure")
		}
	}
	if starts.Load() != 0 || resolutions.Load() != 4 {
		t.Fatal("partial activation or transient endpoint failure was not retried")
	}
}

func TestWireGuardConfigurationPersistsWithoutReadingProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	profilePath := filepath.Join(root, "absent-profile.conf")
	cfg := &Config{LocalToken: "synthetic-local", ProxyResilienceStateDir: filepath.Join(root, "runtime"), CodexWireGuardProfile: profilePath}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExistingConfig()
	if err != nil || loaded.CodexWireGuardProfile != profilePath {
		t.Fatalf("normal profile setting: %v", err)
	}
	bootstrap, err := LoadProxyRescueBootstrapConfig()
	if err != nil || bootstrap.CodexWireGuardProfile != profilePath {
		t.Fatalf("rescue profile setting: %v", err)
	}
	if _, err := os.Stat(profilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("configuration inspection touched the profile")
	}
	paths, err := ResolveDefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`null`, `123`, `"relative.conf"`} {
		if err := os.WriteFile(paths.ConfigFile, []byte(`{"CODEX_WIREGUARD_PROFILE":`+value+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProxyRescueBootstrapConfig(); err == nil {
			t.Fatal("invalid manual profile setting retained stale rescue egress")
		}
	}
	conflicting := []byte(`{"codex_egress_proxy":"http://127.0.0.1:8080","codex_wireguard_profile":"/private/profile.conf"}`)
	if err := os.WriteFile(paths.ConfigFile, conflicting, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProxyRescueBootstrapConfig(); err == nil {
		t.Fatal("conflicting rescue egress accepted")
	}
}
