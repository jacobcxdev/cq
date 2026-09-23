package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type modelHTTPFunc func(*http.Request) (*http.Response, error)

func (f modelHTTPFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type modelWriteFS struct {
	fsutil.FileSystem
	fail string
}

func (f *modelWriteFS) WriteFile(path string, b []byte, m os.FileMode) error {
	if f.fail != "" && strings.Contains(path, f.fail) {
		return errors.New("synthetic write denied")
	}
	return f.FileSystem.WriteFile(path, b, m)
}

type modelTestFixture struct {
	fixture     *v2Fixture
	deps        v2ModelsDependencies
	state       modelsDeps
	fs          *modelWriteFS
	pipeline    *registryPipeline
	claudeFail  bool
	codexFail   bool
	extraNative bool
}

func newModelTestFixture(t *testing.T) *modelTestFixture {
	t.Helper()
	home := t.TempDir()
	fs := &modelWriteFS{FileSystem: fsutil.OSFileSystem{}}
	m := &modelTestFixture{fixture: &v2Fixture{}, fs: fs, state: modelsDeps{FS: fs, HomeDir: home, CWD: home, Roots: userdirs.Roots{Config: filepath.Join(home, "cq")}, Env: func(string) string { return "" }}}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".codex/models_cache.json"), `{"models":[{"slug":"native-example","display_name":"Native example","description":"native description","context_window":1000,"max_context_window":2000,"priority":-1}]}`)
	write(filepath.Join(home, ".claude/cache/model-capabilities.json"), `{"models":[{"id":"claude-example","max_input_tokens":1000,"max_tokens":400}]}`)
	write(filepath.Join(home, ".claude.json"), `{"unrelated":true}`)
	client := modelHTTPFunc(func(req *http.Request) (*http.Response, error) {
		m.fixture.Call("network")
		if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Error("missing source phase deadline")
		}
		body := `{"models":[{"slug":"native-example","display_name":"Fresh native","context_window":1000,"max_context_window":2000,"max_output_tokens":500}]}`
		fail := m.codexFail
		if strings.Contains(req.URL.Host, "claude") {
			body = `{"data":[{"id":"claude-example","display_name":"Claude","context_window":1000,"max_output_tokens":400}],"has_more":false}`
			fail = m.claudeFail
		}
		if m.extraNative && !strings.Contains(req.URL.Host, "claude") {
			body = `{"models":[{"slug":"native-example"},{"slug":"local-example"}]}`
		}
		if fail {
			return nil, errors.New("synthetic source unavailable")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	m.deps = v2ModelsDependencies{Resolve: func() (modelsDeps, error) { m.fixture.Call("resolve"); return m.state, nil }, Refresh: func(ctx context.Context, state modelsDeps) (ModelPublication, []ModelIdentity, error) {
		m.fixture.Call("refresh")
		p, err := newRegistryPipeline(registryPipelineOptions{FS: state.FS, HomeDir: state.HomeDir, CWD: state.CWD, Roots: state.Roots, HTTPClient: client, ClaudeUpstream: "https://claude.invalid", CodexUpstream: "https://codex.invalid", ClaudeToken: func() (string, error) { return "fixture-token", nil }, CodexToken: func() (string, error) { return "fixture-token", nil }, Env: state.Env, Stderr: io.Discard})
		if err != nil {
			return failedModelsPublication("local"), nil, err
		}
		m.pipeline = p
		return refreshV2LocalRegistry(ctx, &localRegistry{Catalog: p.Catalog, Refresher: p.Refresher, PublishReport: p.PublishReport})
	}}
	m.fixture.Lookup = func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Models(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ModelsWithDependencies(ctx, inv, s, m.deps)
		}, ok
	}
	return m
}
func init() {
	registerV2Fixture("models-publish-partial", func(t *testing.T) *v2Fixture {
		m := newModelTestFixture(t)
		m.fs.fail = "model-capabilities.json.tmp"
		return m.fixture
	})
	registerV2Fixture("models-native-shadow", func(t *testing.T) *v2Fixture {
		m := newModelTestFixture(t)
		m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "native-example", Source: modelregistry.SourceOverlay, DisplayName: "Shadow"})
		return m.fixture
	})
	registerV2Fixture("models-proxy-reachable-error", func(t *testing.T) *v2Fixture {
		m := newModelTestFixture(t)
		m.deps.Refresh = func(ctx context.Context, _ modelsDeps) (ModelPublication, []ModelIdentity, error) {
			p, ids, _, err := attemptV2ProxyRegistryRefresh(ctx, modelHTTPFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":"synthetic"}`))}, nil
			}), 1234, "fixture-token")
			return p, ids, err
		}
		return m.fixture
	})
}
func (m *modelTestFixture) save(t *testing.T, entries ...modelregistry.Entry) {
	t.Helper()
	if err := saveModelsOverlayFile(m.state, modelregistry.OverlayFile{Version: 1, Models: entries}); err != nil {
		t.Fatal(err)
	}
}
func (m *modelTestFixture) run(t *testing.T, args ...string) cli.Outcome {
	t.Helper()
	inv, err := cli.Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return handleV2ModelsWithDependencies(context.Background(), inv, &cli.Session{}, m.deps)
}
func assertModelCode(t *testing.T, out cli.Outcome, exit int, code string) {
	t.Helper()
	if out.ExitCode != exit {
		t.Fatalf("exit %d want %d: %+v", out.ExitCode, exit, out)
	}
	if code != "" && (len(out.Errors) == 0 || out.Errors[0].Code != code) {
		t.Fatalf("code want %s: %+v", code, out)
	}
}
func TestCLIV2ModelsContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "saved overlay survives publication failure", Scenario: "models-publish-partial", Args: []string{"models", "overlay", "add", "--provider", "codex", "--id", "local-example", "--clone-from", "native-example", "--json"}, Exit: 8, Command: "models overlay add", Code: "models_refresh_partial", WantJSON: `{"overlay_saved":true}`, Forbid: []string{"consume", "credential-activate", "service"}})
}
func TestCLIV2ModelsNativeShadow(t *testing.T) {
	runV2Case(t, v2Case{Name: "native wins", Scenario: "models-native-shadow", Args: []string{"models", "list", "--provider=codex", "-j"}, Command: "models list", WantJSON: `{"models":[{"id":"native-example","source":"native","display_name":"Native example"}],"provider":"codex"}`, Forbid: []string{"network", "refresh"}})
}
func TestCLIV2ModelsOptionsBeforeAccess(t *testing.T) {
	for _, args := range [][]string{{"models", "list", "extra"}, {"models", "refresh", "--timeout", "1s"}, {"models", "refresh", "--dry-run"}, {"models", "overlay", "prune", "--provider", "codex"}, {"models", "overlay", "remove", "--provider", "codex", "--id", "x", "--clone-from", "x"}, {"models", "overlay", "add", "--provider", "gemini", "--id", "x"}, {"models", "overlay", "add", "--provider", "codex"}, {"models", "list", "--provider", "codex", "--provider", "claude"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			exit := cli.Run(context.Background(), append(args, "--json"), &cli.Session{Out: &out, Err: io.Discard}, func(string) (cli.Handler, bool) { calls++; return nil, false })
			if exit != 2 || calls != 0 {
				t.Fatalf("exit=%d lookup=%d", exit, calls)
			}
		})
	}
	for _, id := range []string{"", " leading", "trailing ", "a\nb", "a\x00b", string([]byte{0xff})} {
		m := newModelTestFixture(t)
		inv := cli.Invocation{Path: "models overlay add", Options: map[string][]string{"provider": {"codex"}, "id": {id}}}
		out := handleV2ModelsWithDependencies(context.Background(), inv, nil, m.deps)
		assertModelCode(t, out, 2, "cli_invalid_usage")
		if m.fixture.calls["resolve"] != 0 {
			t.Fatal("invalid identity read state")
		}
	}
}
func TestCLIV2ModelsHelpAndPresentation(t *testing.T) {
	for _, path := range []string{"models list", "models refresh", "models overlay add", "models overlay remove", "models overlay prune"} {
		t.Run(path, func(t *testing.T) {
			var out bytes.Buffer
			args := append(strings.Fields(path), "--help", "--json")
			exit := cli.Run(context.Background(), args, &cli.Session{Out: &out, Err: io.Discard}, func(string) (cli.Handler, bool) { t.Fatal("help accessed handler"); return nil, false })
			want, err := os.ReadFile(filepath.Join("../../specs/cli-v2/help", strings.ReplaceAll(path, " ", "-")+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("help mismatch exit=%d", exit)
			}
		})
	}
	m := newModelTestFixture(t)
	var a, b bytes.Buffer
	for _, row := range []struct {
		out  *bytes.Buffer
		flag string
	}{{&a, "-j"}, {&b, "--json"}} {
		if exit := cli.Run(context.Background(), []string{"models", "list", row.flag}, &cli.Session{Out: row.out, Err: io.Discard}, m.fixture.Lookup); exit != 0 {
			t.Fatal(exit)
		}
	}
	if a.String() != b.String() {
		t.Fatal("JSON aliases differ")
	}
	o := m.run(t, "models", "list")
	if o.Human != "MODEL\tPROVIDER\tSOURCE\nclaude-example\tclaude\tnative\nnative-example\tcodex\tnative\n" {
		t.Fatal(o.Human)
	}
}
func TestCLIV2ModelsCloneAndMutation(t *testing.T) {
	for _, scenario := range []string{"explicit", "missing", "wrong-provider", "case-sensitive", "none", "replace", "conflict", "remove", "remove-missing", "partial-remove", "atomic-failure"} {
		t.Run(scenario, func(t *testing.T) {
			m := newModelTestFixture(t)
			args := []string{"models", "overlay", "add", "--provider", "codex", "--id", "local-example"}
			exit, code := 0, ""
			switch scenario {
			case "explicit":
				args = append(args, "--clone-from", "native-example")
			case "missing":
				args = append(args, "--clone-from", "absent")
				exit, code = 3, "models_clone_not_found"
			case "wrong-provider":
				args = append(args, "--clone-from", "claude-example")
				exit, code = 3, "models_clone_not_found"
			case "case-sensitive":
				args = append(args, "--clone-from", "Native-example")
				exit, code = 3, "models_clone_not_found"
			case "replace":
				m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "local-example", Source: modelregistry.SourceOverlay, Description: "old"}, modelregistry.Entry{Provider: modelregistry.ProviderAnthropic, ID: "keep", Source: modelregistry.SourceOverlay})
			case "conflict":
				args[6] = "CLAUDE-EXAMPLE"
				exit, code = 6, "models_conflict"
			case "remove", "partial-remove":
				m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "local-example", Source: modelregistry.SourceOverlay})
				args[2] = "remove"
				if scenario == "partial-remove" {
					m.fs.fail = "model-capabilities.json.tmp"
					exit, code = 8, "models_refresh_partial"
				}
			case "remove-missing":
				args[2] = "remove"
				exit, code = 3, "models_overlay_not_found"
			case "atomic-failure":
				m.fs.fail = "models.json.tmp"
				exit, code = 1, "models_store_failed"
			}
			out := m.run(t, args...)
			assertModelCode(t, out, exit, code)
			stored, err := loadModelsOverlayFile(m.state)
			if err != nil {
				t.Fatal(err)
			}
			if exit == 3 || exit == 6 || scenario == "atomic-failure" {
				if len(stored.Models) != 0 || m.fixture.calls["refresh"] != 0 {
					t.Fatal("failed validation committed or refreshed")
				}
				return
			}
			if scenario == "remove" || scenario == "partial-remove" {
				if len(stored.Models) != 0 || !bytes.Contains(out.Data, []byte(`"overlay_removed":true`)) {
					t.Fatal("removal not committed")
				}
				return
			}
			if scenario == "explicit" {
				if !bytes.Contains(out.Data, []byte(`"mode":"explicit","source_id":"native-example"`)) || stored.Models[0].MaxContextWindow != 2000 {
					t.Fatal(string(out.Data))
				}
			}
			if scenario == "none" && !bytes.Contains(out.Data, []byte(`"mode":"none","source_id":null`)) {
				t.Fatal(string(out.Data))
			}
			if scenario == "replace" && (len(stored.Models) != 2 || stored.Models[0].Description == "old" || stored.Models[1].ID != "keep") {
				t.Fatal(stored)
			}
		})
	}
}
func TestCLIV2ModelsPublicationAndPrune(t *testing.T) {
	for _, scenario := range []string{"complete", "optional-absent", "source-partial", "all-sources-failed", "target-partial", "prune-fresh", "prune-stale", "prune-repeat"} {
		t.Run(scenario, func(t *testing.T) {
			m := newModelTestFixture(t)
			args := []string{"models", "refresh"}
			exit, code := 0, ""
			if strings.HasPrefix(scenario, "prune") {
				m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "native-example", Source: modelregistry.SourceOverlay}, modelregistry.Entry{Provider: modelregistry.ProviderAnthropic, ID: "claude-example", Source: modelregistry.SourceOverlay}, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "unproven", Source: modelregistry.SourceOverlay})
				args = []string{"models", "overlay", "prune"}
			}
			switch scenario {
			case "optional-absent":
				os.Remove(filepath.Join(m.state.HomeDir, ".claude.json"))
				os.Remove(filepath.Join(m.state.HomeDir, ".claude/cache/model-capabilities.json"))
			case "source-partial", "prune-stale":
				m.codexFail = true
				exit, code = 8, "models_refresh_partial"
			case "all-sources-failed":
				m.codexFail = true
				m.claudeFail = true
				exit, code = 1, "models_refresh_failed"
			case "target-partial":
				m.fs.fail = "models_cache.json.tmp"
				exit, code = 8, "models_refresh_partial"
			}
			out := m.run(t, args...)
			assertModelCode(t, out, exit, code)
			var data struct {
				Publication  ModelPublication `json:"publication"`
				RemovedCount int              `json:"removed_count"`
			}
			if err := json.Unmarshal(out.Data, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Publication.Sources) != 2 || data.Publication.Sources[0].Provider != "claude" {
				t.Fatal(data)
			}
			if scenario != "all-sources-failed" {
				if len(data.Publication.Targets) != 3 {
					t.Fatal(data)
				}
				for _, target := range data.Publication.Targets {
					if !filepath.IsAbs(target.Path) {
						t.Fatal(target)
					}
				}
			}
			if scenario == "optional-absent" && (data.Publication.Targets[1].Status != "skipped" || data.Publication.Targets[2].Status != "skipped") {
				t.Fatal(data)
			}
			if strings.HasPrefix(scenario, "prune") {
				want := 2
				if scenario == "prune-stale" {
					want = 1
				}
				if data.RemovedCount != want {
					t.Fatalf("removed=%d want=%d", data.RemovedCount, want)
				}
				stored, _ := loadModelsOverlayFile(m.state)
				if len(stored.Models) != 3-want {
					t.Fatal(stored)
				}
			}
			if scenario == "prune-repeat" {
				again := m.run(t, args...)
				assertModelCode(t, again, 0, "")
				if !bytes.Contains(again.Data, []byte(`"removed_count":0`)) {
					t.Fatal(string(again.Data))
				}
			}
		})
	}
}
func TestCLIV2ModelsPublicationReload(t *testing.T) {
	m := newModelTestFixture(t)
	out := m.run(t, "models", "overlay", "add", "--provider", "codex", "--id", "local-example", "--clone-from", "native-example")
	assertModelCode(t, out, 0, "")
	list := m.run(t, "models", "list", "--provider", "codex")
	var data struct {
		Models []ModelEntry `json:"models"`
	}
	json.Unmarshal(list.Data, &data)
	if len(data.Models) != 2 || data.Models[0].ID != "local-example" || data.Models[0].Source != "overlay" {
		t.Fatal(string(list.Data))
	}
	clone := m.run(t, "models", "overlay", "add", "--provider", "codex", "--id", "another", "--clone-from", "local-example")
	assertModelCode(t, clone, 3, "models_clone_not_found")
	prune := m.run(t, "models", "overlay", "prune")
	assertModelCode(t, prune, 0, "")
	if !bytes.Contains(prune.Data, []byte(`"removed_count":0`)) {
		t.Fatal(string(prune.Data))
	}
	m.extraNative = true
	prune = m.run(t, "models", "overlay", "prune")
	if !bytes.Contains(prune.Data, []byte(`"removed_count":1`)) {
		t.Fatal(string(prune.Data))
	}
}
func TestCLIV2ModelsStoreFailuresAndEmpty(t *testing.T) {
	for _, scenario := range []string{"malformed-codex", "malformed-claude", "malformed-entry", "overlay-invalid", "home-unavailable", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			m := newModelTestFixture(t)
			switch scenario {
			case "malformed-codex":
				os.WriteFile(filepath.Join(m.state.HomeDir, ".codex/models_cache.json"), []byte(`{`), 0600)
			case "malformed-claude":
				os.WriteFile(filepath.Join(m.state.HomeDir, ".claude/cache/model-capabilities.json"), []byte(`{`), 0600)
			case "malformed-entry":
				os.WriteFile(filepath.Join(m.state.HomeDir, ".codex/models_cache.json"), []byte(`{"models":[{}]}`), 0600)
			case "overlay-invalid":
				os.MkdirAll(m.state.Roots.Config, 0700)
				os.WriteFile(filepath.Join(m.state.Roots.Config, "models.json"), []byte(`{"version":1,"models":[{"provider":"codex","id":" x"}]}`), 0600)
			case "home-unavailable":
				m.state.HomeDir = ""
				m.state.FS = v2ModelNoHomeFS{m.fs}
			case "empty":
				os.Remove(filepath.Join(m.state.HomeDir, ".codex/models_cache.json"))
				os.Remove(filepath.Join(m.state.HomeDir, ".claude/cache/model-capabilities.json"))
			}
			out := m.run(t, "models", "list")
			if scenario == "empty" {
				assertModelCode(t, out, 0, "")
				if !bytes.Contains(out.Data, []byte(`"models":[]`)) {
					t.Fatal(string(out.Data))
				}
			} else {
				if out.ExitCode == 0 {
					t.Fatal("invalid state succeeded")
				}
				if m.fixture.calls["network"] != 0 {
					t.Fatal("read fetched network")
				}
			}
		})
	}
}

