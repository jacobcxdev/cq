package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

type v2ReserveInventory struct {
	active codex.AccountKey
	err    error
}

func (i *v2ReserveInventory) List(context.Context) (codex.Inventory, error) {
	return codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "system", Active: i.active == "system"}, {Key: "other", Active: i.active == "other"}}}, i.err
}

type v2ReserveFS struct {
	*fsutil.MemFS
	fixture *v2Fixture
}

func (fs *v2ReserveFS) MkdirAll(path string, mode os.FileMode) error {
	fs.fixture.Call("filesystem-write")
	return fs.MemFS.MkdirAll(path, mode)
}

func init() {
	for _, scenario := range []string{"reserve-stale-disable", "reserve-equality", "reserve-unconfigured"} {
		registerV2Fixture(scenario, func(t *testing.T) *v2Fixture { return newV2ReserveFixture(t, scenario) })
	}
}

type v2ReserveRig struct {
	fixture   *v2Fixture
	reserve   *proxy.CodexReserve
	ledger    *proxy.CodexCapacityLedger
	inventory *v2ReserveInventory
	now       *time.Time
}

func newV2ReserveFixture(t *testing.T, scenario string) *v2Fixture {
	return newV2ReserveRig(t, scenario).fixture
}

func newV2ReserveRig(t *testing.T, scenario string) *v2ReserveRig {
	t.Helper()
	f := &v2Fixture{Secrets: []string{"synthetic-reserve-token"}}
	now := time.Unix(1800000000, 0)
	fs := &v2ReserveFS{MemFS: fsutil.NewMemFS(), fixture: f}
	ledger := proxy.NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	inventory := &v2ReserveInventory{active: "system"}
	reserve, err := proxy.OpenCodexReserve(fs, "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []codex.AccountKey{"system", "other"} {
		ledger.ObserveQuotaSnapshot(key, proxy.QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 2, ResetAtUnix: now.Add(time.Hour).Unix()}, "3h:custom-model": {RemainingPct: 25}}}})
	}
	if scenario != "reserve-unconfigured" {
		if _, err := reserve.Control("set", "7d", 2); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == "reserve-stale-disable" {
		now = now.Add(time.Minute)
	}
	f.calls = map[string]int{}
	handler, err := (&proxy.Server{Config: &proxy.Config{ClaudeUpstream: "https://example.test"}, Reserve: reserve}).RuntimeHandler()
	if err != nil {
		t.Fatal(err)
	}
	deps := proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) {
		f.Call("config-read")
		return &proxy.Config{LocalToken: "synthetic-reserve-token"}, nil
	}, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
		f.Call("control")
		if r.URL.Host != "127.0.0.1:19280" || r.Header.Get("Authorization") != "Bearer synthetic-reserve-token" {
			t.Fatal("wrong authority")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response.Result(), nil
	})}
	f.Lookup = func(path string) (cli.Handler, bool) {
		if _, ok := lookupV2Reserve(path); !ok {
			return nil, false
		}
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ReserveWithDependencies(ctx, inv, s, deps)
		}, true
	}
	return &v2ReserveRig{f, reserve, ledger, inventory, &now}
}
func TestCLIV2ReserveContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "disable requires fresh reset evidence", Scenario: "reserve-stale-disable", Args: []string{"codex", "proxy", "reserve", "disable", "--json"}, Exit: 6, Command: "codex proxy reserve disable", Code: "reserve_evidence_required", Forbid: []string{"filesystem-write", "consume", "credential-activate"}})
}

