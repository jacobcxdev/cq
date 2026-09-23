package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func init() {
	registerV2Fixture("canary-missing", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		deps := v2CanaryDependencies{fs: fsutil.OSFileSystem{}, path: filepath.Join(t.TempDir(), "absent", "state.json")}
		for _, kind := range []proxy.CodexCanaryProtectionKind{proxy.CodexCanarySystemAuth, proxy.CodexCanaryRegistry, proxy.CodexCanaryCQManagedAuth, proxy.CodexCanaryCodexBarManifest, proxy.CodexCanaryCodexBarAuth, proxy.CodexCanaryRoutingDefault} {
			deps.protected = append(deps.protected, proxy.CodexCanaryOptionalSnapshotProtection(kind, func() ([]byte, error) { return []byte("synthetic"), nil }))
		}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Canary(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2CanaryWithDependencies(ctx, inv, s, deps)
			}, ok
		}
		return f
	})
}
func TestCLIV2CanaryContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "missing canary is explicit", Scenario: "canary-missing", Args: []string{"codex", "proxy", "canary", "status", "--json"}, Exit: 3, Command: "codex proxy canary status", Code: "canary_missing", Forbid: []string{"service", "consume", "filesystem-write"}})
}

func newV2CanaryFixture(t *testing.T) (v2CanaryDependencies, *[]byte) {
	t.Helper()
	value := []byte("cq-fixture-secret-never-output")
	deps := v2CanaryDependencies{fs: fsutil.NewMemFS(), path: "/state/canary.json", now: func() time.Time { return time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC) }, loadConfig: func() (*proxy.Config, error) { return &proxy.Config{CodexTurnRouting: proxy.CodexRoutingEnforce}, nil }, currentTuple: func() (proxy.CodexCanaryTuple, error) { return v2CanaryTestTuple(), nil }}
	for _, kind := range []proxy.CodexCanaryProtectionKind{proxy.CodexCanarySystemAuth, proxy.CodexCanaryRegistry, proxy.CodexCanaryCQManagedAuth, proxy.CodexCanaryCodexBarManifest, proxy.CodexCanaryCodexBarAuth, proxy.CodexCanaryRoutingDefault} {
		deps.protected = append(deps.protected, proxy.CodexCanaryOptionalSnapshotProtection(kind, func() ([]byte, error) { return value, nil }))
	}
	return deps, &value
}
func v2CanaryTestTuple() proxy.CodexCanaryTuple {
	return proxy.CodexCanaryTuple{CQBuild: "1.0.0", ClientBuild: "1.2.3", ParserSchema: 1, LeaseSchema: 3, SemanticsRevision: "http-conservative-routing-v3", RetryBudget: 1, FixtureHash: "618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1", ReadinessFingerprint: strings.Repeat("a", 64)}
}
func callV2Canary(t *testing.T, deps v2CanaryDependencies, action string) cli.Outcome {
	t.Helper()
	out := handleV2CanaryWithDependencies(context.Background(), cli.Invocation{Path: "codex proxy canary " + action}, nil, deps)
	if bytes.Contains(out.Data, []byte("cq-fixture-secret-never-output")) {
		t.Fatal("secret leaked")
	}
	return out
}
func TestCLIV2CanaryLifecycle(t *testing.T) {
	deps, _ := newV2CanaryFixture(t)
	start := callV2Canary(t, deps, "start")
	if start.ExitCode != 0 {
		t.Fatalf("start: %+v", start)
	}
	var data struct {
		State v2CanaryState `json:"state"`
	}
	if err := json.Unmarshal(start.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.State.RunID == "" || !data.State.Active || data.State.Finalised || data.State.EndedAt != nil || data.State.LastObservedAt != nil {
		t.Fatalf("start data: %s", start.Data)
	}
	if string(start.Data) != string(callV2Canary(t, deps, "status").Data) {
		t.Fatal("status differs from start")
	}
	if got := callV2Canary(t, deps, "start"); got.ExitCode != 6 {
		t.Fatalf("active start: %+v", got)
	}
	before, _ := deps.fs.ReadFile(deps.path)
	stop := callV2Canary(t, deps, "stop")
	if stop.ExitCode != 0 || string(stop.Data) != string(start.Data) {
		t.Fatalf("stop claimed completion: %+v", stop)
	}
	requestPath := "/state/codex-canary-stop/request.json"
	request, _ := deps.fs.ReadFile(requestPath)
	if len(request) == 0 {
		t.Fatal("stop intent absent")
	}
	repeated := callV2Canary(t, deps, "stop")
	after, _ := deps.fs.ReadFile(deps.path)
	requestAfter, _ := deps.fs.ReadFile(requestPath)
	if repeated.ExitCode != 0 || !bytes.Equal(request, requestAfter) || !bytes.Equal(before, after) {
		t.Fatal("repeated stop changed request or canary")
	}
}
func TestCLIV2CanaryProtectedDriftAndUnavailable(t *testing.T) {
	for _, action := range []string{"start", "status", "stop"} {
		t.Run(action, func(t *testing.T) {
			deps, value := newV2CanaryFixture(t)
			if got := callV2Canary(t, deps, "start"); got.ExitCode != 0 {
				t.Fatal(got)
			}
			before, _ := deps.fs.ReadFile(deps.path)
			*value = []byte("drift")
			if got := callV2Canary(t, deps, action); got.ExitCode != 6 {
				t.Fatalf("drift: %+v", got)
			}
			after, _ := deps.fs.ReadFile(deps.path)
			if !bytes.Equal(before, after) {
				t.Fatal("drift modified state")
			}
			deps.protected[0] = proxy.CodexCanaryOptionalSnapshotProtection(proxy.CodexCanarySystemAuth, func() ([]byte, error) { return nil, errors.New("source unreadable") })
			if got := callV2Canary(t, deps, action); got.ExitCode != 1 {
				t.Fatalf("unavailable: %+v", got)
			}
		})
	}
}
func TestCLIV2CanaryStartPreconditions(t *testing.T) {
	for _, name := range []string{"enforcement", "payload", "readiness", "config"} {
		t.Run(name, func(t *testing.T) {
			deps, _ := newV2CanaryFixture(t)
			want := 6
			switch name {
			case "enforcement":
				deps.loadConfig = func() (*proxy.Config, error) { return &proxy.Config{}, nil }
			case "payload":
				deps.loadConfig = func() (*proxy.Config, error) {
					return &proxy.Config{CodexTurnRouting: proxy.CodexRoutingEnforce, PayloadDiagnosticsLog: "private"}, nil
				}
			case "readiness":
				deps.currentTuple = func() (proxy.CodexCanaryTuple, error) { return proxy.CodexCanaryTuple{}, errors.New("stale") }
			case "config":
				deps.loadConfig = func() (*proxy.Config, error) { return nil, os.ErrNotExist }
				want = 1
			}
			if got := callV2Canary(t, deps, "start"); got.ExitCode != want {
				t.Fatalf("outcome: %+v", got)
			}
			if _, err := deps.fs.Stat(deps.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("precondition wrote canary")
			}
		})
	}
}
func TestCLIV2CanaryProjection(t *testing.T) {
	var state proxy.CodexCanaryState
	// Projection does not equate inactive with finalised. Stored-state acceptance
	// continues to use the frozen finalisation validator.
	out := projectV2Canary(state)
	if out.Active || out.Finalised {
		t.Fatal("inactive invented finalisation")
	}
	if err := json.Unmarshal([]byte(`{"finalisation":{"stop_request_digest":"a","process_binding_digest":"b","counters_digest":"c","active_sessions":0}}`), &state); err != nil {
		t.Fatal(err)
	}
	if !projectV2Canary(state).Finalised {
		t.Fatal("retained finalisation omitted")
	}
	deps, _ := newV2CanaryFixture(t)
	got := callV2Canary(t, deps, "start")
	var top map[string]json.RawMessage
	_ = json.Unmarshal(got.Data, &top)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(top["state"], &fields)
	if len(fields) != 17 || fields["version"] != nil || string(fields["finalisation"]) != "null" {
		t.Fatalf("resource fields: %s", got.Data)
	}
	var data struct {
		State v2CanaryState `json:"state"`
	}
	_ = json.Unmarshal(got.Data, &data)
	if !sort.SliceIsSorted(data.State.ProtectedDigests, func(i, j int) bool { return data.State.ProtectedDigests[i].Kind < data.State.ProtectedDigests[j].Kind }) {
		t.Fatal("digests not sorted")
	}
}

// These fixtures are signed with a synthetic canary key produced by Start;
// production finalisation remains exclusively owned by the serving process.
func setV2CanaryFixtureFinalised(t *testing.T, deps v2CanaryDependencies, finalised bool) {
	t.Helper()
	var envelope struct {
		Version    int                    `json:"version"`
		Generation uint64                 `json:"generation"`
		State      proxy.CodexCanaryState `json:"state"`
		MAC        string                 `json:"mac"`
	}
	data, err := deps.fs.ReadFile(deps.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.State.Active = false
	envelope.State.EndedAt = deps.now().Add(time.Minute)
	if finalised {
		counters := []byte(`{"version":2,"admitted_turns":0,"keyed_mismatches":0,"automatic_protected_state_changes":0,"secret_leaks":0,"unexplained_lifecycles":0,"live_session_repairs":0,"protected_state_failures":0,"consecutive_calendar_days":0}`)
		digest := sha256.Sum256(append([]byte("cq-codex-canary-counters-v2\x00"), counters...))
		fixture := fmt.Sprintf(`{"finalisation":{"stop_request_digest":%q,"process_binding_digest":%q,"counters_digest":%q,"active_sessions":0}}`, strings.Repeat("b", 64), strings.Repeat("c", 64), hex.EncodeToString(digest[:]))
		if err = json.Unmarshal([]byte(fixture), &envelope.State); err != nil {
			t.Fatal(err)
		}
	}
	envelope.MAC = ""
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	key, err := deps.fs.ReadFile(deps.path + ".key")
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("cq-codex-canary-state-v2\x00"))
	mac.Write(body)
	envelope.MAC = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	data, err = json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = deps.fs.WriteFile(deps.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
func TestCLIV2CanaryStoppedAndUnfinalised(t *testing.T) {
	for _, finalised := range []bool{false, true} {
		t.Run(fmt.Sprint(finalised), func(t *testing.T) {
			deps, _ := newV2CanaryFixture(t)
			start := callV2Canary(t, deps, "start")
			if start.ExitCode != 0 {
				t.Fatal(start)
			}
			setV2CanaryFixtureFinalised(t, deps, finalised)
			before, _ := deps.fs.ReadFile(deps.path)
			for _, action := range []string{"status", "stop"} {
				got := callV2Canary(t, deps, action)
				want := 1
				if finalised {
					want = 0
				}
				if got.ExitCode != want {
					t.Fatalf("%s finalised=%t: %+v", action, finalised, got)
				}
				if finalised {
					var data struct {
						State v2CanaryState `json:"state"`
					}
					_ = json.Unmarshal(got.Data, &data)
					if data.State.Active || !data.State.Finalised || data.State.EndedAt == nil {
						t.Fatalf("final state: %s", got.Data)
					}
				}
			}
			after, _ := deps.fs.ReadFile(deps.path)
			if !bytes.Equal(before, after) {
				t.Fatal("inspection/repeated final stop changed record")
			}
			if _, err := deps.fs.Stat("/state/codex-canary-stop/request.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("final stop wrote request")
			}
			restarted := callV2Canary(t, deps, "start")
			if finalised {
				if restarted.ExitCode != 0 || bytes.Equal(start.Data, restarted.Data) {
					t.Fatal("completed run was not replaced")
				}
			} else if restarted.ExitCode != 1 {
				t.Fatal("invalid final state replaced")
			}
		})
	}
}
func TestCLIV2CanaryMissingKeyIsUnavailable(t *testing.T) {
	deps, _ := newV2CanaryFixture(t)
	if got := callV2Canary(t, deps, "start"); got.ExitCode != 0 {
		t.Fatal(got)
	}
	if err := deps.fs.Remove(deps.path + ".key"); err != nil {
		t.Fatal(err)
	}
	if got := callV2Canary(t, deps, "status"); got.ExitCode != 1 {
		t.Fatalf("missing key relabelled missing run: %+v", got)
	}
}
func TestCLIV2CanaryAndHookPureParsing(t *testing.T) {
	for _, path := range []string{"codex proxy canary start", "codex proxy canary status", "codex proxy canary stop", "codex proxy hook stop", "codex proxy canary", "codex proxy hook"} {
		for _, suffix := range [][]string{{"--help"}, {"--json", "--help"}, {"--bad"}, {"extra"}, {"--json", "-j"}, {"--json=invalid"}} {
			t.Run(path+strings.Join(suffix, " "), func(t *testing.T) {
				var out, diagnostic bytes.Buffer
				exit := cli.Run(context.Background(), append(strings.Fields(path), suffix...), &cli.Session{In: noAccessV2Input{}, Out: &out, Err: &diagnostic}, func(string) (cli.Handler, bool) { t.Fatal("pure parser accessed handler"); return nil, false })
				if suffix[len(suffix)-1] == "--help" {
					want, err := os.ReadFile(filepath.Join("../../specs/cli-v2/help", strings.ReplaceAll(path, " ", "-")+".txt"))
					if err != nil {
						t.Fatal(err)
					}
					if exit != 0 || out.String() != string(want) {
						t.Fatal("exact help mismatch")
					}
				} else if exit != 2 {
					t.Fatalf("invalid argv exit=%d", exit)
				}
			})
		}
	}
}

func TestCLIV2CanaryStopInflightAndInvalidIntent(t *testing.T) {
	for _, kind := range []string{"inflight", "expired", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			deps, _ := newV2CanaryFixture(t)
			if got := callV2Canary(t, deps, "start"); got.ExitCode != 0 {
				t.Fatal(got)
			}
			if got := callV2Canary(t, deps, "stop"); got.ExitCode != 0 {
				t.Fatal(got)
			}
			request := "/state/codex-canary-stop/request.json"
			inflight := "/state/codex-canary-stop/inflight.json"
			want := 0
			switch kind {
			case "inflight":
				if err := deps.fs.Rename(request, inflight); err != nil {
					t.Fatal(err)
				}
			case "expired":
				now := deps.now()
				deps.now = func() time.Time { return now.Add(10 * time.Minute) }
				want = 1
			case "corrupt":
				if err := deps.fs.WriteFile(request, []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = 1
			}
			got := callV2Canary(t, deps, "stop")
			if got.ExitCode != want {
				t.Fatalf("%s stop: %+v", kind, got)
			}
			if kind == "inflight" {
				if _, err := deps.fs.Stat(request); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("repeated inflight stop published another request")
				}
			}
		})
	}
}

func TestCLIV2CanaryProductionMissingUsesResolvedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix root overrides only; native Windows folders use their separate test seams")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", "relative-invalid-unused-cache")
	var out, diagnostic bytes.Buffer
	exit := cli.Run(context.Background(), []string{"codex", "proxy", "canary", "status", "--json"}, &cli.Session{Out: &out, Err: &diagnostic}, lookupV2Canary)
	if exit != 3 {
		t.Fatalf("exit=%d output=%s", exit, out.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("missing canary read created directories")
	}
}
