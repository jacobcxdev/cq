package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

var terminalModelsFixtures = []terminalFixtureRegistration{
	{"cq models list", "Success", "stdout", "text/cq-models-list/02-success.txt"},
	{"cq models list", "Empty", "stdout", "text/cq-models-list/03-empty.txt"},
	{"cq models list", "Failure", "stderr", "text/cq-models-list/04-failure.txt"},
	{"cq models refresh", "Success", "stdout", "text/cq-models-refresh/02-success.txt"},
	{"cq models refresh", "Failure", "stderr", "text/cq-models-refresh/03-failure.txt"},
	{"cq models refresh", "Partial publication", "stdout", "text/cq-models-refresh/04-partial-publication.txt"},
	{"cq models refresh", "Partial publication", "stderr", "text/cq-models-refresh/05-partial-publication.txt"},
	{"cq models overlay add", "Success", "stdout", "text/cq-models-overlay-add/02-success.txt"},
	{"cq models overlay add", "Failure before saving", "stderr", "text/cq-models-overlay-add/03-failure-before-saving.txt"},
	{"cq models overlay add", "Saved overlay, partial publication", "stdout", "text/cq-models-overlay-add/04-saved-overlay-partial-publication.txt"},
	{"cq models overlay add", "Saved overlay, partial publication", "stderr", "text/cq-models-overlay-add/05-saved-overlay-partial-publication.txt"},
	{"cq models overlay remove", "Success", "stdout", "text/cq-models-overlay-remove/02-success.txt"},
	{"cq models overlay remove", "Failure", "stderr", "text/cq-models-overlay-remove/03-failure.txt"},
	{"cq models overlay remove", "Removed overlay, partial publication", "stdout", "text/cq-models-overlay-remove/04-removed-overlay-partial-publication.txt"},
	{"cq models overlay remove", "Removed overlay, partial publication", "stderr", "text/cq-models-overlay-remove/05-removed-overlay-partial-publication.txt"},
	{"cq models overlay prune", "Success", "stdout", "text/cq-models-overlay-prune/02-success.txt"},
	{"cq models overlay prune", "No-op", "stdout", "text/cq-models-overlay-prune/03-no-op.txt"},
	{"cq models overlay prune", "Failure", "stderr", "text/cq-models-overlay-prune/04-failure.txt"},
}

func terminalModelsFixturePublication(active int, via string, partial bool) ModelPublication {
	code, message := "models_store_failed", "Cannot write model cache."
	targetStatus, targetReason := "written", "published"
	if partial {
		targetStatus, targetReason = "failed", "write_failed"
	}
	target := ModelPublicationTarget{Target: "codex_cache", Path: "/Users/alex/.codex/models_cache.json", Status: targetStatus, Reason: targetReason}
	if partial {
		target.ErrorCode, target.Message = &code, &message
	}
	status := "complete"
	if partial {
		status = "partial"
	}
	return ModelPublication{Status: status, Via: via, ActiveCount: active,
		Sources: []ModelSourceResult{{Provider: "claude", Status: "refreshed"}, {Provider: "codex", Status: "refreshed", NativeCount: active}},
		Targets: []ModelPublicationTarget{target,
			{Target: "claude_capabilities", Path: "/Users/alex/.claude/cache/model-capabilities.json", Status: "skipped", Reason: "optional_client_absent"},
			{Target: "claude_picker", Path: "/Users/alex/.claude/cache/model-picker.json", Status: "skipped", Reason: "optional_client_absent"}},
	}
}

func terminalModelsFixtureEntry() ModelEntry {
	name, description, visibility, clone := "Example Code 1", "Fictional coding model for presentation fixtures", "list", "example-code-1"
	context, output, priority := 200000, 16000, 1
	return ModelEntry{Provider: "codex", ID: "example-code-2", DisplayName: &name, Description: &description,
		ContextWindow: &context, MaxContextWindow: &context, MaxOutputTokens: &output, Visibility: &visibility,
		Priority: &priority, Source: "overlay", CloneFrom: &clone, InferredFrom: &clone, Aliases: []string{}}
}

