package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const v2FixtureBody = `{"model":"gpt-5.6-sol","input":[],"client_metadata":{"session_id":"raw-session-sentinel","thread_id":"raw-thread-sentinel","turn_id":"raw-turn-sentinel","request_kind":"turn"}}`

func init() {
	registerV2Fixture("fixture-output-exists", func(t *testing.T) *v2Fixture {
		dir := t.TempDir()
		input, output := filepath.Join(dir, "request.json"), filepath.Join(dir, "fixture.json")
		if err := os.WriteFile(input, []byte(v2FixtureBody), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, []byte("existing sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			b, err := os.ReadFile(output)
			if err != nil || string(b) != "existing sentinel" {
				t.Error("existing output changed")
			}
		})
		f := &v2Fixture{In: noAccessV2Input{}}
		f.Lookup = func(path string) (cli.Handler, bool) {
			handler, ok := lookupV2Validation(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				inv.Options["input"] = []string{input}
				inv.Options["output"] = []string{output}
				return handler(ctx, inv, s)
			}, ok
		}
		return f
	})
}
func init() {
	registerV2Fixture("readiness-missing", func(t *testing.T) *v2Fixture {
		dir := validationTempDir(t)
		return &v2Fixture{In: noAccessV2Input{}, Lookup: validationLookup(v2ValidationDependencies{stateDir: func() (string, error) { return dir, nil }, loadMarker: proxy.LoadCodexReadinessMarker})}
	})
	registerV2Fixture("validation-http-accepted", func(t *testing.T) *v2Fixture {
		f := newValidationHTTPFixture(t)
		return &v2Fixture{In: noAccessV2Input{}, Lookup: validationLookup(v2ValidationDependencies{http: func(ctx context.Context, port int, build string) error {
			return runCanonicalProxyValidateHTTPWithOperations(ctx, port, build, f.ops)
		}})}
	})
}

func TestCLIV2ValidationContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "readiness missing", Scenario: "readiness-missing", Args: []string{"codex", "proxy", "readiness", "show", "--client-build", "0.146.0", "--json"}, Exit: 3, Command: "codex proxy readiness show", Code: "readiness_missing", Forbid: []string{"filesystem-write", "service", "consume"}})
	runV2Case(t, v2Case{Name: "HTTP accepted is not completed", Scenario: "validation-http-accepted", Args: []string{"codex", "proxy", "validate", "http", "--port", "19281", "--json"}, Exit: 0, Command: "codex proxy validate http", WantJSON: `{"state":"requested","port":19281,"validation_complete":false,"outcome":"accepted"}`})
	runV2Case(t, v2Case{Name: "fixture cannot overwrite existing file", Scenario: "fixture-output-exists", Args: []string{"codex", "proxy", "fixture", "create", "--input", "request.json", "--output", "fixture.json", "--json"}, Exit: 6, Command: "codex proxy fixture create", Code: "fixture_output_exists", Forbid: []string{"filesystem-write", "service", "consume"}})
}

func TestCLIV2ValidationFixtureSuccessAndAliases(t *testing.T) {
	for _, prefix := range [][]string{{"codex", "proxy", "fixture", "create"}, {"codex", "validate", "capture"}} {
		t.Run(strings.Join(prefix, "-"), func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "request.json")
			output := filepath.Join(dir, "new", "fixture.json")
			body := strings.Replace(v2FixtureBody, `"input":[]`, `"input":[{"role":"user","content":"prompt-secret-sentinel"}],"previous_response_id":"previous-secret","reasoning":{"encrypted_content":"encrypted-secret"}`, 1)
			if err := os.WriteFile(input, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			out, stdout, _ := runValidationCLI(t, append(prefix, "--input", input, "--output", output, "--json"), context.Background(), lookupV2Validation)
			if out != 0 {
				t.Fatalf("exit=%d output=%s", out, stdout)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"raw-session-sentinel", "raw-thread-sentinel", "raw-turn-sentinel", "prompt-secret-sentinel", "previous-secret", "encrypted-secret"} {
				if bytes.Contains(data, []byte(secret)) || strings.Contains(stdout, secret) {
					t.Fatal("raw input leaked")
				}
			}
			var envelope struct {
				Data struct {
					Path    string         `json:"output_path"`
					Fixture map[string]any `json:"fixture"`
				}
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Data.Path != output || len(envelope.Data.Fixture) != 11 {
				t.Fatalf("invalid fixture resource: %s", stdout)
			}
			if info, _ := os.Stat(output); info.Mode().Perm() != 0600 {
				t.Fatal("fixture mode")
			}
			if info, _ := os.Stat(filepath.Dir(output)); info.Mode().Perm() != 0700 {
				t.Fatal("parent mode")
			}
		})
	}
}
func runValidationCLI(t *testing.T, args []string, ctx context.Context, lookup cli.Lookup) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := cli.Run(ctx, args, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, lookup)
	return exit, stdout.String(), stderr.String()
}

