package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateCodexReleaseIgnoresCodexHomeForSystemAuth(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	want := filepath.Join(home, ".codex", "auth.json")
	writeValidateCodexReleaseAuth(t, want, 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")
	writeValidateCodexReleaseAuth(t, filepath.Join(decoy, "auth.json"), 0o600)

	got, _, err := runValidateCodexReleaseShell(t, repositoryRoot, home, decoy, "")
	if err != nil {
		t.Fatalf("validate-codex-release: %v", err)
	}
	if got != want {
		t.Fatalf("CQ_CODEX_LIVE_AUTH_FILE = %q, want %q", got, want)
	}
}

func TestValidateCodexReleasePrefersExplicitSystemAuth(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")
	writeValidateCodexReleaseAuth(t, filepath.Join(decoy, "auth.json"), 0o600)
	want := filepath.Join(t.TempDir(), "system-auth.json")
	writeValidateCodexReleaseAuth(t, want, 0o600)

	got, _, err := runValidateCodexReleaseShell(t, repositoryRoot, home, decoy, want)
	if err != nil {
		t.Fatalf("validate-codex-release: %v", err)
	}
	if got != want {
		t.Fatalf("CQ_CODEX_LIVE_AUTH_FILE = %q, want %q", got, want)
	}
}

func TestValidateCodexReleaseDefaultsNormalClientAuthToSystemAuth(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	want := filepath.Join(home, ".codex", "auth.json")
	writeValidateCodexReleaseAuth(t, want, 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")
	writeValidateCodexReleaseAuth(t, filepath.Join(decoy, "auth.json"), 0o600)

	_, events, err := runValidateCodexReleaseShell(t, repositoryRoot, home, decoy, "")
	if err != nil {
		t.Fatalf("validate-codex-release: %v", err)
	}
	if !strings.Contains(events, "normal-client-auth="+want+"\n") {
		t.Fatalf("normal client auth was not propagated exactly:\n%s", events)
	}
}

func TestValidateCodexReleaseRejectsInvalidNormalClientAuthBeforeStatusOrBuild(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")

	_, events, err := runValidateCodexReleaseShellWithNormalAuth(t, repositoryRoot, home, decoy, "", "relative-auth.json")
	if err == nil || !strings.Contains(err.Error(), "normal client auth file must be absolute") {
		t.Fatalf("validate-codex-release = %v, want invalid normal client auth failure", err)
	}
	if events != "" {
		t.Fatalf("invalid normal client auth performed status or build work:\n%s", events)
	}
}

func TestValidateCodexReleaseHonoursExplicitAcceptanceExecutable(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")
	writeValidateCodexReleaseAuth(t, filepath.Join(decoy, "auth.json"), 0o600)

	_, events, err := runValidateCodexReleaseShell(t, repositoryRoot, home, decoy, "")
	if err != nil {
		t.Fatalf("validate-codex-release: %v", err)
	}
	if !strings.Contains(events, "acceptance-executable=/usr/bin/true\n") {
		t.Fatalf("acceptance executable was not propagated exactly:\n%s", events)
	}
}

func TestValidateCodexReleaseRejectsMissingLiveGate(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")
	writeValidateCodexReleaseAuth(t, filepath.Join(decoy, "auth.json"), 0o600)
	const omitted = "TestCodexExactExecutableDegradedRescuePassesThroughLiveUpstream"

	_, _, err := runValidateCodexReleaseShellOmittingTest(t, repositoryRoot, home, decoy, "", omitted)
	if err == nil || !strings.Contains(err.Error(), omitted+" did not execute") {
		t.Fatalf("validate-codex-release = %v, want missing gate failure", err)
	}
}

func TestValidateCodexReleaseRejectsMissingHardLimitFailoverGate(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")
	writeValidateCodexReleaseAuth(t, filepath.Join(decoy, "auth.json"), 0o600)
	const omitted = "TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover"

	_, _, err := runValidateCodexReleaseShellOmittingTest(t, repositoryRoot, home, decoy, "", omitted)
	if err == nil || !strings.Contains(err.Error(), omitted+" did not execute") {
		t.Fatalf("validate-codex-release = %v, want missing hard-limit failover gate failure", err)
	}
}

func TestValidateCodexReleaseRejectsMissingLatencyGate(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")
	writeValidateCodexReleaseAuth(t, filepath.Join(decoy, "auth.json"), 0o600)
	const omitted = "TestCodexLiveProxyLatencyMatchesDirect"

	_, _, err := runValidateCodexReleaseShellOmittingTest(t, repositoryRoot, home, decoy, "", omitted)
	if err == nil || !strings.Contains(err.Error(), omitted+" did not execute") {
		t.Fatalf("validate-codex-release = %v, want missing latency gate failure", err)
	}
}

func TestCodexValidationWorkflowsRunHardLimitFailoverGate(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	const testName = "TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover"
	const optIn = `CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE: "1"`
	for _, relativePath := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		t.Run(relativePath, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repositoryRoot, relativePath))
			if err != nil {
				t.Fatal(err)
			}
			contents := string(data)
			if strings.Count(contents, testName) != 2 || strings.Count(contents, optIn) != 1 ||
				!strings.Contains(contents, `grep -F -- "--- PASS: ${test_name} " "$test_log"`) {
				t.Fatalf("%s must opt in, select, and require PASS evidence for %s", relativePath, testName)
			}
		})
	}
}

