package fsutil

import (
	"io"
	"os"
)

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

// DurableFile is an exclusively created file that can be synced before close.
type DurableFile interface {
	io.Writer
	Sync() error
	Close() error
}

// DurableFileInspector exposes descriptor metadata for secure writers that
// must prove the temporary pathname still names the exclusively created file.
type DurableFileInspector interface {
	Stat() (os.FileInfo, error)
}

// DurableDirectory retains one directory identity while callers create and
// seal child entries relative to it. Unlike SecureDirectory, an opened parent
// may have ordinary system permissions; every newly created private child is
// validated separately before it becomes an authority boundary.
type DurableDirectory interface {
	Stat() (os.FileInfo, error)
	OpenDirectory(name string) (DurableDirectory, error)
	Mkdir(name string, perm os.FileMode) error
	Sync() error
	Close() error
}

// DurableDirectoryOpener opens the final directory component without
// following it and retains that directory identity for relative operations.
type DurableDirectoryOpener interface {
	OpenDurableDirectory(name string) (DurableDirectory, error)
}

// SecureReadFile is a no-follow file handle whose descriptor metadata can be
// validated before content is consumed.
type SecureReadFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type SecureFileIdentity struct {
	Device uint64
	Inode  uint64
	Links  uint64
	FileID [16]byte `json:"-"`
}

type SecurePrincipalKind uint8

const (
	SecurePrincipalUID SecurePrincipalKind = iota + 1
	SecurePrincipalSID
)

type SecurePrincipal struct {
	Kind      SecurePrincipalKind
	UID       uint64
	SIDLength uint8
	SID       [68]byte
}

// ExclusiveLock is held for the lifetime of its underlying file descriptor.
type ExclusiveLock interface {
	Stat() (os.FileInfo, error)
	Close() error
}

// ExclusiveFileCreator supplies handle-level exclusive creation without
// widening FileSystem for callers that never write durable private state.
type ExclusiveFileCreator interface {
	CreateExclusive(name string, perm os.FileMode) (DurableFile, error)
}

// SecurePathInspector supplies no-follow metadata and owner authority.
type SecurePathInspector interface {
	Lstat(name string) (os.FileInfo, error)
	EffectiveUID() uint64
	FileOwnerUID(info os.FileInfo) (uint64, bool)
	FileIdentity(info os.FileInfo) (SecureFileIdentity, bool)
}

type SecurePrincipalInspector interface {
	EffectivePrincipal() (SecurePrincipal, bool)
	FileOwnerPrincipal(os.FileInfo) (SecurePrincipal, bool)
}

type SecureAncestorInspector interface {
	ValidateRetainedAncestor(os.FileInfo) error
}

type SecureExternalPathInspector interface {
	ValidateExternalCredentialDirectoryInfo(os.FileInfo) error
	ValidateExternalCredential(os.FileInfo) error
	ValidateExternalCache(os.FileInfo) error
	ValidateRetainedExternalImportFileInfo(os.FileInfo) error
}

type RetainedReadDirectory interface {
	Stat() (os.FileInfo, error)
	OpenDirectory(name string) (RetainedReadDirectory, error)
	OpenNoFollow(name string) (SecureReadFile, error)
	Close() error
}

type RetainedReadDirectoryOpener interface {
	OpenRetainedReadDirectory(name string) (RetainedReadDirectory, error)
}

type IdentityBoundRenamer interface {
	RenameChecked(oldName, newName string, expected SecureFileIdentity) error
	RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error
}

type IdentityBoundRemover interface {
	RemoveChecked(name string, expected SecureFileIdentity) error
}

// NoFollowFileOpener opens a final path component without following links.
type NoFollowFileOpener interface {
	OpenNoFollow(name string) (SecureReadFile, error)
}

// ExclusiveLocker supplies a non-blocking lifetime lock.
type ExclusiveLocker interface {
	OpenExclusiveLock(name string, perm os.FileMode) (ExclusiveLock, error)
}

// NewExclusiveLocker creates and locks a new file. Existing entries are never
// opened, even when they are valid unlocked lock files.
type NewExclusiveLocker interface {
	OpenNewExclusiveLock(name string, perm os.FileMode) (ExclusiveLock, error)
}

// ExclusiveLockHeldProber checks an existing lock without creating it or
// retaining ownership if it is currently unlocked.
type ExclusiveLockHeldProber interface {
	ProbeExclusiveLockHeld(name string, perm os.FileMode) (os.FileInfo, error)
}

// SecureDirectory binds a validated directory for one atomic transaction so
// path replacement cannot redirect create, rename, or sync operations.
type SecureDirectory interface {
	Stat() (os.FileInfo, error)
	OpenNoFollow(name string) (SecureReadFile, error)
	CreateExclusive(name string, perm os.FileMode) (DurableFile, error)
	OpenExclusiveLock(name string, perm os.FileMode) (ExclusiveLock, error)
	Rename(oldName, newName string) error
	RenameNoReplace(oldName, newName string) error
	Remove(name string) error
	Sync() error
	Close() error
}

// SecureDirectoryReader enumerates the immediate entries of a retained
// directory capability without resolving its pathname again.
type SecureDirectoryReader interface {
	ReadDir() ([]os.DirEntry, error)
}

// SecureDirectoryOpener opens a directory without following its final path
// component and retains the handle for the complete atomic transaction.
type SecureDirectoryOpener interface {
	OpenSecureDirectory(name string) (SecureDirectory, error)
}

type secureBoundaryPurpose uint8

const (
	secureBoundaryCQPrivate secureBoundaryPurpose = iota + 1
	secureBoundaryExternalDirectory
	secureBoundaryExternalFile
)

type secureBoundarySelection struct {
	AnchorPath        string
	PostAnchorPrivate bool
}

type secureBoundaryResolver interface {
	ResolveSecureBoundary(string, secureBoundaryPurpose) (secureBoundarySelection, error)
}

// OSFileSystem delegates to the real OS. The resolver is an unexported,
// value-scoped Windows test seam; the production zero value resolves roots.
type OSFileSystem struct {
	secureBoundaryResolver secureBoundaryResolver
}

func (fsys OSFileSystem) Stat(name string) (os.FileInfo, error)        { return statOSFileSystem(fsys, name) }
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
