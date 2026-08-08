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
