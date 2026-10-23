package keyring

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserDirsClaudeCredentialsKeepNativeHome(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	setKeyringTestHome(t, home)
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "USERPROFILE", "HOME"} {
		t.Setenv(name, other)
	}
	creds := &ClaudeCredentials{ClaudeAiOauth: &ClaudeOAuth{AccessToken: "fixture", Email: "native@example.test"}}
	if err := WriteCredentialsFile(creds); err != nil {
		t.Fatal(err)
	}
	if got := ActiveClaudeEmail(); got != "native@example.test" {
		t.Fatalf("active identity %q", got)
	}
	if got := discoverCredentialsFile(map[string]bool{}); len(got) != 1 || got[0].Email != "native@example.test" {
		t.Fatalf("native discovery failed")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(other)
	if err != nil || len(entries) != 0 {
		t.Fatalf("poisoned home mutated: %v", err)
	}
}