func TestCLIV2ReserveLeavesAndAliases(t *testing.T) {
	windows := `[{"selector":"3h:custom-model","remaining_pct":25,"remaining_pct_exact":null,"reset_at":null},{"selector":"7d","remaining_pct":2,"remaining_pct_exact":null,"reset_at":"2027-01-15T09:00:00Z"}]`
	for _, tc := range []struct {
		action, fields string
		writes         int
	}{
		{"status", `"configured":true,"window":"7d","percent":2,"account_key":"system","email":null,"enabled":true,"blocked":true,"reason":"reserve_reached","remaining_pct":2,"reset_at":"2027-01-15T09:00:00Z","observed_at":"2027-01-15T08:00:00Z"`, 0},
		{"set", `"configured":true,"window":"7d","percent":2,"enabled":true,"blocked":true,"reason":"reserve_reached"`, 1},
		{"disable", `"configured":true,"window":"7d","percent":2,"enabled":false,"blocked":false,"reason":"disabled_until_reset"`, 1},
		{"enable", `"configured":true,"window":"7d","percent":2,"enabled":true,"blocked":true,"reason":"reserve_reached"`, 1},
		{"clear", `"configured":false,"window":null,"percent":null,"account_key":"system","email":null,"enabled":false,"blocked":false,"reason":null,"remaining_pct":null,"reset_at":null,"observed_at":null`, 1},
		{"windows", "", 0},
	} {
		for _, prefix := range []string{"codex proxy reserve", "proxy reserve"} {
			args := append(strings.Fields(prefix), tc.action)
			if tc.action == "set" {
				args = append(args, "--window", " 7D ", "--percent", "2")
			}
			args = append(args, "--json")
			want := `{"reserve":{` + tc.fields + `,"windows":` + windows + `}}`
			if tc.action == "windows" {
				want = `{"windows":` + windows + `}`
			}
			runV2Case(t, v2Case{Name: prefix + " " + tc.action, Scenario: "reserve-equality", Args: args, Command: "codex proxy reserve " + tc.action, WantJSON: want, Calls: map[string]int{"control": 1, "config-read": 1, "filesystem-write": tc.writes}, Forbid: []string{"service", "consume", "credential-activate"}})
		}
	}
	for _, action := range []string{"enable", "disable"} {
		runV2Case(t, v2Case{Name: "unconfigured " + action, Scenario: "reserve-unconfigured", Args: []string{"codex", "proxy", "reserve", action, "--json"}, Command: "codex proxy reserve " + action, Exit: 6, Code: "reserve_not_configured", Forbid: []string{"filesystem-write"}})
	}
	runV2Case(t, v2Case{Name: "unavailable observed window", Scenario: "reserve-equality", Args: []string{"codex", "proxy", "reserve", "set", "--window", "5h", "--percent", "2", "--json"}, Command: "codex proxy reserve set", Exit: 6, Code: "reserve_window_unavailable", Forbid: []string{"filesystem-write"}})
	runV2Case(t, v2Case{Name: "custom canonical window", Scenario: "reserve-equality", Args: []string{"codex", "proxy", "reserve", "set", "--window", " 3H:CUSTOM_MODEL ", "--percent", "0.5", "--json"}, Command: "codex proxy reserve set", WantJSON: `{"reserve":{"window":"3h:custom-model","percent":0.5,"blocked":true,"reason":"usage_stale","reset_at":null}}`, Calls: map[string]int{"filesystem-write": 1}})
}

