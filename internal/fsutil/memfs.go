package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemFS is an in-memory FileSystem for tests.
type MemFS struct {
	mu    sync.Mutex
	files map[string]memFile
}

type memFile struct {
	data    []byte
	modTime time.Time
	mode    fs.FileMode
}

type memFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	mode    fs.FileMode
}

type memDirEntry struct {
	name  string
	isDir bool
}

func (e memDirEntry) Name() string               { return e.name }
func (e memDirEntry) IsDir() bool                { return e.isDir }
func (e memDirEntry) Type() fs.FileMode          { return 0 }
func (e memDirEntry) Info() (fs.FileInfo, error) { return memFileInfo{name: e.name, mode: 0o600}, nil }

func (i memFileInfo) Name() string       { return i.name }
func (i memFileInfo) Size() int64        { return i.size }
func (i memFileInfo) Mode() fs.FileMode  { return i.mode }
func (i memFileInfo) ModTime() time.Time { return i.modTime }
func (i memFileInfo) IsDir() bool        { return false }
func (i memFileInfo) Sys() any           { return nil }

func NewMemFS() *MemFS {
	return &MemFS{files: make(map[string]memFile)}
}

func (m *MemFS) Stat(name string) (os.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[name]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}
	return memFileInfo{name: name, size: int64(len(f.data)), modTime: f.modTime, mode: f.mode}, nil
}

func (m *MemFS) ReadFile(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out, nil
}

func (m *MemFS) WriteFile(name string, data []byte, mode os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, len(data))
	copy(buf, data)
	m.files[name] = memFile{data: buf, modTime: time.Now(), mode: mode.Perm()}
	return nil
}

func (m *MemFS) Rename(oldpath, newpath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[oldpath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrNotExist}
	}
	m.files[newpath] = f
	delete(m.files, oldpath)
	return nil
}

func (m *MemFS) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[name]; !ok {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
	}
	delete(m.files, name)
	return nil
}

func (m *MemFS) MkdirAll(_ string, _ os.FileMode) error { return nil }

func (m *MemFS) UserHomeDir() (string, error) { return "/home/test", nil }

func (m *MemFS) ReadDir(name string) ([]os.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := filepath.Clean(name) + string(filepath.Separator)
	seen := make(map[string]bool)
	for path := range m.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		part, tail, _ := strings.Cut(rest, string(filepath.Separator))
		if part != "" {
			seen[part] = tail != ""
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("memfs: read directory %s: %w", name, os.ErrNotExist)
	}
	names := make([]string, 0, len(seen))
	for entry := range seen {
		names = append(names, entry)
	}
	sort.Strings(names)
	entries := make([]os.DirEntry, 0, len(names))
	for _, entry := range names {
		entries = append(entries, memDirEntry{name: entry, isDir: seen[entry]})
	}
	return entries, nil
}

func (m *MemFS) Chmod(name string, mode os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[name]
	if !ok {
		return &os.PathError{Op: "chmod", Path: name, Err: os.ErrNotExist}
	}
	f.mode = mode.Perm()
	m.files[name] = f
	return nil
}

func (m *MemFS) SyncFile(name string) error {
	_, err := m.Stat(name)
	return err
}

func (m *MemFS) SyncDir(string) error { return nil }
