package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestTerminalProxyHealthFixture(t *testing.T) {
	status := 200
	data := v2ProxyHealth{Reachable: true, Healthy: true, HTTPStatus: &status, Address: "127.0.0.1:19280", DurationMS: 12}
	assertTerminalFixture(t, "text/cq-proxy-health/02-success.txt", terminalProxyHealth(data, cli.TerminalOptions{Width: 100}))
}

func terminalRuntimeStatusFixture() v2ProxyStatus {
	str := func(s string) *string { return &s }
	pid := 4217
	return v2ProxyStatus{
		State: "ready", Scope: "live", StateDir: str("/Users/alex/cq-runtime"), CollectedAt: "2026-09-23T15:30:00Z", DurationMS: 24,
		Facts: []v2ProxyFact{
			{Name: "inspector", State: proxy.FactKnown, Value: v2ProxyInspectorValue{str("/opt/homebrew/bin/cq"), str("1.0.0"), str("1234567890abcdef1234567890abcdef12345678"), str(strings.Repeat("1", 64))}},
			{Name: "desired", State: proxy.FactKnown, Value: v2ProxyDesiredValue{str("homebrew"), true, str("127.0.0.1:19280")}},
			{Name: "service", State: proxy.FactKnown, Value: v2ProxyServiceValue{str("homebrew"), str("running"), str("/opt/homebrew/bin/cq"), &pid}},
			{Name: "listener", State: proxy.FactKnown, Value: v2ProxyListenerValue{str("listening"), str("127.0.0.1:19280"), str("/opt/homebrew/bin/cq"), &pid}},
			{Name: "process", State: proxy.FactKnown, Value: v2ProxyProcessValue{pid, "/opt/homebrew/bin/cq"}},
			{Name: "runtime", State: proxy.FactKnown, Value: v2ProxyRuntimeValue{true, &pid, str("/opt/homebrew/bin/cq"), str("healthy")}},
			{Name: "data_plane", State: proxy.FactKnown, Value: v2ProxyDataPlaneValue{false, str("unproven")}},
		},
	}
}

func terminalRuntimeFixtureOutput(file string, o cli.TerminalOptions) string {
	switch file {
	case "text/cq-proxy-health/02-success.txt":
		status := 200
		return terminalProxyHealth(v2ProxyHealth{true, true, &status, "127.0.0.1:19280", 12}, o)
	case "text/cq-proxy-health/03-failure.txt":
		return terminalProxyHealth(v2ProxyHealth{Address: "127.0.0.1:19280", DurationMS: 5}, o)
	case "text/cq-proxy-health/05-failure-reachable-but-unhealthy.txt":
		status := 503
		return terminalProxyHealth(v2ProxyHealth{true, false, &status, "127.0.0.1:19280", 14}, o)
	case "text/cq-proxy-operation-status/02-success.txt":
		id, phase, digest := "0123456789abcdef0123456789abcdef", "terminal", strings.Repeat("a", 64)
		return terminalProxyOperation(v2OperationStatus{OperationID: &id, State: "result_available", Phase: &phase, ResultAvailable: true, ResultDigest: &digest}, o)
	case "text/cq-proxy-operation-status/04-success-idle-coordinator.txt":
		return terminalProxyOperation(v2OperationStatus{State: "idle"}, o)
	case "text/cq-proxy-rescue-enter/02-success-transition-acknowledged-drain-pending.txt":
		return terminalProxyRescue(v2RescueState{"rescue_draining", 12, 0, []string{"session:123456789abc", "session:abcdef123456"}}, o)
	case "text/cq-proxy-rescue-exit/02-success-transition-acknowledged-drain-pending.txt":
		return terminalProxyRescue(v2RescueState{"rescue_exit_draining", 13, 1, []string{"session:abcdef123456"}}, o)
	case "text/cq-proxy-rescue-status/02-success-normal.txt":
		return terminalProxyRescue(v2RescueState{"normal", 14, 0, []string{}}, o)
	case "text/cq-proxy-serve/02-success-foreground-run-ended-by-sigterm.txt", "text/cq-proxy-serve/04-interruption-completed-shutdown-after-ctrl-c.txt":
		reason := "interrupted"
		ready := v2ProxyEvent{Event: "ready", ListenAddress: "127.0.0.1:19280", PID: 4217, Providers: []string{"claude", "codex"}}
		stopped := v2ProxyEvent{Event: "stopped", Reason: &reason}
		return terminalProxyServeReady(ready, o) + terminalProxyServeStopped(stopped, o)
	case "text/cq-proxy-state-initialise/02-success.txt":
		return terminalProxyState(v2ProxyState{StateDir: "/Users/alex/cq-runtime", Created: true, RestartRequired: true}, false, o)
	case "text/cq-proxy-state-initialise/04-success-safely-reused-state.txt":
		return terminalProxyState(v2ProxyState{StateDir: "/Users/alex/cq-runtime"}, false, o)
	case "text/cq-proxy-state-initialise/05-partial-creation-committed-later-binding-failed.txt", "text/cq-proxy-state-initialise/07-interruption-preserve-known-creation.txt":
		return terminalProxyState(v2ProxyState{StateDir: "/Users/alex/cq-runtime", Created: true, RestartRequired: true}, true, o)
	case "text/cq-proxy-status/02-success.txt":
		return terminalProxyStatus(terminalRuntimeStatusFixture(), o)
	default:
		return ""
	}
}

