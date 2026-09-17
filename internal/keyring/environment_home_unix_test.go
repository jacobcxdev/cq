//go:build unix

package keyring

import (
	"errors"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"os"
	"testing"
)

func TestUserDirsCredentialWriteRejectsRelativeHomeBeforeMutation(t *testing.T) {
	old := resolveCredentialHome
	t.Cleanup(func() { resolveCredentialHome = old })
	resolveCredentialHome = userdirs.UserHomeDir
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", "relative-private-home")
	err := WriteCredentialsFile(&ClaudeCredentials{})
	var diagnostic *userdirs.EnvironmentError
	if !errors.As(err, &diagnostic) || diagnostic.ExitCode != 2 {
		t.Fatalf("relative HOME result %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid root mutated directory: %v %v", entries, err)
	}
}
