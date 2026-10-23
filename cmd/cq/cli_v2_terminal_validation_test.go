package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var terminalValidationFixtures = []terminalFixtureRegistration{
	{"cq codex proxy readiness show", "Success", "stdout", "text/cq-codex-proxy-readiness-show/02-success.txt"},
	{"cq codex proxy readiness show", "Failure", "stderr", "text/cq-codex-proxy-readiness-show/03-failure.txt"},
	{"cq codex proxy readiness show", "Empty · no marker", "stderr", "text/cq-codex-proxy-readiness-show/04-empty-no-marker.txt"},
	{"cq codex proxy canary start", "Success", "stdout", "text/cq-codex-proxy-canary-start/02-success.txt"},
	{"cq codex proxy canary start", "Failure", "stderr", "text/cq-codex-proxy-canary-start/03-failure.txt"},
	{"cq codex proxy canary start", "Failure after publication", "stderr", "text/cq-codex-proxy-canary-start/04-failure-after-publication.txt"},
	{"cq codex proxy canary status", "Success", "stdout", "text/cq-codex-proxy-canary-status/02-success.txt"},
	{"cq codex proxy canary status", "Failure", "stderr", "text/cq-codex-proxy-canary-status/03-failure.txt"},
	{"cq codex proxy canary stop", "Success · stop requested, still active", "stdout", "text/cq-codex-proxy-canary-stop/02-success-stop-requested-still-active.txt"},
	{"cq codex proxy canary stop", "Failure", "stderr", "text/cq-codex-proxy-canary-stop/03-failure.txt"},
	{"cq codex proxy fixture create", "Success", "stdout", "text/cq-codex-proxy-fixture-create/02-success.txt"},
	{"cq codex proxy fixture create", "Failure", "stderr", "text/cq-codex-proxy-fixture-create/03-failure.txt"},
	{"cq codex proxy validate http", "Success", "stdout", "text/cq-codex-proxy-validate-http/02-success.txt"},
	{"cq codex proxy validate http", "Failure", "stderr", "text/cq-codex-proxy-validate-http/03-failure.txt"},
	{"cq codex proxy validate http", "Timeout · completion unknown", "stderr", "text/cq-codex-proxy-validate-http/04-timeout-completion-unknown.txt"},
	{"cq codex proxy validate websocket", "Success", "stdout", "text/cq-codex-proxy-validate-websocket/02-success.txt"},
	{"cq codex proxy validate websocket", "Failure", "stderr", "text/cq-codex-proxy-validate-websocket/03-failure.txt"},
	{"cq codex proxy validate websocket", "Timeout · no readiness recorded", "stderr", "text/cq-codex-proxy-validate-websocket/04-timeout-no-readiness-recorded.txt"},
}

func validationFixtureMarker(websocket bool) proxy.CodexReadinessMarker {
	marker := proxy.CodexReadinessMarker{
		Version: 3, Transport: proxy.CodexRoutingHTTP, CQBuild: "1.0.0", ClientBuild: "0.146.0", ParserSchema: 1, LeaseSchema: 3,
		SemanticsRevision: "http-conservative-routing-v3", RetryBudget: 1,
		FixtureHash:        "618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1",
		CQExecutableSHA256: strings.Repeat("2", 64), ClientExecutableSHA256: strings.Repeat("3", 64), ServiceIdentitySHA256: strings.Repeat("4", 64), ServiceKind: "launchd", InstalledResult: "passed",
		CompletedGates: []string{"admitted-no-migration", "deterministic-fallback", "exact-pre-admission-hard429-replay", "frozen-single-transform-envelope", "installed-listener", "terminal-default-once", "v2-journal-runtime", "warm-affinity"},
		ValidatedAt:    time.Date(2026, 9, 23, 15, 25, 0, 0, time.UTC),
	}
	if websocket {
		marker.Transport = proxy.CodexRoutingWebSocket
		marker.ClientBuild = "0.114.0"
		marker.SemanticsRevision = "websocket-terminating-routing-v1"
		marker.FixtureHash = "858d90f3e827194cdfc0dbf5e11d35dfb25e7afc395cf547f17a30936889f9f9"
		marker.CQExecutableSHA256, marker.ClientExecutableSHA256, marker.ServiceIdentitySHA256, marker.ServiceKind = "", "", "", ""
		marker.CompletedGates = []string{"admitted-no-migration", "canonical-terminal-error", "compression-subprotocol", "installed-isolated-client", "persistent-account-upstream", "portable-pre-admission-hard429-rotation", "same-account-candidate-auth-recovery", "strong-frame-authority", "upstream-generation-fence"}
		marker.ValidatedAt = time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC)
	}
	return marker
}

