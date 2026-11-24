//go:build windows

package proxy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestWindowsRuntimeEgressPrivatePipe(t *testing.T) {
	anchor := t.TempDir()
	protectProxyConfigTestRoot(t, anchor)
	dir := filepath.Join(anchor, "runtime")
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cfg := &Config{Port: 4567}
	manager := &runtimeEgressFakeManager{}
	owner, err := startRuntimeEgress(ctx, cfg, fsys, dir, runtimeEgressDependencies{
		newManager: func(*Config, fsutil.FileSystem) (runtimeEgressManager, error) { return manager, nil },
		load:       func() (*Config, error) { return cfg, nil },
		interval:   time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	descriptor, err := readRuntimeEgressDescriptor(cfg.Port, fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		strings.Replace(descriptor.Socket, ".", "remote", 1),
		runtimeEgressPipePrefix + strings.Repeat("z", 32),
		descriptor.Socket + "\x00",
	} {
		if validRuntimeEgressEndpoint(fsys, dir, endpoint) {
			t.Errorf("unexpected endpoint accepted: %q", endpoint)
		}
	}
	if conn, err := NewEgressBrokerClient(descriptor.Socket, strings.Repeat("y", 43)).
		DialContext(ctx, "tcp", "origin.synthetic.test:443"); err == nil {
		conn.Close()
		t.Fatal("worker without broker authority connected")
	}
	worker, err := openRuntimeEgressClient(cfg.Port, fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	conn, err := worker.DialContext(ctx, "tcp", "origin.synthetic.test:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len("synthetic"))
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "synthetic" || manager.dials.Load() != 1 {
		t.Fatalf("broker did not use the owner's upstream, read error %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runtimeEgressDescriptorPath(dir, cfg.Port)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owner did not remove its descriptor: %v", err)
	}
}
