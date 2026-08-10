package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestRunCodexValidateCapture(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "raw.json")
	output := filepath.Join(dir, "sanitised.json")
	body := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"raw-session","thread_id":"raw-thread","turn_id":"raw-turn","request_kind":"turn"}}}`)
	if err := os.WriteFile(input, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCodexValidate([]string{"capture", "--input", input, "--output", output}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"raw-session", "raw-thread", "raw-turn"} {
		if strings.Contains(string(data), raw) {
			t.Fatalf("capture leaked %q: %s", raw, data)
		}
	}
}

func TestRunCodexValidateCaptureRequiresExplicitPaths(t *testing.T) {
	if err := runCodexValidate([]string{"capture"}); err == nil {
		t.Fatal("expected missing path error")
	}
}

func TestRunCodexValidateHTTPWritesOnlyCompleteMatchingMarker(t *testing.T) {
	previous := runCodexHTTPReadinessEvidenceFn
	t.Cleanup(func() { runCodexHTTPReadinessEvidenceFn = previous })
	clientBuild := "codex-cli 0.146.0"
	runCodexHTTPReadinessEvidenceFn = func(context.Context) (proxy.CodexHTTPReadinessEvidence, error) {
		return completeCodexHTTPReadinessEvidence(clientBuild), nil
	}
	dir := t.TempDir()
	args := []string{"http", "--client-build", clientBuild, "--state-dir", dir}
	if err := runCodexValidate(args); err != nil {
		t.Fatal(err)
	}
	marker, err := proxy.LoadCodexReadinessMarker(dir, proxy.CodexRoutingHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if marker.Version != proxy.CodexReadinessMarkerVersion || marker.SemanticsRevision != proxy.CodexHTTPReadinessSemanticsRevision || marker.InstalledResult != "passed" || marker.FixtureHash != proxy.CodexHTTPFixtureHash || len(marker.CompletedGates) != len(proxy.CodexHTTPRequiredGates) {
		t.Fatalf("marker = %#v", marker)
	}

	failedDir := t.TempDir()
	runCodexHTTPReadinessEvidenceFn = func(context.Context) (proxy.CodexHTTPReadinessEvidence, error) {
		return proxy.CodexHTTPReadinessEvidence{}, errors.New("acceptance failed")
	}
	if err := runCodexValidate([]string{"http", "--client-build", clientBuild, "--state-dir", failedDir}); err == nil {
		t.Fatal("expected installed acceptance rejection")
	}
	if _, err := os.Stat(filepath.Join(failedDir, "codex-readiness-http.json")); !os.IsNotExist(err) {
		t.Fatalf("failed acceptance marker error = %v", err)
	}
	if err := runCodexValidate([]string{"http", "--client-build", "client", "--installed-result", "passed", "--state-dir", t.TempDir()}); err == nil {
		t.Fatal("expected operator-supplied result rejection")
	}
}

func TestRunCodexValidateHTTPRejectsIncompleteOrMismatchedEvidenceWithoutMarker(t *testing.T) {
	previous := runCodexHTTPReadinessEvidenceFn
	t.Cleanup(func() { runCodexHTTPReadinessEvidenceFn = previous })
	clientBuild := "codex-cli 0.146.0"

	tests := []struct {
		name   string
		mutate func(*proxy.CodexHTTPReadinessEvidence)
	}{
		{name: "synthetic only", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Source = proxy.CodexHTTPReadinessEvidenceSynthetic }},
		{name: "partial corpus", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Acceptance.Turns-- }},
		{name: "partial installed listener", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Acceptance.InstalledRequests = 0 }},
		{name: "mismatched CQ build", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Tuple.CQBuild = "other" }},
		{name: "mismatched client build", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Tuple.ClientBuild = "codex-cli 0.145.0" }},
		{name: "mismatched semantics revision", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Tuple.SemanticsRevision = "old" }},
		{name: "short corpus", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.Stage11CorpusTurns = 999 }},
		{name: "short installed run", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.InstalledTurns = 19 }},
		{name: "unmeasured frozen envelope", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.FrozenSingleTransformEnvelopeCases = 0 }},
		{name: "unmeasured warm affinity", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.WarmAffinityCases = 0 }},
		{name: "unmeasured deterministic fallback", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.DeterministicFallbackCases = 0 }},
		{name: "unmeasured terminal default", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.TerminalDefaultOnceCases = 0 }},
		{name: "unmeasured hard 429 replay", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.ExactPreAdmissionHard429ReplayCases = 0 }},
		{name: "unmeasured admitted no migration", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.AdmittedNoMigrationCases = 0 }},
		{name: "unmeasured v2 runtime", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.V2JournalRuntimeCases = 0 }},
		{name: "routing mismatch", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.RoutingMismatches = 1 }},
		{name: "unknown lifecycle", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.UnknownLifecycleEvents = 1 }},
		{name: "raw identifier leak", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.RawIdentifierLeaks = 1 }},
		{name: "automatic auth write", mutate: func(e *proxy.CodexHTTPReadinessEvidence) { e.Gates.AutomaticAuthWrites = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := completeCodexHTTPReadinessEvidence(clientBuild)
			test.mutate(&evidence)
			runCodexHTTPReadinessEvidenceFn = func(context.Context) (proxy.CodexHTTPReadinessEvidence, error) {
				return evidence, nil
			}
			dir := t.TempDir()
			err := runCodexValidate([]string{"http", "--client-build", clientBuild, "--state-dir", dir})
			if err == nil {
				t.Fatal("expected readiness evidence rejection")
			}
			if _, statErr := os.Stat(filepath.Join(dir, "codex-readiness-http.json")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected evidence marker error = %v", statErr)
			}
		})
	}
}

func completeCodexHTTPReadinessEvidence(clientBuild string) proxy.CodexHTTPReadinessEvidence {
	required, _ := proxy.DefaultCodexRoutingRequirements(version, clientBuild)
	return proxy.CodexHTTPReadinessEvidence{
		Source: proxy.CodexHTTPReadinessEvidenceInstalledListener,
		Tuple: proxy.CodexReadinessTuple{
			Transport:         required.Transport,
			CQBuild:           required.CQBuild,
			ParserSchema:      required.ParserSchema,
			LeaseSchema:       required.LeaseSchema,
			SemanticsRevision: required.SemanticsRevision,
			ClientBuild:       required.ClientBuild,
			RetryBudget:       required.RetryBudget,
			FixtureHash:       required.FixtureHash,
		},
		Gates: proxy.CodexHTTPReadinessGateEvidence{
			Stage11CorpusTurns:                  1_000,
			InstalledTurns:                      20,
			FrozenSingleTransformEnvelopeCases:  1,
			WarmAffinityCases:                   1,
			DeterministicFallbackCases:          1,
			TerminalDefaultOnceCases:            1,
			ExactPreAdmissionHard429ReplayCases: 1,
			AdmittedNoMigrationCases:            1,
			V2JournalRuntimeCases:               1,
		},
		Acceptance: proxy.CodexHTTPAcceptanceResult{
			Turns:                    20,
			Requests:                 40,
			SelectorCalls:            20,
			InstalledVersion:         clientBuild,
			InstalledRequests:        1,
			InstalledModelRequests:   1,
			InstalledAttempts:        1,
			InstalledSelectorCalls:   1,
			InstalledStrongKeys:      1,
			InstalledZstdRequests:    1,
			InstalledQuiescentLeases: 1,
			HeadroomRequests:         1,
			InstalledResolutions:     1,
			PongVerified:             true,
		},
	}
}
