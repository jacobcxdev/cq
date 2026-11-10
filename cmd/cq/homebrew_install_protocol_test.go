package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHomebrewInstallProtocolQualification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell tools")
	}
	command := exec.Command("bash", ".github/scripts/test-homebrew-install-protocol.sh")
	command.Dir = "../.."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Homebrew protocol qualification fixtures failed: %v\n%s", err, output)
	}
}

func TestHomebrewInstallTrailingSlashTemporaryRootAcceptsDowngradeCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell tools")
	}
	source, err := os.ReadFile("../../.github/scripts/validate-homebrew-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	var setup []string
	for line := range strings.SplitSeq(string(source), "\n") {
		if strings.HasPrefix(line, "temporary_root=") {
			setup = append(setup, line)
		}
	}
	if len(setup) == 0 {
		t.Fatal("Homebrew validator temporary-root setup is missing")
	}
	// Execute the validator's actual setup with macOS's trailing-slash TMPDIR,
	// then validate the resulting downgrade argument with the real CLI parser.
	command := exec.Command("bash", "-euo", "pipefail", "-c", strings.Join(setup, "\n")+"\nprintf '%s\\n' \"$temporary_root/previous/cq\"")
	command.Env = append(os.Environ(), "TMPDIR="+t.TempDir()+"/")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("validator temporary-root setup failed: %v\n%s", err, output)
	}
	candidate := strings.TrimSuffix(string(output), "\n")
	parsed, err := parseServiceCommand([]string{"upgrade", "--owner=homebrew", "--candidate-executable=" + candidate, "--json"})
	if err != nil {
		t.Fatalf("validator produced a rejected downgrade candidate %q: %v", candidate, err)
	}
	if parsed.CandidateExecutable != candidate || !filepath.IsAbs(candidate) || candidate != filepath.Clean(candidate) {
		t.Fatalf("downgrade candidate is not a clean absolute path: %q", candidate)
	}
}