func terminalModelsFixtureOutcome(f terminalFixtureRegistration) cli.Outcome {
	command, scenario := f.Command, f.Scenario
	partial := strings.Contains(scenario, "partial publication") || scenario == "Partial publication"
	publication := terminalModelsFixturePublication(1, "proxy", partial)
	if strings.Contains(command, "overlay") {
		publication.Via = "local"
	}
	if command == "cq models overlay add" {
		publication.ActiveCount = 2
	}
	if command == "cq models overlay prune" {
		publication.Via = "proxy"
		publication.ActiveCount = 2
		publication.Sources[1].NativeCount = 2
	}
	if f.Stream == "stderr" && !partial {
		code, message, exit := "models_store_failed", "Cannot access model registry state: invalid local registry data.", 1
		switch command {
		case "cq models refresh":
			code, message = "models_refresh_failed", "Model refresh failed: proxy registry refresh unavailable."
		case "cq models overlay add":
			code, message, exit = "models_clone_not_found", "Native clone source codex/missing-code was not found; run cq models refresh and cq models list --provider codex.", 3
		case "cq models overlay remove":
			code, message, exit = "models_overlay_not_found", "Overlay codex/missing-code was not found.", 3
		case "cq models overlay prune":
			message = "Cannot access model registry state: cannot read overlays."
		}
		return modelsFailure(exit, code, message)
	}
	if partial {
		out := modelsFailure(8, "models_refresh_partial", "Model refresh completed partially; inspect source and publication results.")
		if f.Stream == "stderr" {
			return out
		}
	}
	var out cli.Outcome
	switch command {
	case "cq models list":
		rows := []ModelEntry{}
		if scenario == "Success" {
			row := terminalModelsFixtureEntry()
			row.ID, row.Source, row.Aliases, row.CloneFrom, row.InferredFrom = "example-code-1", "native", []string{"example-code"}, nil, nil
			rows = append(rows, row)
		}
		provider := ""
		if len(rows) > 0 {
			provider = "codex"
		}
		out.Terminal = func(o cli.TerminalOptions) string { return terminalModelsList(rows, provider, o) }
	case "cq models refresh":
		if partial {
			out = modelsFailure(8, "models_refresh_partial", "Model refresh completed partially; inspect source and publication results.")
		}
		out.Terminal = func(o cli.TerminalOptions) string { return terminalModelsPublication(publication, o) }
	case "cq models overlay add":
		if partial {
			out = modelsFailure(8, "models_refresh_partial", "Model refresh completed partially; inspect source and publication results.")
		}
		entry := terminalModelsFixtureEntry()
		out.Terminal = func(o cli.TerminalOptions) string {
			return terminalModelsOverlayAdd(entry, ModelCloneSelectionResult{Mode: "explicit", SourceID: entry.CloneFrom}, publication, out.ExitCode, o)
		}
	case "cq models overlay remove":
		if partial {
			out = modelsFailure(8, "models_refresh_partial", "Model refresh completed partially; inspect source and publication results.")
		}
		out.Terminal = func(o cli.TerminalOptions) string {
			return terminalModelsOverlayRemove("codex", "example-code-2", publication, out.ExitCode, o)
		}
	case "cq models overlay prune":
		removed := []ModelIdentity{}
		if scenario == "Success" {
			removed = append(removed, ModelIdentity{Provider: "codex", ID: "example-code-2"})
		}
		out.Terminal = func(o cli.TerminalOptions) string {
			return terminalModelsOverlayPrune(removed, publication, out.ExitCode, o)
		}
	}
	return out
}

