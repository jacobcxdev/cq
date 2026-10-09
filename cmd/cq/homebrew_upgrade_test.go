package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Run the generated custom block through Homebrew's real artifact classes.
func loadGeneratedCQHomebrewArtifacts(t *testing.T) string {
	return runGeneratedCQHomebrewArtifacts(t, "", false)
}

func runGeneratedCQHomebrewArtifacts(t *testing.T, prelude string, wantFailure bool) string {
	return runGeneratedCQHomebrewArtifactsWithMetadata(t, prelude, wantFailure, false)
}

func runGeneratedCQHomebrewArtifactsWithMetadata(t *testing.T, prelude string, wantFailure, metadata bool) string {
	return runGeneratedCQHomebrewArtifactsWithFormatting(t, prelude, wantFailure, metadata, false)
}

func runGeneratedCQHomebrewArtifactsWithFormatting(t *testing.T, prelude string, wantFailure, metadata, format bool) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("native Homebrew test")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Fatal("native Homebrew test requires brew")
	}
	data, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	block := strings.SplitN(string(data), "    custom_block: |\n", 2)[1]
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		if line != "" && !strings.HasPrefix(line, "      ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "      "))
	}
	block = strings.ReplaceAll(strings.Join(lines, "\n"), "{{ .Version }}", "0.34.0")
	caskContent := `cask "cq" do
  version "0.34.0"
  sha256 :no_check
  url "https://example.invalid/cq.zip"
` + block + "\nend\n"
	if format {
		path := filepath.Join(t.TempDir(), "cq.rb")
		if err := os.WriteFile(path, []byte(caskContent), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command("/bin/bash", "../../.github/scripts/format-homebrew-cask.sh", path).CombinedOutput()
		if err != nil {
			t.Fatalf("Homebrew formatter failed: %v\n%s", err, output)
		}
		formatted, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		caskContent = string(formatted)
	}
	metadataRoot := filepath.Join(t.TempDir(), "caskroom")
	metadataSetup := ""
	metadataProbe := ""
	if metadata {
		metadataSetup = `require "cask/installer"
Cask::Caskroom.singleton_class.define_method(:path) { Pathname(` + strconv.Quote(metadataRoot) + `) }
`
		metadataProbe = `
installer = Cask::Installer.new(cask)
installer.save_caskfile
files = cask.metadata_versioned_path.glob("**/cq.*")
raise "saved metadata missing" unless files.length == 1
cask = Cask::CaskLoader.load_from_installed_caskfile(files.first, api_fallback: false)
`
	}
	script := `require "cask/cask_loader"
require "json"
class CQCommandRecorder
  class << self
    attr_accessor :calls
    def run(executable, **kwargs)
      self.calls << [executable.to_s, kwargs[:args]]
      Struct.new(:stdout, :stderr, :exit_status) { def success?; true; end }.new("", "", 0)
    end
  end
end
CQCommandRecorder.calls = []
` + metadataSetup + prelude + `
cask = Cask::CaskLoader::FromContentLoader.new(<<~'CASK').load(config: nil)
` + caskContent + `CASK
` + metadataProbe + `
artifact = cask.artifacts.find { |a| a.is_a?(Cask::Artifact::Uninstall) }
successor = Cask::Cask.new("cq") { version "0.34.1" }
artifact.uninstall_phase(command: CQCommandRecorder, successor: successor, upgrade: true)
raise "upgrade called removal script" unless CQCommandRecorder.calls.empty?
artifact.uninstall_phase(command: CQCommandRecorder, successor: successor, reinstall: true)
raise "reinstall called removal script" unless CQCommandRecorder.calls.empty?
artifact.uninstall_phase(command: CQCommandRecorder)
raise "true uninstall skipped removal" unless CQCommandRecorder.calls.length == 1
puts JSON.generate(CQCommandRecorder.calls)
`
	path := filepath.Join(t.TempDir(), "homebrew-upgrade.rb")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(brew, "ruby", path).CombinedOutput()
	if wantFailure {
		if err == nil || !strings.Contains(string(output), "CQ upgrade requires a supported Homebrew uninstall callback") {
			t.Fatalf("unsupported callback accepted: %v %s", err, output)
		}
		return string(output)
	}
	if err != nil {
		t.Fatalf("Homebrew callback failed: %v\n%s", err, output)
	}
	return string(output)
}

func TestHomebrewUninstallCallbackReceivesSuccessor(t *testing.T) {
	output := loadGeneratedCQHomebrewArtifacts(t)
	if !strings.Contains(output, "cq-homebrew-lifecycle") {
		t.Fatal("true uninstall script not recorded")
	}
}

func TestHomebrewUnsupportedCallbackFailsBeforeMutation(t *testing.T) {
	runGeneratedCQHomebrewArtifacts(t, `class Cask::Artifact::Uninstall
  def uninstall_phase(command:); raise "unexpected removal mutation"; end
end`, true)
}

func TestHomebrewInstalledMetadataRetainsSuccessorCallback(t *testing.T) {
	runGeneratedCQHomebrewArtifactsWithMetadata(t, "", false, true)
}

func TestHomebrewFormattedMetadataRetainsSuccessorCallback(t *testing.T) {
	runGeneratedCQHomebrewArtifactsWithFormatting(t, "", false, true, true)
}
