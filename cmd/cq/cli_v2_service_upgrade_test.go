package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestCLIV2ServiceUpgradeNativeProtocol(t *testing.T) {
	for _, phase := range []string{"committed", "deferred", "early-failure"} {
		for _, mode := range []string{"human", "json", "json-before", "json-true", "json-true-before", "json-false", "json-false-after", "short-json-before", "native", "native-json"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				lifecycle, platform, _, previous, candidate := homebrewUpgradeServiceFixture(t)
				receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "public-upgrade", Generation: 1, Phase: phase, Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:12345", SupervisorPID: 41}
				if phase == "deferred" {
					receipt.ErrorCode = "active_turns"
				}
				if phase == "early-failure" {
					lifecycle.RuntimePreflight = func(context.Context, installstate.Record) error { return errors.New("synthetic preflight failure") }
				}
				lifecycle.RuntimeApply = func(context.Context, installer.RuntimeArtifact) (proxy.RuntimeUpgradeReceiptV1, error) {
					if phase == "committed" {
						platform.live = candidate.Path
					}
					return receipt, nil
				}
				native := []string{"upgrade", "--owner=homebrew", "--candidate-executable=" + candidate.Path, "--service-executable=" + lifecycle.Executable}
				args := []string{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", candidate.Path, "--service-executable", lifecycle.Executable}
				jsonMode := false
				switch mode {
				case "json":
					jsonMode = true
					args = append(args, "--json")
				case "json-before":
					jsonMode = true
					args = append([]string{"--json"}, args...)
				case "json-true":
					jsonMode = true
					args = append(args, "--json=true")
				case "json-true-before":
					jsonMode = true
					args = append([]string{"--json=true"}, args...)
				case "short-json-before":
					jsonMode = true
					args = append([]string{"-j"}, args...)
				case "native":
					args = append([]string{"service"}, native...)
				case "native-json":
					jsonMode = true
					args = append(append([]string{"service"}, native...), "--json")
				case "json-false-after":
					args = append(args, "--json=false")
				case "json-false":
					args = append([]string{"--json=false"}, args...)
				}
				if jsonMode {
					native = append(native, "--json")
				}
				if !isMachineABI(append([]string{"service"}, native...)) {
					t.Fatal("released exact upgrade grammar lost machine dispatch")
				}
				original := serviceLifecycleFactory
				t.Cleanup(func() { serviceLifecycleFactory = original })
				serviceLifecycleFactory = func(stable string) (*serviceLifecycle, error) {
					if stable != lifecycle.Executable {
						t.Fatalf("stable executable = %q", stable)
					}
					return lifecycle, nil
				}
				wantOut, wantErr, wantExit := captureServiceUpgradeDispatch(t, append([]string{"service"}, native...))
				var stdout, stderr bytes.Buffer
				session := &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: cli.TerminalOptions{Width: 80, Colour: true}, ErrTerminal: cli.TerminalOptions{Width: 80, Colour: true}}
				exit := cli.Run(context.Background(), args, session, lookupCLIV2)
				if exit != wantExit || stdout.String() != wantOut || stderr.String() != wantErr {
					t.Fatalf("public/native protocol differs: exit %d/%d\nstdout %q/%q\nstderr %q/%q", exit, wantExit, stdout.String(), wantOut, stderr.String(), wantErr)
				}
				if strings.Contains(stdout.String(), `"ok"`) || strings.Contains(stdout.String(), `"data"`) || strings.Contains(stdout.String(), "\x1b[") {
					t.Fatal("native receipt was wrapped or styled")
				}
			})
		}
	}
}