func validationFixtureCanary(start bool) v2CanaryState {
	state := v2CanaryState{
		RunID: strings.Repeat("A", 43), Active: true, StartedAt: "2026-09-22T23:30:00Z", AdmittedTurns: 1248, ConsecutiveCalendarDays: 1,
		Tuple: proxy.CodexCanaryTuple{CQBuild: "1.0.0", ClientBuild: "0.114.0", ParserSchema: 1, LeaseSchema: 3, SemanticsRevision: "http-conservative-routing-v3", RetryBudget: 1, FixtureHash: "618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1", ReadinessFingerprint: "1122334411223344112233441122334411223344112233441122334411223344"},
	}
	last := "2026-09-23T00:00:00Z"
	state.LastObservedAt = &last
	for i, kind := range []string{"account_registry", "codexbar_auth", "codexbar_manifest", "cq_managed_auth", "routing_default", "system_auth"} {
		state.ProtectedDigests = append(state.ProtectedDigests, proxy.CodexCanaryProtectedDigest{Kind: proxy.CodexCanaryProtectionKind(kind), Digest: strings.Repeat(string(rune('a'+i)), 64)})
	}
	if start {
		state.StartedAt, state.LastObservedAt, state.AdmittedTurns, state.ConsecutiveCalendarDays = "2026-09-23T15:30:00Z", nil, 0, 0
	}
	return state
}

func terminalValidationFixtureResult(f terminalFixtureRegistration, invocation string, o cli.TerminalOptions) (string, int) {
	if f.Stream == "stderr" {
		var code, message string
		switch f.File {
		case "text/cq-codex-proxy-readiness-show/03-failure.txt":
			code, message = "readiness_stale", "HTTP readiness evidence does not match the requested build."
		case "text/cq-codex-proxy-readiness-show/04-empty-no-marker.txt":
			code, message = "readiness_missing", "No HTTP readiness evidence is recorded."
		case "text/cq-codex-proxy-canary-start/03-failure.txt":
			code, message = "canary_precondition_failed", "The Codex canary preconditions are not satisfied."
		case "text/cq-codex-proxy-canary-status/03-failure.txt":
			code, message = "canary_missing", "No Codex canary is recorded."
		case "text/cq-codex-proxy-canary-start/04-failure-after-publication.txt", "text/cq-codex-proxy-canary-stop/03-failure.txt":
			code, message = "canary_state_unavailable", "The Codex canary state is unavailable."
		case "text/cq-codex-proxy-fixture-create/03-failure.txt":
			code, message = "fixture_output_exists", "The output file already exists."
		case "text/cq-codex-proxy-validate-http/03-failure.txt":
			code, message = "validation_candidate_unavailable", "The installed candidate service is unavailable."
		case "text/cq-codex-proxy-validate-http/04-timeout-completion-unknown.txt":
			code, message = "validation_timeout", "The HTTP validation operation timed out; inspect readiness before retrying."
		case "text/cq-codex-proxy-validate-websocket/03-failure.txt":
			code, message = "validation_build_mismatch", "The Codex executable does not match --client-build."
		case "text/cq-codex-proxy-validate-websocket/04-timeout-no-readiness-recorded.txt":
			code, message = "validation_timeout", "WebSocket validation timed out; readiness was not recorded."
		}
		var out, err bytes.Buffer
		outcome := v2SelectionFailure(1, code, message)
		if f.File == "text/cq-codex-proxy-readiness-show/03-failure.txt" || f.File == "text/cq-codex-proxy-canary-start/03-failure.txt" || f.File == "text/cq-codex-proxy-fixture-create/03-failure.txt" || f.File == "text/cq-codex-proxy-validate-websocket/03-failure.txt" {
			outcome.ExitCode = 6
		}
		if f.File == "text/cq-codex-proxy-readiness-show/04-empty-no-marker.txt" || f.File == "text/cq-codex-proxy-canary-status/03-failure.txt" {
			outcome.ExitCode = 3
		}
		if f.File == "text/cq-codex-proxy-validate-http/03-failure.txt" {
			outcome.ExitCode = 4
		}
		if strings.Contains(f.Scenario, "Timeout") {
			outcome.ExitCode = 7
		}
		s := &cli.Session{Out: &out, Err: &err, OutTerminal: o, ErrTerminal: o}
		args := strings.Fields(strings.TrimPrefix(invocation, "cq "))
		exit := cli.Run(context.Background(), args, s, func(string) (cli.Handler, bool) {
			return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome { return outcome }, true
		})
		return err.String(), exit
	}
	switch f.Command {
	case "cq codex proxy readiness show":
		return terminalReadiness(validationFixtureMarker(false), "0.146.0", o), 0
	case "cq codex proxy canary start":
		return terminalCanary(validationFixtureCanary(true), "start", o), 0
	case "cq codex proxy canary status":
		return terminalCanary(validationFixtureCanary(false), "status", o), 0
	case "cq codex proxy canary stop":
		return terminalCanary(validationFixtureCanary(false), "stop", o), 0
	case "cq codex proxy validate http":
		return terminalHTTPValidation(29280, o), 0
	case "cq codex proxy validate websocket":
		return terminalWebSocketValidation(validationFixtureMarker(true), o), 0
	case "cq codex proxy fixture create":
		fixture := proxy.SanitisedCodexFixture{SchemaVersion: 1, CapturedAt: time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC), BodyBytes: 2048, BodyHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MetadataSource: "nested", RequestKind: "turn", SessionHint: "session:111122223333", ThreadHint: "thread:444455556666", TurnHint: "turn:777788889999"}
		return terminalFixture("/Users/alex/cq-inputs/fixture.json", fixture, o), 0
	}
	return "", -1
}

