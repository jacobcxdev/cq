package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	dir := t.TempDir()
	args := []string{"http", "--client-build", "client", "--fixture-hash", "618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1", "--installed-result", "passed", "--state-dir", dir}
	for _, gate := range []string{"strong-metadata", "lease-pinning", "pre-admission-failover", "synchronous-journal", "continuity-affinity", "compressed-replay", "installed-listener"} {
		args = append(args, "--gate", gate)
	}
	if err := runCodexValidate(args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codex-readiness-http.json")); err != nil {
		t.Fatal(err)
	}
	bad := []string{"http", "--client-build", "client", "--fixture-hash", "wrong", "--installed-result", "passed", "--state-dir", t.TempDir()}
	if err := runCodexValidate(bad); err == nil {
		t.Fatal("expected incomplete validation rejection")
	}
}
