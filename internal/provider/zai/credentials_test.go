package zai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestReadAPIKey(t *testing.T) {
	for _, tt := range []struct {
		name, env, file, want string
		wantErr               bool
	}{
		{name: "missing"},
		{name: "environment", env: " environment-key ", want: "environment-key"},
		{name: "file", file: `{"api_key":"file-key"}`, want: "file-key"},
		{name: "environment wins", env: "environment-key", file: `not json`, want: "environment-key"},
		{name: "empty file key", file: `{"api_key":" "}`},
		{name: "invalid JSON", file: `{"api_key":"private-key",`, wantErr: true},
		{name: "invalid key type", file: `{"api_key":123}`, wantErr: true},
		{name: "header injection", env: "private-key\r\nX-Secret: secret", wantErr: true},
		{name: "file header injection", file: `{"api_key":"private-key\nsecret"}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if tt.file != "" {
				if err := fsys.WriteFile("/config/zai.json", []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			key, err := ReadAPIKey(fsys, func(name string) string {
				if name == "ZAI_API_KEY" {
					return tt.env
				}
				return ""
			}, "/config")
			if (err != nil) != tt.wantErr || key != tt.want {
				t.Fatalf("key present=%t, error=%v; expected key present=%t, error=%t", key != "", err, tt.want != "", tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "private-key") {
				t.Fatal("credential leaked through error")
			}
		})
	}
}

type readErrorFS struct{ fsutil.FileSystem }

func (readErrorFS) ReadFile(string) ([]byte, error) { return nil, os.ErrPermission }

func TestReadAPIKeyReadError(t *testing.T) {
	_, err := ReadAPIKey(readErrorFS{fsutil.NewMemFS()}, func(string) string { return "" }, "/config")
	if err == nil {
		t.Fatal("expected read error")
	}
}

func noEnvironment(string) string { return "" }

func TestAccountsPreserveLegacyAndPreferredKey(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.WriteFile("/config/zai.json", []byte(`{"api_key":"old-private-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := StoreAccount(fsys, "/config", "new-private-key", "Second subscription", false)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].APIKey != "old-private-key" || accounts[1].ID != added.ID || accounts[1].Label != "Second subscription" {
		t.Fatalf("account migration failed: count=%d err=%v", len(accounts), err)
	}
	if accounts[0].ID == added.ID || strings.Contains(added.ID, "private") {
		t.Fatal("expected distinct opaque account IDs")
	}
	if err := SetActiveAccount(fsys, "/config", added.ID); err != nil {
		t.Fatal(err)
	}
	key, err := ReadAPIKey(fsys, noEnvironment, "/config")
	if err != nil || key != "new-private-key" {
		t.Fatalf("preferred key was not selected: err=%v", err)
	}
	data, err := fsys.ReadFile("/config/zai.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(data, &legacy) != nil || legacy.APIKey != key {
		t.Fatal("legacy api_key does not match preferred account")
	}
	for path, want := range map[string]os.FileMode{"/config": 0o700, "/config/zai.json": 0o600} {
		info, err := fsys.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("permissions for %s: want %o, err=%v", path, want, err)
		}
	}
	public, err := json.Marshal(accounts)
	if err != nil || strings.Contains(string(public), "private-key") {
		t.Fatal("public account serialisation leaked API keys")
	}
}

func TestAccountsDeduplicateActivateAndRemove(t *testing.T) {
	fsys := fsutil.NewMemFS()
	first, err := StoreAccount(fsys, "/config", "first-key", "First", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StoreAccount(fsys, "/config", "second-key", "Second", false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := StoreAccount(fsys, "/config", " second-key ", "Renamed", true)
	if err != nil || again.ID != second.ID {
		t.Fatalf("duplicate account changed identity: %v", err)
	}
	accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].ID != second.ID || accounts[0].Label != "Renamed" {
		t.Fatalf("duplicate or activation failed: count=%d err=%v", len(accounts), err)
	}
	if err := SetActiveAccount(fsys, "/config", "First"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccount(fsys, "/config", first.ID); err != nil {
		t.Fatal(err)
	}
	accounts, err = ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].ID != second.ID {
		t.Fatalf("remove failed: %v", err)
	}
	if err := RemoveAccount(fsys, "/config", "Renamed"); err != nil {
		t.Fatal(err)
	}
	accounts, err = ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 0 {
		t.Fatalf("remove final account failed: %v", err)
	}
	key, err := ReadAPIKey(fsys, noEnvironment, "/config")
	if err != nil || key != "" {
		t.Fatalf("legacy key retained after removing final account: %v", err)
	}
}

func TestAccountsEnvironmentOverridesStoredAccounts(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := StoreAccount(fsys, "/config", "file-key", "File", false); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fsys, func(string) string { return "env-key" }, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].APIKey != "env-key" {
		t.Fatalf("environment override failed: %v", err)
	}
}

func TestAccountsRejectInvalidCredentialsWithoutChangingFile(t *testing.T) {
	for _, test := range []struct{ name, key, label string }{
		{"empty key", " ", ""},
		{"invalid key", "private-key\r\nX: private", ""},
		{"invalid label", "private-key", "Label\nprivate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			original := []byte(`{"api_key":"existing-key"}`)
			if err := fsys.WriteFile("/config/zai.json", original, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := StoreAccount(fsys, "/config", test.key, test.label, false)
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("expected credential-safe validation error")
			}
			got, err := fsys.ReadFile("/config/zai.json")
			if err != nil || string(got) != string(original) {
				t.Fatal("invalid account changed stored credentials")
			}
		})
	}
}

