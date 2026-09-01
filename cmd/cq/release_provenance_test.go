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
		"-X main.version={{ .Version }}",
	} {
		if !strings.Contains(releaserText, required) {
			t.Fatalf("GoReleaser missing %q", required)
		}
	}
	if strings.Count(releaserText, "- windows") != 1 || strings.Contains(releaserText, "id: cq-install") || strings.Contains(releaserText, "headroom-ai") || strings.Contains(releaserText, "python@3") {
		t.Fatal("release artifact targets or optional dependency separation differ")
	}
	workflowText := string(workflow)
	for _, required := range []string{
		"goreleaser/goreleaser-action@v7",
		"runs-on: windows-latest",
		"wix --version 5.0.2",
		`.\.github\scripts\build-windows-msi.ps1`,
		`@("amd64", "arm64")`,
		`cq_${version}_windows_${architecture}.msi`,
		"gh release upload",
	} {
		if !strings.Contains(workflowText, required) {
			t.Fatalf("release workflow missing %q", required)
		}
	}
}

func TestWindowsMSIPackageOwnsCompleteLifecycle(t *testing.T) {
	source, err := os.ReadFile("../../packaging/windows/cq.wxs")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`Scope="perUser"`,
		`Version="$(var.CQVersion)"`,
		`Source="$(var.CQExecutable)"`,
		`Name="PATH"`,
		`Value="[INSTALLFOLDER]"`,
		`ExeCommand="service install --owner=winget"`,
		`ExeCommand="service uninstall --owner=winget"`,
		`Execute="rollback"`,
		`Return="check"`,
		`<MajorUpgrade`,
		`Condition="NOT Installed"`,
		`Condition="Installed AND REMOVE~=&quot;ALL&quot;"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Windows MSI missing %q", required)
		}
	}
	for _, forbidden := range []string{"SignTool", "notar", "App Store Connect"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Windows MSI depends on forbidden release mechanism %q", forbidden)
		}
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
		`attributes = system_command "/usr/bin/xattr",`,
		`args: ["-d", "com.apple.quarantine", "#{HOMEBREW_PREFIX}/bin/cq"]`,
		`raise "cq remains quarantined after installation"`,
		`"service", "install", "--owner=homebrew",`,
		`"service", "uninstall", "--owner=homebrew",`,
		`"--service-executable=#{HOMEBREW_PREFIX}/bin/cq",`,
		`args: ["bootout", "gui/#{Process.uid}/dev.jacobcx.cq.proxy"]`,
		`args: ["bootout", "gui/#{Process.uid}/dev.jacobcx.cq.refresh"]`,
		`args: ["-f", "#{Dir.home}/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist"]`,
		`args: ["-f", "#{Dir.home}/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist"]`,
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
	if count := strings.Count(text, "must_succeed: false"); count != 2 {
		t.Fatalf("Homebrew Cask has %d fail-open commands, want two launchd backstops", count)
	}
	if strings.Contains(text, `system_command "/usr/bin/sudo"`) {
		t.Fatal("Homebrew Cask uninstall backstop requires privilege escalation")
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
		`abort "missing Homebrew uninstall backstop for #{backstop}"`,
		`abort "production CQ launchd label survived validation isolation"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Homebrew Cask validation missing fail-closed guard %q", required)
		}
	}
}