type v2ModelNoHomeFS struct{ fsutil.FileSystem }

func (v2ModelNoHomeFS) UserHomeDir() (string, error) { return "", errors.New("home unavailable") }
func TestCLIV2ModelsProxyAuthority(t *testing.T) {
	for _, scenario := range []string{"refused", "enoent", "eof", "timeout", "404", "500", "legacy-200", "malformed", "complete", "partial", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			m := newModelTestFixture(t)
			out := m.run(t, "models", "refresh")
			var data struct {
				Publication ModelPublication `json:"publication"`
			}
			json.Unmarshal(out.Data, &data)
			data.Publication.Via = "proxy"
			if scenario == "partial" {
				data.Publication.Targets[1].Status = "failed"
				data.Publication.Targets[1].Reason = "write_failed"
				data.Publication.Targets[1].ErrorCode = modelString("models_publication_failed")
				data.Publication.Targets[1].Message = modelString("Model cache publication failed.")
				data.Publication.RecomputeStatus()
			}
			body, _ := json.Marshal(map[string]any{"publication": data.Publication, "prunable": []ModelIdentity{}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			handledWant := scenario != "refused" && scenario != "enoent"
			errWant := scenario != "refused" && scenario != "enoent" && scenario != "complete" && scenario != "partial"
			client := modelHTTPFunc(func(*http.Request) (*http.Response, error) {
				status := 200
				switch scenario {
				case "refused":
					return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
				case "enoent":
					return nil, &net.OpError{Op: "dial", Err: syscall.ENOENT}
				case "eof":
					return nil, io.EOF
				case "timeout":
					return nil, context.DeadlineExceeded
				case "cancelled":
					return nil, context.Canceled
				case "404":
					status = 404
					body = []byte(`{}`)
				case "500":
					status = 500
					body = []byte(`{}`)
				case "legacy-200":
					body = []byte(`{"ok":true}`)
				case "malformed":
					body = []byte(`{`)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			p, _, handled, err := attemptV2ProxyRegistryRefresh(ctx, client, 1234, "fixture-token")
			if handled != handledWant || (err != nil) != errWant {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if scenario == "partial" && p.Status != "partial" {
				t.Fatal(p)
			}
		})
	}
	runV2Case(t, v2Case{Name: "reachable failure", Scenario: "models-proxy-reachable-error", Args: []string{"models", "refresh", "--json"}, Command: "models refresh", Exit: 1, Code: "models_refresh_failed", Forbid: []string{"network", "refresh"}})
}

func TestCLIV2ModelsCanonicalCredentialAuthority(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, mode := range []string{"owned", "denied", "pending-before", "pending-after", "cancelled"} {
			t.Run(fmt.Sprintf("remote=%t/%s", remote, mode), func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", "")
				store, record, journal := resetProductionFixture(t)
				t.Setenv("CODEX_HOME", filepath.Join(store.Home, ".codex"))
				t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(store.Home, ".claude"))
				plan := codexprov.RemovalPlan{Version: 1, OperationID: "models-pending", AccountKey: record.Metadata.AccountKey, Candidates: []codexprov.RemovalCandidate{{CandidateID: record.Metadata.CandidateID, Revision: record.Metadata.Revision}}}
				var exchanged atomic.Int32
				client := modelHTTPFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/oauth/token" {
						exchanged.Add(1)
						body, _ := json.Marshal(auth.CodexTokenResponse{AccessToken: "models-refreshed", RefreshToken: "next-synthetic", IDToken: record.Credential.IDToken, ExpiresIn: 3600})
						return resetProductionResponse(200, string(body)), nil
					}
					if strings.HasSuffix(req.URL.Path, "/models") {
						if req.Header.Get("Authorization") == "Bearer models-refreshed" {
							return resetProductionResponse(200, `{"models":[{"slug":"fresh-model"}]}`), nil
						}
						if mode == "pending-after" {
							if err := journal.Save(plan); err != nil {
								t.Error(err)
							}
						}
						return resetProductionResponse(401, `{}`), nil
					}
					t.Errorf("unexpected HTTP request %s", req.URL.Path)
					return nil, errors.New("unexpected HTTP")
				})
				coordinator, err := codexprov.NewCredentialCoordinator(store, journal.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "denied" {
					coordinator.CredentialOwner = resetProductionRecorder{}
					coordinator.RefreshMutations = resetProductionRecorder{}
				}
				coordinator.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
					return auth.RefreshCodexToken(ctx, client, token)
				}
				owner, err := codexprov.OpenCredentialControl(codexprov.DefaultCredentialControlPath(journal.StateDir), coordinator)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
				if mode == "pending-before" {
					if err := journal.Save(plan); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var reg *localRegistry
				if remote {
					saved := newHTTPClientFn
					newHTTPClientFn = func(time.Duration, string) httputil.Doer { return &http.Client{Transport: modelRoundTripper{client}} }
					defer func() { newHTTPClientFn = saved }()
					roots := userdirs.Roots{Config: filepath.Join(store.Home, "config"), State: journal.StateDir, Cache: filepath.Join(store.Home, "cache")}
					reg, err = buildCanonicalLocalRegistryWithControl(ctx, &proxy.Config{ClaudeUpstream: "https://claude.invalid", CodexUpstream: "https://codex.invalid"}, modelsDeps{FS: store.FS, HomeDir: store.Home, CWD: store.Home, Roots: roots, Env: os.Getenv}, func(ctx context.Context, _ fsutil.DurableFileSystem, client httputil.Doer) (*codexprov.CredentialControl, error) {
						return openResetProductionControl(ctx, store, client)
					})
					if err != nil {
						t.Fatal(err)
					}
					defer reg.Close()
				} else {
					reg, err = buildLocalRegistryFromAuthority(&proxy.Config{ClaudeUpstream: "https://claude.invalid", CodexUpstream: "https://codex.invalid"}, localRegistryDependencies{FS: store.FS, HomeDir: store.Home, CWD: store.Home, HTTPClient: client, ClaudeToken: func() (string, error) { return "synthetic", nil }, CredentialAuthority: newCanonicalCodexRegistryControlAdapter(owner), Env: os.Getenv})
					if err != nil {
						t.Fatal(err)
					}
				}
				if mode == "cancelled" {
					cancel()
				}
				result, fetchErr := reg.Refresher.Codex.Fetch(ctx)
				if mode == "owned" {
					if fetchErr != nil || len(result.Entries) != 1 || exchanged.Load() != 1 {
						t.Fatalf("owned refresh lost entries=%d exchanges=%d err=%v", len(result.Entries), exchanged.Load(), fetchErr)
					}
				} else {
					if fetchErr == nil || exchanged.Load() != 0 {
						t.Fatalf("denied refresh exchanged=%d err=%v", exchanged.Load(), fetchErr)
					}
				}
				if mode == "denied" && !errors.Is(fetchErr, errCodexRegistryCredentialAuthorityUnavailable) {
					t.Fatalf("typed authority denial lost: %v", fetchErr)
				}
				if strings.HasPrefix(mode, "pending") {
					if _, present, err := journal.Load(); err != nil || !present {
						t.Fatal("pending removal implicitly recovered")
					}
					_, err := store.Load(record.Path)
					if err != nil {
						t.Fatal("managed record removed")
					}
				}
			})
		}
	}
}

