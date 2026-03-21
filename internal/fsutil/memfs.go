package fsutil

import (
	"fmt"
	"io/fs"
	"os"
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
}

type memFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (i memFileInfo) Name() string      { return i.name }
func (i memFileInfo) Size() int64       { return i.size }
func (i memFileInfo) Mode() fs.FileMode { return 0o644 }
func (i memFileInfo) ModTime() time.Time { return i.modTime }
func (i memFileInfo) IsDir() bool       { return false }
func (i memFileInfo) Sys() any          { return nil }

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
	return memFileInfo{name: name, size: int64(len(f.data)), modTime: f.modTime}, nil
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

func (m *MemFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, len(data))
	copy(buf, data)
	m.files[name] = memFile{data: buf, modTime: time.Now()}
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
	return nil, fmt.Errorf("memfs: ReadDir not implemented")
}
