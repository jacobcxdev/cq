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
		"goos:\n      - darwin",
		"goarch:\n      - amd64\n      - arm64",
	} {
		if !strings.Contains(releaserText, required) {
			t.Fatalf("GoReleaser missing %q", required)
		}
	}
	for _, forbidden := range []string{"- linux", "- windows", "format_overrides:"} {
		if strings.Contains(releaserText, forbidden) {
			t.Fatalf("GoReleaser still targets non-macOS platform %q", forbidden)
		}
	}
	if strings.Contains(releaserText, "id: cq-install") || strings.Contains(releaserText, "headroom-ai") || strings.Contains(releaserText, "python@3") {
		t.Fatal("release artifact targets or optional dependency separation differ")
	}
	workflowText := string(workflow)
	for _, required := range []string{
		"goreleaser/goreleaser-action@v7",
		"runs-on: macos-15",
		`archive="cq_${version}_darwin_${arch}.tar.gz"`,
		"needs: release",
	} {
		if !strings.Contains(workflowText, required) {
			t.Fatalf("release workflow missing %q", required)
		}
	}
	for _, forbidden := range []string{"runs-on: ubuntu", "runs-on: windows", "windows-packages:", "windows-acceptance:", "linux-install:", "windows-deployed:", "winget-publish:"} {
		if strings.Contains(workflowText, forbidden) {
			t.Fatalf("release workflow still uses non-macOS surface %q", forbidden)
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
		`if File.executable?("#{HOMEBREW_PREFIX}/bin/cq")`,
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
		`find "$validation_binary" -depth -delete`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Homebrew Cask validation missing fail-closed guard %q", required)
		}
	}
}

func TestReleasePublishesAfterMacOSPackageProof(t *testing.T) {
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
		"needs: release",
		`gh release view "$RELEASE_TAG" --json tagName,isDraft,assets`,
		`gh release edit "$RELEASE_TAG" --draft=false`,
		`false) echo "Release $RELEASE_TAG already public; resuming publication" ;;`,
		`brew style --fix "$cask"`,
		`for arch in amd64 arm64; do`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("release workflow missing macOS publication gate %q", required)
		}
	}
	if count := strings.Count(text, "args: release --clean"); count != 1 {
		t.Errorf("release workflow builds artifacts %d times, want one exact draft build", count)
	}
	if strings.Contains(text, "[IO.File]::WriteAllLines($checksums") {
		t.Error("release workflow rewrites checksums with platform-native CRLF endings")
	}
	for _, unsafe := range []string{
		`grep -Fvx "$RELEASE_TAG" | head -1`,
		`grep -Fvx "$GITHUB_REF_NAME" | head -1`,
	} {
		if strings.Contains(text, unsafe) {
			t.Errorf("release workflow uses pipefail-unsafe previous-tag selection %q", unsafe)
		}
	}
	if count := strings.Count(text, "done < <(git tag --sort=-v:refname)"); count != 1 {
		t.Errorf("release workflow uses pipe-safe previous-tag selection %d times, want 1", count)
	}
	releaseStart := strings.Index(text, "\n  release:\n")
	publishStart := strings.Index(text, "\n  publish:\n")
	if releaseStart < 0 || publishStart <= releaseStart || !strings.Contains(text[releaseStart:publishStart], "fetch-depth: 0") {
		t.Error("release job does not fetch prior tags for upgrade validation")
	}
	styleIndex := strings.Index(text, "brew style --fix dist/homebrew/Casks/cq.rb")
	lifecycleIndex := strings.Index(text, ".github/scripts/validate-homebrew-install.sh")
	if styleIndex < 0 || lifecycleIndex < 0 || styleIndex > lifecycleIndex {
		t.Error("release workflow does not validate formatted Homebrew Cask bytes")
	}
	publishEnd := strings.Index(text[publishStart+1:], "\n      - name: Audit published Homebrew Cask\n")
	if publishStart < 0 || publishEnd < 0 {
		t.Fatal("release workflow publish job is missing")
	}
	publishBlock := text[publishStart : publishStart+1+publishEnd]
	if strings.Contains(publishBlock, `releases/tags/${RELEASE_TAG}`) {
		t.Error("release workflow queries the public-only tag endpoint before publishing its draft")
	}
}

func TestCIUsesMacOSRunnersOnly(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		"runs-on: macos-15",
		"go build ./...",
		"go vet ./...",
		"go test -race -count=1 ./...",
		"./internal/installer",
		"./cmd/cq-install",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("CI missing macOS gate %q", required)
		}
	}
	for _, forbidden := range []string{"runs-on: ubuntu", "runs-on: windows", "GOOS=windows", "GOOS=linux", "linux-confinement:", "\n  windows:\n"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("CI still uses non-macOS surface %q", forbidden)
		}
	}
	if strings.Contains(text, "self-hosted") || strings.Contains(text, "bespoke") {
		t.Fatal("CI requires a persistent custom runner")
	}
	if strings.Contains(text, `grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1`) {
		t.Error("CI uses pipefail-unsafe previous-tag selection")
	}
	if !strings.Contains(text, "done < <(git tag --sort=-v:refname)") {
		t.Error("CI does not use pipe-safe previous-tag selection")
	}
	if count := strings.Count(text, "go test -race -count=1 ./..."); count != 1 {
		t.Fatalf("full race suite runs %d times, want one macOS run", count)
	}
}