type modelRoundTripper struct{ client modelHTTPFunc }

func (r modelRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return r.client(req) }

func TestCLIV2ModelsExplicitCloneRetainsCachedVendorMetadata(t *testing.T) {
	m := newModelTestFixture(t)
	m.codexFail = true
	path := filepath.Join(m.state.HomeDir, ".codex/models_cache.json")
	if err := os.WriteFile(path, []byte(`{"models":[{"slug":"native-example","vendor_opaque":{"preserved":true}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	out := m.run(t, "models", "overlay", "add", "--provider", "codex", "--id", "custom", "--clone-from", "native-example")
	assertModelCode(t, out, 8, "models_refresh_partial")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range envelope.Models {
		if string(row["slug"]) == `"custom"` {
			found = true
			if !bytes.Contains(row["vendor_opaque"], []byte(`true`)) {
				t.Fatal("explicit cached clone lost vendor metadata")
			}
		}
	}
	if !found {
		t.Fatal("clone missing")
	}
	if bytes.Contains(out.Data, []byte("vendor_opaque")) || bytes.Contains(out.Data, []byte(`"raw"`)) {
		t.Fatal("raw vendor metadata exposed in DTO")
	}
}
func TestCLIV2ModelsPublicationSnapshotBound(t *testing.T) {
	m := newModelTestFixture(t)
	out := m.run(t, "models", "refresh")
	assertModelCode(t, out, 0, "")
	original := m.pipeline.Catalog.Snapshot()
	m.pipeline.Catalog.Replace(modelregistry.Snapshot{Entries: []modelregistry.Entry{{Provider: modelregistry.ProviderCodex, ID: "concurrent-new", Source: modelregistry.SourceNative}}})
	targets := m.pipeline.PublishReport(original)
	if targets[0].Status != "written" {
		t.Fatal(targets)
	}
	rows, err := loadCachedNativeEntries(m.state, modelregistry.ProviderCodex)
	if err != nil || len(rows) != 1 || rows[0].ID != "native-example" {
		t.Fatalf("publication borrowed another refresh snapshot: %+v %v", rows, err)
	}
}
func TestCLIV2ModelsRejectsIncompleteReceipt(t *testing.T) {
	m := newModelTestFixture(t)
	out := m.run(t, "models", "refresh")
	var data struct {
		Publication ModelPublication `json:"publication"`
	}
	json.Unmarshal(out.Data, &data)
	data.Publication.Via = "proxy"
	original, _ := json.Marshal(map[string]any{"publication": data.Publication, "prunable": []ModelIdentity{}})
	for _, scenario := range []string{"missing-count", "missing-error-code", "null-targets", "bad-order", "relative-path", "stale-prune", "missing-prune", "wrong-status"} {
		t.Run(scenario, func(t *testing.T) {
			var root map[string]any
			json.Unmarshal(original, &root)
			p := root["publication"].(map[string]any)
			sources := p["sources"].([]any)
			targets := p["targets"].([]any)
			switch scenario {
			case "missing-count":
				delete(sources[0].(map[string]any), "native_count")
			case "missing-error-code":
				delete(targets[0].(map[string]any), "error_code")
			case "null-targets":
				p["targets"] = nil
			case "bad-order":
				sources[0], sources[1] = sources[1], sources[0]
			case "relative-path":
				targets[0].(map[string]any)["path"] = "relative"
			case "stale-prune":
				sources[0].(map[string]any)["native_count"] = 0
				root["prunable"] = []any{map[string]any{"provider": "claude", "id": "stale"}}
			case "missing-prune":
				delete(root, "prunable")
			case "wrong-status":
				p["status"] = "partial"
			}
			body, _ := json.Marshal(root)
			_, _, handled, err := attemptV2ProxyRegistryRefresh(context.Background(), modelHTTPFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
			}), 1234, "fixture")
			if !handled || err == nil {
				t.Fatal("malformed receipt accepted or alternate authority selected")
			}
		})
	}
}
func TestCLIV2ModelsProviderAliasAndLazyPaths(t *testing.T) {
	m := newModelTestFixture(t)
	os.WriteFile(filepath.Join(m.state.HomeDir, ".codex/models_cache.json"), []byte(`broken`), 0600)
	var stdout, stderr bytes.Buffer
	exit := cli.Run(context.Background(), []string{"models", "list", "--provider", "anthropic", "--json"}, &cli.Session{Out: &stdout, Err: &stderr}, m.fixture.Lookup)
	if exit != 0 || !bytes.Contains(stdout.Bytes(), []byte(`"provider":"claude"`)) || !bytes.Contains(stdout.Bytes(), []byte(`"deprecated_option"`)) {
		t.Fatalf("alias/filter failed exit=%d %s", exit, stdout.String())
	}
}

func TestCLIV2ModelsFallbackOnceAndPhaseBudgets(t *testing.T) {
	for _, scenario := range []string{"unreachable", "reachable-error", "deadline", "no-config"} {
		t.Run(scenario, func(t *testing.T) {
			m := newModelTestFixture(t)
			assertModelCode(t, m.run(t, "models", "refresh"), 0, "")
			proxyCalls, localCalls := 0, 0
			client := modelHTTPFunc(func(req *http.Request) (*http.Response, error) {
				proxyCalls++
				deadline, ok := req.Context().Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("proxy phase deadline missing")
				}
				if scenario == "unreachable" {
					return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
				}
				if scenario == "deadline" {
					return nil, context.DeadlineExceeded
				}
				return resetProductionResponse(404, `{}`), nil
			})
			cfg := &proxy.Config{Port: 1234}
			if scenario == "no-config" {
				cfg = nil
			}
			builder := func(ctx context.Context, _ *proxy.Config, _ modelsDeps) (*localRegistry, error) {
				localCalls++
				if _, ok := ctx.Deadline(); ok {
					t.Fatal("proxy phase deadline leaked into local publication")
				}
				return &localRegistry{Catalog: m.pipeline.Catalog, Refresher: m.pipeline.Refresher, PublishReport: func(snap modelregistry.Snapshot) []modelregistry.PublicationTarget {
					if _, ok := ctx.Deadline(); ok {
						t.Error("source phase deadline leaked into publication")
					}
					return m.pipeline.PublishReport(snap)
				}, Close: func() error { return nil }}, nil
			}
			p, _, err := refreshV2ModelsFromConfig(context.Background(), m.state, cfg, client, builder)
			wantLocal := 0
			if scenario == "unreachable" || scenario == "no-config" {
				wantLocal = 1
				if err != nil || p.Status != "complete" {
					t.Fatalf("fallback failed %v %+v", err, p)
				}
			} else {
				if err == nil {
					t.Fatal("reachable authority error hidden")
				}
			}
			wantProxy := 1
			if scenario == "no-config" {
				wantProxy = 0
			}
			if proxyCalls != wantProxy || localCalls != wantLocal {
				t.Fatalf("proxy=%d local=%d", proxyCalls, localCalls)
			}
		})
	}
}

func TestCLIV2ModelsKnownZeroPriority(t *testing.T) {
	m := newModelTestFixture(t)
	if err := os.WriteFile(filepath.Join(m.state.HomeDir, ".codex/models_cache.json"), []byte(`{"models":[{"slug":"native-example","priority":0}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	out := m.run(t, "models", "list", "--provider", "codex")
	var list struct {
		Models []ModelEntry `json:"models"`
	}
	json.Unmarshal(out.Data, &list)
	if len(list.Models) != 1 || list.Models[0].Priority == nil || *list.Models[0].Priority != 0 {
		t.Fatal("known priority zero became unknown")
	}
	out = m.run(t, "models", "overlay", "add", "--provider", "codex", "--id", "custom", "--clone-from", "native-example")
	assertModelCode(t, out, 0, "")
	if !bytes.Contains(out.Data, []byte(`"priority":0`)) {
		t.Fatal("clone lost known priority zero")
	}
	stored, err := loadModelsOverlayFile(m.state)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelEntryDTO(stored.Models[0]); got.Priority == nil || *got.Priority != 0 {
		t.Fatal("saved clone lost zero priority presence")
	}
}

func TestCLIV2ModelsConflictDiagnostic(t *testing.T) {
	m := newModelTestFixture(t)
	out := m.run(t, "models", "overlay", "add", "--provider", "codex", "--id", "claude-example")
	assertModelCode(t, out, 6, "models_conflict")
	if out.Errors[0].Message != "Model ID claude-example conflicts across providers: claude, codex." {
		t.Fatal(out.Errors[0])
	}
	p := modelregistry.NewPublication(modelregistry.RefreshDiagnostics{Counts: map[string]int{"anthropic": 1, "codex": 1}}, 0, nil, "proxy")
	body, _ := json.Marshal(map[string]any{"publication": p, "prunable": []ModelIdentity{}, "error_code": "models_conflict", "conflict": &modelregistry.ConflictError{ID: "shared", Providers: []string{"anthropic", "codex"}}})
	publication, _, handled, err := attemptV2ProxyRegistryRefresh(context.Background(), modelHTTPFunc(func(*http.Request) (*http.Response, error) { return resetProductionResponse(500, string(body)), nil }), 1234, "fixture")
	if !handled {
		t.Fatal("conflict changed authority")
	}
	out = modelsPublicationOutcome(publication, err)
	assertModelCode(t, out, 6, "models_conflict")
	if out.Errors[0].Message != "Model ID shared conflicts across providers: claude, codex." {
		t.Fatal(out.Errors[0])
	}
}
