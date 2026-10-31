//go:build !windows

package proxy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCodexAcceptanceExecutionFailureClassification(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		mode string
		want string
	}{
		{mode: "stream", want: "exit_code=23 signal=0 stderr_class=stream_disconnected"},
		{mode: "rate", want: "exit_code=23 signal=0 stderr_class=rate_limited"},
		{mode: "unknown", want: "exit_code=23 signal=0 stderr_class=unknown"},
		{mode: "signal", want: "exit_code=-1 signal=15 stderr_class=unknown"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			_, err := runCodexAcceptanceExecution(context.Background(), codexAcceptanceExecution{
				executable: executable,
				args:       []string{"-test.run=^TestCodexAcceptanceDiagnosticHelperProcess$"},
				command: codexAcceptanceCommand{
					env: append(os.Environ(), "CQ_ACCEPTANCE_DIAGNOSTIC_HELPER="+test.mode),
				},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("command failure = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), "Authorization") {
				t.Fatalf("command failure exposed raw diagnostics: %v", err)
			}
		})
	}
}

func TestCodexAcceptanceDiagnosticHelperProcess(t *testing.T) {
	mode := os.Getenv("CQ_ACCEPTANCE_DIAGNOSTIC_HELPER")
	if mode == "" {
		return
	}
	fmt.Fprintln(os.Stderr, "Authorization: synthetic-private-credential")
	switch mode {
	case "stream":
		fmt.Fprintln(os.Stderr, "stream disconnected before completion: websocket closed by server")
	case "rate":
		fmt.Fprintln(os.Stderr, "unexpected status 429 Too Many Requests")
	case "signal":
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		// Signal delivery is asynchronous. Do not race the expected signal
		// termination against the ordinary failure exit below.
		time.Sleep(5 * time.Second)
		t.Fatal("SIGTERM did not terminate helper")
	}
	fmt.Fprintln(os.Stderr, strings.Repeat("synthetic-private-diagnostic", 1024))
	os.Exit(23)
}