func TestTerminalModelsFixtures(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct {
			File string
			Exit json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	exits := map[string]json.RawMessage{}
	for _, f := range index.Fixtures {
		exits[f.File] = f.Exit
	}
	for _, f := range terminalModelsFixtures {
		t.Run(f.File, func(t *testing.T) {
			for _, width := range []int{20, 40, 80, 100, 120} {
				var stdout, stderr bytes.Buffer
				s := &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(width), ErrTerminal: terminalFixtureOptions(width)}
				args := strings.Fields(strings.TrimPrefix(f.Command, "cq "))
				if strings.HasSuffix(f.Command, "overlay add") {
					args = append(args, "--provider", "codex", "--id", "example-code-2")
				}
				if strings.HasSuffix(f.Command, "overlay remove") {
					args = append(args, "--provider", "codex", "--id", "example-code-2")
				}
				exit := cli.Run(context.Background(), args, s, func(string) (cli.Handler, bool) {
					return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome {
						return terminalModelsFixtureOutcome(f)
					}, true
				})
				var wantExit int
				if err := json.Unmarshal(exits[f.File], &wantExit); err != nil {
					t.Fatal(err)
				}
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
						t.Fatalf("width %d: %q", width, line)
					}
				}
				if f.Stream == "stdout" && stderr.Len() > 0 && exit == 0 {
					t.Fatal("unexpected stderr")
				}
				if f.Stream == "stderr" && stdout.Len() > 0 {
					t.Fatal("unexpected stdout")
				}
			}
		})
	}
}

func TestTerminalModelsRegistration(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct{ Fixtures []terminalFixtureRegistration }
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	registered := map[string]terminalFixtureRegistration{}
	for _, f := range terminalModelsFixtures {
		if _, exists := registered[f.File]; exists {
			t.Fatal("duplicate", f.File)
		}
		registered[f.File] = f
	}
	for _, f := range index.Fixtures {
		if f.Scenario == "Help" || !strings.HasPrefix(f.Command, "cq models ") || f.Command == "cq models overlay" {
			continue
		}
		r, exists := registered[f.File]
		if !exists || r.Command != f.Command || r.Scenario != f.Scenario || r.Stream != f.Stream {
			t.Fatal("missing or mismatched fixture", f.File)
		}
		delete(registered, f.File)
	}
	if len(registered) != 0 {
		t.Fatal("unrecognised registrations", fmt.Sprint(registered))
	}
}

func TestTerminalModelsRolesWidthsAndColourGates(t *testing.T) {
	entry := terminalModelsFixtureEntry()
	name := "failed\x1b[31m界界"
	entry.DisplayName = &name
	entry.ID = strings.Repeat("failed-model-", 12)
	negative := -1
	entry.Priority = &negative
	pub := terminalModelsFixturePublication(2, "local", true)
	pub.Sources[0].Status = "failed"
	pub.Sources[0].ErrorCode = modelString("models_source_failed")
	pub.Sources[0].Message = modelString("Source unavailable.")
	for _, width := range []int{20, 40, 80, 100, 120} {
		o := terminalFixtureOptions(width)
		got := terminalModelsOverlayAdd(entry, ModelCloneSelectionResult{Mode: "explicit", SourceID: entry.CloneFrom}, pub, 8, o)
		for _, role := range []string{"\x1b[36mfailed-model-", "\x1b[32mSaved", "\x1b[33mPublication", "\x1b[1;34m2", "\x1b[31mFailed"} {
			if !strings.Contains(got, role) {
				t.Fatalf("width %d missing role %q", width, role)
			}
		}
		if strings.Contains(got, "\x1b[31m界") || strings.Contains(got, "184467440") || !strings.Contains(ansi.Strip(got), "Priority") || !strings.Contains(ansi.Strip(got), "unknown") {
			t.Fatal("identity or signed quantity mistreated", got)
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d: %q", width, line)
			}
		}
		o.Colour = false
		if plain := terminalModelsOverlayAdd(entry, ModelCloneSelectionResult{Mode: "explicit", SourceID: entry.CloneFrom}, pub, 8, o); plain != ansi.Strip(got) {
			t.Fatal("NO_COLOR changed content")
		}
	}
	for _, tc := range []struct {
		noColour, term string
		colour         bool
	}{{"", "xterm-256color", true}, {"1", "xterm-256color", false}, {"", "dumb", false}} {
		t.Setenv("NO_COLOR", tc.noColour)
		t.Setenv("TERM", tc.term)
		fixed := terminalFixtureOptions(100)
		o := terminalPresentation(100, fixed.Now, fixed.Location)
		got := terminalModelsList([]ModelEntry{entry}, "codex", o)
		if strings.Contains(got, "\x1b[") != tc.colour {
			t.Fatal("environment colour gate", got)
		}
	}
}