func TestTerminalRuntimeManifestAndFixtures(t *testing.T) {
	manifestBytes, err := os.ReadFile(filepath.Join("..", "..", "docs", "superpowers", "plans", "terminal-presentation-coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	indexBytes, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Fixtures []struct{ Command, Scenario, Stream, File, Task string }
	}
	var index struct {
		Fixtures []struct{ Command, Scenario, Stream, File string }
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	indexed := make(map[string]terminalFixtureRegistration)
	for _, f := range index.Fixtures {
		indexed[f.File] = terminalFixtureRegistration{f.Command, f.Scenario, f.Stream, f.File}
	}
	registered := 0
	commands := map[string]bool{}
	for _, f := range manifest.Fixtures {
		if f.Task != "T10" {
			continue
		}
		registered++
		commands[f.Command] = true
		if indexed[f.File] != (terminalFixtureRegistration{f.Command, f.Scenario, f.Stream, f.File}) {
			t.Errorf("index mismatch for %s", f.File)
		}
		for _, width := range []int{20, 40, 80, 100, 120} {
			t.Run(fmt.Sprintf("%s/%d", f.File, width), func(t *testing.T) {
				o := terminalFixtureOptions(width)
				got := terminalRuntimeFixtureOutput(f.File, o)
				if f.Stream == "stderr" {
					// Shared diagnostics own these exact authored records.
					got = terminalRuntimeDiagnosticFixture(f.File, o)
				}
				if got == "" {
					t.Fatalf("unregistered fixture %s", f.File)
				}
				if width == 100 {
					assertTerminalFixture(t, f.File, got)
				}
				for _, line := range strings.Split(strings.TrimSuffix(ansi.Strip(got), "\n"), "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Errorf("width %d: %q", width, line)
					}
				}
				plain := terminalRuntimeFixtureOutput(f.File, cli.TerminalOptions{Width: width, Now: o.Now, Location: o.Location})
				if f.Stream == "stderr" {
					plain = terminalRuntimeDiagnosticFixture(f.File, cli.TerminalOptions{Width: width, Now: o.Now, Location: o.Location})
				}
				if ansi.Strip(got) != plain || strings.Contains(plain, "\x1b[") {
					t.Errorf("NO_COLOR changed text for %s", f.File)
				}
			})
		}
	}
	if registered != 30 || len(commands) != 8 {
		t.Fatalf("registered %d fixtures across %d commands", registered, len(commands))
	}
}

func terminalRuntimeDiagnosticFixture(file string, o cli.TerminalOptions) string {
	code, message, command := "", "", ""
	switch {
	case strings.Contains(file, "health/04-"):
		code, message, command = "proxy_unreachable", "Proxy is not reachable at 127.0.0.1:19280.", "proxy health"
	case strings.Contains(file, "health/06-"):
		code, message, command = "proxy_health_failed", "Proxy HTTP health check failed.", "proxy health"
	case strings.Contains(file, "operation-status/"):
		code, message, command = "operation_not_found", "The requested operation does not exist.", "proxy operation status"
	case strings.Contains(file, "rescue-enter/03-"):
		code, message, command = "rescue_transition_conflict", "The proxy cannot perform this rescue transition.", "proxy rescue enter"
	case strings.Contains(file, "rescue-enter/04-"):
		code, message, command = "rescue_timeout", "The proxy rescue operation timed out; inspect status before retrying.", "proxy rescue enter"
	case strings.Contains(file, "rescue-exit/03-"):
		code, message, command = "rescue_transition_conflict", "The proxy cannot perform this rescue transition.", "proxy rescue exit"
	case strings.Contains(file, "rescue-exit/04-"):
		code, message, command = "rescue_timeout", "The proxy rescue operation timed out; inspect status before retrying.", "proxy rescue exit"
	case strings.Contains(file, "rescue-status/"):
		code, message, command = "rescue_unavailable", "Proxy rescue control is unavailable.", "proxy rescue status"
	case strings.Contains(file, "serve/03-"):
		code, message, command = "proxy_port_in_use", "Port 19280 is already in use.", "proxy serve"
	case strings.Contains(file, "serve/05-"):
		code, message, command = "interrupted", "Operation interrupted; inspect state before retrying.", "proxy serve"
	case strings.Contains(file, "state-initialise/03-"):
		code, message, command = "proxy_state_conflict", "Proxy state ownership conflicts with the requested directory.", "proxy state initialise"
	case strings.Contains(file, "state-initialise/06-"):
		code, message, command = "proxy_state_initialise_partial", "Proxy state initialisation partially completed; inspect state before retrying.", "proxy state initialise"
	case strings.Contains(file, "state-initialise/08-"):
		code, message, command = "interrupted", "Operation interrupted; inspect state before retrying.", "proxy state initialise"
	case strings.Contains(file, "status/03-"):
		code, message, command = "proxy_indeterminate", "Proxy readiness could not be determined.", "proxy status"
	case strings.Contains(file, "status/04-"):
		code, message, command = "proxy_status_timeout", "Operation timed out; inspect state before retrying.", "proxy status"
	}
	if code == "" {
		return ""
	}
	var stdout, stderr bytes.Buffer
	args := strings.Fields(command)
	if command == "proxy state initialise" {
		args = append(args, "--state-dir", "/Users/alex/cq-runtime")
	}
	if command == "proxy serve" {
		args = append(args, "--port", "19280")
	}
	if command == "proxy operation status" {
		args = append(args, strings.Repeat("a", 32))
	}
	if strings.HasPrefix(command, "proxy rescue") && code == "rescue_timeout" {
		args = append(args, "--timeout", "1s")
	}
	cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, func(string) (cli.Handler, bool) {
		return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome {
			return v2SelectionFailure(1, code, message)
		}, true
	})
	return stderr.String()
}

