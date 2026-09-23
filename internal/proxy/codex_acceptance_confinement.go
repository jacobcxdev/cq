package proxy

import (
	"context"
	"errors"
	"io"
	"os"
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
	if command.cleanupContext != nil {
		return runCodexAcceptanceExecutionWithCleanup(ctx, execution)
	}
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

// The canonical runner owns its output pipe so a normally exited child cannot
// leave os/exec draining a descendant's inherited pipe beyond the caller budget.
func runCodexAcceptanceExecutionWithCleanup(ctx context.Context, execution codexAcceptanceExecution) ([]byte, error) {
	command := execution.command
	work, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	cmd := exec.CommandContext(work, execution.executable, execution.args...)
	cmd.Env = append([]string(nil), command.env...)
	cmd.Dir = command.dir
	// Nil stdin/stderr and uncaptured stdout use the null device, without copy goroutines.
	var reader, writer *os.File
	var err error
	if command.captureOutput {
		reader, writer, err = os.Pipe()
		if err != nil {
			return nil, errors.New("Codex acceptance output unavailable")
		}
		defer reader.Close()
		defer writer.Close()
		cmd.Stdout = writer
	}
	callbackDone := make(chan struct{})
	stopCleanup := context.AfterFunc(command.cleanupContext, func() {
		defer close(callbackDone)
		cancelWork()
		if reader != nil {
			_ = reader.Close()
		}
	})
	defer func() {
		if !stopCleanup() {
			<-callbackDone
		}
	}()
	if err := cmd.Start(); err != nil {
		return nil, errors.New("Codex acceptance command failed")
	}
	var output codexAcceptanceLimitedBuffer
	output.limit = codexAcceptanceOutputLimit
	var copied chan error
	var pipeErr error
	if reader != nil {
		pipeErr = writer.Close()
		copied = make(chan error, 1)
		go func() {
			var copyErr error
			defer func() {
				if recover() != nil {
					copyErr = errors.New("Codex acceptance output panicked")
				}
				if copyErr != nil {
					cancelWork()
				}
				closeErr := reader.Close()
				if errors.Is(closeErr, os.ErrClosed) {
					closeErr = nil
				}
				copied <- errors.Join(copyErr, closeErr)
			}()
			n, err := io.Copy(&output, io.LimitReader(reader, int64(output.limit)+1))
			copyErr = err
			if n > int64(output.limit) {
				copyErr = errors.New("Codex acceptance output exceeded limit")
			}
		}()
	}
	waitErr := cmd.Wait() // Always reap the direct child before returning.
	if copied != nil {
		pipeErr = errors.Join(pipeErr, <-copied)
	}
	if errors.Join(waitErr, pipeErr, ctx.Err(), command.cleanupContext.Err()) != nil {
		return nil, errors.New("Codex acceptance command failed")
	}
	return output.Bytes(), nil
}
