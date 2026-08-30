//go:build linux

package proxy

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	linuxProcDirectoryMaxEntries = 4_096
	linuxExecutableMaxBytes      = 1 << 30
)

type LinuxExecutableIdentity struct {
	Path   string
	Device uint64
	Inode  uint64
	Links  uint64
	Owner  uint64
	Size   int64
	Mode   uint32
	SHA256 [sha256.Size]byte
}

func (identity LinuxExecutableIdentity) Valid() bool {
	permissions := identity.Mode & 0o777
	return filepath.IsAbs(identity.Path) && filepath.Clean(identity.Path) == identity.Path &&
		identity.Device != 0 && identity.Inode != 0 && identity.Links == 1 && identity.Size > 0 &&
		identity.Size <= linuxExecutableMaxBytes && identity.Mode&unix.S_IFMT == unix.S_IFREG &&
		permissions&0o111 != 0 && permissions&0o022 == 0 && identity.SHA256 != ([sha256.Size]byte{})
}

type LinuxProcessIdentity struct {
	PID        int
	ParentPID  int
	StartTime  uint64
	UID        uint64
	Arguments  []string
	CgroupPath string
	Executable LinuxExecutableIdentity
}

func (identity LinuxProcessIdentity) Valid() bool {
	return identity.PID > 1 && identity.ParentPID >= 0 && identity.StartTime != 0 &&
		len(identity.Arguments) != 0 && identity.CgroupPath != "" && identity.Executable.Valid()
}

func (identity LinuxProcessIdentity) Equal(other LinuxProcessIdentity) bool {
	return identity.PID == other.PID && identity.ParentPID == other.ParentPID && identity.StartTime == other.StartTime &&
		identity.UID == other.UID && identity.CgroupPath == other.CgroupPath &&
		identity.Executable == other.Executable && slices.Equal(identity.Arguments, other.Arguments)
}

type linuxProcOperations struct {
	readFile          func(string, int64) ([]byte, error)
	readDir           func(string, int) ([]os.DirEntry, error)
	readlink          func(string) (string, error)
	captureExecutable func(string) (LinuxExecutableIdentity, error)
}

func defaultLinuxProcOperations() linuxProcOperations {
	return linuxProcOperations{
		readFile:          readLinuxProcFile,
		readDir:           readLinuxProcDirectory,
		readlink:          os.Readlink,
		captureExecutable: captureLinuxProcExecutable,
	}
}

func CaptureLinuxProcess(pid int) (LinuxProcessIdentity, error) {
	return captureLinuxProcessWithOperations(pid, defaultLinuxProcOperations())
}

func RevalidateLinuxProcess(expected LinuxProcessIdentity) error {
	if !expected.Valid() {
		return errLinuxProcIdentity
	}
	current, err := CaptureLinuxProcess(expected.PID)
	if err != nil || !current.Equal(expected) {
		return errLinuxProcIdentity
	}
	return nil
}

func captureLinuxProcessWithOperations(pid int, operations linuxProcOperations) (LinuxProcessIdentity, error) {
	if pid <= 1 || operations.readFile == nil || operations.captureExecutable == nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	statBefore, err := operations.readFile(filepath.Join(root, "stat"), linuxProcFileMaxBytes)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	parentPID, startTime, err := parseLinuxProcStat(statBefore, pid)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	status, err := operations.readFile(filepath.Join(root, "status"), linuxProcFileMaxBytes)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	uid, err := parseLinuxProcStatusUID(status)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	cmdline, err := operations.readFile(filepath.Join(root, "cmdline"), linuxProcFileMaxBytes)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	arguments, err := parseLinuxProcCmdline(cmdline)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	cgroup, err := operations.readFile(filepath.Join(root, "cgroup"), linuxProcFileMaxBytes)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	cgroupPath, err := parseLinuxProcCgroup(cgroup)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	executable, err := operations.captureExecutable(filepath.Join(root, "exe"))
	if err != nil || !executable.Valid() {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	statAfter, err := operations.readFile(filepath.Join(root, "stat"), linuxProcFileMaxBytes)
	if err != nil {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	parentAfter, startAfter, err := parseLinuxProcStat(statAfter, pid)
	if err != nil || parentAfter != parentPID || startAfter != startTime {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	identity := LinuxProcessIdentity{
		PID: pid, ParentPID: parentPID, StartTime: startTime, UID: uid,
		Arguments: arguments, CgroupPath: cgroupPath, Executable: executable,
	}
	if !identity.Valid() {
		return LinuxProcessIdentity{}, errLinuxProcIdentity
	}
	return identity, nil
}

func readLinuxProcFile(path string, maxBytes int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || maxBytes <= 0 {
		return nil, errLinuxProcIdentity
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		return nil, errors.Join(errLinuxProcIdentity, err)
	}
	return data, nil
}

func readLinuxProcDirectory(path string, maxEntries int) ([]os.DirEntry, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || maxEntries <= 0 || maxEntries > linuxProcDirectoryMaxEntries {
		return nil, errLinuxProcIdentity
	}
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxEntries {
		return nil, errLinuxProcIdentity
	}
	return entries, nil
}

func captureLinuxProcExecutable(procPath string) (LinuxExecutableIdentity, error) {
	if !filepath.IsAbs(procPath) || filepath.Clean(procPath) != procPath {
		return LinuxExecutableIdentity{}, errLinuxProcIdentity
	}
	resolved, err := os.Readlink(procPath)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved || len(resolved) > 4_096 || strings.HasSuffix(resolved, " (deleted)") {
		return LinuxExecutableIdentity{}, errLinuxProcIdentity
	}
	fd, err := unix.Open(procPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return LinuxExecutableIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), "linux-process-executable")
	if file == nil {
		_ = unix.Close(fd)
		return LinuxExecutableIdentity{}, errLinuxProcIdentity
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return LinuxExecutableIdentity{}, err
	}
	if stat.Size <= 0 || stat.Size > linuxExecutableMaxBytes {
		return LinuxExecutableIdentity{}, errLinuxProcIdentity
	}
	hasher := sha256.New()
	copied, err := io.Copy(hasher, io.LimitReader(file, linuxExecutableMaxBytes+1))
	if err != nil || copied != stat.Size {
		return LinuxExecutableIdentity{}, errors.Join(errLinuxProcIdentity, err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	identity := LinuxExecutableIdentity{
		Path: resolved, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Links: uint64(stat.Nlink),
		Owner: uint64(stat.Uid), Size: stat.Size, Mode: stat.Mode, SHA256: digest,
	}
	if !identity.Valid() {
		return LinuxExecutableIdentity{}, fmt.Errorf("%w: executable", errLinuxProcIdentity)
	}
	return identity, nil
}