func TestValidateCodexReleaseRejectsInvalidSystemAuthBeforeStatusOrBuild(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	decoy := filepath.Join(t.TempDir(), "decoy-codex-home")

	unreadable := filepath.Join(t.TempDir(), "unreadable-auth.json")
	writeValidateCodexReleaseAuth(t, unreadable, 0o000)
	if file, err := os.Open(unreadable); err == nil {
		_ = file.Close()
		unreadable = ""
	}

	fixtures := []struct {
		name string
		path string
		want string
	}{
		{name: "relative", path: "relative-auth.json", want: "auth file must be absolute"},
		{name: "directory", path: t.TempDir(), want: "auth file must be a regular file"},
	}
	if unreadable != "" {
		fixtures = append(fixtures, struct {
			name string
			path string
			want string
		}{name: "unreadable", path: unreadable, want: "auth file must be readable"})
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			_, events, err := runValidateCodexReleaseShell(t, repositoryRoot, home, decoy, fixture.path)
			if err == nil || !strings.Contains(err.Error(), fixture.want) {
				t.Fatalf("validate-codex-release = %v, want %q", err, fixture.want)
			}
			if events != "" {
				t.Fatalf("invalid auth performed status or build work:\n%s", events)
			}
		})
	}
}

func validateCodexReleaseRepositoryRoot(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return repositoryRoot
}

