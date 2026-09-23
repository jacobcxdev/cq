package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
)

var terminalServiceFixtures = []terminalFixtureRegistration{
	{"cq service install", "Success", "stdout", "text/cq-service-install/02-success.txt"},
	{"cq service install", "Failure", "stderr", "text/cq-service-install/03-failure.txt"},
	{"cq service install", "Partial · proxy started, refresh failed", "stdout", "text/cq-service-install/04-partial-proxy-started-refresh-failed.txt"},
	{"cq service install", "Partial · proxy started, refresh failed", "stderr", "text/cq-service-install/05-partial-proxy-started-refresh-failed.txt"},
	{"cq service start", "Success", "stdout", "text/cq-service-start/02-success.txt"},
	{"cq service start", "Failure", "stderr", "text/cq-service-start/03-failure.txt"},
	{"cq service start", "Timeout · selected verification incomplete", "stderr", "text/cq-service-start/04-timeout-selected-verification-incomplete.txt"},
	{"cq service restart", "Success", "stdout", "text/cq-service-restart/02-success.txt"},
	{"cq service restart", "Failure", "stderr", "text/cq-service-restart/03-failure.txt"},
	{"cq service restart", "Timeout · restart completion unknown", "stderr", "text/cq-service-restart/04-timeout-restart-completion-unknown.txt"},
	{"cq service status", "Success", "stdout", "text/cq-service-status/02-success.txt"},
	{"cq service status", "Failure", "stderr", "text/cq-service-status/03-failure.txt"},
	{"cq service status", "Success · component absent", "stdout", "text/cq-service-status/04-success-component-absent.txt"},
	{"cq service status", "Failure · strict absence", "stdout", "text/cq-service-status/05-failure-strict-absence.txt"},
	{"cq service status", "Failure · strict absence", "stderr", "text/cq-service-status/06-failure-strict-absence.txt"},
	{"cq service stop", "Success", "stdout", "text/cq-service-stop/02-success.txt"},
	{"cq service stop", "Failure", "stderr", "text/cq-service-stop/03-failure.txt"},
	{"cq service stop", "Timeout · stop completion unknown", "stderr", "text/cq-service-stop/04-timeout-stop-completion-unknown.txt"},
	{"cq service uninstall", "Success", "stdout", "text/cq-service-uninstall/02-success.txt"},
	{"cq service uninstall", "Failure", "stderr", "text/cq-service-uninstall/03-failure.txt"},
}

func TestTerminalServiceRegistration(t *testing.T) {
	path := filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", "index.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []terminalFixtureRegistration `json:"fixtures"`
	}
	if err := json.Unmarshal(contents, &index); err != nil {
		t.Fatal(err)
	}
	want := map[string]terminalFixtureRegistration{}
	for _, item := range index.Fixtures {
		if strings.HasPrefix(item.Command, "cq service ") && !strings.HasSuffix(item.File, "/01-help.txt") {
			want[item.File] = item
		}
	}
	if len(want) != len(terminalServiceFixtures) {
		t.Fatalf("registered %d of %d service fixtures", len(terminalServiceFixtures), len(want))
	}
	for _, item := range terminalServiceFixtures {
		if want[item.File] != item {
			t.Fatalf("fixture not registered exactly: %+v", item)
		}
	}
}

func terminalServiceFixtureComponent(id serviceSelection, now time.Time) v2ServiceComponent {
	exe := "/opt/homebrew/bin/cq"
	manager := "launchd"
	config := "/Users/alex/Library/Application Support/cq"
	cache := "/Users/alex/Library/Caches/cq"
	runtime := config + "/runtime"
	logs := "/Users/alex/Library/Logs/cq"
	last := now.UTC().Format(time.RFC3339Nano)
	zero := 0
	c := v2ServiceComponent{ID: id, Manager: &manager, Owner: "cq", Installed: true, Enabled: serviceBool(true), State: "running", Healthy: serviceBool(true), Executable: &exe, LastRunAt: &last, ErrorCode: nil, ConfigDir: &config, StateDir: &config, CacheDir: &cache, RuntimeDir: &runtime, LogDir: &logs}
	if id == serviceProxy {
		pid := 4217
		c.PID = &pid
	} else {
		c.State = "idle"
		last = now.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
		c.LastRunAt = &last
		c.LastExitCode = &zero
	}
	return c
}