func TestTerminalModelsAdaptersReceiptsAndStreams(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		exit      int
		refreshes int
		receipt   string
	}{
		{"list", []string{"models", "list"}, 0, 0, "Models ·"},
		{"refresh partial", []string{"models", "refresh"}, 8, 1, "Publication · Partial"},
		{"add partial", []string{"models", "overlay", "add", "--provider", "codex", "--id", "overlay-example", "--clone-from", "native-example"}, 8, 1, "Model overlay · Saved · Publication incomplete"},
		{"remove partial", []string{"models", "overlay", "remove", "--provider", "codex", "--id", "overlay-example"}, 8, 1, "Model overlay · Removed · Publication incomplete"},
		{"prune partial", []string{"models", "overlay", "prune"}, 8, 1, "Model overlays · Pruned 1"},
		{"prune save failure", []string{"models", "overlay", "prune"}, 1, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModelTestFixture(t)
			if tc.name != "list" {
				m.fs.fail = "model-capabilities.json.tmp"
			}
			if tc.name == "remove partial" {
				m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "overlay-example", Source: modelregistry.SourceOverlay})
			}
			if tc.name == "prune partial" {
				m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "native-example", Source: modelregistry.SourceOverlay})
			}
			if tc.name == "prune save failure" {
				m.extraNative = true
				m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "native-example", Source: modelregistry.SourceOverlay})
				m.fs.fail = "models.json.tmp"
			}
			var stdout, stderr bytes.Buffer
			s := &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(100), ErrTerminal: terminalFixtureOptions(100)}
			var captured cli.Outcome
			lookup := func(path string) (cli.Handler, bool) {
				return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
					captured = handleV2ModelsWithDependencies(ctx, inv, session, m.deps)
					return captured
				}, true
			}
			if exit := cli.Run(context.Background(), tc.args, s, lookup); exit != tc.exit {
				t.Fatalf("exit %d want %d", exit, tc.exit)
			}
			if m.fixture.calls["refresh"] != tc.refreshes || m.fixture.calls["resolve"] != 1 {
				t.Fatalf("unexpected calls: %v", m.fixture.calls)
			}
			if !strings.Contains(ansi.Strip(stdout.String()), tc.receipt) {
				t.Fatal("missing receipt", stdout.String())
			}
			if tc.name == "add partial" || tc.name == "remove partial" {
				if !bytes.Contains(captured.Data, []byte(`"overlay_saved":true`)) && !bytes.Contains(captured.Data, []byte(`"overlay_removed":true`)) {
					t.Fatal("committed receipt lost", string(captured.Data))
				}
				if !strings.Contains(stderr.String(), "\x1b[33mWarning") {
					t.Fatal("partial diagnostic role", stderr.String())
				}
			}
			if tc.name == "prune save failure" {
				if strings.Contains(stdout.String(), "Pruned") || !bytes.Contains(captured.Data, []byte(`"removed_count":0`)) {
					t.Fatal("save failure claimed removal", stdout.String(), string(captured.Data))
				}
			}
			if tc.name == "prune partial" && (!bytes.Contains(captured.Data, []byte(`"removed_count":1`)) || !strings.Contains(stdout.String(), "Publication incomplete")) {
				t.Fatal("saved prune receipt lost", stdout.String(), string(captured.Data))
			}
			if captured.Terminal != nil {
				_ = captured.Terminal(terminalFixtureOptions(40))
				if m.fixture.calls["refresh"] != tc.refreshes {
					t.Fatal("renderer called refresh")
				}
			}
		})
	}
	// The terminal callback has no effect on the redirected human or JSON paths.
	m := newModelTestFixture(t)
	var plain, diagnostics bytes.Buffer
	if exit := cli.Run(context.Background(), []string{"models", "list"}, &cli.Session{Out: &plain, Err: &diagnostics}, m.fixture.Lookup); exit != 0 {
		t.Fatal(exit)
	}
	if plain.String() != "MODEL\tPROVIDER\tSOURCE\nclaude-example\tclaude\tnative\nnative-example\tcodex\tnative\n" {
		t.Fatal("plain bytes changed", plain.String())
	}
	var machine bytes.Buffer
	if exit := cli.Run(context.Background(), []string{"models", "list", "--json"}, &cli.Session{Out: &machine, Err: &diagnostics, OutTerminal: terminalFixtureOptions(100)}, m.fixture.Lookup); exit != 0 || !json.Valid(machine.Bytes()) || bytes.Contains(machine.Bytes(), []byte("\x1b[")) {
		t.Fatal("JSON changed", machine.String())
	}
	if m.fixture.calls["refresh"] != 0 {
		t.Fatal("list fetched source")
	}
}

