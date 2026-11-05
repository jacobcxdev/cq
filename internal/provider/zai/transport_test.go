package zai

import (
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestAccountTransportSurvivesAccountChanges(t *testing.T) {
	fsys := fsutil.NewMemFS()
	first, err := StoreAccount(fsys, "/config", "first-key", "first", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StoreAccount(fsys, "/config", "second-key", "second", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTransport(fsys, "/config", first.ID, "zcode"); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreAccount(fsys, "/config", "first-key", "renamed", false); err != nil {
		t.Fatal(err)
	}
	if err := SetActiveAccount(fsys, "/config", second.ID); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccount(fsys, "/config", second.ID); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].Transport != "zcode" || accounts[0].Label != "renamed" {
		t.Fatalf("transport lost after account update: count=%d, err=%v", len(accounts), err)
	}
	before, _ := fsys.ReadFile("/config/zai.json")
	if err := SetTransport(fsys, "/config", first.ID, "other"); err == nil {
		t.Fatal("accepted unknown transport")
	}
	after, _ := fsys.ReadFile("/config/zai.json")
	if string(before) != string(after) {
		t.Fatal("invalid transport changed credentials")
	}
	if err := SetTransport(fsys, "/config", first.ID, "responses"); err != nil {
		t.Fatal(err)
	}
	accounts, err = ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || accounts[0].Transport != "" {
		t.Fatal("failed to restore native transport")
	}
}

func TestReadAccountsRejectedUnknownTransport(t *testing.T) {
	fsys := fsutil.NewMemFS()
	_ = fsys.WriteFile("/config/zai.json", []byte(`{"accounts":[{"api_key":"private-key","transport":"other"}]}`), 0o600)
	_, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err == nil || strings.Contains(err.Error(), "private-key") {
		t.Fatal("invalid transport was not safely rejected")
	}
}