func TestCLIV2ValidationReadiness(t *testing.T) {
	for _, scenario := range []string{"current", "systemd-retained", "stale", "missing", "malformed", "duplicate-gates", "bad-digest"} {
		t.Run(scenario, func(t *testing.T) {
			dir := validationTempDir(t)
			required, _ := proxy.DefaultCodexRoutingRequirements(version, "0.146.0")
			marker := completeCodexHTTPReadinessMarker(required)
			if scenario == "systemd-retained" {
				marker.ServiceKind = "systemd-user"
			}
			if scenario == "stale" {
				marker.ClientBuild = "0.145.0"
			}
			if scenario == "duplicate-gates" {
				marker.CompletedGates = append(marker.CompletedGates, marker.CompletedGates[0])
			}
			if scenario == "bad-digest" {
				marker.CQExecutableSHA256 = "invalid"
			}
			if scenario != "missing" {
				writeTestCodexHTTPReadinessMarker(t, dir, marker)
			}
			path := filepath.Join(dir, "codex-readiness-http.json")
			if scenario == "malformed" {
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			exit, stdout, _ := runValidationCLI(t, []string{"codex", "proxy", "readiness", "show", "--client-build", "0.146.0", "--state-dir", dir, "--json"}, context.Background(), lookupV2Validation)
			want := map[string]int{"current": 0, "systemd-retained": 0, "stale": 6, "missing": 3, "malformed": 1, "duplicate-gates": 6, "bad-digest": 1}[scenario]
			if exit != want {
				t.Fatalf("exit=%d want=%d stdout=%s", exit, want, stdout)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("readiness mutated marker")
			}
			if scenario == "current" {
				var envelope struct {
					Data struct {
						Current bool
						Marker  struct {
							Gates []string `json:"completed_gates"`
						}
					}
				}
				if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
					t.Fatal(err)
				}
				if !envelope.Data.Current || !sort.StringsAreSorted(envelope.Data.Marker.Gates) {
					t.Fatal("marker projection")
				}
			}
		})
	}
}
func validationTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCLIV2ValidationHelpAndLexicalRejection(t *testing.T) {
	for _, path := range []string{"codex proxy fixture create", "codex proxy readiness show", "codex proxy validate http", "codex proxy validate websocket"} {
		t.Run(path, func(t *testing.T) {
			lookup := func(string) (cli.Handler, bool) { t.Fatal("help touched state"); return nil, false }
			args := append(strings.Fields(path), "--help")
			exit, stdout, stderr := runValidationCLI(t, args, context.Background(), lookup)
			want, err := os.ReadFile(filepath.Join("../../specs/cli-v2/help", strings.ReplaceAll(path, " ", "-")+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || stdout != string(want) || stderr != "" {
				t.Fatal("help differs")
			}
		})
	}
	for _, args := range [][]string{
		{"codex", "proxy", "fixture", "create", "--input", "x", "--output", "y", "--input", "z"},
		{"codex", "proxy", "fixture", "create", "--input", "x", "--output", "y", "--content-encoding", "gzip"},
		{"codex", "proxy", "readiness", "show", "--client-build", " 0.146.0"},
		{"codex", "proxy", "readiness", "show", "--client-build", "0.146.0", "extra"},
		{"codex", "proxy", "validate", "http", "--port", "19280"},
		{"codex", "proxy", "validate", "http", "--port", "19281", "--timeout", "999ms"},
		{"codex", "proxy", "validate", "websocket", "--client-build", "0.146.0", "--timeout", "29s"},
		{"codex", "proxy", "validate", "websocket", "--client-build", "0.146.0", "--unknown"},
	} {
		exit, _, _ := runValidationCLI(t, args, context.Background(), func(string) (cli.Handler, bool) { t.Fatal("lexical rejection accessed handler"); return nil, false })
		if exit != 2 {
			t.Fatalf("args=%v exit=%d", args, exit)
		}
	}
	for _, path := range []string{"codex proxy fixture", "codex proxy readiness", "codex proxy validate"} {
		exit, _, _ := runValidationCLI(t, strings.Fields(path), context.Background(), func(string) (cli.Handler, bool) { t.Fatal("bare parent accessed state"); return nil, false })
		if exit != 0 {
			t.Fatal("bare group failed")
		}
	}
}

func TestCLIV2ValidationHTTPAccepted(t *testing.T) {
	f := newValidationHTTPFixture(t)
	deps := v2ValidationDependencies{http: func(ctx context.Context, port int, build string) error {
		return runCanonicalProxyValidateHTTPWithOperations(ctx, port, build, f.ops)
	}}
	exit, stdout, _ := runValidationCLI(t, []string{"codex", "proxy", "validate", "http", "--port", "19281", "--json"}, context.Background(), validationLookup(deps))
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	if err := validateV2Envelope([]byte(stdout), v2Case{Command: "codex proxy validate http", WantJSON: `{"state":"requested","port":19281,"validation_complete":false,"outcome":"accepted"}`}); err != nil {
		t.Fatal(err)
	}
	if f.restarts != 1 {
		t.Fatal("candidate not restarted")
	}
	if _, err := os.Stat(f.store.path); err != nil {
		t.Fatal("request not persisted")
	}
	if strings.Contains(stdout, "request_id") || strings.Contains(stdout, "marker") {
		t.Fatal("invented completion correlation")
	}
}
func validationLookup(deps v2ValidationDependencies) cli.Lookup {
	return func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Validation(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ValidationWithPreparation(ctx, inv, s, func(context.Context) (v2ValidationDependencies, error) { return deps, nil })
		}, ok
	}
}

type validationHTTPFixture struct {
	store                   installedHTTPValidationRequestStore
	ops                     canonicalHTTPValidationOperations
	restarts, invalidations int
	authority               installedHTTPValidationCandidateAuthority
}

func newValidationHTTPFixture(t *testing.T) *validationHTTPFixture {
	t.Helper()
	f := &validationHTTPFixture{}
	f.authority = installedHTTPValidationCandidateAuthority{binding: installedHTTPValidationServiceBinding{label: candidateProxyAgentLabel, port: 19281, executableSHA256: strings.Repeat("a", 64), serviceSHA256: strings.Repeat("b", 64)}, pid: 4242}
	f.store = installedHTTPValidationRequestStore{fs: fsutil.OSFileSystem{}, path: filepath.Join(validationTempDir(t), "request", "request.json"), now: time.Now, random: bytes.NewReader(bytes.Repeat([]byte{1}, 64)), resolveService: func(string) (installedHTTPValidationServiceBinding, error) { return f.authority.binding, nil }}
	f.ops = canonicalHTTPValidationOperations{store: func(context.Context) (installedHTTPValidationRequestStore, error) { return f.store, nil }, validate: func(context.Context, int) (installedHTTPValidationCandidateAuthority, error) { return f.authority, nil }, restart: func(context.Context, string) error { f.restarts++; return nil }, invalidate: func() error { f.invalidations++; return nil }}
	return f
}

func TestCLIV2ValidationBudgetsIncludePreparation(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			args := []string{"codex", "proxy", "validate", transport, "--json"}
			if transport == "http" {
				args = append(args, "--port", "19281")
			} else {
				args = append(args, "--client-build", "0.146.0")
			}
			duration := 30 * time.Millisecond
			if transport == "websocket" {
				duration += 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			lookup := func(string) (cli.Handler, bool) {
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					return handleV2ValidationWithPreparation(ctx, inv, s, func(work context.Context) (v2ValidationDependencies, error) {
						<-work.Done()
						return v2ValidationDependencies{}, errors.New("preparation failure")
					})
				}, true
			}
			start := time.Now()
			exit, stdout, _ := runValidationCLI(t, args, ctx, lookup)
			if exit != 7 || time.Since(start) > time.Second {
				t.Fatalf("exit=%d stdout=%s", exit, stdout)
			}
		})
	}
}