func terminalServiceFixtureOutcome(f terminalFixtureRegistration, now time.Time) cli.Outcome {
	action := serviceAction(strings.TrimPrefix(f.Command, "cq service "))
	data := v2ServiceData{Action: action, Component: serviceAll, Rollback: "not_needed"}
	proxy := terminalServiceFixtureComponent(serviceProxy, now)
	refresh := terminalServiceFixtureComponent(serviceRefresh, now)
	out := cli.Outcome{}
	switch f.Scenario {
	case "Success":
		switch action {
		case serviceStart:
			data.Component, data.Components = serviceProxy, []v2ServiceComponent{proxy}
		case serviceRestart:
			data.Component = serviceRefresh
			refresh.Enabled, refresh.Healthy = serviceBool(false), serviceBool(false)
			last := now.UTC().Format(time.RFC3339Nano)
			refresh.LastRunAt = &last
			data.Components = []v2ServiceComponent{refresh}
		case serviceStop:
			proxy.State, refresh.State = "stopped", "stopped"
			proxy.PID = nil
			proxy.Enabled, refresh.Enabled = serviceBool(false), serviceBool(false)
			proxy.Healthy, refresh.Healthy = serviceBool(false), serviceBool(false)
			zero := 0
			proxy.LastExitCode = &zero
			data.Components = []v2ServiceComponent{proxy, refresh}
		case serviceUninstall:
			data.Component = serviceProxy
			data.Components = []v2ServiceComponent{terminalServiceAbsent(serviceProxy)}
		default:
			data.Components = []v2ServiceComponent{proxy, refresh}
		}
	case "Partial · proxy started, refresh failed":
		data.Rollback = "failed"
		proxy.State = "running"
		last := now.UTC().Format(time.RFC3339Nano)
		refresh.LastRunAt = &last
		refresh.State, refresh.Healthy = "failed", serviceBool(false)
		one := 1
		refresh.LastExitCode = &one
		errorCode := "service_unhealthy"
		refresh.ErrorCode = &errorCode
		data.Components = []v2ServiceComponent{proxy, refresh}
		out = v2SelectionFailure(8, "service_partial", "Service operation was only partially completed; inspect component results.")
	case "Success · component absent", "Failure · strict absence":
		data.Component = serviceRefresh
		data.Components = []v2ServiceComponent{terminalServiceAbsent(serviceRefresh)}
		if f.Scenario == "Failure · strict absence" {
			out = v2SelectionFailure(3, "service_not_installed", "Service component token-refresh is not installed.")
		}
	case "Failure":
		switch action {
		case serviceInstall, serviceStop, serviceUninstall:
			out = v2SelectionFailure(6, "service_owner_conflict", "Service ownership conflicts with this operation.")
		case serviceStart:
			out = v2SelectionFailure(3, "service_not_installed", "Service component proxy is not installed.")
		case serviceRestart:
			out = v2SelectionFailure(1, "service_unhealthy", "Service component token-refresh is unhealthy.")
		case serviceInspect:
			out = v2SelectionFailure(4, "service_unavailable", "The current-user service manager is unavailable.")
		}
	case "Timeout · selected verification incomplete", "Timeout · restart completion unknown", "Timeout · stop completion unknown":
		out = v2SelectionFailure(7, "service_"+string(action)+"_timeout", "Operation timed out; inspect state before retrying.")
	}
	out.Terminal = func(o cli.TerminalOptions) string { return terminalService(data, out.ExitCode, o) }
	return out
}

