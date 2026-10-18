//go:build darwin || linux

package dots

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSpawnHelperRejectedSymlinkLog(t *testing.T) {
	s := processSettings(t)
	executable := filepath.Join(s.RuntimeRoot, "cq-dots-runtime")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.RuntimeRoot, "release-relay.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := SpawnHelper(s, executable, "relay", nil, os.Environ()); err == nil {
		t.Fatal("log symlink accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "unchanged" {
		t.Fatal("log target modified")
	}
}

func TestPlatformCanIdentifyOwnArgumentsAndDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uid, args, err := dotsProcessArguments(ctx, os.Getpid())
	if err != nil || uid != os.Getuid() || len(args) == 0 {
		t.Fatalf("own process %d %#v %v", uid, args, err)
	}
	cwd, err := dotsProcessCWD(ctx, os.Getpid())
	want, _ := os.Getwd()
	want, _ = filepath.EvalSymlinks(want)
	if err != nil || cwd != want {
		t.Fatalf("own directory %q, %v; want %q", cwd, err, want)
	}
}
