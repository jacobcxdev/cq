package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
)

// testDoer is an httputil.Doer stub for refresh tests.
type testDoer func(*http.Request) (*http.Response, error)

func (f testDoer) Do(req *http.Request) (*http.Response, error) { return f(req) }

func profileJSON(email string) *http.Response {
	body := `{"account":{"email":"` + email + `","uuid":"uuid-test"}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestSyncAnonymousToIdentifiedReturnsChangedTrue verifies that when
// syncAnonymousToIdentifiedWithChange successfully syncs an anonymous entry's
// fresh tokens into an identified account, it returns changed=true.
func TestSyncAnonymousToIdentifiedReturnsChangedTrue(t *testing.T) {
	nowMs := time.Now().UnixMilli()

	identified := keyring.ClaudeOAuth{
		Email:        "user@example.com",
		AccountUUID:  "uuid-user",
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ExpiresAt:    nowMs - 1000, // stale
	}
	anon := keyring.ClaudeOAuth{
		AccessToken:  "new-at",
		RefreshToken: "new-rt",
		ExpiresAt:    nowMs + 999999,
	}

	doer := testDoer(func(req *http.Request) (*http.Response, error) {
		return profileJSON("user@example.com"), nil
	})

	accounts := []keyring.ClaudeOAuth{identified, anon}
	result, changed := syncAnonymousToIdentifiedWithChange(context.Background(), doer, accounts, nowMs)

	if !changed {
		t.Fatal("expected changed=true when anon sync updated a stored account")
	}
	found := false
	for _, a := range result {
		if a.Email == "user@example.com" && a.AccessToken == "new-at" {
			found = true
		}
	}
	if !found {
		t.Errorf("identified account should have new-at after sync; got %+v", result)
	}
}

// TestSyncAnonymousToIdentifiedNoMatchReturnsChangedFalse verifies that when
// no anonymous entry can be resolved to an identified account, changed=false.
func TestSyncAnonymousToIdentifiedNoMatchReturnsChangedFalse(t *testing.T) {
	nowMs := time.Now().UnixMilli()

	identified := keyring.ClaudeOAuth{
		Email:       "other@example.com",
		AccessToken: "at",
		ExpiresAt:   nowMs + 999999,
	}
	anon := keyring.ClaudeOAuth{
		AccessToken: "anon-at",
		ExpiresAt:   nowMs + 999999,
	}

	// Profile resolves to an email not in identified list — no match.
	doer := testDoer(func(req *http.Request) (*http.Response, error) {
		return profileJSON("nobody@example.com"), nil
	})

	accounts := []keyring.ClaudeOAuth{identified, anon}
	_, changed := syncAnonymousToIdentifiedWithChange(context.Background(), doer, accounts, nowMs)

	if changed {
		t.Fatal("expected changed=false when no anonymous entry matched an identified account")
	}
}

// TestInvalidateProviderCacheRemovesFile verifies that invalidateProviderCache
// removes the cached result file for the given provider.
func TestInvalidateProviderCacheRemovesFile(t *testing.T) {
	dir := t.TempDir()
	// Point XDG_CACHE_HOME so DefaultDir() resolves to our temp tree.
	t.Setenv("XDG_CACHE_HOME", dir)

	// Compute what DefaultDir returns and write a placeholder cache file.
	cacheDir := filepath.Join(dir, "cq")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cachePath := filepath.Join(cacheDir, string(provider.Claude)+".json")
	if err := os.WriteFile(cachePath, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	invalidateProviderCache(provider.Claude)

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("cache file should have been removed; stat err = %v", err)
	}
}
