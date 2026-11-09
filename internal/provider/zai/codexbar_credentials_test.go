package zai

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestBrokenCodexBarPreservesOwnedZAIAvailability(t *testing.T) {
	fs := fsutil.NewMemFS()
	home, _ := fs.UserHomeDir()
	path := filepath.Join(home, ".codexbar", "config.json")
	_ = fs.MkdirAll(filepath.Dir(path), 0o700)
	_ = fs.WriteFile(path, []byte(`{broken`), 0o600)
	if _, err := StoreAccount(fs, "/config", "owned-key", "CQ", true); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].APIKey != "owned-key" {
		t.Fatalf("healthy account or source diagnostic lost: %d accounts, %v", len(accounts), err)
	}
}

func TestInvalidCodexBarRecordPreservesOwnedZAIAvailability(t *testing.T) {
	for _, record := range []map[string]string{{"id": "bad", "token": "bad\nkey"}, {"id": "bad", "token": "valid-key", "label": strings.Repeat("x", 129)}} {
		fs := fsutil.NewMemFS()
		home, _ := fs.UserHomeDir()
		path := filepath.Join(home, ".codexbar", "config.json")
		_ = fs.MkdirAll(filepath.Dir(path), 0o700)
		body, _ := json.Marshal(map[string]any{"version": 1, "providers": []any{map[string]any{"id": "zai", "tokenAccounts": map[string]any{"version": 1, "activeIndex": 0, "accounts": []any{record}}}}})
		_ = fs.WriteFile(path, body, 0o600)
		if _, err := StoreAccount(fs, "/config", "owned-key", "CQ", true); err != nil {
			t.Fatal(err)
		}
		accounts, err := ReadAccounts(fs, noEnvironment, "/config")
		if err != nil || len(accounts) != 1 || accounts[0].APIKey != "owned-key" {
			t.Fatalf("healthy account or invalid-source diagnostic lost: %d accounts, %v", len(accounts), err)
		}
	}
}

func TestReadAccountsExcludedUnverifiedCodexBarWithoutAdoption(t *testing.T) {
	fs := fsutil.NewMemFS()
	home, err := fs.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codexbar", "config.json")
	if err := fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"providers":[{"id":"zai","tokenAccounts":{"version":1,"activeIndex":1,"accounts":[{"id":"one","label":"First","token":"external-one","usageScope":"personal"},{"id":"two","label":"Second","token":"external-two","usageScope":"personal"}]}}]}`)
	if err := fs.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(accounts) != 0 {
		t.Fatalf("count=%d err=%v", len(accounts), err)
	}
	if err := SetActiveAccount(fs, "/config", "Second"); err == nil {
		t.Fatal("borrowed account activated through owned store")
	}
	if err := RemoveAccount(fs, "/config", "Second"); err == nil {
		t.Fatal("borrowed account removed through owned store")
	}
	if _, err := StoreAccount(fs, "/config", "owned", "CQ", true); err != nil {
		t.Fatal(err)
	}
	owned, err := fs.ReadFile("/config/zai.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(owned), "external-") {
		t.Fatal("borrowed secrets adopted")
	}
	after, _ := fs.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("CodexBar source changed")
	}
	accounts, err = ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].APIKey != "owned" {
		t.Fatal("CQ preference not preserved")
	}
	public, _ := json.Marshal(accounts)
	if strings.Contains(string(public), "external-one") || strings.Contains(string(public), "external-two") {
		t.Fatal("secret leaked")
	}
}

func TestReadAccountsCodexBarOverrideAndDedup(t *testing.T) {
	fs := fsutil.NewMemFS()
	home, _ := fs.UserHomeDir()
	path := filepath.Join(home, ".codexbar", "config.json")
	_ = fs.MkdirAll(filepath.Dir(path), 0o700)
	_ = fs.WriteFile(path, []byte(`{"version":1,"providers":[{"id":"zai","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"one","token":"same"}]}}]}`), 0o600)
	if _, err := StoreAccount(fs, "/config", "same", "CQ", true); err != nil {
		t.Fatal(err)
	}
	a, err := ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(a) != 1 || a[0].Source == "codexbar" {
		t.Fatal("dedup lost owned precedence")
	}
	env := func(k string) string {
		if k == "ZAI_API_KEY" {
			return "override"
		}
		return ""
	}
	a, err = ReadAccounts(fs, env, "/config")
	if err != nil || len(a) != 1 || a[0].APIKey != "override" {
		t.Fatal("environment override changed")
	}
}
