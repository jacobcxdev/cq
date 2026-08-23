package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestReleaseEmbedsCodexStage11Provenance(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "CODEX_STAGE11_PROVENANCE_SHA256") {
		t.Fatal("release workflow does not produce Codex Stage11 provenance")
	}
	if !strings.Contains(string(workflow), "TestCodexStage11LifecycleCorpusSmoke") ||
		!strings.Contains(string(workflow), "TestCodexStage11ReviewedManifestMatchesCorpusAuthority") {
		t.Fatal("release workflow does not verify full Stage11 provenance authority")
	}
	if !strings.Contains(string(releaser), "codexStage11CorpusBuildProvenanceSHA256={{ .Env.CODEX_STAGE11_PROVENANCE_SHA256 }}") {
		t.Fatal("GoReleaser does not embed Codex Stage11 provenance")
	}
	command := exec.Command("go", "run", "../../.github/scripts/codex-stage11-provenance.go", "0.21.3", "../../internal/proxy/testdata/codex_stage11_reviewed_manifest.json")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("calculate release provenance: %v", err)
	}
	const reviewed = "bb44beba8777c53101f880e4d5c039cc976cf43d4f99cbd129850cddfc224969\n"
	if !bytes.Equal(output, []byte(reviewed)) {
		t.Fatalf("release provenance = %q, want %q", output, reviewed)
	}
}

func TestReleaseRequiresAndEmbedsGeminiOAuthSecret(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflowText := string(workflow)
	if !strings.Contains(workflowText, "run: test -n \"$GEMINI_ANTIGRAVITY_CLIENT_SECRET\"") {
		t.Fatal("release workflow does not fail closed when Gemini OAuth secret is absent")
	}
	if strings.Count(workflowText, "secrets.GEMINI_ANTIGRAVITY_CLIENT_SECRET") < 2 {
		t.Fatal("release workflow does not supply Gemini OAuth secret to validation and packaging")
	}
	if !strings.Contains(string(releaser), "main.geminiOAuthClientSecret={{ .Env.GEMINI_ANTIGRAVITY_CLIENT_SECRET }}") {
		t.Fatal("GoReleaser does not embed Gemini OAuth secret")
	}
}