// Exercise the CLI transport against the real service engine across one lifecycle.
func TestCLIV2ReserveLifecycle(t *testing.T) {
	rig := newV2ReserveRig(t, "reserve-equality")
	run := func(args ...string) v2ReserveStatus {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), append(append([]string{"codex", "proxy", "reserve"}, args...), "--json"), &cli.Session{Out: &stdout, Err: &stderr}, rig.fixture.Lookup)
		if exit != 0 {
			t.Fatalf("exit=%d output=%s error=%s", exit, &stdout, &stderr)
		}
		var envelope struct {
			Data struct {
				Reserve v2ReserveStatus `json:"reserve"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data.Reserve
	}
	if blocked, _ := rig.reserve.Blocked("other"); blocked {
		t.Fatal("equal non-system balance was protected")
	}
	if state := run("status"); !state.Blocked || *state.RemainingPct != 2 {
		t.Fatal("full-window threshold equality lost")
	}
	if state := run("disable"); state.Enabled || state.Blocked {
		t.Fatal("verified temporary bypass not applied")
	}
	*rig.now = rig.now.Add(2 * time.Hour)
	if state := run("status"); state.Enabled || !state.Blocked || *state.Reason != "usage_stale" {
		t.Fatal("expired bypass became optimistically available")
	}
	// A failed service refresh leaves the old ledger evidence intact.
	if state := run("status"); !state.Blocked {
		t.Fatal("missing refresh unblocked account")
	}
	rig.ledger.ObserveQuotaSnapshot("system", proxy.QuotaSnapshot{FetchedAt: *rig.now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 100, ResetAtUnix: rig.now.Add(7 * 24 * time.Hour).Unix()}}}})
	if state := run("status"); !state.Enabled || state.Blocked || state.Reason != nil {
		t.Fatal("verified reset did not rearm")
	}
	run("disable")
	rig.inventory.active = "other"
	if state := run("status"); !state.Enabled || *state.AccountKey != "other" || !state.Blocked {
		t.Fatal("system replacement did not rearm")
	}
	if blocked, _ := rig.reserve.Blocked("system"); blocked {
		t.Fatal("former system account stayed protected")
	}
	rig.inventory.err = errors.New("inventory unavailable")
	if state := run("status"); !state.Blocked || *state.Reason != "system_account_unavailable" || *state.AccountKey != "other" {
		t.Fatal("identity failure lost fail-closed evidence")
	}
	rig.inventory.err = nil
	first, second := run("clear"), run("clear")
	if first.Configured || !reflect.DeepEqual(first, second) {
		t.Fatal("clear is not idempotent")
	}
}

func TestCLIV2ReserveValidationBeforeAccess(t *testing.T) {
	for _, tail := range [][]string{
		{"set"}, {"set", "--window", "7d"}, {"set", "--percent", "2"},
		{"set", "--window", "7d", "--percent", "0"}, {"set", "--window", "7d", "--percent", "100"}, {"set", "--window", "7d", "--percent", "NaN"}, {"set", "--window", "7d", "--percent", "Inf"},
		{"status", "--port", "0"}, {"status", "--port", "65536"}, {"status", "--port", "1", "--port", "2"},
		{"status", "--timeout", "0s"}, {"status", "--timeout", "oops"}, {"status", "--timeout", "1s", "--timeout", "2s"},
		{"disable", "--window", "7d"}, {"status", "--percent", "2"}, {"set", "--window", "7d", "--percent", "2", "--account", "system"}, {"clear", "unexpected"},
	} {
		runV2Case(t, v2Case{Name: strings.Join(tail, " "), Scenario: "no-access", Args: append(append([]string{"codex", "proxy", "reserve"}, tail...), "--json"), Command: "codex proxy reserve " + tail[0], Exit: 2, Code: "routing_invalid_argument", Forbid: []string{"lookup"}})
	}
}

func TestCLIV2ReserveExactHumanAndResourceShape(t *testing.T) {
	for _, tc := range []struct{ scenario, action, want string }{
		{"reserve-equality", "status", "System account reserve: true\nAccount: unavailable\nWindow: 7d\nThreshold: 2%\nEnabled: true\nBlocked: true\nReason: reserve_reached\nRemaining: 2\nReset: 2027-01-15T09:00:00Z\nObserved: 2027-01-15T08:00:00Z\n"},
		{"reserve-unconfigured", "status", "System account reserve: false\nAccount: unavailable\nWindow: none\nThreshold: unknown%\nEnabled: false\nBlocked: false\nReason: none\nRemaining: unknown\nReset: unknown\nObserved: unknown\n"},
		{"reserve-equality", "windows", "3h:custom-model\n7d\n"},
	} {
		f := newV2ReserveFixture(t, tc.scenario)
		var stdout, stderr bytes.Buffer
		if exit := cli.Run(context.Background(), []string{"codex", "proxy", "reserve", tc.action}, &cli.Session{Out: &stdout, Err: &stderr}, f.Lookup); exit != 0 || stdout.String() != tc.want || stderr.Len() != 0 {
			t.Fatalf("human exit=%d stdout=%q stderr=%q", exit, &stdout, &stderr)
		}
	}
	inv, _ := cli.Parse([]string{"codex", "proxy", "reserve", "windows"})
	deps := proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"windows":{}}`))}, nil
	})}
	out := handleV2ReserveWithDependencies(context.Background(), inv, nil, deps)
	if out.Human != "No system-account quota windows available.\n" || string(out.Data) != `{"windows":[]}` {
		t.Fatalf("empty windows=%+v", out)
	}
	inv, _ = cli.Parse([]string{"codex", "proxy", "reserve", "status"})
	out = handleV2ReserveWithDependencies(context.Background(), inv, nil, deps)
	want := `{"reserve":{"configured":false,"window":null,"percent":null,"account_key":null,"email":null,"enabled":false,"blocked":false,"reason":null,"remaining_pct":null,"reset_at":null,"observed_at":null,"windows":[]}}`
	if string(out.Data) != want {
		t.Fatalf("nullable resource=%s", out.Data)
	}
}