func TestReleasePublishesAfterNativePackageProof(t *testing.T) {
	releaser, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(releaser), "release:\n  draft: true") {
		t.Fatal("GoReleaser does not stage release as a draft")
	}
	for _, required := range []string{
		"use_existing_draft: true",
		"replace_existing_artifacts: true",
	} {
		if !strings.Contains(string(releaser), required) {
			t.Fatalf("GoReleaser draft is not retry-safe: missing %q", required)
		}
	}

	workflow, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		"args: release --clean",
		`.github/scripts/validate-homebrew-install.sh`,
		`winget validate --manifest $manifestPath --disable-interactivity`,
		`$metadataText -notmatch "GOOS=windows"`,
		`$metadataText -notmatch ("GOARCH=" + [regex]::Escape($architecture))`,
		`$manifestArgs = @(`,
		`& go run ./internal/tools/wingetmanifest @manifestArgs`,
		`.\.github\scripts\validate-windows-msi.ps1`,
		"runs-on: ubuntu-24.04-arm",
		`.github/scripts/validate-linux-install.sh`,
		"name: windows-msi-validation",
		"runner: [windows-latest, windows-11-arm]",
		`$metadataText -notmatch ("GOARCH=" + [regex]::Escape($architecture))`,
		`$validationArguments.PreviousGoVersion = $previousGoVersion`,
		`if ($statusCode -ne 404)`,
		`$publishedVersions | Sort-Object -Descending | Select-Object -First 1`,
		`$validationArguments.PreviousVersion = $previousVersion`,
		"needs: [release, windows-packages, windows-acceptance]",
		"needs: [windows-deployed, linux-install]",
		`gh release edit "$RELEASE_TAG" --draft=false`,
		`false) echo "Release $RELEASE_TAG already public; resuming publication" ;;`,
		`.\.github\scripts\validate-windows-install.ps1`,
		"secrets.WINGET_PKGS_TOKEN",
		"microsoft/winget-pkgs",
		`if gh api "$upstream_path" >/dev/null 2>&1`,
		`git -C "$checkout" diff --cached --quiet`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("release workflow missing native publication gate %q", required)
		}
	}
	if count := strings.Count(text, "args: release --clean"); count != 1 {
		t.Errorf("release workflow builds artifacts %d times, want one exact draft build", count)
	}
	releaseStart := strings.Index(text, "\n  release:\n")
	windowsStart := strings.Index(text, "\n  windows-packages:\n")
	if releaseStart < 0 || windowsStart <= releaseStart || !strings.Contains(text[releaseStart:windowsStart], "fetch-depth: 0") {
		t.Error("release job does not fetch prior tags for upgrade validation")
	}
	styleIndex := strings.Index(text, "brew style --fix dist/homebrew/Casks/cq.rb")
	lifecycleIndex := strings.Index(text, ".github/scripts/validate-homebrew-install.sh")
	if styleIndex < 0 || lifecycleIndex < 0 || styleIndex > lifecycleIndex {
		t.Error("release workflow does not validate formatted Homebrew Cask bytes")
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
		`.\.github\scripts\build-windows-msi.ps1`,
		"wix --version 5.0.2",
		`.\.github\scripts\validate-windows-msi.ps1`,
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

func TestCIProfilesUbuntuConfinementWithoutWeakeningAppArmor(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		"profile cq-ci-userns flags=(unconfined)",
		"userns,",
		"sudo apparmor_parser --replace",
		"aa-exec --profile=cq-ci-userns -- go test -race -count=1 ./...",
		"sudo apparmor_parser --remove",
		"trap cleanup EXIT",
		"AppArmor profile cleanup failed",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("CI missing Ubuntu confinement contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"apparmor_restrict_unprivileged_userns",
		"t.Skip",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("CI weakens Ubuntu confinement with %q", forbidden)
		}
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
		"Get-CQARPEntries",
		"msiexec.exe",
		"Set-PrivateTree",
		"if ($ownsCQTasks)",
		"if ($ownsWinGetPackage)",
		"Remove-Item -LiteralPath $temporaryRoot -Recurse -Force",
		"native-transport-probe.go",
		`$temporaryCodex = Join-Path $temporaryHome ".codex"`,
		`"install", "--manifest", $PreviousManifestPath`,
		`"upgrade", "--manifest", $ManifestPath`,
		`"uninstall", "--id", "jacobcxdev.cq"`,
		`github.com/jacobcxdev/cq/cmd/cq-install@v$Version`,
		"PreviousGoVersion",
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
	homebrewInstall, err := os.ReadFile("../../.github/scripts/validate-homebrew-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	homebrewText := string(homebrewInstall)
	for _, required := range []string{
		`"$live_executable" -ef "$installed_cq"`,
		`jq . <<<"$status_json" >&2`,
		`tail -n 80 "$log" >&2`,
	} {
		if !strings.Contains(homebrewText, required) {
			t.Errorf("Homebrew installation script missing identity/diagnostic proof %q", required)
		}
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
