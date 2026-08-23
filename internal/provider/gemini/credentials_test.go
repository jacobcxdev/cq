package gemini

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type staticCredentialReader struct {
	raw string
	err error
}

func (r staticCredentialReader) Get(_, _ string) (string, error) {
	return r.raw, r.err
}

func credentialFixture(accessToken, refreshToken, expiry string) string {
	return "{\"auth_method\":\"oauth-personal\",\"token\":{\"access_token\":\"" + accessToken +
		"\",\"refresh_token\":\"" + refreshToken + "\",\"expiry\":\"" + expiry +
		"\",\"token_type\":\"Bearer\"}}"
}

func TestReadCredentialsDecodesRequiredFields(t *testing.T) {
	wantExpiry := "2026-08-23T12:34:56Z"
	got, err := readCredentials(staticCredentialReader{raw: credentialFixture("access", "refresh", wantExpiry)})
	if err != nil {
		t.Fatalf("readCredentials() error = %v", err)
	}
	if got.AccessToken != "access" || got.RefreshToken != "refresh" || got.TokenType != "Bearer" {
		t.Fatalf("tokens/type = %#v, want decoded values", got)
	}
	if want := time.Date(2026, 8, 23, 12, 34, 56, 0, time.UTC); !got.Expiry.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got.Expiry, want)
	}
}

func TestReadCredentialsClassifiesMissingAndMalformedData(t *testing.T) {
	tests := []struct {
		name   string
		reader credentialReader
		want   error
	}{
		{name: "missing", reader: staticCredentialReader{err: os.ErrNotExist}, want: errCredentialsNotFound},
		{name: "invalid json", reader: staticCredentialReader{raw: "{\"token\":"}, want: errInvalidCredentials},
		{name: "missing expiry", reader: staticCredentialReader{raw: credentialFixture("access", "refresh", "")}, want: errInvalidCredentials},
		{name: "invalid expiry", reader: staticCredentialReader{raw: credentialFixture("access", "refresh", "soon")}, want: errInvalidCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readCredentials(tt.reader)
			if !errors.Is(err, tt.want) {
				t.Fatalf("readCredentials() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReadCredentialsDoesNotExposePayload(t *testing.T) {
	secret := "private-access-token"
	_, err := readCredentials(staticCredentialReader{raw: credentialFixture(secret, "refresh", "bad")})
	if err == nil {
		t.Fatal("readCredentials() error = nil, want malformed credential error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed credential payload: %q", err)
	}
}

func TestReadProjectIDTrimsValidCache(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := filepath.Join("/home/test", antigravityProjectCachePath)
	if err := fsys.WriteFile(path, []byte("  project-123 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readProjectID(fsys)
	if err != nil {
		t.Fatalf("readProjectID() error = %v", err)
	}
	if got != "project-123" {
		t.Fatalf("readProjectID() = %q, want project-123", got)
	}
}

func TestReadProjectIDAllowsMissingCache(t *testing.T) {
	got, err := readProjectID(fsutil.NewMemFS())
	if err != nil {
		t.Fatalf("readProjectID() error = %v", err)
	}
	if got != "" {
		t.Fatalf("readProjectID() = %q, want empty fallback signal", got)
	}
}

func TestReadProjectIDRejectsMalformedCache(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: []byte("  \n")},
		{name: "oversized", data: []byte(strings.Repeat("x", maxProjectIDBytes+1))},
		{name: "control", data: []byte("project\x00id")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			path := filepath.Join("/home/test", antigravityProjectCachePath)
			if err := fsys.WriteFile(path, tt.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readProjectID(fsys); !errors.Is(err, errInvalidProjectID) {
				t.Fatalf("readProjectID() error = %v, want %v", err, errInvalidProjectID)
			}
		})
	}
}
