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
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	authCapture := filepath.Join(root, "auth-capture")
	eventsPath := filepath.Join(root, "events")

	commands := map[string]string{
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
    printf 'test\n' >>"$CQ_TEST_EVENTS"
    printf '%s\n' "$CQ_CODEX_LIVE_AUTH_FILE" >"$CQ_TEST_AUTH_CAPTURE"
	for release_test in \
		TestCodexInstalledNormalPassesThroughLiveUpstream \
		TestCodexInstalledNormalContinuesAfterLiveToolCall \
		TestCodexInstalledRescuePassesThroughLiveUpstream \
		TestCodexInstalledLiveRescueTaskResumesInNormal \
		TestCodexExactExecutableNormalPassesThroughLiveUpstream \
		TestCodexExactExecutableDegradedRescuePassesThroughLiveUpstream
	do
		[ "$release_test" = "$CQ_TEST_OMIT_RELEASE_TEST" ] || printf '%s\n' "--- PASS: $release_test (0.01s)"
	done
    ;;
  *) printf 'unexpected go arguments: %s\n' "$*" >&2; exit 97 ;;
esac
`,
		"lsof": "#!/bin/sh\nexit 0\n",
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
		"CQ_RELEASE_VALIDATION_VERSION=0.24.20",
		"CQ_CODEX_ACCEPTANCE_EXECUTABLE=/usr/bin/true",
		"CQ_CODEXBAR_LIVE_ROOT=" + filepath.Join(home, "CodexBar"),
		"CQ_TEST_OMIT_RELEASE_TEST=" + omittedTest,
	}
	if explicitAuth != "" {
		env = append(env, "CQ_CODEX_LIVE_AUTH_FILE="+explicitAuth)
	}

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