func TestCLIV2ServiceUpgradePreparationAndOutputFailure(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "candidate-cq")
	original := serviceLifecycleFactory
	t.Cleanup(func() { serviceLifecycleFactory = original })
	serviceLifecycleFactory = func(string) (*serviceLifecycle, error) { return nil, errors.New("synthetic preparation failure") }
	for _, args := range [][]string{
		{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", candidate, "--json"},
		{"--json=true", "service", "upgrade", "--owner=homebrew", "--candidate-executable=" + candidate},
		{"--json=false", "service", "upgrade", "--owner=homebrew", "--candidate-executable=" + candidate},
	} {
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr}, lookupCLIV2)
		if exit != 1 || stdout.Len() != 0 || stderr.String() != "cq: synthetic preparation failure\n" {
			t.Fatalf("preparation failure: exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
		}
	}
	inv := cli.Invocation{Path: "service upgrade", JSON: true, Options: map[string][]string{"owner": {"homebrew"}, "candidate-executable": {candidate}}}
	var stderr bytes.Buffer
	out := handleV2ServiceUpgradeWithRunner(inv, &cli.Session{Out: serviceUpgradeBrokenWriter{}, Err: &stderr}, func(args []string, output io.Writer) error {
		if !reflect.DeepEqual(args, []string{"upgrade", "--owner=homebrew", "--candidate-executable=" + candidate, "--json"}) {
			t.Fatalf("normalised arguments: %q", args)
		}
		_, err := io.WriteString(output, "receipt\n")
		return err
	})
	if out.ExitCode != 1 || !out.Streamed || stderr.String() != "cq: synthetic output failure\n" {
		t.Fatalf("output failure: %+v stderr=%q", out, stderr.String())
	}
}

type serviceUpgradeBrokenWriter struct{}

func (serviceUpgradeBrokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}

func TestCLIV2ServiceUpgradeHelpAndValidationArePure(t *testing.T) {
	original := serviceLifecycleFactory
	t.Cleanup(func() { serviceLifecycleFactory = original })
	serviceLifecycleFactory = func(string) (*serviceLifecycle, error) {
		t.Fatal("pure parse/help accessed lifecycle")
		return nil, nil
	}
	for _, args := range [][]string{{"help", "service", "upgrade"}, {"service", "upgrade", "--help"}, {"--json", "service", "upgrade", "--help"}} {
		var stdout, stderr bytes.Buffer
		if exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr}, lookupCLIV2); exit != 0 {
			t.Fatalf("help %q: exit=%d stderr=%s", args, exit, stderr.String())
		}
		for _, flag := range []string{"--owner", "--candidate-executable", "--service-executable", "--json"} {
			if !strings.Contains(stdout.String(), flag) {
				t.Fatalf("help omits %s", flag)
			}
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var cancelledOut, cancelledErr bytes.Buffer
	if exit := cli.Run(cancelled, []string{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", filepath.Join(t.TempDir(), "cq")}, &cli.Session{Out: &cancelledOut, Err: &cancelledErr}, lookupCLIV2); exit != 130 {
		t.Fatalf("already-cancelled upgrade exit=%d stdout=%s stderr=%s", exit, cancelledOut.String(), cancelledErr.String())
	}

	for _, args := range [][]string{
		{"service", "upgrade"},
		{"service", "upgrade", "--owner", "homebrew"},
		{"service", "upgrade", "--candidate-executable", "/tmp/cq"},
		{"service", "upgrade", "--owner", "go", "--candidate-executable", "/tmp/cq"},
		{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", "relative"},
		{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", "/tmp/../cq"},
		{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", "/tmp/cq", "--component", "proxy"},
		{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", "/tmp/cq", "--timeout", "10s"},
		{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", "/tmp/cq", "--installer-lock-held"},
		{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", "/tmp/cq", "--service-executable", ""},
	} {
		var stdout, stderr bytes.Buffer
		if exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr}, lookupCLIV2); exit != 2 {
			t.Fatalf("invalid syntax %q: exit=%d stdout=%s stderr=%s", args, exit, stdout.String(), stderr.String())
		}
	}
}

func captureServiceUpgradeDispatch(t *testing.T, args []string) (string, string, int) {
	t.Helper()
	root := t.TempDir()
	stdout, err := os.Create(filepath.Join(root, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(root, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	os.Stdout, os.Stderr = stdout, stderr
	handled, exit := dispatchMachineABI(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	if !handled {
		t.Fatalf("native upgrade was not dispatched: %q", args)
	}
	out, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(diagnostic), exit
}
