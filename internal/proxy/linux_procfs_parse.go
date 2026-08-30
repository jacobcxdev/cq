package proxy

import (
	"bytes"
	"errors"
	"net"
	"path"
	"strconv"
	"strings"
)

const (
	linuxProcFileMaxBytes = 64 << 10
	linuxProcTCPMaxRows   = 65_536
)

var errLinuxProcIdentity = errors.New("Linux process identity unavailable")

type linuxTCPSocket struct {
	Address      string
	Inode        uint64
	LoopbackIPv4 bool
}

func parseLinuxProcStat(data []byte, expectedPID int) (int, uint64, error) {
	if expectedPID <= 1 || len(data) == 0 || len(data) > linuxProcFileMaxBytes || bytes.IndexByte(data, 0) >= 0 || data[len(data)-1] != '\n' || bytes.Count(data, []byte{'\n'}) != 1 {
		return 0, 0, errLinuxProcIdentity
	}
	line := strings.TrimSuffix(string(data), "\n")
	prefix := strconv.Itoa(expectedPID) + " ("
	if !strings.HasPrefix(line, prefix) {
		return 0, 0, errLinuxProcIdentity
	}
	closeIndex := strings.LastIndex(line, ") ")
	if closeIndex < len(prefix) {
		return 0, 0, errLinuxProcIdentity
	}
	fields := strings.Fields(line[closeIndex+2:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, errLinuxProcIdentity
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return 0, 0, errLinuxProcIdentity
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, 0, errLinuxProcIdentity
	}
	return parent, start, nil
}

func parseLinuxProcStatusUID(data []byte) (uint64, error) {
	if len(data) == 0 || len(data) > linuxProcFileMaxBytes || bytes.IndexByte(data, 0) >= 0 {
		return 0, errLinuxProcIdentity
	}
	seen := 0
	var uid uint64
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		seen++
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) != 4 {
			return 0, errLinuxProcIdentity
		}
		for index, field := range fields {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return 0, errLinuxProcIdentity
			}
			if index == 0 {
				uid = value
			} else if value != uid {
				return 0, errLinuxProcIdentity
			}
		}
	}
	if seen != 1 {
		return 0, errLinuxProcIdentity
	}
	return uid, nil
}

func parseLinuxProcCmdline(data []byte) ([]string, error) {
	if len(data) < 2 || len(data) > linuxProcFileMaxBytes || data[len(data)-1] != 0 {
		return nil, errLinuxProcIdentity
	}
	raw := bytes.Split(data[:len(data)-1], []byte{0})
	if len(raw) == 0 || len(raw) > 256 {
		return nil, errLinuxProcIdentity
	}
	arguments := make([]string, len(raw))
	for index, argument := range raw {
		if len(argument) == 0 || bytes.IndexByte(argument, '\n') >= 0 {
			return nil, errLinuxProcIdentity
		}
		arguments[index] = string(argument)
	}
	return arguments, nil
}

func parseLinuxProcCgroup(data []byte) (string, error) {
	if len(data) == 0 || len(data) > linuxProcFileMaxBytes || bytes.IndexByte(data, 0) >= 0 {
		return "", errLinuxProcIdentity
	}
	seen := 0
	var membership string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			return "", errLinuxProcIdentity
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			return "", errLinuxProcIdentity
		}
		candidate := parts[2]
		if !strings.HasPrefix(candidate, "/") || path.Clean(candidate) != candidate || candidate == "/" {
			return "", errLinuxProcIdentity
		}
		seen++
		membership = candidate
	}
	if seen != 1 {
		return "", errLinuxProcIdentity
	}
	return membership, nil
}

func parseLinuxProcTCP(data []byte, expectedPort int, ipv6 bool) ([]linuxTCPSocket, error) {
	if expectedPort < 1 || expectedPort > 65_535 || len(data) == 0 || len(data) > 16<<20 || bytes.IndexByte(data, 0) >= 0 {
		return nil, errLinuxProcIdentity
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) < 1 || len(lines) > linuxProcTCPMaxRows+1 || !strings.Contains(lines[0], "local_address") || !strings.Contains(lines[0], "inode") {
		return nil, errLinuxProcIdentity
	}
	seenInodes := make(map[uint64]struct{})
	sockets := make([]linuxTCPSocket, 0, 1)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			return nil, errLinuxProcIdentity
		}
		local := strings.Split(fields[1], ":")
		if len(local) != 2 {
			return nil, errLinuxProcIdentity
		}
		port, err := strconv.ParseUint(local[1], 16, 16)
		if err != nil {
			return nil, errLinuxProcIdentity
		}
		if int(port) != expectedPort || fields[3] != "0A" {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			return nil, errLinuxProcIdentity
		}
		if _, duplicate := seenInodes[inode]; duplicate {
			return nil, errLinuxProcIdentity
		}
		seenInodes[inode] = struct{}{}
		address, loopbackIPv4, err := parseLinuxProcTCPAddress(local[0], expectedPort, ipv6)
		if err != nil {
			return nil, errLinuxProcIdentity
		}
		sockets = append(sockets, linuxTCPSocket{Address: address, Inode: inode, LoopbackIPv4: loopbackIPv4})
	}
	return sockets, nil
}

func parseLinuxProcTCPAddress(encoded string, port int, ipv6 bool) (string, bool, error) {
	if ipv6 {
		if len(encoded) != 32 {
			return "", false, errLinuxProcIdentity
		}
		if _, err := strconv.ParseUint(encoded[:16], 16, 64); err != nil {
			return "", false, errLinuxProcIdentity
		}
		if _, err := strconv.ParseUint(encoded[16:], 16, 64); err != nil {
			return "", false, errLinuxProcIdentity
		}
		return "[ipv6]:" + strconv.Itoa(port), false, nil
	}
	if len(encoded) != 8 {
		return "", false, errLinuxProcIdentity
	}
	raw, err := strconv.ParseUint(encoded, 16, 32)
	if err != nil {
		return "", false, errLinuxProcIdentity
	}
	ip := net.IPv4(byte(raw), byte(raw>>8), byte(raw>>16), byte(raw>>24))
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), ip.Equal(net.IPv4(127, 0, 0, 1)), nil
}

func parseLinuxSocketInode(link string) (uint64, bool) {
	if !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
		return 0, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
	if raw == "" || strings.Contains(raw, "]") {
		return 0, false
	}
	inode, err := strconv.ParseUint(raw, 10, 64)
	return inode, err == nil && inode != 0
}
