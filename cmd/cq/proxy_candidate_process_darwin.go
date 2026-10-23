//go:build darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

type darwinCandidateProcess struct {
	pid, port int
	started   unix.Timeval
}

func candidateDarwinListenerPID(ctx context.Context, port int) (int, error) {
	command := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-Fp")
	var output candidateDarwinOutput
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return 0, proxy.ErrCandidateLifecycleInvalid
	}
	return parseCandidateDarwinListenerPID(output.String())
}

func parseCandidateDarwinListenerPID(body string) (int, error) {
	pid := 0
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		// lsof emits selected file descriptor records even with -Fp.
		if strings.HasPrefix(line, "f") && pid != 0 {
			if fd, err := strconv.Atoi(line[1:]); err == nil && fd >= 0 {
				continue
			}
		}
		if !strings.HasPrefix(line, "p") {
			return 0, proxy.ErrCandidateLifecycleInvalid
		}
		value, err := strconv.Atoi(line[1:])
		if err != nil || value <= 1 || (pid != 0 && pid != value) {
			return 0, proxy.ErrCandidateLifecycleInvalid
		}
		pid = value
	}
	return pid, nil
}
func captureCandidateProcess(ctx context.Context, s proxy.CandidateLifecycleStateV1, _ uint64) (candidateProcessLease, error) {
	pid, err := candidateDarwinListenerPID(ctx, s.Port)
	if err != nil {
		return nil, err
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info.Proc.P_pid != int32(pid) || info.Eproc.Ucred.Uid != uint32(os.Geteuid()) || info.Proc.P_starttime.Sec == 0 {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	return &darwinCandidateProcess{pid: pid, port: s.Port, started: info.Proc.P_starttime}, nil
}
func (p *darwinCandidateProcess) Revalidate(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	pid, err := candidateDarwinListenerPID(ctx, p.port)
	if err != nil || pid != p.pid {
		return proxy.ErrCandidateLifecycleInvalid
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", p.pid)
	if err != nil || info.Proc.P_starttime != p.started || info.Eproc.Ucred.Uid != uint32(os.Geteuid()) {
		return proxy.ErrCandidateLifecycleInvalid
	}
	return nil
}

var candidateDarwinProcesses = unix.SysctlKinfoProcSlice

func (p *darwinCandidateProcess) Exited(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// The singular wrapper maps a successful zero-byte (absent PID) response
	// to EIO. The slice API preserves authoritative absence separately from errors.
	infos, err := candidateDarwinProcesses("kern.proc.pid", p.pid)
	if err != nil {
		return false, err
	}
	if len(infos) == 0 {
		return true, nil
	}
	if len(infos) != 1 || infos[0].Proc.P_pid != int32(p.pid) || infos[0].Proc.P_starttime != p.started || infos[0].Eproc.Ucred.Uid != uint32(os.Geteuid()) {
		return false, proxy.ErrCandidateLifecycleInvalid
	}
	return false, nil
}
func (*darwinCandidateProcess) Close() error { return nil }

type candidateDarwinOutput struct{ buffer bytes.Buffer }

func (b *candidateDarwinOutput) String() string { return b.buffer.String() }

func (b *candidateDarwinOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 64<<10 {
		return 0, proxy.ErrCandidateLifecycleInvalid
	}
	return b.buffer.Write(p)
}