func TestTerminalRuntimeRolesAndStates(t *testing.T) {
	o := terminalFixtureOptions(100)
	checks := []struct{ text, role string }{
		{terminalProxyHealth(v2ProxyHealth{Healthy: true, Address: "127.0.0.1:19280"}, o), "\x1b[32mhealthy"},
		{terminalProxyHealth(v2ProxyHealth{Address: "127.0.0.1:19280"}, o), "\x1b[31mfailed"},
		{terminalProxyOperation(v2OperationStatus{State: "pending"}, o), "\x1b[33mpending"},
		{terminalProxyOperation(v2OperationStatus{State: "result_available"}, o), "\x1b[36mresult_available"},
		{terminalProxyRescue(v2RescueState{Mode: "normal", Generation: 14}, o), "\x1b[1;34m0"},
		{terminalProxyStatus(v2ProxyStatus{State: "stopped", Scope: "live", CollectedAt: o.Now.Format(time.RFC3339)}, o), "\x1b[36mstopped"},
	}
	for _, check := range checks {
		if !strings.Contains(check.text, check.role) {
			t.Errorf("missing semantic role %q in %q", check.role, check.text)
		}
	}
	if strings.Contains(terminalProxyStatus(v2ProxyStatus{State: "stopped", Scope: "live", CollectedAt: o.Now.Format(time.RFC3339)}, o), "\x1b[31m") {
		t.Fatal("normal stopped runtime is not failure")
	}
	if got := terminalProxyRescue(v2RescueState{Mode: "normal", Generation: ^uint64(0)}, o); !strings.Contains(ansi.Strip(got), strconv.FormatUint(^uint64(0), 10)) {
		t.Fatal("generation rounded")
	}
}

