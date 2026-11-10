//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestProxyServiceStderrShutdownFlushesAndCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stop := monitorProxyServiceStderr(file, path)
	want := []byte("final service evidence\n")
	if _, err := file.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(want); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("shutdown left descriptor open: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("shutdown lost final evidence: %q, %v", got, err)
	}
}

func TestProxyServiceStderrShutdownReportsSyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stop := monitorProxyServiceStderr(file, path)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var failure *os.PathError
	if err := stop(); !errors.As(err, &failure) || failure.Op != "sync" || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("shutdown hid final sync failure: %v", err)
	}
}

func TestProxyServiceStderrRetentionKeepsInheritedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	inherited, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	if _, err := file.WriteString(strings.Repeat("old evidence\n", 200)); err != nil {
		t.Fatal(err)
	}
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := compactProxyServiceStderr(file, path, 1024, 2); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProxyServiceStderrExecHelper$")
	command.Env = append(os.Environ(), "CQ_STDERR_EXEC_HELPER=exec")
	command.Stderr = inherited
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("retention detached inherited stderr descriptor")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("concurrent")) || !bytes.Contains(data, []byte("new inherited evidence")) {
		t.Fatalf("missing retention marker or inherited write: %q", data)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(backup) > 1024 || !bytes.Contains(backup, []byte("retained tail")) || !bytes.Contains(backup, []byte("old evidence")) {
		t.Fatalf("invalid retained tail: %d bytes %q", len(backup), backup)
	}
}

func TestProxyServiceStderrExecHelper(t *testing.T) {
	switch os.Getenv("CQ_STDERR_EXEC_HELPER") {
	case "exec":
		if err := os.Setenv("CQ_STDERR_EXEC_HELPER", "write"); err != nil {
			t.Fatal(err)
		}
		if err := unix.Exec(os.Args[0], os.Args, os.Environ()); err != nil {
			t.Fatal(err)
		}
	case "write":
		if _, err := fmt.Fprintln(os.Stderr, "new inherited evidence"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Skip("subprocess helper")
	}
}

func TestProxyServiceStderrRetentionMakesInheritedWritesAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.log")
	stderr, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	managed, err := openProxyServiceStderr(path, stderr)
	if err != nil || managed == nil {
		t.Fatalf("adopt stderr: %v", err)
	}
	defer managed.Close()
	flags, err := unix.FcntlInt(stderr.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_APPEND == 0 {
		t.Fatal("inherited writes may leave sparse holes after truncation")
	}
}

func TestProxyServiceStderrRetentionDoesNotTruncateAfterBackupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	want := []byte(strings.Repeat("evidence", 200))
	if _, err := file.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := compactProxyServiceStderr(file, path, 1024, 2); err == nil {
		t.Fatal("backup failure was ignored")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("backup failure discarded current evidence")
	}
}

func TestProxyServiceStderrRetentionIgnoresForegroundAndReplacedPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.log")
	foreground, err := os.CreateTemp(dir, "foreground")
	if err != nil {
		t.Fatal(err)
	}
	defer foreground.Close()
	if err := os.WriteFile(path, []byte("service evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	managed, err := openProxyServiceStderr(path, foreground)
	if err != nil || managed != nil {
		t.Fatalf("foreground stderr adopted: %v %v", managed, err)
	}
	service, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compactProxyServiceStderr(service, path, 256, 1); err == nil {
		t.Fatal("replacement canonical path accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "replacement evidence" {
		t.Fatal("replacement file changed")
	}
}
