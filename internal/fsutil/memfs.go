package fsutil

import (
	"bytes"
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
	mu        sync.Mutex
	files     map[string]memFile
	dirs      map[string]memFile
	locks     map[uint64]uint64
	euid      uint64
	nextInode uint64
	nextLock  uint64
}

type memFile struct {
	data    []byte
	modTime time.Time
	mode    fs.FileMode
	owner   uint64
	inode   uint64
}

type memFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	mode    fs.FileMode
	owner   uint64
	inode   uint64
	links   uint64
	isDir   bool
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
func (i memFileInfo) IsDir() bool        { return i.isDir }
func (i memFileInfo) Sys() any           { return nil }

func NewMemFS() *MemFS {
	return &MemFS{
		files:     make(map[string]memFile),
		dirs:      map[string]memFile{string(filepath.Separator): {mode: os.ModeDir | 0o755, owner: 1, inode: 1}},
		locks:     make(map[uint64]uint64),
		euid:      1,
		nextInode: 2,
		nextLock:  1,
	}
}

func (m *MemFS) Stat(name string) (os.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[name]
	if ok {
		return m.fileInfo(name, f, false), nil
	}
	dir, ok := m.dirs[filepath.Clean(name)]
	if ok {
		return m.fileInfo(name, dir, true), nil
	}
	return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
}

func (m *MemFS) Lstat(name string) (os.FileInfo, error) { return m.Stat(name) }

func (m *MemFS) EffectiveUID() uint64 { return m.euid }

func (m *MemFS) FileOwnerUID(info os.FileInfo) (uint64, bool) {
	memInfo, ok := info.(memFileInfo)
	return memInfo.owner, ok
}

func (m *MemFS) FileIdentity(info os.FileInfo) (SecureFileIdentity, bool) {
	memInfo, ok := info.(memFileInfo)
	if !ok {
		return SecureFileIdentity{}, false
	}
	return SecureFileIdentity{Inode: memInfo.inode, Links: memInfo.links}, true
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
	existing, ok := m.files[name]
	if !ok {
		existing.inode = m.allocateInodeLocked()
		existing.owner = m.euid
	}
	existing.data = buf
	existing.modTime = time.Now()
	existing.mode = mode.Perm()
	m.files[name] = existing
	return nil
}

func (m *MemFS) Rename(oldpath, newpath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[oldpath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrNotExist}
	}
	if _, ok := m.dirs[filepath.Clean(newpath)]; ok {
		return &os.PathError{Op: "rename", Path: newpath, Err: os.ErrExist}
	}
	m.files[newpath] = f
	delete(m.files, oldpath)
	return nil
}

func (m *MemFS) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[name]; !ok {
		if _, dirOK := m.dirs[filepath.Clean(name)]; !dirOK {
			return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
		}
		delete(m.dirs, filepath.Clean(name))
		return nil
	}
	delete(m.files, name)
	return nil
}

