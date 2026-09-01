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

func TestReleaseArchivesContainOnlyCQExecutable(t *testing.T) {
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(releaser), "    files:\n      - none*\n") {
		t.Fatal("GoReleaser archives include default metadata files")
	}
}

func TestReleaseDoesNotDependOnAppStoreConnect(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflowText := string(workflow)
	combined := string(releaser) + workflowText
	for _, forbidden := range []string{
		"notarize:",
		"MACOS_SIGN_P12",
		"MACOS_SIGN_PASSWORD",
		"MACOS_NOTARY_ISSUER_ID",
		"MACOS_NOTARY_KEY_ID",
		"MACOS_NOTARY_KEY",
	} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("release depends on Apple release credential %q", forbidden)
		}
	}
	for _, required := range []string{
		`archive="cq_${version}_darwin_${arch}.tar.gz"`,
		`go version -m "${verification_dir}/cq"`,
		"github.com/jacobcxdev/cq/cmd/cq",
	} {
		if !strings.Contains(workflowText, required) {
			t.Fatalf("release does not verify Darwin artifact %q", required)
		}
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
		`attributes = system_command "/usr/bin/xattr", args: ["#{HOMEBREW_PREFIX}/bin/cq"], print_stdout: false`,
		`system_command "/usr/bin/xattr", args: ["-d", "com.apple.quarantine", "#{HOMEBREW_PREFIX}/bin/cq"]`,
		`raise "cq remains quarantined after installation"`,
		`args: ["service", "install", "--owner=homebrew", "--service-executable=#{HOMEBREW_PREFIX}/bin/cq"]`,
		`args: ["service", "uninstall", "--owner=homebrew", "--service-executable=#{HOMEBREW_PREFIX}/bin/cq"]`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Homebrew Cask missing %q", required)
		}
	}
	if !strings.Contains(text, "skip_upload: true") {
		t.Fatal("GoReleaser publishes the unformatted generated Homebrew Cask")
	}
	if strings.Contains(text, "\n    uninstall:\n") {
		t.Fatal("Homebrew Cask duplicates transactional uninstall with privileged fallback")
	}
	if strings.Contains(text, "must_succeed: false") {
		t.Fatal("Homebrew Cask quarantine removal fails open")
	}

	workflow, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflowText := string(workflow)
	for _, required := range []string{
		"brew style --fix",
		"brew style \"$cask\"",
		"repos/jacobcxdev/homebrew-tap/contents/Casks/cq.rb",
		"secrets.HOMEBREW_TAP_TOKEN",
		"brew audit --cask --strict jacobcxdev/tap/cq",
		`.github/scripts/validate-homebrew-cask.sh "$PWD/dist/homebrew/Casks/cq.rb" "$archive"`,
	} {
		if !strings.Contains(workflowText, required) {
			t.Fatalf("Homebrew Cask publish workflow missing %q", required)
		}
	}
}

func TestHomebrewCaskValidationFailsClosed(t *testing.T) {
	script, err := os.ReadFile("../../.github/scripts/validate-homebrew-cask.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		`validation_caskroom="$(brew --caskroom)/$validation_token"`,
		`installed_casks=$(brew list --cask)`,
		`installed_taps=$(brew tap)`,
		`validation Cask cleanup failed`,
		`validation Caskroom cleanup failed`,
		`validation binary cleanup failed`,
		`validation tap cleanup failed`,
		`validation temporary directory cleanup failed`,
		`lifecycle_commands == %w[install uninstall]`,
		`abort "CQ lifecycle command survived validation isolation"`,
		`abort "production CQ binary path survived validation isolation"`,
		`abort "unexpected Homebrew launchctl uninstall fallback"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Homebrew Cask validation missing fail-closed guard %q", required)
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
		"go test -race -count=1 ./internal/fsutil",
		"go test -race -count=1 ./internal/installer -run '^(TestWindows|TestInstallLock|TestInstaller)'",
		"go test -race -count=1 ./cmd/cq -run '^(TestWindows|TestRunService|TestServiceSnapshot)'",
		`CQ_NATIVE_WINDOWS_SCHEDULER_TEST: "1"`,
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
	if count := strings.Count(text, "go test -race -count=1 ./..."); count != 1 {
		t.Fatalf("full race suite runs on %d platforms, want Linux only", count)
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