func writeValidateCodexReleaseAuth(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func runValidateCodexReleaseShell(t *testing.T, repositoryRoot, home, codexHome, explicitAuth string) (string, string, error) {
	return runValidateCodexReleaseShellOmittingTest(t, repositoryRoot, home, codexHome, explicitAuth, "")
}

func runValidateCodexReleaseShellOmittingTest(t *testing.T, repositoryRoot, home, codexHome, explicitAuth, omittedTest string) (string, string, error) {
	return runValidateCodexReleaseShellOptions(t, repositoryRoot, home, codexHome, explicitAuth, "", omittedTest)
}

func runValidateCodexReleaseShellWithNormalAuth(t *testing.T, repositoryRoot, home, codexHome, explicitAuth, normalAuth string) (string, string, error) {
	return runValidateCodexReleaseShellOptions(t, repositoryRoot, home, codexHome, explicitAuth, normalAuth, "")
}

func runValidateCodexReleaseShellOptions(t *testing.T, repositoryRoot, home, codexHome, explicitAuth, normalAuth, omittedTest string, extraEnvironment ...string) (string, string, error) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".config", "cq", "proxy.json")
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		writeValidateCodexReleaseAuth(t, configPath, 0o600)
	}
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal("jq is required for release validation shell tests")
	}
	authCapture := filepath.Join(root, "auth-capture")
	eventsPath := filepath.Join(root, "events")

	commands := map[string]string{
		"jq":    "#!/bin/sh\nexec \"$CQ_TEST_JQ\" \"$@\"\n",
		"uname": "#!/bin/sh\nprintf 'Darwin\\n'\n",
		"git": `#!/bin/sh
case "$*" in
  "rev-parse --show-toplevel") printf '%s\n' "$CQ_TEST_REPOSITORY_ROOT" ;;
  "status --porcelain=v1 --untracked-files=all") ;;
  "rev-parse HEAD") printf '1111111111111111111111111111111111111111\n' ;;
  "describe --tags --abbrev=0 --match v[0-9]*.[0-9]*.[0-9]*") printf 'v0.24.19\n' ;;
  *) printf 'unexpected git arguments: %s\n' "$*" >&2; exit 97 ;;
esac
`,
		"gh": `#!/bin/sh
case "$1" in
  repo) printf 'owner/repository\n' ;;
  api) printf 'status\n' >>"$CQ_TEST_EVENTS" ;;
  *) printf 'unexpected gh arguments: %s\n' "$*" >&2; exit 97 ;;
esac
`,
		"go": `#!/bin/sh
case "$1" in
  run)
    printf 'go-run\n' >>"$CQ_TEST_EVENTS"
    printf 'provenance\n'
    ;;
  build) printf 'build\n' >>"$CQ_TEST_EVENTS" ;;
  test)
	printf 'acceptance-executable=%s\n' "$CQ_CODEX_ACCEPTANCE_EXECUTABLE" >>"$CQ_TEST_EVENTS"
	printf 'egress-port=%s\n' "$CQ_CODEX_LIVE_EGRESS_PORT" >>"$CQ_TEST_EVENTS"
	case "$*" in
	  *TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover*) ;;
	  *) printf 'hard-limit failover missing from test pattern\n' >&2; exit 96 ;;
	esac
	[ "${CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE:-}" = 1 ] || {
	  printf 'hard-limit failover opt-in missing\n' >&2
	  exit 96
	}
	[ "${CQ_RUN_CODEX_LIVE_LATENCY_ACCEPTANCE:-}" = 1 ] || {
	  printf 'live latency opt-in missing\n' >&2
	  exit 96
	}
    printf 'test\n' >>"$CQ_TEST_EVENTS"
	printf 'normal-client-auth=%s\n' "$CQ_CODEX_RELEASE_NORMAL_CLIENT_AUTH_FILE" >>"$CQ_TEST_EVENTS"
    printf '%s\n' "$CQ_CODEX_LIVE_AUTH_FILE" >"$CQ_TEST_AUTH_CAPTURE"
	for release_test in \
		TestCodexInstalledNormalPassesThroughLiveUpstream \
		TestCodexInstalledNormalContinuesAfterLiveToolCall \
		TestCodexInstalledRescuePassesThroughLiveUpstream \
		TestCodexInstalledLiveRescueTaskResumesInNormal \
		TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover \
		TestCodexExactExecutableNormalPassesThroughLiveUpstream \
		TestCodexExactExecutableDegradedRescuePassesThroughLiveUpstream \
		TestCodexLiveProxyLatencyMatchesDirect
	do
		[ "$release_test" = "$CQ_TEST_OMIT_RELEASE_TEST" ] || printf '%s\n' "--- PASS: $release_test (0.01s)"
	done
    ;;
  *) printf 'unexpected go arguments: %s\n' "$*" >&2; exit 97 ;;
esac
`,
		"lsof": `#!/bin/sh
if [ "${CQ_TEST_LIVE_LISTENER:-}" = 1 ]; then
 printf 'listener-args=%s\n' "$*" >>"$CQ_TEST_EVENTS"
 printf '12345\n'
fi
`,
		"cq": `#!/bin/sh
printf 'rescue-args=%s\n' "$*" >>"$CQ_TEST_EVENTS"
case "$*" in
 "proxy rescue status --port 4567") printf '{"mode":"normal"}\n' ;;
 *) printf 'unexpected rescue status target\n' >&2; exit 97 ;;
esac
`,
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	path := bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin"
	if runtime.GOOS == "windows" {
		t.Fatal("POSIX shell test ran on Windows")
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		"CODEX_HOME=" + codexHome,
		"CQ_TEST_REPOSITORY_ROOT=" + repositoryRoot,
		"CQ_TEST_AUTH_CAPTURE=" + authCapture,
		"CQ_TEST_EVENTS=" + eventsPath,
		"CQ_TEST_JQ=" + jq,
		"CQ_RELEASE_VALIDATION_VERSION=0.24.20",
		"CQ_CODEX_ACCEPTANCE_EXECUTABLE=/usr/bin/true",
		"CQ_CODEXBAR_LIVE_ROOT=" + filepath.Join(home, "CodexBar"),
		"CQ_TEST_OMIT_RELEASE_TEST=" + omittedTest,
	}
	if explicitAuth != "" {
		env = append(env, "CQ_CODEX_LIVE_AUTH_FILE="+explicitAuth)
	}
	if normalAuth != "" {
		env = append(env, "CQ_CODEX_RELEASE_NORMAL_CLIENT_AUTH_FILE="+normalAuth)
	}

	env = append(env, extraEnvironment...)
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "validate-codex-release"))
	command.Dir = repositoryRoot
	command.Env = env
	output, err := command.CombinedOutput()
	authBytes, authErr := os.ReadFile(authCapture)
	if authErr != nil && !errors.Is(authErr, os.ErrNotExist) {
		t.Fatal(authErr)
	}
	eventBytes, eventErr := os.ReadFile(eventsPath)
	if eventErr != nil && !errors.Is(eventErr, os.ErrNotExist) {
		t.Fatal(eventErr)
	}
	if err != nil {
		err = fmt.Errorf("%w: %s", err, output)
	}
	return strings.TrimSpace(string(authBytes)), string(eventBytes), err
}