func (m *MemFS) MkdirAll(path string, mode os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	clean := filepath.Clean(path)
	if _, ok := m.files[clean]; ok {
		return &os.PathError{Op: "mkdir", Path: clean, Err: os.ErrExist}
	}
	missing := make([]string, 0)
	for current := clean; ; current = filepath.Dir(current) {
		if _, ok := m.dirs[current]; ok {
			break
		}
		if _, ok := m.files[current]; ok {
			return &os.PathError{Op: "mkdir", Path: current, Err: os.ErrExist}
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	for index := len(missing) - 1; index >= 0; index-- {
		m.dirs[missing[index]] = memFile{modTime: time.Now(), mode: os.ModeDir | mode.Perm(), owner: m.euid, inode: m.allocateInodeLocked()}
	}
	return nil
}

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
	for path := range m.dirs {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		part, _, _ := strings.Cut(rest, string(filepath.Separator))
		if part != "" {
			seen[part] = true
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
	if ok {
		f.mode = mode.Perm()
		m.files[name] = f
		return nil
	}
	clean := filepath.Clean(name)
	dir, ok := m.dirs[clean]
	if !ok {
		return &os.PathError{Op: "chmod", Path: name, Err: os.ErrNotExist}
	}
	dir.mode = os.ModeDir | mode.Perm()
	m.dirs[clean] = dir
	return nil
}

func (m *MemFS) SyncFile(name string) error {
	_, err := m.Stat(name)
	return err
}

func (m *MemFS) SyncDir(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.dirs[filepath.Clean(name)]; !ok {
		return &os.PathError{Op: "sync", Path: name, Err: os.ErrNotExist}
	}
	return nil
}

func (m *MemFS) CreateExclusive(name string, mode os.FileMode) (DurableFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createExclusiveLocked(name, mode)
}

func (m *MemFS) createExclusiveLocked(name string, mode os.FileMode) (DurableFile, error) {
	if _, ok := m.files[name]; ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
	}
	if _, ok := m.dirs[filepath.Clean(name)]; ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
	}
	opened := memFile{modTime: time.Now(), mode: mode.Perm(), owner: m.euid, inode: m.allocateInodeLocked()}
	m.files[name] = opened
	return &memDurableFile{fsys: m, path: name, opened: opened}, nil
}

func (m *MemFS) OpenExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openExclusiveLockLocked(name, mode)
}

func (m *MemFS) openExclusiveLockLocked(name string, mode os.FileMode) (ExclusiveLock, error) {
	if _, ok := m.dirs[filepath.Clean(name)]; ok {
		return nil, fmt.Errorf("%w: lock type", ErrUnsafeSecurePath)
	}
	if _, ok := m.files[name]; !ok {
		m.files[name] = memFile{modTime: time.Now(), mode: mode.Perm(), owner: m.euid, inode: m.allocateInodeLocked()}
	}
	file := m.files[name]
	if m.locks[file.inode] != 0 {
		return nil, ErrExclusiveLockHeld
	}
	token := m.nextLock
	m.nextLock++
	m.locks[file.inode] = token
	return &memExclusiveLock{fsys: m, path: name, opened: file, inode: file.inode, token: token}, nil
}

func (m *MemFS) OpenNoFollow(name string) (SecureReadFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openNoFollowLocked(name)
}

func (m *MemFS) openNoFollowLocked(name string) (SecureReadFile, error) {
	file, ok := m.files[name]
	if ok {
		data := append([]byte(nil), file.data...)
		return &memSecureReadFile{Reader: bytes.NewReader(data), fsys: m, path: name, opened: file}, nil
	}
	directory, ok := m.dirs[filepath.Clean(name)]
	if ok {
		return &memSecureReadFile{Reader: bytes.NewReader(nil), fsys: m, path: filepath.Clean(name), opened: directory, isDir: true}, nil
	}
	return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
}

func (m *MemFS) OpenDurableDirectory(name string) (DurableDirectory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clean := filepath.Clean(name)
	directory, ok := m.dirs[clean]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	return &memSecureDirectory{fsys: m, inode: directory.inode}, nil
}

func (m *MemFS) OpenSecureDirectory(name string) (SecureDirectory, error) {
	directory, err := m.OpenDurableDirectory(name)
	if err != nil {
		return nil, err
	}
	opened := directory.(*memSecureDirectory)
	if err := validateSecureDirectoryDescriptor(m, opened); err != nil {
		_ = opened.Close()
		return nil, err
	}
	return opened, nil
}

func (m *MemFS) fileInfo(name string, file memFile, isDir bool) os.FileInfo {
	return memFileInfo{name: filepath.Base(name), size: int64(len(file.data)), modTime: file.modTime, mode: file.mode, owner: file.owner, inode: file.inode, links: m.linkCountLocked(file.inode, isDir), isDir: isDir}
}

func (m *MemFS) linkCountLocked(inode uint64, isDir bool) uint64 {
	var links uint64
	entries := m.files
	if isDir {
		entries = m.dirs
	}
	for _, file := range entries {
		if file.inode == inode {
			links++
		}
	}
	return links
}

