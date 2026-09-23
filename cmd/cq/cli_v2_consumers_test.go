package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Only these allowlisted local utilities run outside the fake tool directory.
// No live release, service, credential, or network command is reachable.
func TestCLIV2ReleaseConsumerHermetic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX release script; native PowerShell consumers have separate coverage")
	}
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal("jq is required for release consumer acceptance")
	}
	source, err := os.ReadFile("../../scripts/validate-codex-release")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "/usr/bin/") || strings.Contains(string(source), "/opt/") {
		t.Fatal("release script acquired absolute tool escape; review fixture isolation")
	}
	for _, mode := range []string{"normal", "rescue_draining", "rescue", "recovering", "blocked", "malformed", "error", "missing", "unknown", "command-failed", "changed-pid", "changed-mode"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"env", "mktemp", "chmod", "rm", "cat", "grep"} {
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(jq, filepath.Join(bin, "jq")); err != nil {
				t.Fatal(err)
			}
			write("uname", "printf 'Darwin\\n'\n")
			write("git", `case "$*" in
 'rev-parse --show-toplevel') printf '%s\n' "$TEST_ROOT";;
 'rev-parse HEAD') printf '0123456789012345678901234567890123456789\n';;
 'status --porcelain=v1 --untracked-files=all') :;;
 *) printf 'forbidden git %s\n' "$*" >>"$TEST_LOG"; exit 98;;
esac
`)
			write("gh", `printf 'gh %s\n' "$*" >>"$TEST_LOG"
case "$*" in
 'repo view --json nameWithOwner --jq .nameWithOwner') printf 'synthetic/repository\n';;
 'api --method POST repos/synthetic/repository/statuses/'*) :;;
 *) printf 'forbidden gh\n' >>"$TEST_LOG"; exit 98;;
esac
`)
			write("lsof", `count=0; if [ -f "$TEST_ROOT/lsof-count" ];then read -r count <"$TEST_ROOT/lsof-count";fi
count=$((count+1));printf '%s\n' "$count" >"$TEST_ROOT/lsof-count"
if [ "$TEST_MODE" = changed-pid ] && [ "$count" -gt 1 ];then printf '222\n';else printf '111\n';fi
`)
			write("cq", `[ "$*" = 'proxy rescue status --json' ] || { printf 'forbidden cq %s\n' "$*" >>"$TEST_LOG";exit 98; }
count=0;if [ -f "$TEST_ROOT/cq-count" ];then read -r count <"$TEST_ROOT/cq-count";fi
count=$((count+1));printf '%s\n' "$count" >"$TEST_ROOT/cq-count"
mode=$TEST_MODE
case "$mode" in
 malformed) printf '{bad';exit 0;;
 error) printf '{"schema_version":2,"ok":false,"data":{"mode":"normal"}}';exit 0;;
 missing) printf '{"schema_version":2,"ok":true,"data":{}}';exit 0;;
 command-failed) printf '{"schema_version":2,"ok":true,"data":{"mode":"normal"}}';exit 1;;
 changed-pid) mode=normal;;
 changed-mode) if [ "$count" -gt 1 ];then mode=rescue;else mode=normal;fi;;
esac
printf '{"schema_version":2,"ok":true,"command":"proxy rescue status","data":{"mode":"%s"},"errors":[],"warnings":[]}\n' "$mode"
`)
			write("go", `case "$1" in
 run) printf 'synthetic-provenance\n';;
 build) :;;
 test)
 [ "$2" = -race ] || exit 98
 for name in TestCodexInstalledNormalPassesThroughLiveUpstream TestCodexInstalledNormalContinuesAfterLiveToolCall TestCodexInstalledRescuePassesThroughLiveUpstream TestCodexInstalledLiveRescueTaskResumesInNormal TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover TestCodexExactExecutableNormalPassesThroughLiveUpstream TestCodexExactExecutableDegradedRescuePassesThroughLiveUpstream TestCodexLiveProxyLatencyMatchesDirect;do printf -- '--- PASS: %s (0.00s)\n' "$name";done;;
 *) printf 'forbidden go\n' >>"$TEST_LOG";exit 98;;
esac
`)
			for _, name := range []string{"curl", "wget", "ssh", "systemctl", "launchctl", "brew", "service", "codex"} {
				write(name, `printf 'forbidden external tool\n' >>"$TEST_LOG";exit 98`)
			}
			script := filepath.Join(root, "validate")
			if err := os.WriteFile(script, source, 0o700); err != nil {
				t.Fatal(err)
			}
			auth := filepath.Join(root, "synthetic-auth.json")
			if err := os.WriteFile(auth, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(root, "calls")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/sh", script)
			command.Env = []string{"PATH=" + bin, "HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "XDG_STATE_HOME=" + filepath.Join(root, "state"), "TEST_ROOT=" + root, "TEST_LOG=" + log, "TEST_MODE=" + mode, "CQ_CODEX_LIVE_AUTH_FILE=" + auth, "CQ_CODEX_RELEASE_NORMAL_CLIENT_AUTH_FILE=" + auth, "CQ_CODEXBAR_LIVE_ROOT=" + root, "CQ_CODEX_ACCEPTANCE_EXECUTABLE=" + filepath.Join(bin, "codex"), "CQ_RELEASE_VALIDATION_VERSION=1.0.0"}
			output, runErr := command.CombinedOutput()
			calls, _ := os.ReadFile(log)
			good := mode == "normal" || mode == "rescue_draining" || mode == "rescue" || mode == "recovering" || mode == "blocked"
			if (runErr == nil) != good || strings.Contains(string(calls), "forbidden") || (!good && strings.Contains(string(calls), "state=success")) {
				t.Fatalf("mode=%s err=%v output=%s calls=%s", mode, runErr, output, calls)
			}
			if good && !strings.Contains(string(calls), "state=success") {
				t.Fatal("successful fixture did not finish")
			}
			if !good && !strings.HasPrefix(mode, "changed-") && strings.Contains(string(calls), "api --method POST") {
				t.Fatalf("invalid initial snapshot published status: %s", calls)
			}
		})
	}
}