func TestTerminalModelsIndependentStreamGates(t *testing.T) {
	f := terminalFixtureRegistration{Command: "cq models overlay add", Scenario: "Saved overlay, partial publication", Stream: "stdout"}
	for _, tc := range []struct{ outTTY, errTTY, colour, jsonMode bool }{
		{true, true, true, false}, {true, false, true, false}, {false, true, true, false},
		{true, true, false, false}, {true, true, true, true},
	} {
		var stdout, stderr bytes.Buffer
		out, errOptions := terminalFixtureOptions(100), terminalFixtureOptions(100)
		out.Colour, errOptions.Colour = tc.colour, tc.colour
		if !tc.outTTY {
			out.Width = 0
		}
		if !tc.errTTY {
			errOptions.Width = 0
		}
		args := []string{"models", "overlay", "add", "--provider", "codex", "--id", "example-code-2"}
		if tc.jsonMode {
			args = append(args, "--json")
		}
		fixture := terminalModelsFixtureOutcome(f)
		fixture.Human = "Overlay saved: codex/example-code-2\nMetadata source: example-code-1 (explicit)\nPublication: partial\n"
		fixture.Data = json.RawMessage(`{"overlay_saved":true}`)
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: out, ErrTerminal: errOptions}, func(string) (cli.Handler, bool) {
			return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome { return fixture }, true
		})
		if exit != 8 {
			t.Fatal("exit", exit)
		}
		if strings.Contains(stdout.String(), "\x1b[") != (tc.outTTY && tc.colour && !tc.jsonMode) {
			t.Fatal("stdout colour gate", stdout.String())
		}
		if strings.Contains(stderr.String(), "\x1b[") != (tc.errTTY && tc.colour && !tc.jsonMode) {
			t.Fatal("stderr colour gate", stderr.String())
		}
		if !tc.outTTY && !tc.jsonMode && stdout.String() != fixture.Human {
			t.Fatal("redirected human changed", stdout.String())
		}
		if tc.jsonMode && (!json.Valid(stdout.Bytes()) || !bytes.Contains(stdout.Bytes(), []byte(`"overlay_saved":true`))) {
			t.Fatal("JSON changed", stdout.String())
		}
	}
}

func TestTerminalModelsFilteredEmptyScope(t *testing.T) {
	for _, width := range []int{20, 40, 80, 100, 120} {
		o := terminalFixtureOptions(width)
		got := terminalModelsList(nil, "codex", o)
		plain := ansi.Strip(got)
		if !strings.Contains(plain, "Models · 0 · Codex") || !strings.Contains(got, "\x1b[36mCodex") || (width >= 100 && !strings.Contains(plain, "No Codex models in the local registry.")) {
			t.Fatal("filtered empty scope lost", got)
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d: %q", width, line)
			}
		}
	}
}

