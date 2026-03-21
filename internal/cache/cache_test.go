package cache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCacheGetMiss(t *testing.T) {
	c, _ := New(fsutil.NewMemFS(), "/cache", 30*time.Second)
	_, ok, err := c.Get(context.Background(), string(provider.Codex))
	if err != nil || ok {
		t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
	}
}

func TestCachePutAndGet(t *testing.T) {
	c, _ := New(fsutil.NewMemFS(), "/cache", 30*time.Second)
	ctx := context.Background()
	results := []quota.Result{{Status: quota.StatusOK, Plan: "test"}}
	if err := c.Put(ctx, string(provider.Codex), results); err != nil {
		t.Fatalf("Put error: %v", err)
	}
	got, ok, err := c.Get(ctx, string(provider.Codex))
	if !ok || err != nil {
		t.Fatalf("expected hit, got ok=%v err=%v", ok, err)
	}
	if got[0].Plan != "test" {
		t.Fatalf("plan = %q, want test", got[0].Plan)
	}
}

func TestCacheExpiry(t *testing.T) {
	c, _ := New(fsutil.NewMemFS(), "/cache", 1*time.Second)
	ctx := context.Background()
	c.Put(ctx, string(provider.Codex), []quota.Result{{Status: quota.StatusOK}})
	// Advance the clock past the TTL without sleeping.
	c.nowFunc = func() time.Time { return time.Now().Add(2 * time.Second) }
	_, ok, _ := c.Get(ctx, string(provider.Codex))
	if ok {
		t.Fatal("expected expired cache miss")
	}
}

func TestCacheInvalidJSON(t *testing.T) {
	fs := fsutil.NewMemFS()
	c, _ := New(fs, "/cache", 30*time.Second)
	// Write garbage directly
	fs.WriteFile("/cache/codex.json", []byte("not json"), 0o644)
	_, ok, err := c.Get(context.Background(), string(provider.Codex))
	if ok || err != nil {
		t.Fatalf("expected miss on invalid JSON, got ok=%v err=%v", ok, err)
	}
}

func TestCacheGetStatError(t *testing.T) {
	// MemFS returns a PathError for missing files; cache.Get should treat it as a miss.
	c, _ := New(fsutil.NewMemFS(), "/cache", 30*time.Second)
	_, ok, err := c.Get(context.Background(), "nonexistent")
	if ok {
		t.Fatal("expected miss when file does not exist")
	}
	if err != nil {
		t.Fatalf("expected nil error on stat miss, got %v", err)
	}
}

// errWriteFS is a FileSystem whose WriteFile always fails.
type errWriteFS struct {
	*fsutil.MemFS
}

func (e *errWriteFS) WriteFile(_ string, _ []byte, _ os.FileMode) error {
	return errors.New("write error")
}

func TestCacheEmptyID(t *testing.T) {
	c, _ := New(fsutil.NewMemFS(), "/cache", 30*time.Second)
	ctx := context.Background()

	_, _, err := c.Get(ctx, "")
	if err == nil {
		t.Fatal("expected error from Get with empty ID")
	}

	err = c.Put(ctx, "", []quota.Result{{Status: quota.StatusOK}})
	if err == nil {
		t.Fatal("expected error from Put with empty ID")
	}

	// IDs that filepath.Base reduces to "." or "/" are also invalid.
	for _, bad := range []string{".", "/", ".."} {
		_, _, err := c.Get(ctx, bad)
		if err == nil {
			t.Errorf("expected error from Get with ID %q", bad)
		}
		err = c.Put(ctx, bad, []quota.Result{{Status: quota.StatusOK}})
		if err == nil {
			t.Errorf("expected error from Put with ID %q", bad)
		}
	}
}

func TestCacheZeroTTLExpires(t *testing.T) {
	fs := fsutil.NewMemFS()
	c, err := New(fs, "/cache", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	results := []quota.Result{{Status: quota.StatusOK, Plan: "pro"}}
	if err := c.Put(ctx, "test", results); err != nil {
		t.Fatal(err)
	}
	// With zero TTL, any elapsed time causes expiry. Advance the clock by 1ns
	// so that nowFunc().Sub(modTime) == 1ns > 0 == ttl.
	c.nowFunc = func() time.Time { return time.Now().Add(time.Nanosecond) }
	_, ok, _ := c.Get(ctx, "test")
	if ok {
		t.Error("expected cache miss with zero TTL")
	}
}

func TestCachePutWriteError(t *testing.T) {
	fs := &errWriteFS{MemFS: fsutil.NewMemFS()}
	c, _ := New(fs, "/cache", 30*time.Second)
	err := c.Put(context.Background(), string(provider.Codex), []quota.Result{{Status: quota.StatusOK}})
	if err == nil {
		t.Fatal("expected error from Put when WriteFile fails")
	}
}
