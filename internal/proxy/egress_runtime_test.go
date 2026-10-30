package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type runtimeEgressFakeManager struct{ reloads, blocks, closes, dials atomic.Int32 }

func TestCodexEgressReloadIgnoresUnrelatedConfigErrors(t *testing.T) {
	fs := fsutil.NewMemFS()
	const path = "/private/proxy.json"
	for _, document := range []string{
		`{"port":"invalid-normal-setting","codex_egress_proxy":"direct"}`,
		`{"codex_routing_http":"invalid-normal-setting","codex_wireguard_profile":"/private/vpn/us.conf"}`,
		`{"codex_egress_proxy":"http://127.0.0.1:8080","codex_upstream":"https://custom.example/codex"}`,
	} {
		if err := fs.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadCodexEgressConfig(fs, path)
		if err != nil || cfg == nil {
			t.Fatalf("egress-specific reload rejected unrelated settings: %v", err)
		}
		if cfg.CodexUpstream == "" {
			t.Fatal("missing default Codex origin")
		}
	}
}

func TestCodexEgressReloadRejectsInvalidPolicy(t *testing.T) {
	fs := fsutil.NewMemFS()
	const path = "/private/proxy.json"
	for _, document := range []string{
		`{`,
		`null`,
		`{"codex_egress_proxy":null}`,
		`{"codex_egress_proxy":5}`,
		`{"codex_egress_proxy":"http://secret:secret@proxy.example"}`,
		`{"codex_wireguard_profile":null}`,
		`{"codex_wireguard_profile":"relative.conf"}`,
		`{"codex_egress_proxy":"direct","codex_wireguard_profile":"/private/us.conf"}`,
		`{"codex_egress_interface":"en0"}`,
		`{"codex_upstream":null}`,
	} {
		if err := fs.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadCodexEgressConfig(fs, path); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid policy was accepted or its values leaked")
		}
	}
	if err := fs.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCodexEgressConfig(fs, path); err == nil {
		t.Fatal("missing config accepted")
	}
}

func (m *runtimeEgressFakeManager) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	m.dials.Add(1)
	left, right := net.Pipe()
	go func() { defer func() { _ = recover() }(); defer right.Close(); _, _ = right.Write([]byte("synthetic")) }()
	return left, nil
}
func (*runtimeEgressFakeManager) AllowsTarget(string) bool { return true }
func (m *runtimeEgressFakeManager) Reload(*Config) error   { m.reloads.Add(1); return nil }
func (m *runtimeEgressFakeManager) Block()                 { m.blocks.Add(1) }
func (m *runtimeEgressFakeManager) Close() error           { m.closes.Add(1); return nil }
func runtimeEgressTestDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cqeg-")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(resolved) })
	return resolved
}

