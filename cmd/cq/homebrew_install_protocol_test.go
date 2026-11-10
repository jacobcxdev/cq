package main

import (
	"os/exec"
	"runtime"
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
