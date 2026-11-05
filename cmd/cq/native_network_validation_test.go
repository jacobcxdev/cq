//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeNetworkValidationRestoresReviewedCask(t *testing.T) {
	if _, err := exec.LookPath("ruby"); err != nil {
		t.Skip("native Cask validation requires Ruby")
	}
	helper, err := os.ReadFile("../../.github/scripts/install-native-network-validation.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, failedInstall := range []bool{false, true} {
		name := "installed"
		if failedInstall {
			name = "install-failed"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(path string, data string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(data), mode); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(bin, "uname"), "#!/bin/bash\nprintf 'Darwin\\n'\n", 0o700)
			write(filepath.Join(bin, "brew"), `#!/bin/bash
set -euo pipefail
tap_root="$CQ_NATIVE_VALIDATION_FIXTURE_ROOT/tap"
case "$*" in
  tap) ;;
  'list --cask cq-network') exit 1 ;;
  'tap-new --no-git jacobcxdev/tap') mkdir -p "$tap_root" ;;
  '--repository jacobcxdev/tap') printf '%s\n' "$tap_root" ;;
  'install --cask jacobcxdev/tap/cq-network')
    cp "$tap_root/Casks/cq-network.rb" "$CQ_NATIVE_VALIDATION_FIXTURE_ROOT/install-input.rb"
    [[ "$HOMEBREW_NO_AUTO_UPDATE" == 1 ]]
    if [[ "$CQ_NATIVE_VALIDATION_FIXTURE_FAIL" == true ]]; then exit 19; fi
    ;;
  *) echo "unexpected Homebrew invocation: $*" >&2; exit 64 ;;
esac
`, 0o700)
			const reviewedCask = `cask "cq-network" do
  version "0.41.8"
  sha256 "64978e713f3deae386c974ecda7f7dc2926ab52bab9b54e377e625c9e648e836"
  url "https://github.com/jacobcxdev/cq/releases/download/cq-network-v#{version}/CQNetwork_#{version}_universal.zip"
  app "CQNetwork.app"
end
`
			cask := filepath.Join(root, "reviewed.rb")
			archive := filepath.Join(root, "verified companion.zip")
			write(cask, reviewedCask, 0o600)
			write(archive, "verified archive fixture", 0o600)
			// Keep the installation-ownership guard inside this fixture as well;
			// all Homebrew operations and archive inputs are already synthetic.
			fixtureHelper := strings.ReplaceAll(string(helper), "/Applications/CQNetwork.app", filepath.Join(root, "Applications", "CQNetwork.app"))
			script := filepath.Join(root, "install-native-network-validation.sh")
			write(script, fixtureHelper, 0o700)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/bash", script, cask, archive)
			fail := "false"
			if failedInstall {
				fail = "true"
			}
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CI=true", "CQ_NATIVE_VALIDATION_FIXTURE_ROOT="+root, "CQ_NATIVE_VALIDATION_FIXTURE_FAIL="+fail)
			output, err := command.CombinedOutput()
			if failedInstall {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 19 {
					t.Fatalf("failed Homebrew install status was not preserved: %v\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("validation install failed: %v\n%s", err, output)
			}
			installed, err := os.ReadFile(filepath.Join(root, "install-input.rb"))
			if err != nil {
				t.Fatal(err)
			}
			localCask := strings.ReplaceAll(reviewedCask, `  url "https://github.com/jacobcxdev/cq/releases/download/cq-network-v#{version}/CQNetwork_#{version}_universal.zip"`, `  url "file://`+archive+`"`)
			if !bytes.Equal(installed, []byte(localCask)) {
				t.Fatalf("Homebrew did not install the exact local-archive Cask:\n%s", installed)
			}
			selected, err := os.ReadFile(filepath.Join(root, "tap", "Casks", "cq-network.rb"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(selected, []byte(reviewedCask)) {
				t.Fatalf("selected companion definition was not restored to the reviewed bytes:\n%s", selected)
			}
		})
	}
}
