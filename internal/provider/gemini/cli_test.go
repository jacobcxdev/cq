package gemini

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestOSCommandRunnerReturnsOutput(t *testing.T) {
	t.Setenv("CQ_TEST_CLI_HELPER", "output")
	t.Setenv("CQ_TEST_CLI_OUTPUT", "quota output")

	output, err := (osCommandRunner{}).Run(
		context.Background(),
		os.Args[0],
		"-test.run=^TestCLIHelperProcess$",
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(output) != "quota output" {
		t.Fatalf("Run() output = %q, want %q", output, "quota output")
	}
}

func TestOSCommandRunnerRejectsOversizedOutput(t *testing.T) {
	t.Setenv("CQ_TEST_CLI_HELPER", "oversized")

	_, err := (osCommandRunner{}).Run(
		context.Background(),
		os.Args[0],
		"-test.run=^TestCLIHelperProcess$",
	)
	if !errors.Is(err, errCLIOutputTooLarge) {
		t.Fatalf("Run() error = %v, want %v", err, errCLIOutputTooLarge)
	}
}

func TestOSCommandRunnerHonoursCancellation(t *testing.T) {
	t.Setenv("CQ_TEST_CLI_HELPER", "block")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := (osCommandRunner{}).Run(
		ctx,
		os.Args[0],
		"-test.run=^TestCLIHelperProcess$",
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Run() cancellation took %s, want under 5s", elapsed)
	}
}

func TestCLIHelperProcess(t *testing.T) {
	switch os.Getenv("CQ_TEST_CLI_HELPER") {
	case "":
		return
	case "output":
		_, _ = os.Stdout.WriteString(os.Getenv("CQ_TEST_CLI_OUTPUT"))
	case "oversized":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxCLIOutputBytes+1))
	case "block":
		time.Sleep(time.Minute)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