func TestTerminalValidationRendererFixtures(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct {
			Command, Scenario, Stream, File, Invocation string
			Exit                                        json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, f := range terminalValidationFixtures {
		registered[f.File] = true
	}
	for _, f := range index.Fixtures {
		if strings.HasPrefix(f.Command, "cq codex proxy ") && strings.Contains("|cq codex proxy readiness show|cq codex proxy canary start|cq codex proxy canary status|cq codex proxy canary stop|cq codex proxy fixture create|cq codex proxy validate http|cq codex proxy validate websocket|", "|"+f.Command+"|") && f.Scenario != "Help" && !registered[f.File] {
			t.Fatalf("unregistered fixture %s", f.File)
		}
	}
	if len(registered) != 18 {
		t.Fatalf("registered %d fixtures", len(registered))
	}
	for _, f := range terminalValidationFixtures {
		t.Run(f.File, func(t *testing.T) {
			found := false
			invocation := ""
			for _, entry := range index.Fixtures {
				if entry.File == f.File {
					found = true
					invocation = entry.Invocation
					if entry.Command != f.Command || entry.Scenario != f.Scenario || entry.Stream != f.Stream {
						t.Fatal("fixture index mismatch")
					}
					break
				}
			}
			if !found {
				t.Fatal("fixture missing from index")
			}
			for _, width := range []int{20, 40, 80, 100, 120} {
				o := terminalFixtureOptions(width)
				got, exit := terminalValidationFixtureResult(f, invocation, o)
				for _, entry := range index.Fixtures {
					if entry.File == f.File {
						var wantExit int
						if err := json.Unmarshal(entry.Exit, &wantExit); err != nil || wantExit != exit {
							t.Fatalf("exit %d; indexed %s", exit, entry.Exit)
						}
					}
				}
				if width >= 100 {
					assertTerminalFixture(t, f.File, got)
				}
				for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Fatalf("width %d overflow: %q", width, line)
					}
				}
				o.Colour = false
				plain, _ := terminalValidationFixtureResult(f, invocation, o)
				if strings.Contains(plain, "\x1b[") || plain != ansi.Strip(got) {
					t.Fatalf("NO_COLOR differs at %d", width)
				}
			}
		})
	}
}