func (m *MemFS) allocateInodeLocked() uint64 {
	inode := m.nextInode
	m.nextInode++
	return inode
}

type memDurableFile struct {
	fsys   *MemFS
	path   string
	opened memFile
	closed bool
}

func (file *memDurableFile) Stat() (os.FileInfo, error) {
	file.fsys.mu.Lock()
	defer file.fsys.mu.Unlock()
	if file.closed {
		return nil, os.ErrClosed
	}
	return file.fsys.fileInfo(file.path, file.opened, false), nil
}

func (file *memDurableFile) Write(data []byte) (int, error) {
	file.fsys.mu.Lock()
	defer file.fsys.mu.Unlock()
	if file.closed {
		return 0, os.ErrClosed
	}
	file.opened.data = append(file.opened.data, data...)
	file.opened.modTime = time.Now()
	for path, stored := range file.fsys.files {
		if stored.inode == file.opened.inode {
			file.path = path
			file.fsys.files[path] = file.opened
			break
		}
	}
	return len(data), nil
}

func (file *memDurableFile) Sync() error {
	file.fsys.mu.Lock()
	defer file.fsys.mu.Unlock()
	if file.closed {
		return os.ErrClosed
	}
	return nil
}

func (file *memDurableFile) Close() error {
	file.fsys.mu.Lock()
	defer file.fsys.mu.Unlock()
	file.closed = true
	return nil
}

type memExclusiveLock struct {
	fsys   *MemFS
	path   string
	opened memFile
	inode  uint64
	token  uint64
	closed bool
}

func (lock *memExclusiveLock) Stat() (os.FileInfo, error) {
	lock.fsys.mu.Lock()
	defer lock.fsys.mu.Unlock()
	if lock.closed {
		return nil, os.ErrClosed
	}
	return lock.fsys.fileInfo(lock.path, lock.opened, false), nil
}

func (lock *memExclusiveLock) Close() error {
	lock.fsys.mu.Lock()
	defer lock.fsys.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	if lock.fsys.locks[lock.inode] == lock.token {
		delete(lock.fsys.locks, lock.inode)
	}
	return nil
}

type memSecureReadFile struct {
	*bytes.Reader
	fsys   *MemFS
	path   string
	opened memFile
	isDir  bool
	closed bool
}

func (file *memSecureReadFile) Stat() (os.FileInfo, error) {
	file.fsys.mu.Lock()
	defer file.fsys.mu.Unlock()
	if file.closed {
		return nil, os.ErrClosed
	}
	return file.fsys.fileInfo(file.path, file.opened, file.isDir), nil
}

func (file *memSecureReadFile) Close() error {
	file.fsys.mu.Lock()
	defer file.fsys.mu.Unlock()
	file.closed = true
	return nil
}

type memSecureDirectory struct {
	fsys   *MemFS
	inode  uint64
	closed bool
}

func (directory *memSecureDirectory) Stat() (os.FileInfo, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	path, file, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	return directory.fsys.fileInfo(path, file, true), nil
}

func (directory *memSecureDirectory) ReadDir() ([]os.DirEntry, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	path, _, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	prefix := path + string(filepath.Separator)
	entries := make([]os.DirEntry, 0)
	for childPath, file := range directory.fsys.files {
		name := strings.TrimPrefix(childPath, prefix)
		if childPath != path && name != childPath && name != "" && !strings.ContainsRune(name, filepath.Separator) {
			entries = append(entries, fs.FileInfoToDirEntry(directory.fsys.fileInfo(childPath, file, false)))
		}
	}
	for childPath, file := range directory.fsys.dirs {
		name := strings.TrimPrefix(childPath, prefix)
		if childPath != path && name != childPath && name != "" && !strings.ContainsRune(name, filepath.Separator) {
			entries = append(entries, fs.FileInfoToDirEntry(directory.fsys.fileInfo(childPath, file, true)))
		}
	}
	sort.Slice(entries, func(first, second int) bool { return entries[first].Name() < entries[second].Name() })
	return entries, nil
}

