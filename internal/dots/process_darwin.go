//go:build darwin

package dots

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func dotsProcessIDs(ctx context.Context) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, errors.New("Dots process query failed")
	}
	var pids []int
	for _, row := range rows {
		if row.Eproc.Ucred.Uid == uint32(os.Getuid()) && row.Proc.P_pid > 1 {
			pids = append(pids, int(row.Proc.P_pid))
		}
	}
	return pids, nil
}

func parseDarwinProcessArguments(data []byte) ([]string, error) {
	if len(data) < 5 || len(data) > 2<<20 {
		return nil, errors.New("Invalid process arguments")
	}
	count := int(binary.NativeEndian.Uint32(data[:4]))
	if count < 1 || count > 4096 {
		return nil, errors.New("Invalid process arguments")
	}
	data = data[4:]
	end := bytes.IndexByte(data, 0)
	if end < 0 {
		return nil, errors.New("Invalid process arguments")
	}
	data = data[end+1:]
	for len(data) > 0 && data[0] == 0 {
		data = data[1:]
	}
	args := make([]string, 0, count)
	for range count {
		end = bytes.IndexByte(data, 0)
		if end < 0 {
			return nil, errors.New("Invalid process arguments")
		}
		args = append(args, string(data[:end]))
		data = data[end+1:]
	}
	return args, nil
}

func dotsProcessArguments(ctx context.Context, pid int) (int, []string, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, nil, err
	}
	if info.Proc.P_pid != int32(pid) {
		return 0, nil, errors.New("Process unavailable")
	}
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return 0, nil, err
	}
	args, err := parseDarwinProcessArguments(data)
	if err == nil {
		end := bytes.IndexByte(data[4:], 0)
		if !runtimeExecutableMatches(args, string(data[4:4+end])) {
			return 0, nil, errors.New("Process executable differs from its arguments")
		}
	}
	return int(info.Eproc.Ucred.Uid), args, err
}

func dotsProcessCWD(ctx context.Context, pid int) (string, error) {
	query, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := exec.CommandContext(query, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", errors.New("Process directory unavailable")
	}
	var directories []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "n") {
			directories = append(directories, line[1:])
		}
	}
	if len(directories) != 1 {
		return "", errors.New("Process directory unavailable")
	}
	return directories[0], nil
}
