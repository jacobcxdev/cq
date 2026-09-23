//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jacobcxdev/cq/internal/proxy"
)

type linuxCandidateProcess struct{ listener proxy.LinuxListenerIdentity }

func captureCandidateProcess(ctx context.Context, s proxy.CandidateLifecycleStateV1, generation uint64) (candidateProcessLease, error) {
	s.Generation = generation
	want := candidateRuntimeArguments(s)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var found *linuxCandidateProcess
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		body, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		args := bytes.Split(bytes.TrimSuffix(body, []byte{0}), []byte{0})
		if len(args) != len(want)+1 {
			continue
		}
		matches := true
		for i, arg := range want {
			if string(args[i+1]) != arg {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		listener, err := proxy.CaptureLinuxListener(pid, s.Port)
		if err != nil {
			return nil, err
		}
		if listener.Process.UID != uint64(os.Geteuid()) || found != nil {
			return nil, proxy.ErrCandidateLifecycleInvalid
		}
		found = &linuxCandidateProcess{listener: listener}
	}
	if found == nil {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	return found, nil
}
func (p *linuxCandidateProcess) Revalidate(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return proxy.RevalidateLinuxListener(p.listener)
}
func (p *linuxCandidateProcess) Exited(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(p.listener.Process.PID)))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	current, err := proxy.CaptureLinuxProcess(p.listener.Process.PID)
	if err != nil {
		return false, err
	}
	if !p.listener.Process.Equal(current) {
		return false, proxy.ErrCandidateLifecycleInvalid
	}
	return false, nil
}
func (*linuxCandidateProcess) Close() error { return nil }
