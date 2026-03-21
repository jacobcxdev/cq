//go:build darwin

package keyring

import "testing"

// ── parseKeychainEntry ────────────────────────────────────────────────────────

func TestParseKeychainEntry(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		if got := parseKeychainEntry(""); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("valid JSON with access token returns entry", func(t *testing.T) {
		raw := `{"claudeAiOauth":{"accessToken":"tok123","refreshToken":"rt","expiresAt":9999}}`
		got := parseKeychainEntry(raw)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.AccessToken != "tok123" {
			t.Errorf("AccessToken = %q, want tok123", got.AccessToken)
		}
		if got.RefreshToken != "rt" {
			t.Errorf("RefreshToken = %q, want rt", got.RefreshToken)
		}
	})

	t.Run("invalid JSON returns nil", func(t *testing.T) {
		if got := parseKeychainEntry("{not valid json}"); got != nil {
			t.Errorf("expected nil for invalid JSON, got %+v", got)
		}
	})

	t.Run("missing claudeAiOauth key returns nil", func(t *testing.T) {
		if got := parseKeychainEntry(`{"other":"value"}`); got != nil {
			t.Errorf("expected nil for missing claudeAiOauth, got %+v", got)
		}
	})

	t.Run("claudeAiOauth present but empty access token returns nil", func(t *testing.T) {
		raw := `{"claudeAiOauth":{"accessToken":"","refreshToken":"rt","expiresAt":100}}`
		if got := parseKeychainEntry(raw); got != nil {
			t.Errorf("expected nil for empty accessToken, got %+v", got)
		}
	})
}