func TestCLIV2ValidationSelectedStateAndCleanupReserve(t *testing.T) {
	dir := validationTempDir(t)
	required, _ := proxy.DefaultCodexRoutingRequirements(version, "0.146.0")
	writeTestCodexHTTPReadinessMarker(t, dir, completeCodexHTTPReadinessMarker(required))
	defaultReads := 0
	deps := v2ValidationDependencies{stateDir: func() (string, error) { defaultReads++; return dir, nil }, loadMarker: proxy.LoadCodexReadinessMarker}
	exit, stdout, _ := runValidationCLI(t, []string{"codex", "proxy", "readiness", "show", "--client-build", "0.146.0", "--json"}, context.Background(), validationLookup(deps))
	if exit != 0 || defaultReads != 1 {
		t.Fatalf("default state failed: %d %s", exit, stdout)
	}
	deps.websocket = func(work, cleanup context.Context, _, _, _, state string) (proxy.CodexReadinessMarker, error) {
		workDeadline, ok := work.Deadline()
		if !ok {
			t.Fatal("work has no deadline")
		}
		cleanupDeadline, ok := cleanup.Deadline()
		if !ok {
			t.Fatal("cleanup has no deadline")
		}
		if cleanupDeadline.Sub(workDeadline) != 5*time.Second || state != dir {
			t.Fatal("cleanup reserve or state binding")
		}
		return proxy.CodexReadinessMarker{}, proxy.ErrCodexValidationClientUnavailable
	}
	exit, _, _ = runValidationCLI(t, []string{"codex", "proxy", "validate", "websocket", "--client-build", "0.146.0", "--state-dir", dir, "--json"}, context.Background(), validationLookup(deps))
	if exit != 4 || defaultReads != 1 {
		t.Fatal("explicit state not honoured")
	}
	link := filepath.Join(validationTempDir(t), "linked")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, path := range []string{link, filepath.Join(link, "missing")} {
		exit, _, _ = runValidationCLI(t, []string{"codex", "proxy", "readiness", "show", "--client-build", "0.146.0", "--state-dir", path, "--json"}, context.Background(), lookupV2Validation)
		if exit != 1 {
			t.Fatal("state symlink traversal accepted")
		}
	}
}

func TestCLIV2ValidationInterruptedBeforePreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{{"codex", "proxy", "validate", "http", "--port", "19281", "--json"}, {"codex", "proxy", "validate", "websocket", "--client-build", "0.146.0", "--json"}} {
		lookup := func(string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ValidationWithPreparation(ctx, inv, s, func(context.Context) (v2ValidationDependencies, error) {
					t.Fatal("cancelled operation prepared dependencies")
					return v2ValidationDependencies{}, nil
				})
			}, true
		}
		exit, stdout, _ := runValidationCLI(t, args, ctx, lookup)
		if exit != 130 {
			t.Fatalf("exit=%d output=%s", exit, stdout)
		}
	}
}

func TestCLIV2ValidationFixtureInputOutputAliases(t *testing.T) {
	for _, kind := range []string{"same", "hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "request.json")
			if err := os.WriteFile(input, []byte(v2FixtureBody), 0600); err != nil {
				t.Fatal(err)
			}
			output := input
			if kind != "same" {
				output = filepath.Join(dir, "alias")
				link := os.Link
				if kind == "symlink" {
					link = os.Symlink
				}
				if err := link(input, output); err != nil {
					t.Skipf("link unavailable: %v", err)
				}
			}
			exit, _, _ := runValidationCLI(t, []string{"codex", "proxy", "fixture", "create", "--input", input, "--output", output, "--json"}, context.Background(), lookupV2Validation)
			if exit != 6 {
				t.Fatal("input alias accepted")
			}
			body, err := os.ReadFile(input)
			if err != nil || string(body) != v2FixtureBody {
				t.Fatal("input changed")
			}
		})
	}
}
