//go:build windows

package main

import (
	"context"

	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/windows"
)

type windowsCandidateProcess struct {
	handle   windows.Handle
	identity windowsServiceProcessIdentity
	port     int
}

func captureCandidateProcess(ctx context.Context, s proxy.CandidateLifecycleStateV1, _ uint64) (candidateProcessLease, error) {
	output, err := runWindowsNetstat(ctx)
	if err != nil || len(output) > 4<<20 {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	pid, err := parseWindowsListeningPID(output, s.Port)
	if err != nil {
		return nil, err
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, err
	}
	fail := true
	defer func() {
		if fail {
			_ = windows.CloseHandle(handle)
		}
	}()
	identity, err := readWindowsServiceProcessIdentity(pid)
	if err != nil {
		return nil, err
	}
	sid, err := currentWindowsServiceSID()
	if err != nil || sid != identity.SID {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	var created, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return nil, err
	}
	if identity.Created != uint64(created.HighDateTime)<<32|uint64(created.LowDateTime) || exited.HighDateTime != 0 || exited.LowDateTime != 0 {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	fail = false
	return &windowsCandidateProcess{handle, identity, s.Port}, nil
}
func (p *windowsCandidateProcess) Revalidate(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	output, err := runWindowsNetstat(ctx)
	if err != nil || len(output) > 4<<20 {
		return proxy.ErrCandidateLifecycleInvalid
	}
	pid, err := parseWindowsListeningPID(output, p.port)
	if err != nil || pid != p.identity.PID {
		return proxy.ErrCandidateLifecycleInvalid
	}
	identity, err := readWindowsServiceProcessIdentity(pid)
	if err != nil || identity.Created != p.identity.Created || identity.SID != p.identity.SID || identity.Executable != p.identity.Executable {
		return proxy.ErrCandidateLifecycleInvalid
	}
	return nil
}
func (p *windowsCandidateProcess) Exited(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	status, err := windows.WaitForSingleObject(p.handle, 0)
	if err != nil {
		return false, err
	}
	if status == windows.WAIT_OBJECT_0 {
		return true, nil
	}
	if status == uint32(windows.WAIT_TIMEOUT) {
		return false, nil
	}
	return false, proxy.ErrCandidateLifecycleInvalid
}
func (p *windowsCandidateProcess) Close() error { return windows.CloseHandle(p.handle) }
