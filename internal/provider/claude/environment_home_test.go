package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserDirsActiveCredentialUsesNativeHome(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	setClaudeTestHome(t, home)
	for _, name := range []string{"HOME", "USERPROFILE", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(name, other)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"email":"native@example.test"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := activeCredentialEmail(); got != "native@example.test" {
		t.Fatalf("active email %q", got)
	}
}