func TestTerminalModelsUnknownAndKnownAbsentMetadata(t *testing.T) {
	entry := ModelEntry{Provider: "codex", ID: "model-1", Source: "native"}
	for _, width := range []int{20, 40, 80, 100, 120} {
		o := terminalFixtureOptions(width)
		for _, got := range []string{terminalModelsList([]ModelEntry{entry}, "codex", o), terminalModelsOverlayAdd(entry, ModelCloneSelectionResult{Mode: "none"}, terminalModelsFixturePublication(1, "local", false), 0, o)} {
			plain := ansi.Strip(got)
			for _, label := range []string{"Description", "Context", "Maximum context", "Maximum output", "Visibility", "Priority"} {
				if !strings.Contains(plain, label) {
					t.Fatalf("missing %s: %s", label, plain)
				}
			}
			if strings.Count(plain, "unknown") < 7 || !strings.Contains(got, "\x1b[33munknown") {
				t.Fatal("missing metadata became known absence", got)
			}
			for _, label := range []string{"Aliases", "Explicit clone", "Metadata source"} {
				if !strings.Contains(plain, label) {
					t.Fatalf("known absence %s omitted: %s", label, plain)
				}
			}
			if strings.Count(plain, "none") < 3 {
				t.Fatal("known absence became unknown", plain)
			}
			for _, line := range strings.Split(got, "\n") {
				if ansi.StringWidth(line) > min(width, 100) {
					t.Fatalf("width %d: %q", width, line)
				}
			}
		}
	}
	empty := ""
	entry.DisplayName, entry.Description, entry.Visibility = &empty, &empty, &empty
	got := ansi.Strip(terminalModelsList([]ModelEntry{entry}, "codex", terminalFixtureOptions(100)))
	if strings.Count(got, "unknown") < 4 {
		t.Fatal("empty metadata became known absence", got)
	}
	zero := 0
	entry.ContextWindow, entry.Priority = &zero, &zero
	got = ansi.Strip(terminalModelsList([]ModelEntry{entry}, "codex", terminalFixtureOptions(100)))
	if !strings.Contains(got, "Context           0 tokens") || !strings.Contains(got, "Priority          0") {
		t.Fatal("known zero became unknown", got)
	}
}

func TestTerminalModelsFailedPublicationEvidence(t *testing.T) {
	p := failedModelsPublication("proxy")
	for _, width := range []int{20, 40, 80, 100, 120} {
		got := terminalModelsPublication(p, terminalFixtureOptions(width))
		plain := ansi.Strip(got)
		if !strings.Contains(plain, "Publication") || !strings.Contains(plain, "Models: unknown") || !strings.Contains(plain, "Claude") || !strings.Contains(plain, "Codex") || !strings.Contains(plain, "Client caches") || (width >= 100 && (!strings.Contains(plain, "models_source_failed: Model source refresh failed.") || !strings.Contains(plain, "No client cache publication results."))) {
			t.Fatal("failed publication evidence lost", got)
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d: %q", width, line)
			}
		}
	}
	entry := terminalModelsFixtureEntry()
	got := ansi.Strip(terminalModelsOverlayAdd(entry, ModelCloneSelectionResult{Mode: "explicit", SourceID: entry.CloneFrom}, p, 1, terminalFixtureOptions(100)))
	if !strings.Contains(got, "Model overlay · Saved") || !strings.Contains(got, "models_source_failed") {
		t.Fatal("saved overlay hid failed source", got)
	}
	p.Sources[0].ErrorCode, p.Sources[0].Message = nil, nil
	got = ansi.Strip(terminalModelsPublication(p, terminalFixtureOptions(100)))
	if !strings.Contains(got, "Claude    Failed") || !strings.Contains(got, "Reason: unknown") {
		t.Fatal("failed source without reason claimed known result", got)
	}
}

func TestTerminalModelsCompletePublicationOnNonzeroExit(t *testing.T) {
	p := terminalModelsFixturePublication(2, "proxy", false)
	o := terminalFixtureOptions(100)
	entry := terminalModelsFixtureEntry()
	for _, got := range []string{
		terminalModelsOverlayAdd(entry, ModelCloneSelectionResult{Mode: "explicit", SourceID: entry.CloneFrom}, p, 1, o),
		terminalModelsOverlayRemove("codex", entry.ID, p, 1, o),
	} {
		plain := ansi.Strip(got)
		if !strings.Contains(plain, "Publication · Complete") || !strings.Contains(plain, "Codex cache · Written · Published") || !strings.Contains(plain, "Command incomplete") || !strings.Contains(plain, "Model overlay · ") {
			t.Fatal("explicit publication receipt lost on nonzero exit", plain)
		}
	}
	unknown := ModelPublication{Status: "unknown", Via: "proxy"}
	got := ansi.Strip(terminalModelsPublication(unknown, o))
	if !strings.Contains(got, "Publication · Unknown") || !strings.Contains(got, "Models: unknown") || strings.Contains(got, "Written") || strings.Contains(got, "0 active") {
		t.Fatal("unknown publication promoted to complete", got)
	}
}

