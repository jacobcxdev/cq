package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

var testCodexHTTPMarkerTime = time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)

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

func TestRunCodexValidateHTTPReportsCurrentMarkerWithoutWriting(t *testing.T) {
	clientBuild := "codex-cli 0.146.0"
	required, _ := proxy.DefaultCodexRoutingRequirements(version, clientBuild)
	marker := completeCodexHTTPReadinessMarker(required)
	dir := t.TempDir()
	writeTestCodexHTTPReadinessMarker(t, dir, marker)
	before, err := os.ReadFile(filepath.Join(dir, "codex-readiness-http.json"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"http", "--client-build", clientBuild, "--state-dir", dir}
	if err := runCodexValidate(args); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "codex-readiness-http.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("report command rewrote readiness marker")
	}

	failedDir := t.TempDir()
	if err := runCodexValidate([]string{"http", "--client-build", clientBuild, "--state-dir", failedDir}); err == nil {
		t.Fatal("expected missing readiness rejection")
	}
	if _, err := os.Stat(filepath.Join(failedDir, "codex-readiness-http.json")); !os.IsNotExist(err) {
		t.Fatalf("report command created marker: %v", err)
	}
	if err := runCodexValidate([]string{"http", "--client-build", "client", "--installed-result", "passed", "--state-dir", t.TempDir()}); err == nil {
		t.Fatal("expected operator-supplied result rejection")
	}
}

func TestRunCodexValidateHTTPRejectsStaleOrMalformedMarker(t *testing.T) {
	clientBuild := "codex-cli 0.146.0"
	required, _ := proxy.DefaultCodexRoutingRequirements(version, clientBuild)
	valid := completeCodexHTTPReadinessMarker(required)
	tests := []struct {
		name   string
		mutate func(*proxy.CodexReadinessMarker)
	}{
		{name: "mismatched CQ build", mutate: func(marker *proxy.CodexReadinessMarker) { marker.CQBuild = "other" }},
		{name: "mismatched client build", mutate: func(marker *proxy.CodexReadinessMarker) { marker.ClientBuild = "codex-cli 0.145.0" }},
		{name: "mismatched semantics", mutate: func(marker *proxy.CodexReadinessMarker) { marker.SemanticsRevision = "old" }},
		{name: "incomplete gates", mutate: func(marker *proxy.CodexReadinessMarker) { marker.CompletedGates = marker.CompletedGates[:1] }},
		{name: "unpassed result", mutate: func(marker *proxy.CodexReadinessMarker) { marker.InstalledResult = "failed" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marker := valid
			marker.CompletedGates = append([]string(nil), valid.CompletedGates...)
			test.mutate(&marker)
			dir := t.TempDir()
			encoded, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "codex-readiness-http.json"), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runCodexValidate([]string{"http", "--client-build", clientBuild, "--state-dir", dir}); err == nil {
				t.Fatal("expected stale readiness marker rejection")
			}
		})
	}
}

func completeCodexHTTPReadinessMarker(required proxy.CodexTransportRequirements) proxy.CodexReadinessMarker {
	return proxy.CodexReadinessMarker{
		Version:                proxy.CodexReadinessMarkerVersion,
		Transport:              required.Transport,
		CQBuild:                required.CQBuild,
		ParserSchema:           required.ParserSchema,
		LeaseSchema:            required.LeaseSchema,
		SemanticsRevision:      required.SemanticsRevision,
		ClientBuild:            required.ClientBuild,
		RetryBudget:            required.RetryBudget,
		FixtureHash:            required.FixtureHash,
		CQExecutableSHA256:     strings.Repeat("a", 64),
		ClientExecutableSHA256: strings.Repeat("b", 64),
		ServiceKind:            "launchd",
		ServiceIdentitySHA256:  strings.Repeat("c", 64),
		InstalledResult:        "passed",
		CompletedGates:         append([]string(nil), required.RequiredGates...),
		ValidatedAt:            testCodexHTTPMarkerTime,
	}
}

func writeTestCodexHTTPReadinessMarker(t *testing.T, dir string, marker proxy.CodexReadinessMarker) {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(dir, "codex-readiness-http.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