func TestValidateCodexReleaseSelectedConfiguredEgressPort(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	for _, tt := range []struct {
		name, configuration, override, want string
		invalid                             bool
	}{
		{name: "missing", configuration: `{}`, want: "19280"},
		{name: "null", configuration: `{"port":null}`, want: "19280"},
		{name: "configured", configuration: `{"port":4567}`, want: "4567"},
		{name: "explicit", configuration: `{"port":4567}`, override: "5678", want: "5678"},
		{name: "boolean", configuration: `{"port":false}`, invalid: true},
		{name: "string", configuration: `{"port":"4567"}`, invalid: true},
		{name: "fraction", configuration: `{"port":4567.5}`, invalid: true},
		{name: "out of range", configuration: `{"port":65536}`, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
			configuration := filepath.Join(home, ".config", "cq", "proxy.json")
			writeValidateCodexReleaseAuth(t, configuration, 0o600)
			if err := os.WriteFile(configuration, []byte(tt.configuration), 0o600); err != nil {
				t.Fatal(err)
			}
			var extraEnvironment []string
			if tt.override != "" {
				extraEnvironment = append(extraEnvironment, "CQ_CODEX_LIVE_EGRESS_PORT="+tt.override)
			}
			_, events, err := runValidateCodexReleaseShellOptions(t, repositoryRoot, home, t.TempDir(), "", "", "", extraEnvironment...)
			if tt.invalid {
				if err == nil || events != "" {
					t.Fatal("invalid configured port reached validation")
				}
				return
			}
			if err != nil || !strings.Contains(events, "egress-port="+tt.want+"\n") {
				t.Fatalf("selected port=%q err=%v events=%q", tt.want, err, events)
			}
		})
	}
}

func TestValidateCodexReleaseSnapshotUsedSelectedLiveInstance(t *testing.T) {
	repositoryRoot := validateCodexReleaseRepositoryRoot(t)
	home := t.TempDir()
	writeValidateCodexReleaseAuth(t, filepath.Join(home, ".codex", "auth.json"), 0o600)
	_, events, err := runValidateCodexReleaseShellOptions(t, repositoryRoot, home, t.TempDir(), "", "", "",
		"CQ_CODEX_LIVE_EGRESS_PORT=4567", "CQ_TEST_LIVE_LISTENER=1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(events, "rescue-args=proxy rescue status --port 4567\n") != 2 || strings.Count(events, "listener-args=-nP -tiTCP:4567 -sTCP:LISTEN\n") != 2 {
		t.Fatalf("snapshot did not preserve selected instance: %q", events)
	}
}
