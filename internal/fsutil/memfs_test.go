package fsutil

import (
	"errors"
	"os"
	"sync"
	"testing"
)

func TestMemFSWriteReadRoundTrip(t *testing.T) {
	m := NewMemFS()
	original := []byte("hello world")
	if err := m.WriteFile("/tmp/test.txt", original, 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	// Mutate original slice — stored copy must not be affected.
	original[0] = 'X'

	got, err := m.ReadFile("/tmp/test.txt")
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("ReadFile = %q, want %q", got, "hello world")
	}
	// Also verify the returned slice is independent (mutating it doesn't affect a second read).
	got[0] = 'Y'
	got2, _ := m.ReadFile("/tmp/test.txt")
	if string(got2) != "hello world" {
		t.Errorf("second ReadFile = %q, want %q after mutation of returned slice", got2, "hello world")
	}
}

func TestMemFSReadFileMissing(t *testing.T) {
	m := NewMemFS()
	_, err := m.ReadFile("/nonexistent/file")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadFile missing: got %v, want os.ErrNotExist", err)
	}
}

func TestMemFSStatPresent(t *testing.T) {
	m := NewMemFS()
	data := []byte("statme")
	if err := m.WriteFile("/tmp/stat.txt", data, 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	info, err := m.Stat("/tmp/stat.txt")
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	if info.Size() != int64(len(data)) {
		t.Errorf("Size = %d, want %d", info.Size(), len(data))
	}
	if info.ModTime().IsZero() {
		t.Error("ModTime is zero, want non-zero")
	}
}

func TestMemFSStatMissing(t *testing.T) {
	m := NewMemFS()
	_, err := m.Stat("/nonexistent/file")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat missing: got %v, want os.ErrNotExist", err)
	}
}

func TestMemFSRename(t *testing.T) {
	m := NewMemFS()
	if err := m.WriteFile("/tmp/old.txt", []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	if err := m.Rename("/tmp/old.txt", "/tmp/new.txt"); err != nil {
		t.Fatalf("Rename error: %v", err)
	}
	// Old path must be gone.
	if _, err := m.ReadFile("/tmp/old.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old path after rename: got %v, want os.ErrNotExist", err)
	}
	// New path must have the data.
	got, err := m.ReadFile("/tmp/new.txt")
	if err != nil {
		t.Fatalf("ReadFile new path: %v", err)
	}
	if string(got) != "data" {
		t.Errorf("new path data = %q, want %q", got, "data")
	}
}

func TestMemFSRenameMissing(t *testing.T) {
	m := NewMemFS()
	if err := m.Rename("/nonexistent/src", "/tmp/dst"); err == nil {
		t.Error("Rename of non-existent file: expected error, got nil")
	}
}

func TestMemFSMkdirAll(t *testing.T) {
	m := NewMemFS()
	if err := m.MkdirAll("/some/deep/path", 0o755); err != nil {
		t.Errorf("MkdirAll returned error: %v", err)
	}
}

func TestMemFSUserHomeDir(t *testing.T) {
	m := NewMemFS()
	dir, err := m.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir error: %v", err)
	}
	if dir != "/home/test" {
		t.Errorf("UserHomeDir = %q, want %q", dir, "/home/test")
	}
}

func TestMemFSConcurrent(t *testing.T) {
	m := NewMemFS()
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := "/tmp/concurrent_" + string(rune('a'+i))
			val := []byte{byte(i)}
			if err := m.WriteFile(key, val, 0o644); err != nil {
				t.Errorf("goroutine %d WriteFile: %v", i, err)
			}
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		key := "/tmp/concurrent_" + string(rune('a'+i))
		got, err := m.ReadFile(key)
		if err != nil {
			t.Errorf("ReadFile key %q: %v", key, err)
			continue
		}
		if len(got) != 1 || got[0] != byte(i) {
			t.Errorf("key %q: got %v, want [%d]", key, got, i)
		}
	}
}