func (directory *memSecureDirectory) OpenDirectory(name string) (DurableDirectory, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	childPath := filepath.Join(path, name)
	child, ok := directory.fsys.dirs[childPath]
	if !ok {
		if _, fileExists := directory.fsys.files[childPath]; fileExists {
			return nil, &os.PathError{Op: "open", Path: childPath, Err: os.ErrExist}
		}
		return nil, &os.PathError{Op: "open", Path: childPath, Err: os.ErrNotExist}
	}
	return &memSecureDirectory{fsys: directory.fsys, inode: child.inode}, nil
}

func (directory *memSecureDirectory) Mkdir(name string, mode os.FileMode) error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return err
	}
	childPath := filepath.Join(path, name)
	if _, ok := directory.fsys.files[childPath]; ok {
		return &os.PathError{Op: "mkdir", Path: childPath, Err: os.ErrExist}
	}
	if _, ok := directory.fsys.dirs[childPath]; ok {
		return &os.PathError{Op: "mkdir", Path: childPath, Err: os.ErrExist}
	}
	directory.fsys.dirs[childPath] = memFile{
		modTime: time.Now(),
		mode:    os.ModeDir | mode.Perm(),
		owner:   directory.fsys.euid,
		inode:   directory.fsys.allocateInodeLocked(),
	}
	return nil
}

func (directory *memSecureDirectory) OpenNoFollow(name string) (SecureReadFile, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	return directory.fsys.openNoFollowLocked(filepath.Join(path, name))
}

func (directory *memSecureDirectory) CreateExclusive(name string, mode os.FileMode) (DurableFile, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	return directory.fsys.createExclusiveLocked(filepath.Join(path, name), mode)
}

func (directory *memSecureDirectory) OpenExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	return directory.fsys.openExclusiveLockLocked(filepath.Join(path, name), mode)
}

func (directory *memSecureDirectory) OpenNewExclusiveLock(name string, mode os.FileMode) (ExclusiveLock, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(path, name)
	if _, exists := directory.fsys.files[lockPath]; exists {
		return nil, &os.PathError{Op: "open", Path: lockPath, Err: os.ErrExist}
	}
	if _, exists := directory.fsys.dirs[filepath.Clean(lockPath)]; exists {
		return nil, &os.PathError{Op: "open", Path: lockPath, Err: os.ErrExist}
	}
	file := memFile{modTime: time.Now(), mode: mode.Perm(), owner: directory.fsys.euid, inode: directory.fsys.allocateInodeLocked()}
	directory.fsys.files[lockPath] = file
	token := directory.fsys.nextLock
	directory.fsys.nextLock++
	directory.fsys.locks[file.inode] = token
	return &memExclusiveLock{fsys: directory.fsys, path: lockPath, opened: file, inode: file.inode, token: token}, nil
}

func (directory *memSecureDirectory) ProbeExclusiveLockHeld(name string, mode os.FileMode) (os.FileInfo, error) {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(path, name)
	file, ok := directory.fsys.files[lockPath]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: lockPath, Err: os.ErrNotExist}
	}
	info := directory.fsys.fileInfo(lockPath, file, false)
	if file.mode.Perm() != mode.Perm() || directory.fsys.locks[file.inode] == 0 {
		return info, ErrExclusiveLockNotHeld
	}
	return info, nil
}

func (directory *memSecureDirectory) Rename(oldName, newName string) error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(oldName); err != nil {
		return err
	}
	if err := validateSecureEntryName(newName); err != nil {
		return err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return err
	}
	oldPath := filepath.Join(path, oldName)
	file, ok := directory.fsys.files[oldPath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldPath, Err: os.ErrNotExist}
	}
	newPath := filepath.Join(path, newName)
	if _, ok := directory.fsys.dirs[filepath.Clean(newPath)]; ok {
		return &os.PathError{Op: "rename", Path: newPath, Err: os.ErrExist}
	}
	directory.fsys.files[newPath] = file
	delete(directory.fsys.files, oldPath)
	return nil
}