func TestTerminalRescueGenerationIdentifier(t *testing.T) {
	o := terminalFixtureOptions(100)
	got := terminalProxyRescue(v2RescueState{
		Mode: "normal", Generation: 1234567890, ActiveRescueRequests: 1234,
	}, o)
	if !strings.Contains(got, "\x1b[36m1234567890\x1b[0m") {
		t.Errorf("generation should be an exact cyan identifier: %q", got)
	}
	if strings.Contains(ansi.Strip(got), "1,234,567,890") {
		t.Errorf("generation should not be grouped: %q", got)
	}
	if !strings.Contains(got, "\x1b[1;34m1,234\x1b[0m") {
		t.Errorf("active requests should remain a grouped blue count: %q", got)
	}
}

func TestTerminalProxyAdaptersAndPlainCompatibility(t *testing.T) {
	requests := 0
	health := v2ProxyDependencies{Client: &http.Client{Transport: v2ProxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/health" {
			t.Fatalf("request path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
	})}}
	lookup := func(deps v2ProxyDependencies) cli.Lookup {
		return func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Proxy(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ProxyWithDependencies(ctx, inv, s, deps)
			}, ok
		}
	}
	run := func(args []string, deps v2ProxyDependencies, outOptions, errOptions cli.TerminalOptions) (int, string, string) {
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: outOptions, ErrTerminal: errOptions}, lookup(deps))
		return exit, stdout.String(), stderr.String()
	}
	o := terminalFixtureOptions(100)
	exit, out, diagnostic := run([]string{"proxy", "health", "--port", "19280"}, health, o, o)
	if exit != 0 || diagnostic != "" || !strings.Contains(ansi.Strip(out), "Proxy health · healthy") || !strings.Contains(out, "\x1b[32mhealthy") || requests != 1 {
		t.Fatalf("health %d %q %q calls=%d", exit, out, diagnostic, requests)
	}
	exit, out, diagnostic = run([]string{"proxy", "health", "--port", "19280"}, health, cli.TerminalOptions{}, cli.TerminalOptions{})
	if exit != 0 || diagnostic != "" || !strings.HasPrefix(out, "Proxy HTTP health: ok\n") || strings.Contains(out, "\x1b[") || requests != 2 {
		t.Fatalf("plain health %d %q %q calls=%d", exit, out, diagnostic, requests)
	}
	exit, out, diagnostic = run([]string{"proxy", "health", "--port", "19280", "--json"}, health, o, o)
	if exit != 0 || diagnostic != "" || !json.Valid([]byte(out)) || strings.Contains(out, "\x1b[") || requests != 3 {
		t.Fatalf("JSON health %d %q %q calls=%d", exit, out, diagnostic, requests)
	}
	statusCalls := 0
	stopped := v2ProxyDependencies{Inspect: func(context.Context, string) proxy.ProxySnapshot {
		statusCalls++
		s := absentV2ProxySnapshot()
		s.CollectedAt = o.Now.UTC().Format(time.RFC3339)
		s.Desired = proxy.KnownFact(proxy.DesiredProxyState{Configured: true})
		s.Service = proxy.KnownFact(proxy.ServiceState{Manager: "manual", State: "stopped"})
		return s
	}}
	exit, out, diagnostic = run([]string{"proxy", "status"}, stopped, o, o)
	if exit != 0 || diagnostic != "" || !strings.Contains(ansi.Strip(out), "Proxy · stopped") || strings.Contains(out, "\x1b[31mstopped") || statusCalls != 1 {
		t.Fatalf("stopped %d %q %q calls=%d", exit, out, diagnostic, statusCalls)
	}
	stateCalls := 0
	state := v2ProxyDependencies{LoadConfig: func() (*proxy.Config, error) { return nil, os.ErrNotExist }, Initialise: func(context.Context, string, *proxy.Config) (v2ProxyState, error) {
		stateCalls++
		return v2ProxyState{StateDir: "/Users/alex/cq-runtime", Created: true, RestartRequired: true}, errors.New("after durable creation")
	}}
	exit, out, diagnostic = run([]string{"proxy", "state", "initialise", "--state-dir", "/Users/alex/cq-runtime"}, state, o, cli.TerminalOptions{})
	if exit != 8 || !strings.Contains(ansi.Strip(out), "Proxy state · created · incomplete") || !strings.Contains(diagnostic, "cq: Proxy state initialisation partially completed") || stateCalls != 1 {
		t.Fatalf("partial %d %q %q calls=%d", exit, out, diagnostic, stateCalls)
	}
}

