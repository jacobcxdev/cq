//go:build linux

package proxy

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

type LinuxListenerIdentity struct {
	Address string
	Inode   uint64
	Process LinuxProcessIdentity
}

func (identity LinuxListenerIdentity) Valid() bool {
	address, err := net.ResolveTCPAddr("tcp4", identity.Address)
	return err == nil && address.IP.Equal(net.IPv4(127, 0, 0, 1)) && address.Port > 0 &&
		identity.Inode != 0 && identity.Process.Valid()
}

func CaptureLinuxListener(pid, port int) (LinuxListenerIdentity, error) {
	return captureLinuxListenerWithOperations(pid, port, defaultLinuxProcOperations())
}

func RevalidateLinuxListener(expected LinuxListenerIdentity) error {
	if !expected.Valid() {
		return errLinuxProcIdentity
	}
	_, portText, err := net.SplitHostPort(expected.Address)
	if err != nil {
		return errLinuxProcIdentity
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return errLinuxProcIdentity
	}
	current, err := CaptureLinuxListener(expected.Process.PID, port)
	if err != nil || current.Address != expected.Address || current.Inode != expected.Inode || !current.Process.Equal(expected.Process) {
		return errLinuxProcIdentity
	}
	return nil
}

func captureLinuxListenerWithOperations(pid, port int, operations linuxProcOperations) (LinuxListenerIdentity, error) {
	if pid <= 1 || port < 1 || port > 65_535 || operations.readFile == nil || operations.readDir == nil || operations.readlink == nil {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	process, err := captureLinuxProcessWithOperations(pid, operations)
	if err != nil {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	tcp, err := operations.readFile("/proc/net/tcp", 16<<20)
	if err != nil {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	tcp4Sockets, err := parseLinuxProcTCP(tcp, port, false)
	if err != nil {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	tcp6, err := operations.readFile("/proc/net/tcp6", 16<<20)
	if err != nil {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	tcp6Sockets, err := parseLinuxProcTCP(tcp6, port, true)
	if err != nil || len(tcp4Sockets) != 1 || len(tcp6Sockets) != 0 || !tcp4Sockets[0].LoopbackIPv4 {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	socket := tcp4Sockets[0]
	entries, err := operations.readDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"), linuxProcDirectoryMaxEntries)
	if err != nil {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	matches := 0
	for _, entry := range entries {
		if entry.IsDir() {
			return LinuxListenerIdentity{}, errLinuxProcIdentity
		}
		link, err := operations.readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return LinuxListenerIdentity{}, errLinuxProcIdentity
		}
		inode, ok := parseLinuxSocketInode(link)
		if ok && inode == socket.Inode {
			matches++
		}
	}
	if matches != 1 {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	processAfter, err := captureLinuxProcessWithOperations(pid, operations)
	if err != nil || !processAfter.Equal(process) {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	identity := LinuxListenerIdentity{Address: socket.Address, Inode: socket.Inode, Process: process}
	if !identity.Valid() {
		return LinuxListenerIdentity{}, errLinuxProcIdentity
	}
	return identity, nil
}