func terminalServiceAbsent(id serviceSelection) v2ServiceComponent {
	manager := "launchd"
	return v2ServiceComponent{ID: id, Manager: &manager, Owner: "none", Installed: false, Enabled: serviceBool(false), State: "absent", Healthy: serviceBool(false)}
}

func TestTerminalServiceFixtures(t *testing.T) {
	for _, f := range terminalServiceFixtures {
		for _, width := range []int{20, 40, 80, 100, 120} {
			t.Run(fmt.Sprintf("%s/%d", f.File, width), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				o := terminalFixtureOptions(width)
				args := strings.Fields(strings.TrimPrefix(f.Command, "cq "))
				exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, func(string) (cli.Handler, bool) {
					return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome {
						return terminalServiceFixtureOutcome(f, o.Now)
					}, true
				})
				wantExit := terminalServiceFixtureOutcome(f, o.Now).ExitCode
				if exit != wantExit {
					t.Fatalf("exit %d want %d", exit, wantExit)
				}
				got := stdout.String()
				if f.Stream == "stderr" {
					got = stderr.String()
				}
				if width >= 100 {
					assertTerminalFixture(t, f.File, got)
				}
				for _, line := range strings.Split(got, "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Fatalf("overflow at %d: %q", width, line)
					}
				}
			})
		}
	}
}

func TestTerminalServiceSemanticRolesAndNoColour(t *testing.T) {
	o := terminalFixtureOptions(100)
	data := v2ServiceData{Action: serviceInstall, Component: serviceAll, Rollback: "failed", Components: []v2ServiceComponent{terminalServiceFixtureComponent(serviceProxy, o.Now), terminalServiceFixtureComponent(serviceRefresh, o.Now)}}
	data.Components[1].State = "failed"
	data.Components[1].Healthy = serviceBool(false)
	got := terminalService(data, 8, o)
	for _, want := range []string{"\x1b[1mCQ services", "\x1b[1;34m4217", "\x1b[35m0s ago", "\x1b[32myes", "\x1b[31mfailed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing semantic role %q", want)
		}
	}
	o.Colour = false
	if strings.Contains(terminalService(data, 8, o), "\x1b[") {
		t.Fatal("NO_COLOR output contains ANSI")
	}
}

func TestTerminalServiceUnobservedComponentStaysUnknown(t *testing.T) {
	manager := "launchd"
	data := v2ServiceData{Action: serviceStart, Component: serviceAll, Rollback: "failed", Components: []v2ServiceComponent{
		terminalServiceFixtureComponent(serviceProxy, terminalFixtureOptions(100).Now),
		{ID: serviceRefresh, Manager: &manager, Owner: "none", State: "indeterminate"},
	}}
	got := ansi.Strip(terminalService(data, 8, terminalFixtureOptions(100)))
	if !strings.Contains(got, "Installed   unknown") || !strings.Contains(got, "Owner       unknown") || !strings.Contains(got, "token-refresh  indeterminate") {
		t.Fatalf("unobserved component was presented as certain:\n%s", got)
	}
}

