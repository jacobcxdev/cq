package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// The catalogue owns the invocation and expected stream. Adapters own the
// result; these tests do not construct an Outcome for a fixture.
func TestTerminalValidationHandlerFixtures(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct {
			File, Invocation, Stream string
			Exit                     json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]struct {
		Invocation, Stream string
		Exit               int
	})
	for _, entry := range index.Fixtures {
		if entry.Invocation == "" {
			continue
		}
		var exit int
		if err := json.Unmarshal(entry.Exit, &exit); err != nil {
			continue
		}
		entries[entry.File] = struct {
			Invocation, Stream string
			Exit               int
		}{entry.Invocation, entry.Stream, exit}
	}
	for _, fixture := range terminalValidationFixtures {
		entry, ok := entries[fixture.File]
		if !ok || entry.Invocation == "" {
			t.Fatalf("missing indexed invocation for %s", fixture.File)
		}
		t.Run(fixture.File, func(t *testing.T) {
			for _, mode := range []string{"terminal", "no-colour", "plain", "json"} {
				t.Run(mode, func(t *testing.T) {
					lookup, check, fixturePath := terminalValidationHandlerSetup(t, fixture.File)
					args := strings.Fields(strings.TrimPrefix(entry.Invocation, "cq "))
					if mode == "json" {
						args = append(args, "--json")
					}
					var stdout, stderr bytes.Buffer
					options := terminalFixtureOptions(100)
					if mode == "plain" {
						options.Width = 0
					}
					if mode == "no-colour" {
						options.Colour = false
					}
					exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: options, ErrTerminal: options}, lookup)
					if exit != entry.Exit {
						t.Fatalf("indexed invocation %q: exit %d, want %d; stdout=%s stderr=%s", entry.Invocation, exit, entry.Exit, stdout.String(), stderr.String())
					}
					check(t)
					if mode == "json" {
						var envelope struct {
							Command string           `json:"command"`
							Errors  []cli.Diagnostic `json:"errors"`
							Data    json.RawMessage  `json:"data"`
						}
						if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
							t.Fatalf("invalid JSON: %v, %s", err, stdout.String())
						}
						if envelope.Command != strings.TrimPrefix(fixture.Command, "cq ") || stderr.Len() != 0 {
							t.Fatalf("JSON contract or stream changed: %s / %s", stdout.String(), stderr.String())
						}
						if exit == 0 && len(envelope.Data) == 0 {
							t.Fatal("successful JSON resource missing")
						}
						if exit != 0 && len(envelope.Errors) == 0 {
							t.Fatal("JSON error missing")
						}
						return
					}
					if mode == "plain" || mode == "no-colour" {
						if strings.Contains(stdout.String()+stderr.String(), "\x1b[") {
							t.Fatal("colour leaked into colour-free output")
						}
					}
					got := stdout.String()
					if fixture.Stream == "stderr" {
						got = stderr.String()
						if stdout.Len() != 0 {
							t.Fatalf("failure wrote stdout: %s", stdout.String())
						}
					} else if stderr.Len() != 0 {
						t.Fatalf("success wrote stderr: %s", stderr.String())
					}
					if mode == "terminal" || mode == "no-colour" {
						if fixture.Stream == "stderr" || fixture.Command == "cq codex proxy validate http" || fixture.Command == "cq codex proxy validate websocket" {
							assertTerminalFixture(t, fixture.File, got)
						} else if fixture.Command == "cq codex proxy readiness show" {
							assertTerminalFixture(t, fixture.File, strings.ReplaceAll(ansi.Strip(got), "CQ build  "+version, "CQ build  1.0.0"))
						} else {
							terminalValidationDynamicEvidence(t, fixture.File, ansi.Strip(got), fixturePath)
						}
					} else {
						assertTerminalValidationPlain(t, fixture.File, got)
					}
				})
			}
		})
	}
}