func (directory *memSecureDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.renameChecked(oldName, newName, expected, false)
}

func (directory *memSecureDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.renameChecked(oldName, newName, expected, true)
}

func (directory *memSecureDirectory) renameChecked(oldName, newName string, expected SecureFileIdentity, noReplace bool) error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(oldName); err != nil {
		return err
	}
	if err := validateSecureEntryName(newName); err != nil {
		return err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return err
	}
	oldPath := filepath.Join(path, oldName)
	file, ok := directory.fsys.files[oldPath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldPath, Err: os.ErrNotExist}
	}
	identity := SecureFileIdentity{Inode: file.inode, Links: 1}
	if !SameSecureObject(identity, expected) {
		return fmt.Errorf("%w: checked rename source identity", ErrUnsafeSecurePath)
	}
	newPath := filepath.Join(path, newName)
	if _, exists := directory.fsys.dirs[filepath.Clean(newPath)]; exists {
		return &os.PathError{Op: "rename", Path: newPath, Err: os.ErrExist}
	}
	if noReplace {
		if _, exists := directory.fsys.files[newPath]; exists {
			return &os.PathError{Op: "rename", Path: newPath, Err: os.ErrExist}
		}
	}
	directory.fsys.files[newPath] = file
	delete(directory.fsys.files, oldPath)
	return nil
}

func (directory *memSecureDirectory) RenameNoReplace(oldName, newName string) error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(oldName); err != nil {
		return err
	}
	if err := validateSecureEntryName(newName); err != nil {
		return err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return err
	}
	oldPath := filepath.Join(path, oldName)
	file, ok := directory.fsys.files[oldPath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldPath, Err: os.ErrNotExist}
	}
	newPath := filepath.Join(path, newName)
	if _, exists := directory.fsys.files[newPath]; exists {
		return &os.PathError{Op: "rename", Path: newPath, Err: os.ErrExist}
	}
	if _, exists := directory.fsys.dirs[filepath.Clean(newPath)]; exists {
		return &os.PathError{Op: "rename", Path: newPath, Err: os.ErrExist}
	}
	directory.fsys.files[newPath] = file
	delete(directory.fsys.files, oldPath)
	return nil
}

func (directory *memSecureDirectory) Remove(name string) error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return err
	}
	filePath := filepath.Join(path, name)
	if _, ok := directory.fsys.files[filePath]; !ok {
		return &os.PathError{Op: "remove", Path: filePath, Err: os.ErrNotExist}
	}
	delete(directory.fsys.files, filePath)
	return nil
}

func (directory *memSecureDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	path, _, err := directory.resolveLocked()
	if err != nil {
		return err
	}
	filePath := filepath.Join(path, name)
	file, ok := directory.fsys.files[filePath]
	if !ok {
		return &os.PathError{Op: "remove", Path: filePath, Err: os.ErrNotExist}
	}
	identity := SecureFileIdentity{Inode: file.inode, Links: 1}
	if !SameSecureObject(identity, expected) {
		return fmt.Errorf("%w: checked remove source identity", ErrUnsafeSecurePath)
	}
	delete(directory.fsys.files, filePath)
	return nil
}

func (directory *memSecureDirectory) Sync() error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	_, _, err := directory.resolveLocked()
	return err
}

func (directory *memSecureDirectory) Close() error {
	directory.fsys.mu.Lock()
	defer directory.fsys.mu.Unlock()
	directory.closed = true
	return nil
}

func (directory *memSecureDirectory) resolveLocked() (string, memFile, error) {
	if directory.closed {
		return "", memFile{}, os.ErrClosed
	}
	for path, file := range directory.fsys.dirs {
		if file.inode == directory.inode {
			return path, file, nil
		}
	}
	return "", memFile{}, &os.PathError{Op: "open", Err: os.ErrNotExist}
}
