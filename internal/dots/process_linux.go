//go:build linux

package dots

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jacobcxdev/cq/internal/httputil"
)

func dotsProcessIDs(ctx context.Context) ([]int, error) {
	rows, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(row.Name())
		if err == nil && pid > 1 && row.IsDir() {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func readProcessFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return httputil.ReadBody(file)
}

func dotsProcessArguments(ctx context.Context, pid int) (int, []string, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	status, err := readProcessFile(filepath.Join(root, "status"))
	if err != nil {
		return 0, nil, err
	}
	uid := -1
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			values := strings.Fields(line)
			if len(values) >= 3 {
				uid, _ = strconv.Atoi(values[2])
			}
			break
		}
	}
	if uid < 0 {
		return 0, nil, errors.New("Process owner unavailable")
	}
	data, err := readProcessFile(filepath.Join(root, "cmdline"))
	if err != nil {
		return 0, nil, err
	}
	parts := bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0})
	args := make([]string, len(parts))
	for index, part := range parts {
		args[index] = string(part)
	}
	if len(args) > 0 && (filepath.Base(args[0]) == "cq-dots-runtime" || filepath.Base(args[0]) == "ChatGPT") {
		executable, err := os.Readlink(filepath.Join(root, "exe"))
		if err != nil || !runtimeExecutableMatches(args, executable) {
			return 0, nil, errors.New("Process executable differs from its arguments")
		}
	}
	return uid, args, nil
}

func dotsProcessCWD(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
}
