package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEgressManagerReloadClosesOldGenerationAndBlocksBadReload(t *testing.T) {
	var closes atomic.Int32
	peers := make(chan net.Conn, 8)
	factory := func(cfg *Config) (*CodexEgress, [32]byte, error) {
		if cfg.CodexEgressProxy == "bad" {
			return nil, [32]byte{}, errors.New("secret profile content")
		}
		return &CodexEgress{dial: func(context.Context, string, string) (net.Conn, error) { a, b := net.Pipe(); peers <- b; return a, nil }, close: func() error { closes.Add(1); return nil }}, sha256.Sum256([]byte(cfg.CodexEgressProxy)), nil
	}
	cfg := &Config{CodexEgressProxy: "one", CodexUpstream: "https://origin.example"}
	manager, err := newEgressManager(cfg, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	connection, err := manager.DialContext(context.Background(), "tcp", "origin.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	peer := <-peers
	defer peer.Close()
	if err := manager.Reload(&Config{CodexEgressProxy: "two", CodexUpstream: cfg.CodexUpstream}); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("old generation connection survived replacement")
	}
	if closes.Load() != 1 {
		t.Fatalf("old backend closes: %d", closes.Load())
	}
	if err := manager.Reload(&Config{CodexEgressProxy: "bad"}); err == nil {
		t.Fatal("bad reload accepted")
	}
	if _, err := manager.DialContext(context.Background(), "tcp", "origin.example:443"); err == nil {
		t.Fatal("bad reload continued old egress")
	}
	if manager.AllowsTarget("origin.example:443") {
		t.Fatal("blocked manager still authorizes destination")
	}
	if err := manager.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	next, err := manager.DialContext(context.Background(), "tcp", "origin.example:443")
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	(<-peers).Close()
	if !manager.AllowsTarget("origin.example:443") || manager.AllowsTarget("unrelated.example:443") {
		t.Fatal("destination policy incorrect")
	}
	if !manager.AllowsTarget("auth.openai.com:443") || !manager.AllowsTarget("api.openai.com:443") {
		t.Fatal("official credential or live sideband origin denied")
	}
	if manager.AllowsTarget("api.openai.com:80") || manager.AllowsTarget("api.openai.com.evil.test:443") {
		t.Fatal("official origin allowlist broadened")
	}
}

type egressProfileSequenceFS struct {
	*fsutil.MemFS
	reads atomic.Int32
}
type egressProfileSequenceDirectory struct {
	fsutil.SecureDirectory
	fs *egressProfileSequenceFS
}
type egressProfileSequenceFile struct {
	fsutil.SecureReadFile
	reader *strings.Reader
	fs     *egressProfileSequenceFS
}

func (f *egressProfileSequenceFile) Read(p []byte) (int, error) {
	if f.reader == nil {
		data := wgTestProfile()
		if f.fs.reads.Add(1) > 1 {
			data = strings.Replace(data, wgTestKey(1), wgTestKey(3), 1)
		}
		f.reader = strings.NewReader(data)
	}
	return f.reader.Read(p)
}
func (fs *egressProfileSequenceFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	dir, err := fs.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &egressProfileSequenceDirectory{SecureDirectory: dir, fs: fs}, nil
}
func (d *egressProfileSequenceDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	file, err := d.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	return &egressProfileSequenceFile{SecureReadFile: file, fs: d.fs}, nil
}
func TestEgressManagerHashesAndParsesOneProfileSnapshot(t *testing.T) {
	fs := &egressProfileSequenceFS{MemFS: fsutil.NewMemFS()}
	if err := fs.MkdirAll("/profiles", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/profiles/vpn.conf", []byte(wgTestProfile()), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewCodexEgressManager(&Config{CodexWireGuardProfile: "/profiles/vpn.conf"}, fs)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if fs.reads.Load() != 1 {
		t.Fatalf("profile read %d times; fingerprint and backend may differ", fs.reads.Load())
	}
}
