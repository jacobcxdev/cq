package keyring

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
	gokeyring "github.com/zalando/go-keyring"
)

func failCQManifestRootResolution(t *testing.T, wantErr error) {
	t.Helper()
	original := resolveCQManifestRoots
	t.Cleanup(func() { resolveCQManifestRoots = original })
	resolveCQManifestRoots = func() (userdirs.Roots, error) {
		return userdirs.Roots{}, wantErr
	}
}

func writeCQManifestFixture(t *testing.T, home string, entries []manifestEntry) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".cache", "cq", "accounts.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create manifest directory: %v", err)
	}
	if err := saveManifest(path, entries); err != nil {
		t.Fatalf("save manifest fixture: %v", err)
	}
}

func TestDefaultCQManifestPathPropagatesRootResolutionError(t *testing.T) {
	wantErr := errors.New("roots unavailable")
	failCQManifestRootResolution(t, wantErr)

	_, err := defaultCQManifestPath()
	if !errors.Is(err, wantErr) {
		t.Fatalf("defaultCQManifestPath() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestStoreCQAccountResolvesManifestBeforeKeyringMutation(t *testing.T) {
	gokeyring.MockInit()
	wantErr := errors.New("roots unavailable")
	failCQManifestRootResolution(t, wantErr)
	acct := &ClaudeOAuth{
		AccountUUID: "uuid-store",
		Email:       "alice@example.com",
		AccessToken: "secret",
	}

	err := StoreCQAccount(acct)
	if !errors.Is(err, wantErr) {
		t.Fatalf("StoreCQAccount() error = %v, want wrapping %v", err, wantErr)
	}
	service := ServicePrefix + Hash8(acct.AccountUUID)
	if _, err := gokeyring.Get(service, acct.AccountUUID); !errors.Is(err, gokeyring.ErrNotFound) {
		t.Fatalf("keyring lookup error = %v, want %v", err, gokeyring.ErrNotFound)
	}
}

func TestRemoveCQAccountResolvesManifestBeforeKeyringMutation(t *testing.T) {
	gokeyring.MockInit()
	home := t.TempDir()
	entry := manifestEntry{UUID: "uuid-remove", Email: "alice@example.com"}
	writeCQManifestFixture(t, home, []manifestEntry{entry})
	service := ServicePrefix + Hash8(entry.UUID)
	if err := gokeyring.Set(service, entry.UUID, "secret"); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}
	wantErr := errors.New("roots unavailable")
	failCQManifestRootResolution(t, wantErr)

	err := RemoveCQClaudeAccountsByEmail(entry.Email)
	if !errors.Is(err, wantErr) {
		t.Fatalf("RemoveCQClaudeAccountsByEmail() error = %v, want wrapping %v", err, wantErr)
	}
	if got, err := gokeyring.Get(service, entry.UUID); err != nil || got != "secret" {
		t.Fatalf("keyring entry after failed resolution = %q, %v; want unchanged", got, err)
	}
}

func TestDiscoverCQKeyringReturnsNoRowsWhenManifestResolutionFails(t *testing.T) {
	gokeyring.MockInit()
	home := t.TempDir()
	entry := manifestEntry{UUID: "uuid-discover", Email: "alice@example.com"}
	writeCQManifestFixture(t, home, []manifestEntry{entry})
	service := ServicePrefix + Hash8(entry.UUID)
	account := `{"accessToken":"secret","email":"alice@example.com","accountUUID":"uuid-discover"}`
	if err := gokeyring.Set(service, entry.UUID, account); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}
	failCQManifestRootResolution(t, errors.New("roots unavailable"))

	if got := discoverCQKeyring(make(map[string]bool)); len(got) != 0 {
		t.Fatalf("discoverCQKeyring() = %+v, want no CQ-managed rows", got)
	}
}
