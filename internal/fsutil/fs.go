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

// DurableFileSystem adds operations required by crash-safe credential commits.
// Callers that mutate credentials must fail closed when this capability is absent.
type DurableFileSystem interface {
	FileSystem
	Chmod(name string, mode os.FileMode) error
	SyncFile(name string) error
	SyncDir(name string) error
}

// OSFileSystem delegates to the real OS.
type OSFileSystem struct{}

func (OSFileSystem) Stat(name string) (os.FileInfo, error)             { return os.Stat(name) }
func (OSFileSystem) ReadFile(name string) ([]byte, error)              { return os.ReadFile(name) }
func (OSFileSystem) WriteFile(n string, d []byte, p os.FileMode) error { return os.WriteFile(n, d, p) }
func (OSFileSystem) Rename(o, n string) error                          { return os.Rename(o, n) }
func (OSFileSystem) Remove(name string) error                          { return os.Remove(name) }
func (OSFileSystem) MkdirAll(p string, perm os.FileMode) error         { return os.MkdirAll(p, perm) }
func (OSFileSystem) UserHomeDir() (string, error)                      { return os.UserHomeDir() }
func (OSFileSystem) ReadDir(name string) ([]os.DirEntry, error)        { return os.ReadDir(name) }
func (OSFileSystem) Chmod(name string, mode os.FileMode) error         { return os.Chmod(name, mode) }
func (OSFileSystem) SyncFile(name string) error {
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (OSFileSystem) SyncDir(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
