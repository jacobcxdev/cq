package codexbar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestReadAccountsProviderScopedAndReadOnly(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := "/home/.codexbar/config.json"
	if err := fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"providers":[{"id":"zai","tokenAccounts":{"version":1,"activeIndex":1,"accounts":[{"id":"one","label":"First","token":"private-one"},{"id":"two","label":"Second","token":"private-two"}]}},{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"g","label":"Grok","token":"private-grok"}]}}]}`)
	if err := fs.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fs, func(string) string { return "" }, "/home", "zai")
	if err != nil || len(accounts) != 2 || accounts[0].ID != "two" || !accounts[0].Active || accounts[1].Token != "private-one" {
		t.Fatalf("accounts=%d err=%v", len(accounts), err)
	}
	public, _ := json.Marshal(accounts)
	if strings.Contains(string(public), "private") {
		t.Fatal("secret leaked")
	}
	after, _ := fs.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("source mutated")
	}
}

func TestReadAccountsPrecedenceAndMalformed(t *testing.T) {
	fs := fsutil.NewMemFS()
	if err := fs.MkdirAll("/explicit", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/explicit/config.json", []byte(`{"version":2,"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == "CODEXBAR_CONFIG" {
			return "/explicit/config.json"
		}
		return ""
	}
	if _, err := ReadAccounts(fs, env, "/home", "grok"); err == nil {
		t.Fatal("unsupported version accepted")
	}
	if err := fs.WriteFile("/explicit/config.json", []byte(`{"version":1,"providers":[{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"one","token":"secret"},{"id":"one","token":"other"}]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAccounts(fs, env, "/home", "grok"); err == nil {
		t.Fatal("duplicate account accepted")
	}
}

func TestReadAccountsRejectsUnsafeSource(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			fs := fsutil.NewMemFS()
			_ = fs.MkdirAll("/home/.codexbar", 0o700)
			_ = fs.WriteFile("/home/.codexbar/config.json", []byte(`{"version":1,"providers":[]}`), mode)
			if _, err := ReadAccounts(fs, func(string) string { return "" }, "/home", "grok"); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
	home := t.TempDir()
	path := filepath.Join(home, ".codexbar", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAccounts(fsutil.OSFileSystem{}, func(string) string { return "" }, home, "grok"); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestReadAccountsCurrentAbsenceDoesNotResurrectLegacy(t *testing.T) {
	fs := fsutil.NewMemFS()
	current := "/home/.codexbar/config.json"
	legacy := "/home/Library/Application Support/CodexBar/token-accounts.json"
	for _, path := range []string{current, legacy} {
		if err := fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = fs.WriteFile(legacy, []byte(`{"version":1,"providers":{"grok":{"version":1,"activeIndex":0,"accounts":[{"id":"old","token":"stale"}]}}}`), 0o600)
	_ = fs.WriteFile(current, []byte(`{"version":1,"providers":[{"id":"grok"}]}`), 0o600)
	accounts, err := ReadAccounts(fs, func(string) string { return "" }, "/home", "grok")
	if err != nil || len(accounts) != 0 {
		t.Fatalf("stale accounts resurrected: count=%d err=%v", len(accounts), err)
	}
	if err := fs.Remove(current); err != nil {
		t.Fatal(err)
	}
	accounts, err = ReadAccounts(fs, func(string) string { return "" }, "/home", "grok")
	if err != nil || len(accounts) != 1 || accounts[0].ID != "old" {
		t.Fatal("legacy reader failed")
	}
}

func TestReadAccountsXDGExpandsHomeAndOwnsPrecedence(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := "/home/custom/codexbar/config.json"
	if err := fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = fs.WriteFile(path, []byte(`{"version":1,"providers":[{"id":"grok","tokenAccounts":{"version":1,"activeIndex":0,"accounts":[{"id":"xdg","token":"bearer"}]}}]}`), 0o600)
	env := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "~/custom"
		}
		return ""
	}
	accounts, err := ReadAccounts(fs, env, "/home", "grok")
	if err != nil || len(accounts) != 1 || accounts[0].ID != "xdg" {
		t.Fatalf("XDG selection failed count=%d err=%v", len(accounts), err)
	}
}
