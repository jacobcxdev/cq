package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
)

func TestFileSystemActivatorActivatesExactCandidateAndPreservesSystemUnknowns(t *testing.T) {
	fs := newFakeFS()
	oldJWT := fakeCodexJWT("old@test.com", "acct-old", "user-old", "plus")
	newJWT := fakeCodexJWT("new@test.com", "acct-new", "user-new", "pro")
	systemPath := "/fake/home/.codex/auth.json"
	candidatePath := "/fake/home/.codex/accounts/user-new::acct-new.auth.json"
	fs.files[systemPath] = []byte(`{"auth_mode":"chatgpt","unknown_system":{"keep":true},"_cq":{"remove":true},"tokens":{"access_token":"old","refresh_token":"old-ref","id_token":"` + oldJWT + `","account_id":"acct-old"}}`)
	fs.files[candidatePath] = []byte(`{"auth_mode":"chatgpt","candidate_unknown":false,"_cq":{"private":true},"tokens":{"access_token":"new","refresh_token":"new-ref","id_token":"` + newJWT + `","account_id":"acct-new"}}`)

	activator, err := NewFileSystemActivator(fs)
	if err != nil {
		t.Fatal(err)
	}
	ref := CandidateRef{AccountKey: "user-new::acct-new", CandidateID: "managed:user-new::acct-new", path: candidatePath}
	result, err := activator.Activate(context.Background(), ref, credentialRevision(fs.files[candidatePath]))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !result.SystemCommitted || result.ProjectionError != nil {
		t.Fatalf("result = %+v", result)
	}
	var system map[string]any
	if err := json.Unmarshal(fs.files[systemPath], &system); err != nil {
		t.Fatal(err)
	}
	if _, ok := system["unknown_system"]; !ok {
		t.Fatal("system unknown field was lost")
	}
	if _, ok := system["candidate_unknown"]; ok {
		t.Fatal("candidate unknown field leaked into system auth")
	}
	if _, ok := system["_cq"]; ok {
		t.Fatal("_cq metadata leaked into system auth")
	}
	tokens := system["tokens"].(map[string]any)
	if tokens["access_token"] != "new" {
		t.Fatalf("system access token = %#v, want new", tokens["access_token"])
	}
	if _, ok := fs.files["/fake/home/.codex/accounts/user-old::acct-old.auth.json"]; ok {
		t.Fatal("system activator created managed credential outside coordinator")
	}
}

type registryWriteFailFS struct{ *fakeFS }

func (f registryWriteFailFS) WriteFile(name string, data []byte, mode os.FileMode) error {
	if strings.HasSuffix(name, "registry.json.tmp") {
		return os.ErrPermission
	}
	return f.fakeFS.WriteFile(name, data, mode)
}

func TestFileSystemActivatorReportsProjectionFailureAfterSystemCommit(t *testing.T) {
	base := newFakeFS()
	fs := registryWriteFailFS{fakeFS: base}
	jwt := fakeCodexJWT("new@test.com", "acct-new", "user-new", "pro")
	path := "/fake/home/.codex/accounts/user-new::acct-new.auth.json"
	base.files[path] = codexAuthJSON("new", "acct-new", jwt)
	activator, err := NewFileSystemActivator(fs)
	if err != nil {
		t.Fatal(err)
	}
	ref := CandidateRef{AccountKey: "user-new::acct-new", CandidateID: "managed:user-new::acct-new", path: path}
	result, err := activator.Activate(context.Background(), ref, credentialRevision(base.files[path]))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !result.SystemCommitted || result.ProjectionError == nil {
		t.Fatalf("result = %+v, want committed system plus projection error", result)
	}
	if _, ok := base.files["/fake/home/.codex/auth.json"]; !ok {
		t.Fatal("system auth was not committed")
	}
}

func TestSaveLoginPreservesManagedAndRegistryUnknownFieldsWithoutActivation(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	managedPath := "/fake/home/.codex/accounts/user-1::acct-1.auth.json"
	registryPath := "/fake/home/.codex/accounts/registry.json"
	fs.files[managedPath] = []byte(`{"auth_mode":"chatgpt","managed_unknown":{"keep":true},"tokens":{"access_token":"old","custom_token":"keep"}}`)
	fs.files[registryPath] = []byte(`{"schema_version":3,"active_account_key":"other::active","accounts":[{"account_key":"user-1::acct-1","alias":"work","registry_unknown":true}]}`)
	systemBefore := []byte(`{"system":"untouched"}`)
	fs.files["/fake/home/.codex/auth.json"] = systemBefore

	_, _, err := SaveLogin(fs, "/fake/home", LoginCredential{
		Tokens:    auth.CodexTokenResponse{IDToken: jwt, AccessToken: "new", RefreshToken: "new-ref"},
		Claims:    auth.CodexClaims{Email: "user@test.com", AccountID: "acct-1", UserID: "user-1", PlanType: "plus"},
		CreatedAt: time.Unix(100, 0),
	})
	if err != nil {
		t.Fatalf("SaveLogin: %v", err)
	}
	if got := string(fs.files["/fake/home/.codex/auth.json"]); got != string(systemBefore) {
		t.Fatalf("system auth changed: %s", got)
	}
	var managed map[string]any
	_ = json.Unmarshal(fs.files[managedPath], &managed)
	if _, ok := managed["managed_unknown"]; !ok {
		t.Fatal("managed unknown field was lost")
	}
	if managed["tokens"].(map[string]any)["custom_token"] != "keep" {
		t.Fatal("unknown token field was lost")
	}
	var registry map[string]any
	_ = json.Unmarshal(fs.files[registryPath], &registry)
	if registry["active_account_key"] != "other::active" {
		t.Fatalf("active projection changed: %#v", registry["active_account_key"])
	}
	record := registry["accounts"].([]any)[0].(map[string]any)
	if record["alias"] != "work" || record["registry_unknown"] != true {
		t.Fatalf("registry unknown fields changed: %#v", record)
	}
}

func TestFileSystemActivatorRejectsStaleRevision(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	path := "/fake/home/.codex/accounts/user-1::acct-1.auth.json"
	fs.files[path] = codexAuthJSON("new", "acct-1", jwt)
	activator, _ := NewFileSystemActivator(fs)
	ref := CandidateRef{AccountKey: "user-1::acct-1", CandidateID: "managed:user-1::acct-1", path: path}
	if _, err := activator.Activate(context.Background(), ref, Revision("stale")); err == nil {
		t.Fatal("Activate error = nil, want stale revision rejection")
	}
}

func TestFileSystemActivatorDeactivateRequiresExactActiveRevision(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("current", "acct-1", jwt)
	activator, _ := NewFileSystemActivator(fs)
	active, err := activator.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activator.Deactivate(context.Background(), active.AccountKey, Revision("stale")); err == nil {
		t.Fatal("Deactivate error = nil, want stale revision rejection")
	}
	result, err := activator.Deactivate(context.Background(), active.AccountKey, active.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SystemRemoved || !errors.Is(baseReadError(fs, "/fake/home/.codex/auth.json"), os.ErrNotExist) {
		t.Fatalf("result = %+v, system auth still present", result)
	}
}

func baseReadError(fs *fakeFS, path string) error {
	_, err := fs.ReadFile(path)
	return err
}
