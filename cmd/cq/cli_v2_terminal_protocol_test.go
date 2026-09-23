package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var terminalProtocolFixtures = []terminalFixtureRegistration{
	{"cq codex proxy trace", "Success", "stdout", "text/cq-codex-proxy-trace/02-success.txt"},
	{"cq codex proxy trace", "Failure", "stderr", "text/cq-codex-proxy-trace/03-failure.txt"},
	{"cq codex proxy trace", "Partial history · retained rotation gap", "stdout", "text/cq-codex-proxy-trace/04-partial-history-retained-rotation-gap.txt"},
	{"cq codex proxy trace", "Partial history · retained rotation gap", "stderr", "text/cq-codex-proxy-trace/05-partial-history-retained-rotation-gap.txt"},
	{"cq codex proxy trace", "Timeout · records already emitted", "stdout", "text/cq-codex-proxy-trace/06-timeout-records-already-emitted.txt"},
	{"cq codex proxy trace", "Timeout · records already emitted", "stderr", "text/cq-codex-proxy-trace/07-timeout-records-already-emitted.txt"},
	{"cq codex proxy trace", "Interrupted · records already emitted", "stdout", "text/cq-codex-proxy-trace/08-interrupted-records-already-emitted.txt"},
	{"cq codex proxy trace", "Interrupted · records already emitted", "stderr", "text/cq-codex-proxy-trace/09-interrupted-records-already-emitted.txt"},
	{"cq codex proxy hook stop", "Success · no receipt", "stdout", "text/cq-codex-proxy-hook-stop/02-success-no-receipt.txt"},
	{"cq codex proxy hook stop", "Failure", "stderr", "text/cq-codex-proxy-hook-stop/03-failure.txt"},
	{"cq codex proxy hook stop", "Failure · lookup deadline", "stderr", "text/cq-codex-proxy-hook-stop/04-failure-lookup-deadline.txt"},
	{"cq codex proxy hook stop", "Success · retained receipt protocol", "stdout", "text/cq-codex-proxy-hook-stop/05-success-retained-receipt-protocol.txt"},
}

func TestTerminalProtocolFixtureRegistration(t *testing.T) {
	manifestBytes, err := os.ReadFile(filepath.Join("..", "..", "docs", "superpowers", "plans", "terminal-presentation-coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows struct {
		Fixtures []struct {
			Command, Scenario, Stream, File, Task string
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(manifestBytes, &rows); err != nil {
		t.Fatal(err)
	}
	registered := make(map[string]terminalFixtureRegistration, len(terminalProtocolFixtures))
	for _, fixture := range terminalProtocolFixtures {
		if _, exists := registered[fixture.File]; exists {
			t.Errorf("duplicate protocol fixture %s", fixture.File)
		}
		registered[fixture.File] = fixture
	}
	owned := 0
	for _, row := range rows.Fixtures {
		if row.Task != "T14" {
			continue
		}
		owned++
		want := terminalFixtureRegistration{row.Command, row.Scenario, row.Stream, row.File}
		if got, ok := registered[row.File]; !ok || got != want {
			t.Errorf("protocol fixture ownership mismatch: %s", row.File)
		}
	}
	if owned != len(registered) {
		t.Errorf("protocol fixture count: owned %d registered %d", owned, len(registered))
	}
}

func protocolFixtureText(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", file))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func protocolOptions(width int, colour bool) cli.TerminalOptions {
	if width == 0 {
		return cli.TerminalOptions{}
	}
	o := terminalFixtureOptions(width)
	o.Colour = colour
	return o
}

func assertProtocolStreams(t *testing.T, stdout, stderr string, width int, colour bool, stdoutFile, stderrFile, plainError string) {
	t.Helper()
	wantStdout := ""
	if stdoutFile != "" {
		wantStdout = protocolFixtureText(t, stdoutFile)
	}
	if stdout != wantStdout || strings.Contains(stdout, "\x1b[") {
		t.Fatalf("raw stdout changed at width %d colour %t: %q", width, colour, stdout)
	}
	if stderrFile == "" {
		if stderr != "" {
			t.Fatalf("unexpected stderr %q", stderr)
		}
		return
	}
	if width == 0 {
		if stderr != plainError {
			t.Fatalf("redirected diagnostic changed: %q", stderr)
		}
		return
	}
	if width >= 100 && ansi.Strip(stderr) != protocolFixtureText(t, stderrFile) {
		t.Fatalf("terminal diagnostic changed: %q", stderr)
	}
	for _, line := range strings.Split(stderr, "\n") {
		if ansi.StringWidth(line) > min(width, 100) {
			t.Fatalf("diagnostic exceeds %d cells: %q", width, line)
		}
	}
	if !colour && strings.Contains(stderr, "\x1b[") {
		t.Fatalf("colour leaked into diagnostic: %q", stderr)
	}
}

func protocolTraceRecord() string {
	return `{"time":"2026-09-23T15:29:45Z","event_type":"codex_trace","trace_id":"trace-demo-01","sequence":1,"transport":"http","phase":"route_summary","outcome":"success","account_hint":"acct-alex","attempt":1,"status_code":200,"pool":"work"}` + "\n"
}

func TestTerminalTraceRawFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, outFile, errFile, plainError string
		exit                               int
	}{
		{"success", "text/cq-codex-proxy-trace/02-success.txt", "", "", 0},
		{"failure", "", "text/cq-codex-proxy-trace/03-failure.txt", "cq: Selected trace log is not configured.\n", 4},
		{"gap", "text/cq-codex-proxy-trace/04-partial-history-retained-rotation-gap.txt", "text/cq-codex-proxy-trace/05-partial-history-retained-rotation-gap.txt", "cq: Trace history has a gap: diagnostics log rotated beyond retained history while following.\n", 8},
		{"timeout", "text/cq-codex-proxy-trace/06-timeout-records-already-emitted.txt", "text/cq-codex-proxy-trace/07-timeout-records-already-emitted.txt", "cq: Trace reading timed out.\n", 7},
		{"interrupted", "text/cq-codex-proxy-trace/08-interrupted-records-already-emitted.txt", "text/cq-codex-proxy-trace/09-interrupted-records-already-emitted.txt", "cq: Trace following interrupted.\n", 130},
	} {
		for _, width := range []int{0, 20, 40, 80, 100, 120} {
			for _, colour := range []bool{false, true} {
				t.Run(tc.name+"/"+fmt.Sprint(width, colour), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "trace")
					if tc.name != "failure" {
						if err := os.WriteFile(path, []byte(protocolTraceRecord()), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					var out, diagnostic bytes.Buffer
					o := protocolOptions(width, colour)
					session := &cli.Session{Out: &out, Err: &diagnostic, OutTerminal: o, ErrTerminal: o}
					deps := traceTestDependencies(path, time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC))
					if tc.name == "failure" {
						deps.Trace.LoadConfig = func() (*proxy.Config, error) { return &proxy.Config{}, nil }
					}
					args := []string{"codex", "proxy", "trace"}
					if tc.name == "gap" || tc.name == "timeout" || tc.name == "interrupted" {
						args = append(args, "--follow", "--timeout", "1h")
					}
					lookup := diagnosticsTestLookup(deps)
					if tc.name == "gap" || tc.name == "timeout" || tc.name == "interrupted" {
						lookup = func(pathName string) (cli.Handler, bool) {
							_, ok := lookupV2RoutingDiagnostics(pathName)
							return func(parent context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
								ctx, cancel := context.WithCancel(parent)
								defer cancel()
								var work context.Context = ctx
								if tc.name == "timeout" {
									work = diagnosticsDeadlineContext{ctx}
								}
								copy := *s
								copy.Out = &diagnosticsHookWriter{Writer: s.Out, after: func() {
									if tc.name == "gap" {
										for i := 0; i < 5; i++ {
											rotateProxyTraceTestLog(t, path, protocolTraceRecord())
										}
									} else {
										cancel()
									}
								}}
								return handleV2RoutingDiagnosticsWithDependencies(work, inv, &copy, deps)
							}, ok
						}
					}
					exit := cli.Run(context.Background(), args, session, lookup)
					if exit != tc.exit {
						t.Fatalf("exit %d want %d; stdout=%q stderr=%q", exit, tc.exit, &out, &diagnostic)
					}
					assertProtocolStreams(t, out.String(), diagnostic.String(), width, colour, tc.outFile, tc.errFile, tc.plainError)
				})
			}
		}
	}
}

func TestTerminalHookRawFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, outFile, errFile, plainError string
		exit                               int
	}{
		{"empty", "text/cq-codex-proxy-hook-stop/02-success-no-receipt.txt", "", "", 0},
		{"invalid", "", "text/cq-codex-proxy-hook-stop/03-failure.txt", "cq: Invalid Codex Stop hook input.\n", 2},
		{"deadline", "", "text/cq-codex-proxy-hook-stop/04-failure-lookup-deadline.txt", "cq: The Codex turn receipt lookup timed out.\n", 7},
		{"receipt", "text/cq-codex-proxy-hook-stop/05-success-retained-receipt-protocol.txt", "", "", 0},
	} {
		for _, width := range []int{0, 20, 40, 80, 100, 120} {
			for _, colour := range []bool{false, true} {
				t.Run(tc.name+"/"+fmt.Sprint(width, colour), func(t *testing.T) {
					var out, diagnostic bytes.Buffer
					o := protocolOptions(width, colour)
					input := v2HookEvent("session-one", "turn-one")
					if tc.name == "invalid" {
						input = "{"
					}
					session := &cli.Session{In: strings.NewReader(input), Out: &out, Err: &diagnostic, OutTerminal: o, ErrTerminal: o}
					lookup := func(path string) (cli.Handler, bool) {
						_, ok := lookupV2Hook(path)
						return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
							return handleV2HookWithPreparation(ctx, inv, s, func(context.Context) (proxyCodexHookDependencies, error) {
								return proxyCodexHookDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
									if tc.name == "deadline" {
										return nil, context.DeadlineExceeded
									}
									lookup := proxy.CodexTurnReceiptLookupV2{SchemaVersion: 2}
									if tc.name == "receipt" {
										lookup = v2HookReceipt("")
										lookup.Receipt.Transport = proxy.CodexTurnReceiptTransportWebSocket
									}
									data, _ := json.Marshal(lookup)
									return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
								})}, nil
							})
						}, ok
					}
					exit := cli.Run(context.Background(), []string{"codex", "proxy", "hook", "stop"}, session, lookup)
					if exit != tc.exit {
						t.Fatalf("exit %d want %d; stdout=%q stderr=%q", exit, tc.exit, &out, &diagnostic)
					}
					assertProtocolStreams(t, out.String(), diagnostic.String(), width, colour, tc.outFile, tc.errFile, tc.plainError)
				})
			}
		}
	}
}
