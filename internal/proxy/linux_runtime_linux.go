//go:build linux

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

type LinuxProxyRuntimeIdentity struct {
	Process  LinuxProcessIdentity
	Listener LinuxListenerIdentity
}

func (identity LinuxProxyRuntimeIdentity) Valid() bool {
	return identity.Process.Valid() && identity.Listener.Valid() && identity.Listener.Process.Equal(identity.Process)
}

type linuxRuntimeInspectionOperations struct {
	listPIDs          func() ([]int, error)
	captureExpected   func(string) (LinuxExecutableIdentity, error)
	resolveExecutable func(string) (string, error)
	effectiveUID      func() int
	captureProcess    func(int) (LinuxProcessIdentity, error)
	captureListener   func(int, int) (LinuxListenerIdentity, error)
}

func defaultLinuxRuntimeInspectionOperations() linuxRuntimeInspectionOperations {
	return linuxRuntimeInspectionOperations{
		listPIDs:          listLinuxProcPIDs,
		captureExpected:   CaptureLinuxExecutable,
		resolveExecutable: filepath.EvalSymlinks,
		effectiveUID:      os.Geteuid,
		captureProcess:    CaptureLinuxProcess,
		captureListener:   CaptureLinuxListener,
	}
}

func InspectLinuxProxyRuntime(ctx context.Context, executable string, port int) (LinuxProxyRuntimeIdentity, error) {
	return inspectLinuxProxyRuntimeWithOperations(ctx, executable, port, defaultLinuxRuntimeInspectionOperations())
}

func inspectLinuxProxyRuntimeWithOperations(ctx context.Context, executable string, port int, operations linuxRuntimeInspectionOperations) (LinuxProxyRuntimeIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return LinuxProxyRuntimeIdentity{}, err
	}
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || port < 1 || port > 65_535 ||
		operations.listPIDs == nil || operations.captureExpected == nil || operations.resolveExecutable == nil || operations.effectiveUID == nil ||
		operations.captureProcess == nil || operations.captureListener == nil {
		return LinuxProxyRuntimeIdentity{}, errLinuxProcIdentity
	}
	resolved, err := operations.resolveExecutable(executable)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return LinuxProxyRuntimeIdentity{}, errLinuxProcIdentity
	}
	expected, err := operations.captureExpected(resolved)
	if err != nil || !expected.Valid() || expected.Path != resolved {
		return LinuxProxyRuntimeIdentity{}, errLinuxProcIdentity
	}
	uid := operations.effectiveUID()
	if uid < 0 {
		return LinuxProxyRuntimeIdentity{}, errLinuxProcIdentity
	}
	pids, err := operations.listPIDs()
	if err != nil {
		return LinuxProxyRuntimeIdentity{}, errLinuxProcIdentity
	}
	matches := make([]LinuxProxyRuntimeIdentity, 0, 1)
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return LinuxProxyRuntimeIdentity{}, err
		}
		process, err := operations.captureProcess(pid)
		if err != nil || !process.Valid() || process.UID != uint64(uid) || process.Executable != expected ||
			!linuxProxyRuntimeArgumentsMatch(process.Arguments, executable) || !linuxProxyRuntimeCgroupMatches(process.CgroupPath, uint64(uid)) {
			continue
		}
		listener, err := operations.captureListener(pid, port)
		if err != nil || !listener.Valid() || !listener.Process.Equal(process) {
			continue
		}
		matches = append(matches, LinuxProxyRuntimeIdentity{Process: process, Listener: listener})
	}
	if len(matches) != 1 || !matches[0].Valid() {
		return LinuxProxyRuntimeIdentity{}, errLinuxProcIdentity
	}
	return matches[0], nil
}

func listLinuxProcPIDs() ([]int, error) {
	entries, err := readLinuxProcDirectory("/proc", linuxProcDirectoryMaxEntries)
	if err != nil {
		return nil, errLinuxProcIdentity
	}
	pids := make([]int, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if pid <= 1 || entry.IsDir() == false {
			return nil, errLinuxProcIdentity
		}
		if _, duplicate := seen[pid]; duplicate {
			return nil, errLinuxProcIdentity
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}
