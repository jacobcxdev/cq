//go:build unix

package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func (OSFileSystem) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }

func (OSFileSystem) EffectiveUID() uint64 { return uint64(os.Geteuid()) }

func (OSFileSystem) FileOwnerUID(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Uid), true
}

func (OSFileSystem) FileIdentity(info os.FileInfo) (SecureFileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return SecureFileIdentity{}, false
	}
	return SecureFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Links: uint64(stat.Nlink)}, true
}

type unixSecureDirectory struct {
	file *os.File
}

func (OSFileSystem) OpenDurableDirectory(name string) (DurableDirectory, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: durable directory descriptor", ErrSecureCapabilityUnavailable)
	}
	return &unixSecureDirectory{file: file}, nil
}

func (fsys OSFileSystem) OpenSecureDirectory(name string) (SecureDirectory, error) {
	directory, err := fsys.OpenDurableDirectory(name)
	if err != nil {
		return nil, err
	}
	opened := directory.(*unixSecureDirectory)
	if err := validateSecureDirectoryDescriptor(fsys, opened); err != nil {
		_ = opened.Close()
		return nil, err
	}
	return opened, nil
}

func (directory *unixSecureDirectory) Stat() (os.FileInfo, error) {
	return directory.file.Stat()
}

func (directory *unixSecureDirectory) ReadDir() ([]os.DirEntry, error) {
	if _, err := directory.file.Seek(0, 0); err != nil {
		return nil, err
	}
	return directory.file.ReadDir(-1)
}

func (directory *unixSecureDirectory) OpenDirectory(name string) (DurableDirectory, error) {
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, ErrSecureCapabilityUnavailable
	}
	return &unixSecureDirectory{file: file}, nil
}

func (directory *unixSecureDirectory) Mkdir(name string, perm os.FileMode) error {
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	return unix.Mkdirat(int(directory.file.Fd()), name, uint32(perm.Perm()))
}

func (directory *unixSecureDirectory) OpenNoFollow(name string) (SecureReadFile, error) {
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, ErrSecureCapabilityUnavailable
	}
	return file, nil
}

func (directory *unixSecureDirectory) CreateExclusive(name string, perm os.FileMode) (DurableFile, error) {
	if err := validateSecureEntryName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return nil, err
	}
	if err := validateSecureFD(fd, perm); err != nil {
		unix.Close(fd)
		_ = unix.Unlinkat(int(directory.file.Fd()), name, 0)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		_ = unix.Unlinkat(int(directory.file.Fd()), name, 0)
		return nil, fmt.Errorf("%w: exclusive file descriptor", ErrSecureCapabilityUnavailable)
	}
	return file, nil
}

func (directory *unixSecureDirectory) Rename(oldName, newName string) error {
	if err := validateSecureEntryName(oldName); err != nil {
		return err
	}
	if err := validateSecureEntryName(newName); err != nil {
		return err
	}
	fd := int(directory.file.Fd())
	return unix.Renameat(fd, oldName, fd, newName)
}

func (directory *unixSecureDirectory) RenameNoReplace(oldName, newName string) error {
	if err := validateSecureEntryName(oldName); err != nil {
		return err
	}
	if err := validateSecureEntryName(newName); err != nil {
		return err
	}
	fd := int(directory.file.Fd())
	return renameNoReplaceAt(fd, oldName, fd, newName)
}

func (directory *unixSecureDirectory) Remove(name string) error {
	if err := validateSecureEntryName(name); err != nil {
		return err
	}
	return unix.Unlinkat(int(directory.file.Fd()), name, 0)
}

func (directory *unixSecureDirectory) Sync() error {
	return unix.Fsync(int(directory.file.Fd()))
}

func (directory *unixSecureDirectory) Close() error { return directory.file.Close() }

func (OSFileSystem) CreateExclusive(name string, perm os.FileMode) (DurableFile, error) {
	dir, base := filepath.Dir(name), filepath.Base(name)
	if base == "." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: exclusive file name", ErrUnsafeSecurePath)
	}
	directoryFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(directoryFD)
	fd, err := unix.Openat(directoryFD, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return nil, err
	}
	if err := validateSecureFD(fd, perm); err != nil {
		unix.Close(fd)
		_ = unix.Unlinkat(directoryFD, base, 0)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		_ = unix.Unlinkat(directoryFD, base, 0)
		return nil, fmt.Errorf("%w: exclusive file descriptor", ErrSecureCapabilityUnavailable)
	}
	return file, nil
}

func (OSFileSystem) OpenNoFollow(name string) (SecureReadFile, error) {
	dir, base := filepath.Dir(name), filepath.Base(name)
	if base == "." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: secure file name", ErrUnsafeSecurePath)
	}
	directoryFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(directoryFD)
	fd, err := unix.Openat(directoryFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, ErrSecureCapabilityUnavailable
	}
	return file, nil
}

func validateSecureFD(fd int, perm os.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: descriptor type", ErrUnsafeSecurePath)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("%w: descriptor link count", ErrUnsafeSecurePath)
	}
	if stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 {
		return fmt.Errorf("%w: descriptor special mode", ErrUnsafeSecurePath)
	}
	if os.FileMode(stat.Mode).Perm() != perm.Perm() {
		return fmt.Errorf("%w: descriptor mode %04o", ErrUnsafeSecurePath, os.FileMode(stat.Mode).Perm())
	}
	if uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("%w: descriptor owner", ErrUnsafeSecurePath)
	}
	return nil
}

func validateSecureDirectoryFD(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%w: directory descriptor type", ErrUnsafeSecurePath)
	}
	if stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 {
		return fmt.Errorf("%w: directory descriptor special mode", ErrUnsafeSecurePath)
	}
	if os.FileMode(stat.Mode).Perm() != 0o700 {
		return fmt.Errorf("%w: directory descriptor mode %04o", ErrUnsafeSecurePath, os.FileMode(stat.Mode).Perm())
	}
	if uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("%w: directory descriptor owner", ErrUnsafeSecurePath)
	}
	return nil
}
