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
	previous := runCodexHTTPInstalledAcceptanceFn
	t.Cleanup(func() { runCodexHTTPInstalledAcceptanceFn = previous })
	runCodexHTTPInstalledAcceptanceFn = func(context.Context) (proxy.CodexHTTPAcceptanceResult, error) {
		return proxy.CodexHTTPAcceptanceResult{Turns: 20, Requests: 40, SelectorCalls: 20}, nil
	}
	dir := t.TempDir()
	args := []string{"http", "--client-build", "client", "--state-dir", dir}
	if err := runCodexValidate(args); err != nil {
		t.Fatal(err)
	}
	marker, err := proxy.LoadCodexReadinessMarker(dir, proxy.CodexRoutingHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if marker.InstalledResult != "passed" || marker.FixtureHash != proxy.CodexHTTPFixtureHash || len(marker.CompletedGates) != len(proxy.CodexHTTPRequiredGates) {
		t.Fatalf("marker = %#v", marker)
	}

	failedDir := t.TempDir()
	runCodexHTTPInstalledAcceptanceFn = func(context.Context) (proxy.CodexHTTPAcceptanceResult, error) {
		return proxy.CodexHTTPAcceptanceResult{}, errors.New("acceptance failed")
	}
	if err := runCodexValidate([]string{"http", "--client-build", "client", "--state-dir", failedDir}); err == nil {
		t.Fatal("expected installed acceptance rejection")
	}
	if _, err := os.Stat(filepath.Join(failedDir, "codex-readiness-http.json")); !os.IsNotExist(err) {
		t.Fatalf("failed acceptance marker error = %v", err)
	}
	if err := runCodexValidate([]string{"http", "--client-build", "client", "--installed-result", "passed", "--state-dir", t.TempDir()}); err == nil {
		t.Fatal("expected operator-supplied result rejection")
	}
}
