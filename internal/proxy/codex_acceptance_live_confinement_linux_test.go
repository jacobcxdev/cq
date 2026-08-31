//go:build linux

package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func prepareCodexAcceptanceTestConfinement(t *testing.T) {
	t.Helper()
	helperPath := filepath.Join(t.TempDir(), "cq")
	build := exec.Command("go", "build", "-o", helperPath, "./cmd/cq")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Linux acceptance helper: %v\n%s", err, output)
	}
	previous := openLinuxAcceptanceHelper
	openLinuxAcceptanceHelper = func() (*os.File, error) { return os.Open(helperPath) }
	t.Cleanup(func() { openLinuxAcceptanceHelper = previous })
}
