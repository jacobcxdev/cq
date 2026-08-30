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
		`args: ["service", "install", "--owner=homebrew", "--service-executable=#{HOMEBREW_PREFIX}/bin/cq"]`,
		`args: ["service", "uninstall", "--owner=homebrew", "--service-executable=#{HOMEBREW_PREFIX}/bin/cq"]`,
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

func TestCIExercisesNativeInstallerSurfaces(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		"runs-on: windows-latest",
		"go build -o cq.exe ./cmd/cq",
		"go build -o cq-install.exe ./cmd/cq-install",
		"go test -race -count=1 ./...",
		"GOOS=windows GOARCH=amd64 go build ./...",
		"GOOS=windows GOARCH=arm64 go build ./...",
		"GOOS=linux GOARCH=amd64 go build ./...",
		"GOOS=linux GOARCH=arm64 go build ./...",
		"./internal/installer",
		"./cmd/cq-install",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("CI missing native installer gate %q", required)
		}
	}
	if strings.Contains(text, "self-hosted") || strings.Contains(text, "bespoke") {
		t.Fatal("CI requires a persistent custom runner")
	}
}

func TestNativeInstallationScriptsHaveExactCleanupGuards(t *testing.T) {
	windows, err := os.ReadFile("../../.github/scripts/validate-windows-install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	linux, err := os.ReadFile("../../.github/scripts/validate-linux-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	windowsText := string(windows)
	for _, required := range []string{
		"finally",
		`\cq\Proxy`,
		`\cq\Refresh`,
		"Get-CimInstance Win32_Process",
		"Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\cq",
		"CQPathAdded",
		"if ($ownsCQTasks)",
		"if ($ownsUninstallRegistration)",
		"Remove-Item -LiteralPath $temporaryRoot -Recurse -Force",
		"native-transport-probe.go",
		`$temporaryCodex = Join-Path $temporaryHome ".codex"`,
		`"install", "--manifest", $PreviousManifestPath`,
		`"upgrade", "--manifest", $ManifestPath`,
		`"uninstall", "--id", "jacobcxdev.cq"`,
		`github.com/jacobcxdev/cq/cmd/cq-install@v$Version`,
		"GetSecurityDescriptor(4)",
		"EnginePID",
		"LocalManifestFiles",
	} {
		if !strings.Contains(windowsText, required) {
			t.Errorf("Windows installation script missing cleanup/proof %q", required)
		}
	}
	if strings.Contains(windowsText, "& $Path install --owner=winget") {
		t.Fatal("Windows native validation bypasses WinGet")
	}
	linuxText := string(linux)
	for _, required := range []string{
		"trap cleanup EXIT INT TERM",
		"cq-proxy.service",
		"cq-refresh.service",
		"cq-refresh.timer",
		"systemctl --user daemon-reload",
		`grep -F "cq-proxy.service" "/proc/$proxy_pid/cgroup"`,
		"find \"$temporary_root\" -depth -delete",
		"native-transport-probe.go",
		`export CODEX_HOME="$HOME/.codex"`,
	} {
		if !strings.Contains(linuxText, required) {
			t.Errorf("Linux installation script missing cleanup/proof %q", required)
		}
	}
}