func terminalValidationHandlerSetup(t *testing.T, file string) (cli.Lookup, func(*testing.T), string) {
	t.Helper()
	check := func(*testing.T) {}
	if strings.Contains(file, "readiness-show") {
		calls := 0
		deps := v2ValidationDependencies{stateDir: func() (string, error) { return validationTempDir(t), nil }, loadMarker: func(string, proxy.CodexRoutingTransport) (proxy.CodexReadinessMarker, error) {
			calls++
			if strings.Contains(file, "empty-no-marker") {
				return proxy.CodexReadinessMarker{}, os.ErrNotExist
			}
			marker := validationFixtureMarker(false)
			marker.CQBuild = version
			return marker, nil
		}}
		check = func(t *testing.T) {
			if calls != 1 {
				t.Fatalf("marker loads = %d", calls)
			}
		}
		return validationLookup(deps), check, ""
	}
	if strings.Contains(file, "validate-http") {
		calls := 0
		deps := v2ValidationDependencies{http: func(_ context.Context, port int, _ string) error {
			calls++
			if port != 29280 {
				t.Fatalf("port = %d", port)
			}
			if strings.Contains(file, "/03-") {
				return errValidationCandidateUnavailable
			}
			if strings.Contains(file, "/04-") {
				return context.DeadlineExceeded
			}
			return nil
		}}
		check = func(t *testing.T) {
			if calls != 1 {
				t.Fatalf("HTTP transport calls = %d", calls)
			}
		}
		return validationLookup(deps), check, ""
	}
	if strings.Contains(file, "validate-websocket") {
		calls := 0
		deps := v2ValidationDependencies{stateDir: func() (string, error) { return validationTempDir(t), nil }, websocket: func(_ context.Context, _ context.Context, _, build, _, _ string) (proxy.CodexReadinessMarker, error) {
			calls++
			if strings.Contains(file, "/03-") {
				if build != "0.113.0" {
					t.Fatal("indexed build lost")
				}
				return proxy.CodexReadinessMarker{}, proxy.ErrCodexValidationBuildMismatch
			}
			if strings.Contains(file, "/04-") {
				return proxy.CodexReadinessMarker{}, context.DeadlineExceeded
			}
			if build != "0.114.0" {
				t.Fatal("indexed build lost")
			}
			return validationFixtureMarker(true), nil
		}}
		check = func(t *testing.T) {
			if calls != 1 {
				t.Fatalf("WebSocket transport calls = %d", calls)
			}
		}
		return validationLookup(deps), check, ""
	}
	if strings.Contains(file, "fixture-create") {
		dir := t.TempDir()
		input, output := filepath.Join(dir, "request.json"), filepath.Join(dir, "fixture.json")
		output, err := filepath.Abs(output)
		if err != nil {
			t.Fatal(err)
		}
		nestedBody := strings.Replace(v2FixtureBody, `"client_metadata":`, `"client_metadata":{"x-codex-turn-metadata":`, 1)
		nestedBody = strings.TrimSuffix(nestedBody, "}") + "}}"
		body := []byte(nestedBody)
		body = append(body, bytes.Repeat([]byte(" "), 2048-len(body))...)
		if err := os.WriteFile(input, body, 0600); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(file, "/03-") {
			if err := os.WriteFile(output, []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		check = func(t *testing.T) {
			contents, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(file, "/03-") && string(contents) != "existing" {
				t.Fatal("existing fixture overwritten")
			}
			if strings.Contains(string(contents), "raw-session-sentinel") {
				t.Fatal("raw metadata leaked")
			}
		}
		return func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Validation(path)
			return func(_ context.Context, inv cli.Invocation, _ *cli.Session) cli.Outcome {
				// Only the catalogue's synthetic home paths are redirected to local files.
				if inv.Options["input"][0] != "/Users/alex/cq-inputs/request.json" || inv.Options["output"][0] != "/Users/alex/cq-inputs/fixture.json" {
					t.Fatal("indexed fixture paths changed")
				}
				inv.Options["input"], inv.Options["output"] = []string{input}, []string{output}
				return createV2FixtureWithClock(inv, func() time.Time { return terminalFixtureOptions(100).Now })
			}, ok
		}, check, output
	}
	deps, _ := newV2CanaryFixture(t)
	deps.now = func() time.Time { return terminalFixtureOptions(100).Now }
	deps.currentTuple = func() (proxy.CodexCanaryTuple, error) { return validationFixtureCanary(true).Tuple, nil }
	if strings.Contains(file, "canary-start/03-") {
		if out := callV2Canary(t, deps, "start"); out.ExitCode != 0 {
			t.Fatal(out)
		}
	}
	if strings.Contains(file, "canary-status/02-") || strings.Contains(file, "canary-stop/02-") {
		deps.now = func() time.Time { return terminalFixtureOptions(100).Now.Add(-16 * time.Hour) }
		if out := callV2Canary(t, deps, "start"); out.ExitCode != 0 {
			t.Fatal(out)
		}
		recorder, err := proxy.OpenServingCodexCanary(deps.fs, deps.path, deps.protected)
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.RecordAdmitted(terminalFixtureOptions(100).Now.Add(-15*time.Hour - 30*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(file, "canary-stop/02-") {
			check = func(t *testing.T) {
				request, err := deps.fs.ReadFile("/state/codex-canary-stop/request.json")
				if err != nil || len(request) == 0 {
					t.Fatal("stop request was not persisted")
				}
				retained, err := proxy.OpenCodexCanary(deps.fs, deps.path, deps.protected)
				if err != nil || !retained.State().Active || retained.State().Finalisation != nil {
					t.Fatal("stop request changed finalisation state")
				}
			}
		}
	}
	if strings.Contains(file, "canary-stop/03-") {
		if out := callV2Canary(t, deps, "start"); out.ExitCode != 0 {
			t.Fatal(out)
		}
		if err := deps.fs.WriteFile(deps.path, []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(file, "canary-start/04-") {
		directory := validationTempDir(t)
		fsys := &v2CanaryPostRenameFS{stateDirectory: directory}
		deps.fs, deps.path = fsys.OSFileSystem, filepath.Join(directory, "canary.json")
		if out := callV2Canary(t, deps, "start"); out.ExitCode != 0 {
			t.Fatal(out)
		}
		setV2CanaryFixtureFinalised(t, deps, true)
		deps.fs, fsys.failSync = fsys, true
		check = func(t *testing.T) {
			if !fsys.published {
				t.Fatal("post-publication failure was not exercised")
			}
		}
	}
	return func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Canary(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2CanaryWithDependencies(ctx, inv, s, deps)
		}, ok
	}, check, ""
}

func terminalValidationDynamicEvidence(t *testing.T, file, output, fixturePath string) {
	t.Helper()
	wantBytes, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", file))
	if err != nil {
		t.Fatal(err)
	}
	want := string(wantBytes)
	switch {
	case strings.Contains(file, "canary-start/02-"), strings.Contains(file, "canary-status/02-"), strings.Contains(file, "canary-stop/02-"):
		output = normaliseValidationField(t, output, want, "Run", strings.Repeat("A", 43), `[A-Za-z0-9_-]{43}`)
		for i, kind := range []string{"account_registry", "codexbar_auth", "codexbar_manifest", "cq_managed_auth", "routing_default", "system_auth"} {
			output = normaliseValidationField(t, output, want, kind, strings.Repeat(string(rune('a'+i)), 64), `[0-9a-f]{64}`)
		}
		if !strings.Contains(file, "canary-start/02-") {
			output = normaliseValidationField(t, output, want, "Admitted turns", "1,248", `[0-9]+(?:,[0-9]{3})*`)
		}
	case strings.Contains(file, "fixture-create/02-"):
		output = normaliseValidationPath(t, output, want, fixturePath)
		output = normaliseValidationField(t, output, want, "Session hint", "session:111122223333", `session:[0-9a-f]{12}`)
		output = normaliseValidationField(t, output, want, "Thread hint", "thread:444455556666", `thread:[0-9a-f]{12}`)
		output = normaliseValidationField(t, output, want, "Turn hint", "turn:777788889999", `turn:[0-9a-f]{12}`)
		output = normaliseValidationField(t, output, want, "Body SHA-256", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", `[0-9a-f]{64}`)
	default:
		t.Fatalf("unclassified dynamic fixture %s", file)
	}
	if output != want {
		t.Fatalf("full handler fixture %s differs\nwant:\n%s\ngot:\n%s", file, want, output)
	}
}

func normaliseValidationPath(t *testing.T, got, want, path string) string {
	t.Helper()
	if !filepath.IsAbs(path) || filepath.Base(path) != "fixture.json" {
		t.Fatalf("fixture path is not a native absolute fixture path: %q", path)
	}
	actualLine := ansi.Strip(validationField(terminalFixtureOptions(100), "File", 19, validationID(path)))
	wantLine := "  File               /Users/alex/cq-inputs/fixture.json\n"
	if (!strings.HasPrefix(actualLine, "  File               ") && !strings.HasPrefix(actualLine, "  File\n  ")) || strings.Count(got, actualLine) != 1 || strings.Count(want, wantLine) != 1 {
		t.Fatalf("fixture path line differs: %q", actualLine)
	}
	return strings.Replace(got, actualLine, wantLine, 1)
}

func TestNormaliseValidationPathWrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), strings.Repeat("segment", 20), "fixture.json")
	line := ansi.Strip(validationField(terminalFixtureOptions(100), "File", 19, validationID(path)))
	if !strings.Contains(strings.TrimSuffix(line, "\n"), "\n  ") {
		t.Fatalf("expected a wrapped native path, got %q", line)
	}
	want := "Fixture · created\n  File               /Users/alex/cq-inputs/fixture.json\n  Request kind       turn\n"
	got := "Fixture · created\n" + line + "  Request kind       turn\n"
	if normalised := normaliseValidationPath(t, got, want, path); normalised != want {
		t.Fatalf("normalised wrapped path differs: %q", normalised)
	}
}

func normaliseValidationField(t *testing.T, got, want, label, placeholder, valuePattern string) string {
	t.Helper()
	prefix := ""
	for _, line := range strings.Split(want, "\n") {
		if strings.HasPrefix(line, "  "+label) && strings.HasSuffix(line, placeholder) {
			prefix = strings.TrimSuffix(line, placeholder)
			break
		}
	}
	if prefix == "" {
		t.Fatalf("authored field %s missing", label)
	}
	lines, matches := strings.Split(got, "\n"), 0
	pattern := regexp.MustCompile(`^` + valuePattern + `$`)
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			value := strings.TrimPrefix(line, prefix)
			if !pattern.MatchString(value) {
				t.Fatalf("invalid %s value %q", label, value)
			}
			lines[i] = prefix + placeholder
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("field %s occurred %d times", label, matches)
	}
	return strings.Join(lines, "\n")
}

func assertTerminalValidationPlain(t *testing.T, file, got string) {
	t.Helper()
	if strings.Contains(file, "/03-") || strings.Contains(file, "/04-") {
		fixture, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", file))
		if err != nil {
			t.Fatal(err)
		}
		message, _, ok := strings.Cut(strings.TrimPrefix(string(fixture), "Error · "), "\n")
		if !ok || got != "cq: "+message+"\n" {
			t.Fatalf("plain error changed: %q", got)
		}
		return
	}
	switch {
	case strings.Contains(file, "readiness-show"):
		if got != "HTTP readiness: current\nCodex build: 0.146.0\nValidated: 2026-09-23T15:25:00Z\n" {
			t.Fatalf("plain readiness changed: %q", got)
		}
	case strings.Contains(file, "validate-http"):
		if got != "HTTP validation requested on candidate port 29280.\nValidation has not completed.\n" {
			t.Fatalf("plain request changed: %q", got)
		}
	case strings.Contains(file, "validate-websocket"):
		if got != "WebSocket validation: passed\nCodex build: 0.114.0\nValidated: 2026-09-23T15:30:00Z\n" {
			t.Fatalf("plain WebSocket changed: %q", got)
		}
	case strings.Contains(file, "fixture-create"):
		if !strings.HasPrefix(got, "Created sanitised fixture: ") || !strings.HasSuffix(got, "/fixture.json\n") {
			t.Fatalf("plain fixture changed: %q", got)
		}
	case strings.Contains(file, "canary"):
		if !strings.HasPrefix(got, "Canary: ") || !strings.Contains(got, "\nActive: true\nFinalised: false\nAdmitted turns: ") {
			t.Fatalf("plain canary changed: %q", got)
		}
	default:
		t.Fatalf("unclassified plain fixture: %s", file)
	}
}
