package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/keyring"
)

func TestMain(m *testing.M) {
	resolveActiveCredentialHome = func() (string, error) { return "", os.ErrNotExist }
	os.Exit(m.Run())
}

func setActiveCredentialsTestHome(t *testing.T, home string) {
	t.Helper()
	old := resolveActiveCredentialHome
	resolveActiveCredentialHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { resolveActiveCredentialHome = old })
	t.Setenv("HOME", "untrusted-shell-home")
	t.Setenv("USERPROFILE", "untrusted-shell-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", "untrusted-model-cache")
	t.Setenv("CODEX_HOME", "untrusted-codex-cache")
}

// --- GetActiveCredentials ---

func TestGetActiveCredentials(t *testing.T) {
	writeCredentials := func(t *testing.T, dir string, token, email string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		creds := keyring.ClaudeCredentials{
			ClaudeAiOauth: &keyring.ClaudeOAuth{
				AccessToken: token,
				Email:       email,
			},
		}
		data, err := json.MarshalIndent(creds, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		path := filepath.Join(dir, ".claude", ".credentials.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	t.Run("valid credentials file returns token and email", func(t *testing.T) {
		dir := t.TempDir()
		setActiveCredentialsTestHome(t, dir)
		writeCredentials(t, dir, "mytoken123", "user@example.com")

		tok, email := GetActiveCredentials()
		if tok != "mytoken123" {
			t.Errorf("token = %q, want mytoken123", tok)
		}
		if email != "user@example.com" {
			t.Errorf("email = %q, want user@example.com", email)
		}
	})

	t.Run("missing file returns empty strings", func(t *testing.T) {
		dir := t.TempDir()
		setActiveCredentialsTestHome(t, dir)

		tok, email := GetActiveCredentials()
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
		if email != "" {
			t.Errorf("email = %q, want empty", email)
		}
	})

	t.Run("invalid JSON returns empty strings", func(t *testing.T) {
		dir := t.TempDir()
		setActiveCredentialsTestHome(t, dir)
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, ".claude", ".credentials.json")
		if err := os.WriteFile(path, []byte("not valid json {{{"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		tok, email := GetActiveCredentials()
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
		if email != "" {
			t.Errorf("email = %q, want empty", email)
		}
	})
}
