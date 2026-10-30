//go:build cq_egress_live

package proxy

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	wgconn "golang.zx2c4.com/wireguard/conn"
)

// Opt in only after explicit permission to use a real profile. The path is
// supplied via the environment; the credential and observed IPs are never logged.
// This starts a private test broker, not CQ's installed proxy or system VPN.
func TestCodexWireGuardLiveEgress(t *testing.T) {
	path := os.Getenv("CQ_LIVE_WIREGUARD_PROFILE")
	if path == "" {
		t.Skip("requires explicit live profile selection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	fs := fsutil.OSFileSystem{}
	profile, err := readWireGuardProfile(fs, path)
	if err != nil {
		t.Fatal("secure live profile load failed")
	}
	const traceURL = "https://www.cloudflare.com/cdn-cgi/trace"
	readTrace := func(transport http.RoundTripper) (netip.Addr, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, traceURL, nil)
		if err != nil {
			t.Fatal("create trace request failed")
		}
		client := &http.Client{Transport: transport, Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("trace TLS request failed")
		}
		defer response.Body.Close()
		data, err := httputil.ReadBody(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatal("trace response failed")
		}
		fields := make(map[string]string)
		for _, line := range strings.Split(string(data), "\n") {
			key, value, found := strings.Cut(line, "=")
			if found {
				fields[key] = value
			}
		}
		address, err := netip.ParseAddr(fields["ip"])
		if err != nil || len(fields["loc"]) != 2 {
			t.Fatal("trace metadata unavailable")
		}
		return address, fields["loc"]
	}
	direct, err := NewCodexEgress("direct")
	if err != nil {
		t.Fatal("create host baseline failed")
	}
	defer direct.Close()
	baseline, _ := readTrace(direct.HTTP)
	// These are the same runtime and HTTP/WS dialer constructors used by the
	// production WireGuard backend, with its runtime retained only for safe counters.
	runtime := newWireGuardRuntime(profile, wgconn.NewDefaultBind)
	defer runtime.Close()
	cfg := &Config{Port: 4567, CodexUpstream: "https://www.cloudflare.com", CodexWireGuardProfile: path}
	manager, err := newEgressManager(cfg, func(*Config) (*CodexEgress, [32]byte, error) {
		egress, err := codexEgressWithDialer(runtime.DialContext, runtime.Close)
		return egress, sha256.Sum256([]byte("live-single-generation")), err
	})
	if err != nil {
		t.Fatal("create live manager failed")
	}
	defer manager.Close()
	dir := runtimeEgressTestDirectory(t)
	owner, err := startRuntimeEgress(ctx, cfg, fs, dir, runtimeEgressDependencies{
		newManager: func(*Config, fsutil.FileSystem) (runtimeEgressManager, error) { return manager, nil },
		load:       func() (*Config, error) { return cfg, nil }, interval: time.Hour,
	})
	if err != nil {
		t.Fatal("create isolated live broker failed")
	}
	defer owner.Close()
	worker, err := openRuntimeEgressClient(cfg.Port, fs, dir)
	if err != nil {
		t.Fatal("open isolated worker egress failed")
	}
	defer worker.Close()
	tunneled, country := readTrace(worker.HTTP)
	ownerIP, ownerCountry := readTrace(owner.HTTP)
	if country != "US" || ownerCountry != country || ownerIP != tunneled {
		t.Fatal("shared owner and worker did not prove the expected US egress")
	}
	if baseline == tunneled {
		t.Fatal("tunnel egress did not differ from the host baseline")
	}
	runtime.mu.Lock()
	device := runtime.device
	runtime.mu.Unlock()
	if device == nil {
		t.Fatal("WireGuard device did not start")
	}
	state, err := device.IpcGet()
	if err != nil {
		t.Fatal("WireGuard handshake counters unavailable")
	}
	// IpcGet includes keys: parse locally and never emit its contents.
	var handshakes, received, sent uint64
	for _, line := range strings.Split(state, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "last_handshake_time_sec":
			if number > 0 {
				handshakes++
			}
		case "rx_bytes":
			received += number
		case "tx_bytes":
			sent += number
		}
	}
	state = ""
	if handshakes == 0 || received == 0 || sent == 0 {
		t.Fatal("no authenticated WireGuard handshake and bidirectional traffic proven")
	}
	if err := worker.Close(); err != nil {
		t.Fatal("close worker egress failed")
	}
	if err := owner.Close(); err != nil {
		t.Fatal("close live owner failed")
	}
	runtime.mu.Lock()
	closed := runtime.closed
	runtime.mu.Unlock()
	if !closed {
		t.Fatal("live tunnel was not shut down")
	}
	t.Logf("live CQ egress verified: country=%s, distinct_host_exit=true, shared_owner_worker_exit=true, handshakes=%d, received_bytes=%d, sent_bytes=%d, tunnel_closed=true", country, handshakes, received, sent)
}
