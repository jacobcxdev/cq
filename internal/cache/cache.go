package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// isNotExist reports whether an error indicates a file does not exist.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// Cache provides file-based caching for provider results.
type Cache struct {
	fs      FileSystem
	dir     string
	ttl     time.Duration
	nowFunc func() time.Time
}

// New creates a cache with an injected FileSystem, directory, and TTL.
func New(fs FileSystem, dir string, ttl time.Duration) (*Cache, error) {
	if err := fs.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &Cache{fs: fs, dir: dir, ttl: ttl, nowFunc: time.Now}, nil
}

// Get returns cached results if present and not expired.
func (c *Cache) Get(_ context.Context, id string) ([]quota.Result, bool, error) {
	if id == "" {
		return nil, false, fmt.Errorf("empty cache ID")
	}
	base := filepath.Base(id)
	if base == "." || base == ".." || base == "/" {
		return nil, false, fmt.Errorf("invalid cache ID: %q", id)
	}
	path := filepath.Join(c.dir, base+".json")
	info, err := c.fs.Stat(path)
	if err != nil {
		return nil, false, nil
	}
	if c.nowFunc().Sub(info.ModTime()) > c.ttl {
		return nil, false, nil
	}
	data, err := c.fs.ReadFile(path)
	if err != nil {
		return nil, false, nil
	}
	var results []quota.Result
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, false, nil
	}
	return results, true, nil
}

// Age returns how old the cached entry is, or false if not present.
func (c *Cache) Age(_ context.Context, id string) (time.Duration, bool) {
	if id == "" {
		return 0, false
	}
	base := filepath.Base(id)
	if base == "." || base == ".." || base == "/" {
		return 0, false
	}
	path := filepath.Join(c.dir, base+".json")
	info, err := c.fs.Stat(path)
	if err != nil {
		return 0, false
	}
	return c.nowFunc().Sub(info.ModTime()), true
}

// Delete removes a cached entry. Returns nil if the entry does not exist.
func (c *Cache) Delete(_ context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("empty cache ID")
	}
	base := filepath.Base(id)
	if base == "." || base == ".." || base == "/" {
		return fmt.Errorf("invalid cache ID: %q", id)
	}
	path := filepath.Join(c.dir, base+".json")
	if err := c.fs.Remove(path); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

// Put writes results to cache atomically.
func (c *Cache) Put(_ context.Context, id string, results []quota.Result) error {
	if id == "" {
		return fmt.Errorf("empty cache ID")
	}
	base := filepath.Base(id)
	if base == "." || base == ".." || base == "/" {
		return fmt.Errorf("invalid cache ID: %q", id)
	}
	path := filepath.Join(c.dir, base+".json")
	data, err := json.Marshal(results)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := c.fs.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := c.fs.Rename(tmp, path); err != nil {
		c.fs.Remove(tmp)
		return err
	}
	return nil
}
