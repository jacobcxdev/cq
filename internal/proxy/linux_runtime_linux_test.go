//go:build linux

package proxy

import (
	"context"
	"errors"
	"testing"
)

func TestLinuxRuntimeInspectionRequiresOneExactCandidate(t *testing.T) {
	executable := LinuxExecutableIdentity{
		Path: "/home/test/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 501,
		Size: 4, Mode: 0o100755, SHA256: [32]byte{1},
	}
	process := LinuxProcessIdentity{
		PID: 42, ParentPID: 1, StartTime: 100, UID: 501,
		Arguments:  []string{"/home/test/bin/cq", "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
		Executable: executable,
	}
	listener := LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 7, Process: process}
	operations := linuxRuntimeInspectionOperations{
		listPIDs:          func() ([]int, error) { return []int{41, 42, 43}, nil },
		captureExpected:   func(string) (LinuxExecutableIdentity, error) { return executable, nil },
		resolveExecutable: func(string) (string, error) { return executable.Path, nil },
		effectiveUID:      func() int { return 501 },
		prefilter:         func(pid int, _ uint64, _ string) (bool, error) { return pid == 42, nil },
		captureProcess: func(pid int) (LinuxProcessIdentity, error) {
			if pid == 42 {
				return process, nil
			}
			return LinuxProcessIdentity{}, errLinuxProcIdentity
		},
		captureListener: func(pid, port int) (LinuxListenerIdentity, error) {
			if pid != 42 || port != 19280 {
				return LinuxListenerIdentity{}, errLinuxProcIdentity
			}
			return listener, nil
		},
	}

	identity, err := inspectLinuxProxyRuntimeWithOperations(context.Background(), executable.Path, 19280, operations)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Process.PID != 42 || identity.Listener.Inode != 7 {
		t.Fatalf("unexpected runtime identity: %+v", identity)
	}

	operations.listPIDs = func() ([]int, error) { return []int{42, 44}, nil }
	operations.prefilter = func(int, uint64, string) (bool, error) { return true, nil }
	operations.captureProcess = func(pid int) (LinuxProcessIdentity, error) {
		candidate := process
		candidate.PID = pid
		return candidate, nil
	}
	operations.captureListener = func(pid, _ int) (LinuxListenerIdentity, error) {
		candidate := listener
		candidate.Process = process
		candidate.Process.PID = pid
		return candidate, nil
	}
	if _, err := inspectLinuxProxyRuntimeWithOperations(context.Background(), executable.Path, 19280, operations); err == nil {
		t.Fatal("ambiguous runtime unexpectedly accepted")
	}
}

func TestLinuxRuntimeInspectionHashesOnlyPrefilteredCandidates(t *testing.T) {
	executable := LinuxExecutableIdentity{
		Path: "/home/test/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 501,
		Size: 4, Mode: 0o100755, SHA256: [32]byte{1},
	}
	process := LinuxProcessIdentity{
		PID: 42, ParentPID: 1, StartTime: 100, UID: 501,
		Arguments:  []string{executable.Path, "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service", Executable: executable,
	}
	listener := LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 7, Process: process}
	captured := make([]int, 0, 1)
	operations := linuxRuntimeInspectionOperations{
		listPIDs:          func() ([]int, error) { return []int{40, 41, 42, 43}, nil },
		captureExpected:   func(string) (LinuxExecutableIdentity, error) { return executable, nil },
		resolveExecutable: func(string) (string, error) { return executable.Path, nil },
		effectiveUID:      func() int { return 501 },
		prefilter: func(pid int, uid uint64, path string) (bool, error) {
			if uid != 501 || path != executable.Path {
				t.Fatal("unexpected prefilter authority")
			}
			if pid == 40 {
				return false, errors.New("process exited during prefilter")
			}
			return pid == 42, nil
		},
		captureProcess: func(pid int) (LinuxProcessIdentity, error) {
			captured = append(captured, pid)
			return process, nil
		},
		captureListener: func(int, int) (LinuxListenerIdentity, error) { return listener, nil },
	}
	if _, err := inspectLinuxProxyRuntimeWithOperations(context.Background(), executable.Path, 19280, operations); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || captured[0] != 42 {
		t.Fatalf("fully captured PIDs = %v", captured)
	}
}

func TestLinuxRuntimePIDListSkipsInit(t *testing.T) {
	pids, err := listLinuxProcPIDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		if pid <= 1 {
			t.Fatalf("runtime PID list contains reserved PID %d", pid)
		}
	}
}

func TestLinuxRuntimeInspectionFailsClosed(t *testing.T) {
	operations := linuxRuntimeInspectionOperations{
		listPIDs:          func() ([]int, error) { return nil, errors.New("unavailable") },
		captureExpected:   func(string) (LinuxExecutableIdentity, error) { return LinuxExecutableIdentity{}, errLinuxProcIdentity },
		resolveExecutable: func(string) (string, error) { return "", errLinuxProcIdentity },
		effectiveUID:      func() int { return 501 },
		prefilter:         func(int, uint64, string) (bool, error) { return false, errLinuxProcIdentity },
		captureProcess:    func(int) (LinuxProcessIdentity, error) { return LinuxProcessIdentity{}, errLinuxProcIdentity },
		captureListener:   func(int, int) (LinuxListenerIdentity, error) { return LinuxListenerIdentity{}, errLinuxProcIdentity },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inspectLinuxProxyRuntimeWithOperations(ctx, "/home/test/bin/cq", 19280, operations); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled inspection error = %v", err)
	}
	if _, err := inspectLinuxProxyRuntimeWithOperations(context.Background(), "/home/test/bin/cq", 19280, operations); err == nil {
		t.Fatal("unavailable procfs unexpectedly accepted")
	}
}