func TestCLIV2ReserveControlFailures(t *testing.T) {
	for _, tc := range []struct {
		name, receipt, body, code, message string
		status, exit                       int
	}{
		{"missing receipt", "", "reserve control rejected\n", "routing_conflict", "Routing state conflict: reserve control rejected.", 409, 6},
		{"unknown receipt", "private-secret", "private-secret", "routing_conflict", "Routing state conflict: reserve control rejected.", 409, 6},
		{"not configured", "reserve_not_configured", "", "reserve_not_configured", "Reserve is not configured; use reserve set first.", 409, 6},
		{"evidence", "reserve_evidence_required", "", "reserve_evidence_required", "Fresh usage and reset evidence are required to disable the reserve.", 409, 6},
		{"window", "reserve_window_unavailable", "", "reserve_window_unavailable", "Selected window is unavailable; inspect reserve windows.", 409, 6},
		{"save", "routing_io_failed", "private-secret", "routing_io_failed", "Routing operation failed: persist reserve state; inspect current state before retrying.", 409, 1},
		{"invalid domain", "routing_invalid_argument", "", "routing_invalid_argument", "Invalid argument: reserve control.", 409, 2},
		{"bad request", "", "", "routing_invalid_argument", "Invalid argument: reserve control.", 400, 2},
		{"authentication", "reserve_evidence_required", "private-secret", "routing_auth_failed", "Local proxy authentication failed.", 401, 5},
		{"forbidden", "", "", "routing_auth_failed", "Local proxy authentication failed.", 403, 5},
		{"unavailable", "reserve_not_configured", "", "routing_control_unavailable", "Running CQ proxy control is unavailable.", 503, 4},
		{"invalid response", "", "private-secret", "routing_io_failed", "Routing operation failed: reserve control.", 200, 1},
		{"bounded response", "", strings.Repeat("x", (1<<20)+1), "routing_io_failed", "Routing operation failed: reserve control.", 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			inv, _ := cli.Parse([]string{"codex", "proxy", "reserve", "disable"})
			out := handleV2ReserveWithDependencies(context.Background(), inv, nil, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "private-secret"}, nil }, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost {
					t.Fatal("racy preflight request")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"X-Cq-Reserve-Error": []string{tc.receipt}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})})
			if out.ExitCode != tc.exit || len(out.Errors) != 1 || out.Errors[0].Code != tc.code || out.Errors[0].Message != tc.message || calls != 1 {
				t.Fatalf("outcome=%+v calls=%d", out, calls)
			}
		})
	}
	for _, tc := range []struct {
		name string
		cfg  *proxy.Config
		err  error
		code string
		exit int
	}{
		{"missing config", nil, os.ErrNotExist, "routing_io_failed", 1},
		{"unreadable config", nil, errors.New("private-secret"), "routing_io_failed", 1},
		{"no token", &proxy.Config{}, nil, "routing_auth_failed", 5},
		{"nil config", nil, nil, "routing_auth_failed", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, _ := cli.Parse([]string{"codex", "proxy", "reserve", "status"})
			out := handleV2ReserveWithDependencies(context.Background(), inv, nil, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return tc.cfg, tc.err }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid config contacted control")
				return nil, nil
			})})
			if out.ExitCode != tc.exit || out.Errors[0].Code != tc.code || strings.Contains(out.Errors[0].Message, "private-secret") {
				t.Fatalf("outcome=%+v", out)
			}
		})
	}
}

func TestCLIV2ReserveBudgetAndPorts(t *testing.T) {
	for _, tc := range []struct{ configured, explicit, want int }{{0, 0, 19280}, {12345, 0, 12345}, {12345, 23456, 23456}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			args := []string{"codex", "proxy", "reserve", "status"}
			if tc.explicit != 0 {
				args = append(args, "--port", fmt.Sprint(tc.explicit))
			}
			inv, _ := cli.Parse(args)
			out := handleV2ReserveWithDependencies(context.Background(), inv, nil, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic", Port: tc.configured}, nil }, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != fmt.Sprintf("127.0.0.1:%d", tc.want) {
					t.Fatal("incorrect resolved port")
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 10*time.Second || time.Until(deadline) < 9*time.Second {
					t.Fatal("default budget was not applied")
				}
				return nil, errors.New("private transport failure")
			})})
			if out.ExitCode != 4 || out.Errors[0].Code != "routing_control_unavailable" {
				t.Fatalf("outcome=%+v", out)
			}
		})
	}
	inv, _ := cli.Parse([]string{"codex", "proxy", "reserve", "clear", "--timeout", "1ms"})
	for _, phase := range []string{"preparation", "config", "transport"} {
		t.Run(phase, func(t *testing.T) {
			reads, requests := 0, 0
			out := handleV2ReserveWithPreparation(context.Background(), inv, nil, func(ctx context.Context) (proxyPolicyDependencies, error) {
				if phase == "preparation" {
					<-ctx.Done()
					return proxyPolicyDependencies{}, errors.New("private error")
				}
				return proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) {
					reads++
					if phase == "config" {
						<-ctx.Done()
					}
					return &proxy.Config{LocalToken: "synthetic"}, nil
				}, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
					requests++
					<-r.Context().Done()
					return nil, r.Context().Err()
				})}, nil
			})
			if out.ExitCode != 7 || out.Errors[0].Code != "routing_timeout" || phase == "preparation" && reads != 0 || phase != "transport" && requests != 0 {
				t.Fatalf("outcome=%+v reads=%d requests=%d", out, reads, requests)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := handleV2ReserveWithPreparation(ctx, inv, nil, func(context.Context) (proxyPolicyDependencies, error) {
		t.Fatal("cancelled command prepared dependencies")
		return proxyPolicyDependencies{}, nil
	})
	if out.ExitCode != 130 || out.Errors[0].Code != "interrupted" {
		t.Fatalf("cancelled=%+v", out)
	}
}