func TestGitHubWorkflowsRunOnlyWhenDispatched(t *testing.T) {
	for _, path := range []string{
		"../../.github/workflows/ci.yml",
		"../../.github/workflows/release.yml",
	} {
		workflow, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		header, _, ok := strings.Cut(string(workflow), "\njobs:\n")
		if !ok {
			t.Fatalf("%s has no jobs section", path)
		}
		if !strings.Contains(header, "\n  workflow_dispatch:\n") {
			t.Errorf("%s is not manually dispatched", path)
		}
		for _, automatic := range []string{"\n  push:\n", "\n  pull_request:\n", "\n  schedule:\n"} {
			if strings.Contains(header, automatic) {
				t.Errorf("%s retains automatic trigger %q", path, strings.TrimSpace(automatic))
			}
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
		`$local = [Environment]::GetFolderPath("LocalApplicationData")`,
		`$roaming = [Environment]::GetFolderPath("ApplicationData")`,
		`$homePath = [Environment]::GetFolderPath("UserProfile")`,
		`$codexRoot = Join-Path $homePath ".codex"`,
		`$installedCQ = Join-Path $local "Programs\cq\cq.exe"`,
		`"HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*"`,
		`"HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*"`,
		`"HKCU:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*"`,
		`"HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*"`,
		`$displayName = $_.PSObject.Properties["DisplayName"]`,
		`$publisher = $_.PSObject.Properties["Publisher"]`,
		`$pathSeparators = [char[]]@("\", "/")`,
		`TrimEnd($pathSeparators)`,
		`Remove-ValidationPath -Path $codexRoot`,
		`Remove-ValidationPath -Path $roamingCQ`,
		`Remove-ValidationPath -Path $localCQ`,
		`$SkipWinGet`,
		`DiagOutputDir`,
		`-Filter "WinGet*.log"`,
		`"install", "--manifest", $PreviousManifestPath`,
		`"upgrade", "--manifest", $ManifestPath`,
		`$productCode = [string]$installedEntry.PSChildName`,
		`"uninstall", "--product-code", $productCode`,
		`github.com/jacobcxdev/cq/cmd/cq-install@v$Version`,
		"PreviousGoVersion",
		"GetSecurityDescriptor(4)",
		"EnginePID",
		`$runLevelProperty = $principal.PSObject.Properties["RunLevel"]`,
		"LocalManifestFiles",
		`@(Get-CQARPEntries).Count`,
	} {
		if !strings.Contains(windowsText, required) {
			t.Errorf("Windows installation script missing cleanup/proof %q", required)
		}
	}
	withoutSafeARPCounts := strings.ReplaceAll(windowsText, `@(Get-CQARPEntries).Count`, "")
	if strings.Contains(withoutSafeARPCounts, `(Get-CQARPEntries).Count`) {
		t.Error("Windows installation script reads Count from potentially null or scalar pipeline output")
	}
	if strings.Contains(windowsText, "& $Path install --owner=winget") {
		t.Fatal("Windows native validation bypasses WinGet")
	}
	if strings.Contains(windowsText, "$principal.RunLevel") {
		t.Fatal("Windows deployment validation requires optional RunLevel XML property")
	}
	for _, redirected := range []string{
		`$shellKey.SetValue("AppData"`,
		`$shellKey.SetValue("Local AppData"`,
		`$environmentKey.SetValue("USERPROFILE"`,
	} {
		if strings.Contains(windowsText, redirected) {
			t.Errorf("Windows deployment validation redirects native user profile with %q", redirected)
		}
	}
	windowsMSI, err := os.ReadFile("../../.github/scripts/validate-windows-msi.ps1")
	if err != nil {
		t.Fatal(err)
	}
	windowsMSIText := string(windowsMSI)
	for _, required := range []string{
		"Stop-ScheduledTask",
		"Unregister-ScheduledTask",
	} {
		if !strings.Contains(windowsMSIText, required) {
			t.Errorf("Windows MSI validation cleanup missing non-native task operation %q", required)
		}
	}
	if strings.Contains(windowsMSIText, "& schtasks.exe") {
		t.Error("Windows MSI validation cleanup leaks expected missing-task exit status")
	}
	foregroundIndex := strings.Index(windowsMSIText, `Start-Process -FilePath $serviceProbe -ArgumentList @("proxy", "start")`)
	directInstallIndex := strings.Index(windowsMSIText, `& $serviceProbe service install --owner=winget`)
	directUninstallIndex := strings.Index(windowsMSIText, `& $serviceProbe service uninstall --owner=winget`)
	msiInstallIndex := strings.Index(windowsMSIText, `Invoke-MSI -Action install -Path $PreviousMSI`)
	if foregroundIndex < 0 || directInstallIndex < foregroundIndex || directUninstallIndex < directInstallIndex || msiInstallIndex < directUninstallIndex {
		t.Error("Windows MSI validation does not expose and clean direct service lifecycle before installer execution")
	}
	for _, script := range []struct {
		text string
		seal string
	}{
		{text: windowsText, seal: "Set-PrivateTree -Root $localCQ"},
		{text: windowsMSIText, seal: "Set-PrivateTree -Root $localCQ"},
	} {
		sealIndex := strings.Index(script.text, script.seal)
		fixtureIndex := strings.Index(script.text, "fixtures --config")
		if sealIndex < 0 || fixtureIndex < 0 || sealIndex > fixtureIndex {
			t.Errorf("Windows installation script does not seal fixture state before initialisation: %q", script.seal)
		}
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
		`echo "missing required command: $command" >&2`,
		`previous_version=${2:-}`,
		`export PATH="$GOBIN:$PATH"`,
		`systemd_executable=/usr/lib/systemd/systemd`,
		`echo "systemd user manager unavailable: $systemd_executable" >&2`,
		`"$systemd_executable" --user --unit=basic.target`,
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
	if strings.Contains(linuxText, "for command in go jq systemctl systemd readlink") {
		t.Error("Linux installation script requires systemd daemon on PATH")
	}
}
