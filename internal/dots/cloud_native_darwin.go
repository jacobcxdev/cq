package dots

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Native app-tools authenticate the caller, its parent and its grandparent.
// Replace this wrapper so unsigned CQ never sits between signed native helpers.
func cloudStockExec(path string, arguments, environment []string, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	if stdin != os.Stdin || stdout != os.Stdout || stderr != os.Stderr {
		return false, nil
	}
	if err := syscall.Exec(path, append([]string{path}, arguments...), environment); err != nil {
		return true, errors.New("stock Codex CLI replacement failed")
	}
	return true, nil
}

// Run the RPC bridge beside the native CLI, rather than as its parent. The app
// still owns the CLI PID, signals and exit status; only stdin/stdout cross CQ.
func nativeCloudBridge(ctx context.Context, cfg *Settings, arguments []string, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	if stdin != os.Stdin || stdout != os.Stdout || stderr != os.Stderr {
		return false, nil
	}
	path, nativeArgs, environment, bridge, err := cloudInvocation(cfg, arguments, os.Environ())
	if err != nil {
		return true, err
	}
	if !bridge {
		return false, nil
	}
	input, toNative, err := os.Pipe()
	if err != nil {
		return true, errors.New("native bridge input unavailable")
	}
	defer input.Close()
	defer toNative.Close()
	fromNative, output, err := os.Pipe()
	if err != nil {
		return true, errors.New("native bridge output unavailable")
	}
	defer fromNative.Close()
	defer output.Close()
	executable, err := os.Executable()
	if err != nil {
		return true, errors.New("native bridge executable unavailable")
	}
	brokerArgs := append([]string{"__dots", "cloud-stdio-bridge", filepath.Dir(cfg.RuntimeRoot)}, arguments...)
	broker := exec.CommandContext(ctx, executable, brokerArgs...)
	broker.Env, broker.Stdin, broker.Stdout, broker.Stderr = environment, os.Stdin, os.Stdout, os.Stderr
	broker.ExtraFiles = []*os.File{toNative, fromNative}
	if err := broker.Start(); err != nil {
		return true, errors.New("native bridge launch failed")
	}
	// Successful exec never returns. On any failure, reap the broker and close
	// its handles rather than leaving an unowned bridge or unsigned fallback.
	defer func() {
		_ = broker.Process.Kill()
		_ = broker.Wait()
	}()
	_ = toNative.Close()
	_ = fromNative.Close()
	if err := unix.Dup2(int(input.Fd()), int(os.Stdin.Fd())); err != nil {
		return true, errors.New("native bridge input handoff failed")
	}
	if err := unix.Dup2(int(output.Fd()), int(os.Stdout.Fd())); err != nil {
		return true, errors.New("native bridge output handoff failed")
	}
	_, err = cloudStockExec(path, nativeArgs, environment, os.Stdin, os.Stdout, os.Stderr)
	return true, err
}

func cloudBridgeLocal() (*cloudLocal, error) {
	input, output := os.NewFile(3, "native-input"), os.NewFile(4, "native-output")
	for index, file := range []*os.File{input, output} {
		info, err := file.Stat()
		flags, flagErr := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		mode := unix.O_WRONLY
		if index == 1 {
			mode = unix.O_RDONLY
		}
		if err != nil || flagErr != nil || info.Mode()&os.ModeNamedPipe == 0 || flags&unix.O_ACCMODE != mode {
			_ = input.Close()
			_ = output.Close()
			return nil, errors.New("native bridge descriptors invalid")
		}
	}
	return &cloudLocal{input: input, output: output, wait: func() error { return nil }, stop: func() {
		_ = input.Close()
		_ = output.Close()
	}}, nil
}