func TestTerminalOperationAndRescueAdapters(t *testing.T) {
	o := terminalFixtureOptions(100)
	operationCalls := 0
	id, digest := strings.Repeat("a", 32), strings.Repeat("b", 64)
	deps := v2RescueDependencies{Inspect: func(context.Context, string) (proxy.OperationCoordinatorInspectionV1, error) {
		operationCalls++
		return proxy.OperationCoordinatorInspectionV1{Found: true, OperationID: id, Phase: "terminal", ValueDigest: digest, Receipt: true}, nil
	}}
	var stdout, stderr bytes.Buffer
	exit := cli.Run(context.Background(), []string{"proxy", "operation", "status"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, v2RescueLookup(deps))
	if exit != 0 || stderr.Len() != 0 || !strings.Contains(ansi.Strip(stdout.String()), "Proxy operation · result_available") || !strings.Contains(stdout.String(), "\x1b[36mterminal") || operationCalls != 1 {
		t.Fatalf("operation %d %q %q calls=%d", exit, stdout.String(), stderr.String(), operationCalls)
	}
	for _, tc := range []struct {
		action, mode string
		generation   uint64
	}{{"enter", "rescue_draining", 12}, {"exit", "rescue_exit_draining", 13}, {"status", "normal", 14}} {
		t.Run(tc.action, func(t *testing.T) {
			body := fmt.Sprintf(`{"mode":%q,"generation":%d,"active_rescue_requests":0,"draining_sessions":[]}`, tc.mode, tc.generation)
			stdout.Reset()
			stderr.Reset()
			exit := cli.Run(context.Background(), []string{"proxy", "rescue", tc.action}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, v2RescueLookup(v2RescueReply(body)))
			if exit != 0 || stderr.Len() != 0 || !strings.Contains(ansi.Strip(stdout.String()), "Proxy rescue · "+tc.mode) || !strings.Contains(ansi.Strip(stdout.String()), "No draining sessions.") {
				t.Fatalf("rescue %d %q %q", exit, stdout.String(), stderr.String())
			}
		})
	}
}

func TestTerminalProxyServeStreamsAtLifecyclePoints(t *testing.T) {
	o := terminalFixtureOptions(100)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var stdout, stderr bytes.Buffer
	closed := false
	deps := v2ProxyDependencies{Serve: func(_ context.Context, _ proxyCommandOptions, ready func(string, []string) error) error {
		if err := ready("127.0.0.1:19280", []string{"claude", "codex"}); err != nil {
			return err
		}
		if closed || !strings.Contains(ansi.Strip(stdout.String()), "Proxy · listening") || strings.Contains(ansi.Strip(stdout.String()), "Proxy · stopped") {
			t.Fatal("ready emitted at wrong lifecycle point")
		}
		closed = true
		cancel(context.Canceled)
		return nil
	}}
	exit := cli.Run(ctx, []string{"proxy", "serve"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, func(path string) (cli.Handler, bool) {
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ProxyWithDependencies(ctx, inv, s, deps)
		}, path == "proxy serve"
	})
	text := ansi.Strip(stdout.String())
	if exit != 130 || !closed || !strings.Contains(text, "Proxy · stopped") || strings.Index(text, "Proxy · stopped") < strings.Index(text, "Ctrl-C stops") || !strings.Contains(stderr.String(), "interrupted") {
		t.Fatalf("serve %d %q %q", exit, text, stderr.String())
	}
	if strings.Contains(stdout.String(), "\x1b[31m") {
		t.Fatal("normal stopped block styled as failure")
	}
}