func TestTerminalServiceAdapterFakeManager(t *testing.T) {
	for _, tc := range []struct {
		name, action, selection, fail string
		exit                          int
		want, forbid                  []string
	}{
		{"status", "status", "all", "", 0, []string{"CQ services · status", "proxy", "token-refresh", "Last run"}, nil},
		{"already satisfied", "start", "proxy", "", 0, []string{"CQ services · start", "running"}, []string{"token-refresh"}},
		{"partial", "stop", "all", "token-refresh-stop", 8, []string{"CQ services · stop · incomplete", "proxy", "token-refresh", "Rollback · failed"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			if tc.fail != "" {
				p.fail, p.restoreFails = tc.fail, true
			}
			var stdout, stderr bytes.Buffer
			o := terminalFixtureOptions(100)
			exit := cli.Run(context.Background(), []string{"service", tc.action, "--component", tc.selection}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, func(string) (cli.Handler, bool) {
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					return handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return l, nil })
				}, true
			})
			if exit != tc.exit {
				t.Fatalf("exit %d want %d stderr %s", exit, tc.exit, stderr.String())
			}
			for _, value := range tc.want {
				if !strings.Contains(ansi.Strip(stdout.String()), value) {
					t.Errorf("missing %q in %s", value, stdout.String())
				}
			}
			for _, value := range tc.forbid {
				if strings.Contains(stdout.String(), value) {
					t.Errorf("unexpected %q", value)
				}
			}
			if tc.name == "already satisfied" {
				for _, call := range p.calls {
					if strings.Contains(call, "-start") || strings.Contains(call, "token-refresh") {
						t.Fatalf("unexpected manager call %q", call)
					}
				}
			}
			if tc.name == "partial" && !strings.Contains(stderr.String(), "service_partial") {
				t.Fatalf("missing partial diagnostic: %s", stderr.String())
			}
		})
	}
}

func TestTerminalServiceAbsentNoOpClaimsOnlyFinalState(t *testing.T) {
	for _, tc := range []struct {
		action serviceAction
		want   string
	}{
		{serviceStop, "Selected services stopped or absent; automatic starts disabled where registered."},
		{serviceUninstall, "Selected registrations absent; credentials and user data retained."},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			l, p, store := newSelectedServiceHarness(t)
			store.exists = false
			for _, id := range serviceAll.components() {
				c := p.statuses[id]
				c.Registered, c.Running = false, false
				c.Observed.Enabled = serviceBool(false)
				p.statuses[id] = c
			}
			var stdout, stderr bytes.Buffer
			exit := cli.Run(context.Background(), []string{"service", string(tc.action)}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(100), ErrTerminal: terminalFixtureOptions(100)}, func(string) (cli.Handler, bool) {
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					return handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return l, nil })
				}, true
			})
			if exit != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
			}
			got := ansi.Strip(stdout.String())
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "proxy          absent") || !strings.Contains(got, "token-refresh  absent") {
				t.Fatalf("no-op state not presented accurately:\n%s", got)
			}
			for _, call := range p.calls {
				if strings.Contains(call, "-stop") || strings.Contains(call, "-uninstall") {
					t.Fatalf("unexpected manager mutation %q", call)
				}
			}
		})
	}
}

func TestTerminalServiceUnavailableAndPlainJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := terminalFixtureOptions(100)
	lookup := func(string) (cli.Handler, bool) {
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return nil, errServiceUnavailable })
		}, true
	}
	if exit := cli.Run(context.Background(), []string{"service", "status"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup); exit != 4 {
		t.Fatalf("unavailable exit %d", exit)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "service_unavailable") {
		t.Fatalf("unavailable stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	l, _, _ := newSelectedServiceHarness(t)
	lookup = func(string) (cli.Handler, bool) {
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return l, nil })
		}, true
	}
	stdout.Reset()
	stderr.Reset()
	if exit := cli.Run(context.Background(), []string{"service", "status", "--component", "proxy"}, &cli.Session{Out: &stdout, Err: &stderr}, lookup); exit != 0 {
		t.Fatalf("plain exit %d", exit)
	}
	if !strings.HasPrefix(stdout.String(), "CQ services (proxy): status\n") {
		t.Fatalf("plain output changed: %q", stdout.String())
	}
	stdout.Reset()
	if exit := cli.Run(context.Background(), []string{"service", "status", "--component", "proxy", "--json"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o}, lookup); exit != 0 {
		t.Fatalf("JSON exit %d", exit)
	}
	var envelope struct {
		Data v2ServiceData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || envelope.Data.Component != serviceProxy || len(envelope.Data.Components) != 1 {
		t.Fatalf("JSON result changed: %v %+v", err, envelope.Data)
	}
}
