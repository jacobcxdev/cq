package gemini

import (
	"context"
	"errors"
	"io"
	"os/exec"
)

const maxCLIOutputBytes = 1 << 20

var errCLIOutputTooLarge = errors.New("quota command output exceeds limit")

type commandRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) ([]byte, error)
}

type osCommandRunner struct{}

func (osCommandRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (osCommandRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("prepare quota command output")
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, errors.New("start quota command")
	}

	output, readErr := io.ReadAll(io.LimitReader(stdout, maxCLIOutputBytes+1))
	if readErr != nil || len(output) > maxCLIOutputBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil {
			return nil, errors.New("read quota command output")
		}
		return nil, errCLIOutputTooLarge
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("quota command failed")
	}
	return output, nil
}
