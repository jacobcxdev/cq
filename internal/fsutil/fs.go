package fsutil

import "os"

// FileSystem abstracts OS file operations for testability.
type FileSystem interface {
	Stat(name string) (os.FileInfo, error)
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldpath, newpath string) error
	Remove(name string) error
	MkdirAll(path string, perm os.FileMode) error
	UserHomeDir() (string, error)
	ReadDir(name string) ([]os.DirEntry, error)
}

// OSFileSystem delegates to the real OS.
type OSFileSystem struct{}

func (OSFileSystem) Stat(name string) (os.FileInfo, error)                 { return os.Stat(name) }
func (OSFileSystem) ReadFile(name string) ([]byte, error)                  { return os.ReadFile(name) }
func (OSFileSystem) WriteFile(n string, d []byte, p os.FileMode) error     { return os.WriteFile(n, d, p) }
func (OSFileSystem) Rename(o, n string) error                              { return os.Rename(o, n) }
func (OSFileSystem) Remove(name string) error                              { return os.Remove(name) }
func (OSFileSystem) MkdirAll(p string, perm os.FileMode) error             { return os.MkdirAll(p, perm) }
func (OSFileSystem) UserHomeDir() (string, error)                          { return os.UserHomeDir() }
func (OSFileSystem) ReadDir(name string) ([]os.DirEntry, error)            { return os.ReadDir(name) }
