package proxy

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

type codexAcceptanceExecution struct {
	command    codexAcceptanceCommand
	executable string
	args       []string
	proof      codexInstalledExecutableProof
}

type codexAcceptanceConfinement interface {
	Execute(context.Context, codexAcceptanceExecution) ([]byte, error)
}

func runCodexAcceptanceExecution(ctx context.Context, execution codexAcceptanceExecution) ([]byte, error) {
	if ctx == nil || execution.executable == "" {
		return nil, errors.New("Codex acceptance execution unavailable")
	}
	command := execution.command
	cmd := exec.CommandContext(ctx, execution.executable, execution.args...)
	cmd.Env = append([]string(nil), command.env...)
	cmd.Dir = command.dir
	cmd.Stdin = strings.NewReader("")
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	var output codexAcceptanceLimitedBuffer
	output.limit = codexAcceptanceOutputLimit
	if command.captureOutput {
		cmd.Stdout = &output
	} else {
		cmd.Stdout = io.Discard
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Codex acceptance command timed out")
		}
		return nil, errors.New("Codex acceptance command failed")
	}
	return output.Bytes(), nil
}
