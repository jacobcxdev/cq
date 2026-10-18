//go:build darwin || linux

package dots

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func dotsTerminate(pid int) error {
	err := unix.Kill(pid, unix.SIGTERM)
	if errors.Is(err, unix.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// SpawnHelper starts a detached owned worker with a no-follow private log.
func SpawnHelper(s *Settings, executable, role string, args, env []string) (*os.Process, error) {
	if s == nil || !helperRole(role) || executable != filepath.Join(s.RuntimeRoot, "cq-dots-runtime") {
		return nil, errors.New("Invalid Dots helper identity")
	}
	if err := privateDirectory(s.RuntimeRoot); err != nil {
		return nil, err
	}
	if err := ValidateRuntimePaths(s.RuntimeRoot); err != nil {
		return nil, err
	}
	info, err := os.Lstat(executable)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o700 {
		return nil, errors.New("Dots helper executable must be owned and mode 0700")
	}
	fd, err := unix.Open(filepath.Join(s.RuntimeRoot, "release-"+role+".log"), unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	log := os.NewFile(uintptr(fd), "Dots helper log")
	defer log.Close()
	info, err = log.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o600 {
		return nil, errors.New("Dots helper log must be owned and mode 0600")
	}
	commandArgs := append([]string{"__dots", role, filepath.Dir(s.RuntimeRoot)}, args...)
	command := exec.Command(executable, commandArgs...)
	command.Dir = s.RuntimeRoot
	command.Env = env
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	go func() { defer func() { _ = recover() }(); _ = command.Wait() }()
	return command.Process, nil
}