func TestCLIV2WindowsServiceConsumerSchemas(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell native/package job provides consumer coverage")
	}
	for _, file := range []string{"validate-windows-install.ps1", "validate-windows-msi.ps1"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../.github/scripts", file))
			if err != nil {
				t.Fatal(err)
			}
			source := string(raw)
			start := strings.Index(source, "function Convert-CQServiceStatus {")
			end := strings.Index(source[start:], "function Wait-ServiceStatus {")
			if start < 0 || end < 0 {
				t.Fatal("missing schema adapter")
			}
			script := source[start:start+end] + `
$good='{"schema_version":2,"ok":true,"data":{"components":[{"id":"proxy"},{"id":"token-refresh"}]}}' | ConvertFrom-Json
$legacy='{"schema_version":1,"owner":"go","proxy":{},"refresh":{}}' | ConvertFrom-Json
$null=Convert-CQServiceStatus -Envelope $good
$null=Convert-CQServiceStatus -Envelope $legacy -PreviousRelease
foreach($test in @(@($legacy,$false),@($good,$true),@(('{}'|ConvertFrom-Json),$false),@((' {"schema_version":2,"ok":false,"data":{"components":[]}} '|ConvertFrom-Json),$false))){
 $failed=$false;try{$null=Convert-CQServiceStatus -Envelope $test[0] -PreviousRelease:$test[1]}catch{$failed=$true};if(-not $failed){throw "schema fallback accepted"}
}
`
			path := filepath.Join(t.TempDir(), "fixture.ps1")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-File", path)
			cmd.Env = []string{"HOME=" + filepath.Dir(path), "PATH=" + filepath.Dir(pwsh)}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("PowerShell schema fixture: %v %s", err, output)
			}
		})
	}
}

func TestCLIV2PackageJQConsumers(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"validate-linux-install.sh", "validate-homebrew-install.sh"} {
		raw, err := os.ReadFile(filepath.Join("../../.github/scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		source := string(raw)
		marker := "'select(.schema_version == 2 and .ok == true) |"
		offset := 0
		count := 0
		for {
			i := strings.Index(source[offset:], marker)
			if i < 0 {
				break
			}
			start := offset + i + 1
			end := strings.Index(source[start:], "'") + start
			query := source[start:end]
			offset = end + 1
			if !strings.Contains(query, "$executable") {
				continue
			}
			count++
			for _, good := range []bool{true, false} {
				body := fmt.Sprintf(`{"schema_version":2,"ok":%t,"data":{"components":[{"id":"proxy","installed":true,"state":"running","healthy":true,"owner":"package","executable":"/synthetic/cq","pid":123},{"id":"token-refresh","installed":true,"healthy":true}]}}`, good)
				cmd := exec.Command(jq, "-e", "--arg", "executable", "/synthetic/cq", query)
				cmd.Stdin = strings.NewReader(body)
				out, err := cmd.CombinedOutput()
				if (err == nil) != good {
					t.Fatalf("%s good=%t err=%v out=%s", name, good, err, out)
				}
			}
		}
		if count == 0 {
			t.Fatalf("no candidate status assertion in %s", name)
		}

		if name == "validate-homebrew-install.sh" {
			start := strings.Index(source, "'select(.schema_version == 1)")
			if start < 0 {
				t.Fatal("missing explicit previous release schema")
			}
			start++
			end := start + strings.Index(source[start:], "'")
			query := source[start:end]
			for _, schema := range []int{1, 2} {
				body := fmt.Sprintf(`{"schema_version":%d,"owner":"homebrew","proxy":{"running":true,"healthy":true,"configured_executable":"/synthetic/cq","listener":"127.0.0.1:19280","pid":123},"refresh":{"healthy":true}}`, schema)
				cmd := exec.Command(jq, "-e", "--arg", "executable", "/synthetic/cq", query)
				cmd.Stdin = strings.NewReader(body)
				output, err := cmd.CombinedOutput()
				if (err == nil) != (schema == 1) {
					t.Fatalf("previous schema%d: %v %s", schema, err, output)
				}
			}
		}
	}
}