func TestTerminalValidationStateBoundaries(t *testing.T) {
	o := terminalFixtureOptions(100)
	if strings.Contains(terminalHTTPValidation(29280, o), "· passed") {
		t.Fatal("accepted request claimed completed validation")
	}
	state := validationFixtureCanary(false)
	if !strings.Contains(terminalCanary(state, "stop", o), "drain completion has not been observed") {
		t.Fatal("stop claimed completion")
	}
	state.Active = false
	state.Finalised = true
	state.Finalisation = &v2CanaryFinalisation{StopRequestDigest: "stop", ProcessBindingDigest: "process", CountersDigest: "counters", ActiveSessions: 0}
	if !strings.Contains(terminalCanary(state, "status", o), "Finalised") {
		t.Fatal("missing finalisation")
	}
	state.KeyedMismatches = 2
	failed := terminalCanary(state, "status", o)
	if !strings.Contains(failed, "\x1b[1;34m2\x1b[0m") || !strings.Contains(failed, "\x1b[31mFailure observed\x1b[0m") {
		t.Fatal("failure count or cause colour omitted")
	}
}

func TestTerminalValidationAdapterResults(t *testing.T) {
	o := terminalFixtureOptions(100)
	httpCalls := 0
	http := handleV2ValidationWithPreparation(context.Background(), cli.Invocation{Path: "codex proxy validate http", Options: map[string][]string{"port": {"29280"}}}, nil, func(context.Context) (v2ValidationDependencies, error) {
		return v2ValidationDependencies{http: func(context.Context, int, string) error { httpCalls++; return nil }}, nil
	})
	if http.ExitCode != 0 || http.Terminal == nil || httpCalls != 1 || !strings.Contains(ansi.Strip(http.Terminal(o)), "validation has not completed") {
		t.Fatal("HTTP request result changed")
	}
	websocketCalls := 0
	websocket := handleV2ValidationWithPreparation(context.Background(), cli.Invocation{Path: "codex proxy validate websocket", Options: map[string][]string{"client-build": {"0.114.0"}, "state-dir": {validationTempDir(t)}}}, nil, func(context.Context) (v2ValidationDependencies, error) {
		return v2ValidationDependencies{websocket: func(context.Context, context.Context, string, string, string, string) (proxy.CodexReadinessMarker, error) {
			websocketCalls++
			return validationFixtureMarker(true), nil
		}}, nil
	})
	if websocket.ExitCode != 0 || websocket.Terminal == nil || websocketCalls != 1 || !strings.Contains(ansi.Strip(websocket.Terminal(o)), "Gates passed  9") {
		t.Fatal("WebSocket marker result changed")
	}
	deps, _ := newV2CanaryFixture(t)
	start := callV2Canary(t, deps, "start")
	if start.ExitCode != 0 || start.Terminal == nil || !strings.Contains(ansi.Strip(start.Terminal(o)), "Last observed day  unknown") {
		t.Fatal("canary start result changed")
	}
	stop := callV2Canary(t, deps, "stop")
	if stop.ExitCode != 0 || stop.Terminal == nil || !strings.Contains(ansi.Strip(stop.Terminal(o)), "drain completion has not been observed") || !bytes.Equal(start.Data, stop.Data) {
		t.Fatal("stop request changed retained state")
	}
}

func TestTerminalFixtureClockAndSecretBoundary(t *testing.T) {
	dir := t.TempDir()
	input, output := dir+"/input.json", dir+"/fixture.json"
	if err := os.WriteFile(input, []byte(v2FixtureBody), 0600); err != nil {
		t.Fatal(err)
	}
	out := createV2FixtureWithClock(cli.Invocation{Options: map[string][]string{"input": {input}, "output": {output}}}, func() time.Time { return terminalFixtureOptions(100).Now })
	if out.ExitCode != 0 || out.Terminal == nil {
		t.Fatalf("fixture failed: %+v", out)
	}
	terminal := out.Terminal(terminalFixtureOptions(100))
	if !strings.Contains(ansi.Strip(terminal), "Converted          0s ago") || strings.Contains(terminal, "raw-session-sentinel") {
		t.Fatal("fixture time or redaction failed")
	}
	bytes, err := os.ReadFile(output)
	if err != nil || strings.Contains(string(bytes), "raw-session-sentinel") {
		t.Fatal("saved fixture missing or contains raw metadata")
	}
}