func TestCLIV2ReserveProductionDoesNotBootstrap(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	for _, action := range []string{"status", "windows", "clear", "enable", "disable", "set"} {
		args := []string{"codex", "proxy", "reserve", action}
		if action == "set" {
			args = append(args, "--window", "7d", "--percent", "2")
		}
		inv, err := cli.Parse(args)
		if err != nil {
			t.Fatal(err)
		}
		out := handleV2Reserve(context.Background(), inv, nil)
		if out.ExitCode != 1 || out.Errors[0].Code != "routing_io_failed" {
			t.Fatalf("%s outcome=%+v", action, out)
		}
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("bootstrapped authority: entries=%v err=%v", entries, err)
	}
}

func TestCLIV2ReserveFractionalAndMissingResetEvidence(t *testing.T) {
	for _, missingReset := range []bool{false, true} {
		rig := newV2ReserveRig(t, "reserve-equality")
		*rig.now = rig.now.Add(time.Second + 123*time.Nanosecond)
		remaining := 2.1
		reset := rig.now.Add(time.Hour).Unix()
		if missingReset {
			reset = 0
		}
		rig.ledger.ObserveQuotaSnapshot("system", proxy.QuotaSnapshot{FetchedAt: *rig.now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 2, RemainingPctExact: &remaining, ResetAtUnix: reset}}}})
		action := "status"
		if missingReset {
			action = "disable"
		}
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), []string{"codex", "proxy", "reserve", action, "--json"}, &cli.Session{Out: &stdout, Err: &stderr}, rig.fixture.Lookup)
		if missingReset {
			if err := validateV2Envelope(stdout.Bytes(), v2Case{Command: "codex proxy reserve disable", Exit: 6, Code: "reserve_evidence_required"}); err != nil || exit != 6 {
				t.Fatalf("missing reset exit=%d err=%v", exit, err)
			}
			if rig.fixture.calls["filesystem-write"] != 0 {
				t.Fatal("missing reset persisted bypass")
			}
		} else {
			if exit != 0 {
				t.Fatalf("status exit=%d", exit)
			}
			var envelope struct {
				Data struct {
					Reserve v2ReserveStatus `json:"reserve"`
				} `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			s := envelope.Data.Reserve
			if s.Blocked || s.RemainingPct == nil || *s.RemainingPct != 2.1 || s.ObservedAt == nil || *s.ObservedAt != "2027-01-15T08:00:01.000000123Z" || *s.Windows[1].RemainingPctExact != 2.1 {
				t.Fatalf("fractional resource=%+v", s)
			}
		}
	}
}

func TestCLIV2ReserveProductionRejectsRedirect(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	port, err := strconv.Atoi(strings.TrimPrefix(source.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(base, "config", "cq")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(proxy.Config{LocalToken: "synthetic-redirect-secret", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "proxy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	inv, _ := cli.Parse([]string{"codex", "proxy", "reserve", "status"})
	out := handleV2Reserve(context.Background(), inv, nil)
	if out.ExitCode != 4 || out.Errors[0].Code != "routing_control_unavailable" || targetCalls.Load() != 0 {
		t.Fatalf("redirect outcome=%+v target calls=%d", out, targetCalls.Load())
	}
}
