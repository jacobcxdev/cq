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

func TestHomebrewServiceLeavesListenerToRuntimeSupervisor(t *testing.T) {
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(releaser), `sockets `) {
		t.Fatal("Homebrew service claims runtime supervisor listener")
	}
}

func TestReleaseBuildsCompletePackageArtifacts(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	releaserText := string(releaser)
	for _, required := range []string{
		"id: cq\n",
		"id: cq-install\n",
		"main: ./cmd/cq-install",
		"-X main.version={{ .Version }}",
		"formats: [binary]",
		"cq-install_{{ .Version }}_{{ .Os }}_{{ .Arch }}",
	} {
		if !strings.Contains(releaserText, required) {
			t.Fatalf("GoReleaser missing %q", required)
		}
	}
	if strings.Count(releaserText, "- windows") < 2 || strings.Contains(releaserText, "headroom-ai") || strings.Contains(releaserText, "python@3") {
		t.Fatal("release artifact targets or optional dependency separation differ")
	}
	workflowText := string(workflow)
	if !strings.Contains(workflowText, "goreleaser/goreleaser-action@v7") {
		t.Fatal("release workflow does not use GoReleaser action v7")
	}
}

func TestReleaseNotarisesAndVerifiesDarwinBinaries(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	releaserText := string(releaser)
	for _, required := range []string{
		"notarize:",
		"macos:",
		`enabled: '{{ isEnvSet "MACOS_SIGN_P12" }}'`,
		"certificate: '{{ .Env.MACOS_SIGN_P12 }}'",
		"password: '{{ .Env.MACOS_SIGN_PASSWORD }}'",
		"issuer_id: '{{ .Env.MACOS_NOTARY_ISSUER_ID }}'",
		"key_id: '{{ .Env.MACOS_NOTARY_KEY_ID }}'",
		"key: '{{ .Env.MACOS_NOTARY_KEY }}'",
		"wait: true",
		"timeout: 20m",
	} {
		if !strings.Contains(releaserText, required) {
			t.Fatalf("GoReleaser notarisation missing %q", required)
		}
	}
	workflowText := string(workflow)
	for _, secret := range []string{
		"MACOS_SIGN_P12",
		"MACOS_SIGN_PASSWORD",
		"MACOS_NOTARY_ISSUER_ID",
		"MACOS_NOTARY_KEY_ID",
		"MACOS_NOTARY_KEY",
	} {
		if strings.Count(workflowText, secret) < 2 || !strings.Contains(workflowText, "secrets."+secret) {
			t.Fatalf("release workflow does not preflight and pass %s", secret)
		}
	}
	if !strings.Contains(workflowText, "codesign --verify --strict") {
		t.Fatal("release workflow does not verify signed Darwin binaries")
	}
}

func TestReleasePublishesHomebrewCaskLifecycle(t *testing.T) {
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(releaser)
	if strings.Contains(text, "brews:") || !strings.Contains(text, "homebrew_casks:") {
		t.Fatal("GoReleaser does not publish only a Homebrew Cask")
	}
	for _, required := range []string{
		"binaries:",
		"- cq",
		"hooks:",
		"install: |",
		"uninstall: |",
		`args: ["service", "install", "--owner=homebrew"]`,
		`args: ["service", "uninstall", "--owner=homebrew"]`,
		"dev.jacobcx.cq.proxy",
		"dev.jacobcx.cq.refresh",
		"~/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist",
		"~/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Homebrew Cask missing %q", required)
		}
	}
}
