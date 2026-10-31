package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
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
	var diagnostic codexAcceptanceLimitedBuffer
	diagnostic.limit = codexAcceptanceOutputLimit
	defer func() { clearBytes(diagnostic.buffer.Bytes()) }()
	cmd.Stderr = &diagnostic
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
		exitCode, signal := -1, 0
		class := "start_failed"
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exitCode = exit.ExitCode()
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signal = int(status.Signal())
			}
			class = codexAcceptanceFailureClass(diagnostic.buffer.Bytes())
		}
		return nil, fmt.Errorf("Codex acceptance command failed: exit_code=%d signal=%d stderr_class=%s", exitCode, signal, class)
	}
	return output.Bytes(), nil
}

func codexAcceptanceFailureClass(diagnostic []byte) string {
	lower := strings.ToLower(string(diagnostic))
	switch {
	case strings.Contains(lower, "status 429"), strings.Contains(lower, "rate limit"), strings.Contains(lower, "usage limit"):
		return "rate_limited"
	case strings.Contains(lower, "status 401"), strings.Contains(lower, "unauthorized"):
		return "unauthorised"
	case strings.Contains(lower, "status 403"), strings.Contains(lower, "forbidden"):
		return "forbidden"
	case strings.Contains(lower, "stream disconnected"), strings.Contains(lower, "websocket closed"):
		return "stream_disconnected"
	case strings.Contains(lower, "waiting for network"), strings.Contains(lower, "network is unreachable"):
		return "network_unavailable"
	case strings.Contains(lower, "timed out"), strings.Contains(lower, "timeout"):
		return "timed_out"
	case strings.Contains(lower, "error sending request"), strings.Contains(lower, "connection failed"):
		return "connection_failed"
	case strings.Contains(lower, "failed to refresh available models"):
		return "model_catalogue"
	default:
		return "unknown"
	}
}
