package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
)

func TestCacheTTL(t *testing.T) {
	tests := []struct {
		name  string
		value string // empty string means unset
		want  time.Duration
	}{
		{"empty string", "", 30 * time.Second},
		{"valid 60", "60", 60 * time.Second},
		{"valid 0", "0", 0},
		{"negative clamped to 0", "-5", 0},
		{"above max clamped to 3600", "3601", 3600 * time.Second},
		{"exactly 3600", "3600", 3600 * time.Second},
		{"non-numeric falls back", "abc", 30 * time.Second},
		{"float non-numeric falls back", "1.5", 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value == "" {
				t.Setenv("CQ_TTL", "")
			} else {
				t.Setenv("CQ_TTL", tt.value)
			}
			got := cacheTTL()
			if got != tt.want {
				t.Errorf("cacheTTL() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- AccountManager ---

func TestAccountManager(t *testing.T) {
	t.Run("Claude returns non-nil", func(t *testing.T) {
		if got := app.AccountManager(provider.Claude, nil); got == nil {
			t.Error("AccountManager(Claude) = nil, want non-nil")
		}
	})

	t.Run("Codex returns non-nil", func(t *testing.T) {
		if got := app.AccountManager(provider.Codex, nil); got == nil {
			t.Error("AccountManager(Codex) = nil, want non-nil")
		}
	})

	t.Run("Gemini returns nil", func(t *testing.T) {
		if got := app.AccountManager(provider.Gemini, nil); got != nil {
			t.Errorf("AccountManager(Gemini) = %v, want nil", got)
		}
	})
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
		t.Setenv("HOME", dir)
		writeCredentials(t, dir, "mytoken123", "user@example.com")

		tok, email := app.GetActiveCredentials()
		if tok != "mytoken123" {
			t.Errorf("token = %q, want mytoken123", tok)
		}
		if email != "user@example.com" {
			t.Errorf("email = %q, want user@example.com", email)
		}
	})

	t.Run("missing file returns empty strings", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		tok, email := app.GetActiveCredentials()
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
		if email != "" {
			t.Errorf("email = %q, want empty", email)
		}
	})

	t.Run("invalid JSON returns empty strings", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, ".claude", ".credentials.json")
		if err := os.WriteFile(path, []byte("not valid json {{{"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		tok, email := app.GetActiveCredentials()
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
		if email != "" {
			t.Errorf("email = %q, want empty", email)
		}
	})
}

// --- isTerminal ---

func TestIsTerminal(t *testing.T) {
	// isTerminal simply inspects os.Stdout; it must not panic.
	// In a test environment stdout is not a char device, so it returns false.
	got := isTerminal()
	if got {
		t.Error("expected false in test environment (stdout is not a char device)")
	}
}

// --- dispatch ---

func TestDispatchUnknownCommandReturnsError(t *testing.T) {
	// We need a kong.Context whose Command() returns something not in the switch.
	// Define a minimal CLI type with a single command that dispatch doesn't handle.
	type unknownCLI struct {
		Bogus struct{} `cmd:""`
	}
	var cli unknownCLI
	// Parse "bogus" against our stub CLI to get a real *kong.Context.
	kctx, err := kong.New(&cli,
		kong.Writers(io.Discard, io.Discard),
		kong.Exit(func(int) {}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	parsed, err := kctx.Parse([]string{"bogus"})
	if err != nil {
		t.Fatalf("kctx.Parse: %v", err)
	}

	// dispatch expects a *kong.Context; pass our real CLI as well (unused for
	// the default branch).
	var mainCLI CLI
	dispatchErr := dispatch(parsed, &mainCLI)
	if dispatchErr == nil {
		t.Fatal("dispatch returned nil error for unknown command, want non-nil")
	}
}

func TestCLIParsesRemoveCommands(t *testing.T) {
	var cli CLI
	kctx, err := kong.New(&cli,
		kong.Writers(io.Discard, io.Discard),
		kong.Exit(func(int) {}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "claude remove", args: []string{"claude", "remove", "user@example.com"}, want: "claude remove <email>"},
		{name: "codex remove", args: []string{"codex", "remove", "user@example.com"}, want: "codex remove <email>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := kctx.Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%v): %v", tt.args, err)
			}
			if got := parsed.Command(); got != tt.want {
				t.Fatalf("Command() = %q, want %q", got, tt.want)
			}
		})
	}
}