func TestTerminalModelsAdapterPreservesCompleteReceiptAfterCancellation(t *testing.T) {
	for _, args := range [][]string{
		{"models", "refresh"},
		{"models", "overlay", "add", "--provider", "codex", "--id", "overlay-example", "--clone-from", "native-example"},
		{"models", "overlay", "remove", "--provider", "codex", "--id", "overlay-example"},
	} {
		t.Run(strings.Join(args[:2], " ")+" "+args[len(args)-1], func(t *testing.T) {
			m := newModelTestFixture(t)
			if len(args) > 2 && args[2] == "remove" {
				m.save(t, modelregistry.Entry{Provider: modelregistry.ProviderCodex, ID: "overlay-example", Source: modelregistry.SourceOverlay})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := terminalModelsFixturePublication(2, "proxy", false)
			m.deps.Refresh = func(context.Context, modelsDeps) (ModelPublication, []ModelIdentity, error) {
				m.fixture.Call("refresh")
				cancel()
				return p, nil, nil
			}
			var stdout, stderr bytes.Buffer
			exit := cli.Run(ctx, args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(100), ErrTerminal: terminalFixtureOptions(100)}, m.fixture.Lookup)
			plain := ansi.Strip(stdout.String())
			if exit != 130 || !strings.Contains(plain, "Publication · Complete") || !strings.Contains(plain, "Codex cache · Written · Published") || m.fixture.calls["refresh"] != 1 {
				t.Fatal("complete receipt lost after cancellation", exit, plain, m.fixture.calls)
			}
			if len(args) > 2 && (!strings.Contains(plain, "Command incomplete") || !strings.Contains(plain, "Model overlay · ")) {
				t.Fatal("command status conflated with publication", plain)
			}
			if !strings.Contains(stderr.String(), "interrupted") {
				t.Fatal("interruption diagnostic lost", stderr.String())
			}
		})
	}
}

func TestTerminalModelsAdapterKeepsFailedSourceReceiptAfterSave(t *testing.T) {
	m := newModelTestFixture(t)
	m.deps.Refresh = func(context.Context, modelsDeps) (ModelPublication, []ModelIdentity, error) {
		m.fixture.Call("refresh")
		return failedModelsPublication("proxy"), nil, errors.New("source unavailable")
	}
	var stdout, stderr bytes.Buffer
	args := []string{"models", "overlay", "add", "--provider", "codex", "--id", "overlay-example", "--clone-from", "native-example"}
	var captured cli.Outcome
	exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(100), ErrTerminal: terminalFixtureOptions(100)}, func(string) (cli.Handler, bool) {
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			captured = handleV2ModelsWithDependencies(ctx, inv, s, m.deps)
			return captured
		}, true
	})
	plain := ansi.Strip(stdout.String())
	if exit != 1 || !strings.Contains(plain, "Model overlay · Saved · Publication incomplete") || !strings.Contains(plain, "models_source_failed: Model source refresh failed.") || !strings.Contains(plain, "No client cache publication results.") || !bytes.Contains(captured.Data, []byte(`"overlay_saved":true`)) || m.fixture.calls["refresh"] != 1 {
		t.Fatal("failed source or saved receipt lost", exit, plain, string(captured.Data), m.fixture.calls)
	}
	stored, err := loadModelsOverlayFile(m.state)
	if err != nil || len(stored.Models) != 1 {
		t.Fatal("local save not confirmed", err, stored)
	}
}