func TestRuntimeEgressOwnerWorkerAndCleanup(t *testing.T) {
	dir := runtimeEgressTestDirectory(t)
	fs := fsutil.OSFileSystem{}
	manager := &runtimeEgressFakeManager{}
	var broken atomic.Bool
	deps := runtimeEgressDependencies{newManager: func(*Config, fsutil.FileSystem) (runtimeEgressManager, error) { return manager, nil }, load: func() (*Config, error) {
		if broken.Load() {
			return nil, errors.New("synthetic-invalid")
		}
		return &Config{Port: 4567}, nil
	}, interval: 10 * time.Millisecond}
	owner, err := startRuntimeEgress(context.Background(), &Config{Port: 4567}, fs, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	path := runtimeEgressDescriptorPath(dir, 4567)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("descriptor is not private")
	}
	worker, err := openRuntimeEgressClient(4567, fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	transport := worker.HTTP.(*http.Transport)
	c, err := transport.DialContext(context.Background(), "tcp", "origin.synthetic.test:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 9)
	if _, err := c.Read(buf); err != nil || string(buf) != "synthetic" {
		t.Fatal("broker did not use shared manager")
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.closes.Load() != 0 {
		t.Fatal("worker closed supervisor manager")
	}
	broken.Store(true)
	deadline := time.Now().Add(time.Second)
	for manager.blocks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if manager.blocks.Load() == 0 {
		t.Fatal("invalid config did not block manager")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.closes.Load() != 1 {
		t.Fatal("owner manager was not closed exactly once")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned descriptor was not removed")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "origin.synthetic.test:443"); err == nil {
		t.Fatal("worker fell back after owner stopped")
	}
}

func TestRuntimeEgressDescriptorValidation(t *testing.T) {
	dir := runtimeEgressTestDirectory(t)
	fs := fsutil.OSFileSystem{}
	path := runtimeEgressDescriptorPath(dir, 4567)
	for _, data := range []string{`{}`, `{"socket":"relative","token":"synthetic-secret"}`, `{"socket":"/tmp/elsewhere","token":"synthetic-secret"}`, strings.Repeat("x", 8193)} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := openRuntimeEgressClient(4567, fs, dir)
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("unsafe descriptor accepted or disclosed")
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openRuntimeEgressClient(4567, fs, dir); err == nil {
		t.Fatal("public descriptor accepted")
	}
}

func TestRuntimeEgressWrongAuthorityAndSuccessorDescriptor(t *testing.T) {
	dir := runtimeEgressTestDirectory(t)
	fs := fsutil.OSFileSystem{}
	manager := &runtimeEgressFakeManager{}
	deps := runtimeEgressDependencies{newManager: func(*Config, fsutil.FileSystem) (runtimeEgressManager, error) { return manager, nil }, load: func() (*Config, error) { return &Config{Port: 4567}, nil }, interval: time.Hour}
	owner, err := startRuntimeEgress(context.Background(), &Config{Port: 4567}, fs, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	path := runtimeEgressDescriptorPath(dir, 4567)
	data, err := fsutil.ReadSecureFile(fs, path, 8192)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor runtimeEgressDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatal(err)
	}
	descriptor.Token = strings.Repeat("A", 43)
	replacement, _ := json.Marshal(descriptor)
	if err := fsutil.SecureAtomicWrite(fs, path, replacement); err != nil {
		t.Fatal(err)
	}
	worker, err := openRuntimeEgressClient(4567, fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	transport := worker.HTTP.(*http.Transport)
	c, err := transport.DialContext(context.Background(), "tcp", "origin.synthetic.test:443")
	if c != nil {
		c.Close()
	}
	if err == nil || manager.dials.Load() != 0 {
		t.Fatal("wrong broker authority reached manager")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("shutdown removed a successor descriptor")
	}
}

func TestRuntimeEgressForegroundReloadAndContextShutdown(t *testing.T) {
	manager := &runtimeEgressFakeManager{}
	deps := runtimeEgressDependencies{newManager: func(*Config, fsutil.FileSystem) (runtimeEgressManager, error) { return manager, nil }, load: func() (*Config, error) { return &Config{}, nil }, interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	egress, err := openReloadingCodexEgress(ctx, &Config{}, fsutil.OSFileSystem{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer egress.Close()
	deadline := time.Now().Add(time.Second)
	for manager.reloads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if manager.reloads.Load() == 0 {
		t.Fatal("foreground owner did not reload")
	}
	cancel()
	deadline = time.Now().Add(time.Second)
	for manager.closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if manager.closes.Load() != 1 {
		t.Fatal("context shutdown did not close foreground owner")
	}
}

type runtimeEgressHoldingManager struct {
	runtimeEgressFakeManager
	done chan struct{}
}

func (m *runtimeEgressHoldingManager) DialContext(context.Context, string, string) (net.Conn, error) {
	left, right := net.Pipe()
	go func() {
		defer func() { _ = recover() }()
		defer close(m.done)
		defer right.Close()
		_, _ = right.Write([]byte("ready"))
		_, _ = io.Copy(io.Discard, right)
	}()
	return left, nil
}
func TestRuntimeEgressCloseJoinsHijackedConnections(t *testing.T) {
	dir := runtimeEgressTestDirectory(t)
	fs := fsutil.OSFileSystem{}
	manager := &runtimeEgressHoldingManager{done: make(chan struct{})}
	deps := runtimeEgressDependencies{newManager: func(*Config, fsutil.FileSystem) (runtimeEgressManager, error) { return manager, nil }, load: func() (*Config, error) { return &Config{}, nil }, interval: time.Hour}
	owner, err := startRuntimeEgress(context.Background(), &Config{Port: 4567}, fs, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	worker, err := openRuntimeEgressClient(4567, fs, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	c, err := worker.HTTP.(*http.Transport).DialContext(context.Background(), "tcp", "origin.synthetic.test:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				closed <- errors.New("close panic")
			}
		}()
		closed <- owner.Close()
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner close left a hijacked broker handler running")
	}
	select {
	case <-manager.done:
	case <-time.After(time.Second):
		t.Fatal("owner close left upstream running")
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("hijacked connection survived owner shutdown")
	}
}