func TestAccountsMissingAndAmbiguousReferencesPreserveFile(t *testing.T) {
	fsys := fsutil.NewMemFS()
	for _, key := range []string{"one-key", "two-key"} {
		if _, err := StoreAccount(fsys, "/config", key, "Shared", false); err != nil {
			t.Fatal(err)
		}
	}
	original, err := fsys.ReadFile("/config/zai.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"Shared", "missing"} {
		if err := SetActiveAccount(fsys, "/config", reference); err == nil {
			t.Fatal("expected reference error")
		}
		if err := RemoveAccount(fsys, "/config", reference); err == nil {
			t.Fatal("expected reference error")
		}
	}
	got, err := fsys.ReadFile("/config/zai.json")
	if err != nil || string(got) != string(original) {
		t.Fatal("failed reference changed credentials")
	}
}

func TestAccountsRejectMalformedFilesWithoutOverwrite(t *testing.T) {
	for _, contents := range []string{
		`{"accounts":[{"api_key":"private-key\nsecret"}]}`,
		`{"accounts":[{"api_key":""}]}`,
		`{"accounts":[{"api_key":"private-key","label":"bad\nlabel"}]}`,
		`{"accounts":[{"api_key":"private-key","id":"wrong"}]}`,
		`{"api_key":"private-key",`,
	} {
		fsys := fsutil.NewMemFS()
		if err := fsys.WriteFile("/config/zai.json", []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAccounts(fsys, noEnvironment, "/config"); err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("expected safe read error")
		}
		if _, err := StoreAccount(fsys, "/config", "new-key", "", false); err == nil {
			t.Fatal("malformed file was overwritten")
		}
		got, err := fsys.ReadFile("/config/zai.json")
		if err != nil || string(got) != contents {
			t.Fatal("malformed file changed")
		}
	}
}

func TestAccountsAtomicWriteFailurePreservesExisting(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.WriteFile("/config/zai.json", []byte(`{"api_key":"old-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A filesystem without retained-directory capabilities fails before any write.
	_, err := StoreAccount(struct{ fsutil.FileSystem }{fsys}, "/config", "new-key", "", false)
	if err == nil {
		t.Fatal("expected persistence error")
	}
	key, err := ReadAPIKey(fsys, noEnvironment, "/config")
	if err != nil || key != "old-key" {
		t.Fatal("failed write replaced existing credentials")
	}
	entries, err := fsys.ReadDir(filepath.Join("/config"))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed write left temporary credential file")
	}
}
