package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestCodexCanaryProtectionsCoverSixNamedSourcesWhenOptionalSourceAbsent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic-home")
	configDirectory := filepath.Join(t.TempDir(), "synthetic-config")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateCanaryFixture(t, filepath.Join(home, ".codex", "auth.json"), `{"fixture":"system"}`)
	writePrivateCanaryFixture(t, filepath.Join(home, ".codex", "accounts", "registry.json"), `{"fixture":"registry"}`)

	protected, err := codexCanaryProtections(home, configDirectory)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := map[proxy.CodexCanaryProtectionKind]bool{
		proxy.CodexCanarySystemAuth:       true,
		proxy.CodexCanaryRegistry:         true,
		proxy.CodexCanaryCQManagedAuth:    true,
		proxy.CodexCanaryCodexBarManifest: true,
		proxy.CodexCanaryCodexBarAuth:     true,
		proxy.CodexCanaryRoutingDefault:   true,
	}
	if len(protected) != len(wantKinds) {
		t.Fatalf("protected source count = %d, want %d", len(protected), len(wantKinds))
	}
	for _, protection := range protected {
		if !wantKinds[protection.Kind] {
			t.Fatalf("unexpected protected source kind %q", protection.Kind)
		}
		delete(wantKinds, protection.Kind)
	}
	if len(wantKinds) != 0 {
		t.Fatalf("missing protected source kinds = %v", wantKinds)
	}

	statePath := filepath.Join(configDirectory, "canary-state.json")
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := proxy.StartCodexCanary(fsutil.OSFileSystem{}, statePath, protected, protectionTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	before := recorder.State().ProtectedDigests
	if err := recorder.RecordAdmitted(now); err != nil {
		t.Fatal(err)
	}
	after := recorder.State()
	if after.AutomaticHashChanges != 0 || len(after.ProtectedDigests) != len(before) {
		t.Fatalf("absence evidence changed: before=%+v after=%+v", before, after)
	}
	for index := range before {
		if before[index] != after.ProtectedDigests[index] {
			t.Fatalf("absence digest %d changed: before=%+v after=%+v", index, before[index], after.ProtectedDigests[index])
		}
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, rawPath := range []string{home, configDirectory} {
		if strings.Contains(string(persisted), rawPath) {
			t.Fatalf("persisted canary contains raw path")
		}
	}
}

func TestCodexCanaryProtectionsTrackOnlyDeclaredOwnedState(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic-home")
	configDirectory := filepath.Join(t.TempDir(), "synthetic-config")
	managedDirectory := filepath.Join(home, ".codex", "accounts")
	codexBarRoot := codexprov.DefaultCodexBarRoot(home)
	declaredHome := filepath.Join(codexBarRoot, "managed-codex-homes", "synthetic-record")
	undeclaredHome := filepath.Join(codexBarRoot, "managed-codex-homes", "synthetic-decoy")
	for _, directory := range []string{managedDirectory, configDirectory, declaredHome, undeclaredHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{codexBarRoot, filepath.Join(codexBarRoot, "managed-codex-homes")} {
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	systemPath := filepath.Join(home, ".codex", "auth.json")
	registryPath := filepath.Join(managedDirectory, "registry.json")
	managedPath := filepath.Join(managedDirectory, "synthetic-record.auth.json")
	managedDecoyPath := filepath.Join(managedDirectory, "synthetic-notes.json")
	managedNestedPath := filepath.Join(managedDirectory, "nested", "synthetic-nested.auth.json")
	declaredAuthPath := filepath.Join(declaredHome, "auth.json")
	undeclaredAuthPath := filepath.Join(undeclaredHome, "auth.json")
	configPath := filepath.Join(configDirectory, "proxy.json")
	declaredAuth := syntheticCodexBarAuthFixture(t, "synthetic-access-before")
	for path, contents := range map[string]string{
		systemPath:         `{"fixture":"system-before"}`,
		registryPath:       `{"fixture":"registry-before"}`,
		managedPath:        `{"fixture":"managed-before"}`,
		managedDecoyPath:   `{"fixture":"managed-decoy-before"}`,
		managedNestedPath:  `{"fixture":"managed-nested-before"}`,
		declaredAuthPath:   string(declaredAuth),
		undeclaredAuthPath: `{"fixture":"undeclared-before"}`,
		configPath:         `{"codex_routing_default_account_key":"synthetic-default-before","codex_routing_account_keys":["account-a","account-b"],"unrelated":"before"}`,
	} {
		writePrivateCanaryFixture(t, path, contents)
	}
	manifestPath := filepath.Join(codexBarRoot, "managed-codex-accounts.json")
	manifest := syntheticCodexBarManifestFixture(t, declaredHome, declaredAuth)
	writePrivateCanaryFixture(t, manifestPath, string(manifest))

	protected, err := codexCanaryProtections(home, configDirectory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := proxy.StartCodexCanary(
		fsutil.OSFileSystem{}, filepath.Join(configDirectory, "canary-state.json"),
		protected, protectionTestTuple(), now,
	)
	if err != nil {
		t.Fatal(err)
	}

	writePrivateCanaryFixture(t, managedDecoyPath, `{"fixture":"managed-decoy-after"}`)
	writePrivateCanaryFixture(t, managedNestedPath, `{"fixture":"managed-nested-after"}`)
	writePrivateCanaryFixture(t, undeclaredAuthPath, `{"fixture":"undeclared-after"}`)
	writePrivateCanaryFixture(t, configPath, `{"codex_routing_account_keys" : [ "account-a", "account-b" ], "codex_routing_default_account_key":"synthetic-default-before","unrelated":"after"}`)
	if err := recorder.RecordAdmitted(now); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().AutomaticHashChanges; got != 0 {
		t.Fatalf("unowned or unrelated state changed protected evidence: count = %d", got)
	}
	writePrivateCanaryFixture(t, configPath, `{"codex_routing_account_keys":["account-b","account-a"],"codex_routing_default_account_key":"synthetic-default-before","unrelated":"after"}`)
	if err := recorder.RecordAdmitted(now); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().AutomaticHashChanges; got != 0 {
		t.Fatalf("allowlist reordering changed protected evidence: count = %d", got)
	}

	mutations := []struct {
		kind           proxy.CodexCanaryProtectionKind
		additionalKind proxy.CodexCanaryProtectionKind
		path           string
		data           string
	}{
		{kind: proxy.CodexCanarySystemAuth, path: systemPath, data: `{"fixture":"system-after"}`},
		{kind: proxy.CodexCanaryRegistry, path: registryPath, data: `{"fixture":"registry-after"}`},
		{kind: proxy.CodexCanaryCQManagedAuth, path: managedPath, data: `{"fixture":"managed-after"}`},
		{kind: proxy.CodexCanaryCodexBarManifest, path: manifestPath, data: string(manifest) + "\n"},
		{kind: proxy.CodexCanaryCodexBarAuth, additionalKind: proxy.CodexCanaryCodexBarManifest, path: declaredAuthPath, data: string(syntheticCodexBarAuthFixture(t, "synthetic-access-after"))},
		{kind: proxy.CodexCanaryRoutingDefault, path: configPath, data: `{"codex_routing_default_account_key":"synthetic-default-after","codex_routing_account_keys":["account-a","account-b"],"unrelated":"after"}`},
	}
	for index, mutation := range mutations {
		before := canaryDigestByKind(recorder.State())
		writePrivateCanaryFixture(t, mutation.path, mutation.data)
		if mutation.kind == proxy.CodexCanaryCodexBarAuth {
			writePrivateCanaryFixture(t, manifestPath, string(syntheticCodexBarManifestFixture(t, declaredHome, []byte(mutation.data))))
		}
		if err := recorder.RecordAdmitted(now); err != nil {
			t.Fatal(err)
		}
		after := recorder.State()
		if got := after.AutomaticHashChanges; got != uint64(index+1) {
			t.Fatalf("mutation %q change count = %d, want %d", mutation.kind, got, index+1)
		}
		afterDigests := canaryDigestByKind(after)
		for kind, beforeDigest := range before {
			changed := beforeDigest != afterDigests[kind]
			wantChanged := kind == mutation.kind || kind == mutation.additionalKind
			if changed != wantChanged {
				t.Fatalf("mutation %q changed digest %q = %t", mutation.kind, kind, changed)
			}
		}
	}

	before := recorder.State().AutomaticHashChanges
	writePrivateCanaryFixture(t, configPath, `{"codex_routing_default_account_key":"synthetic-default-after","codex_routing_account_keys":["account-a"],"unrelated":"after"}`)
	if err := recorder.RecordAdmitted(now); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().AutomaticHashChanges; got != before+1 {
		t.Fatalf("routing allowlist mutation change count = %d, want %d", got, before+1)
	}
}

func TestCodexCanaryProtectionSnapshotRejectsUnsafeCodexBarManifest(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic-home")
	codexBarRoot := codexprov.DefaultCodexBarRoot(home)
	if err := os.MkdirAll(codexBarRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(codexBarRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "synthetic-outside-manifest.json")
	writePrivateCanaryFixture(t, target, `{"version":3,"accounts":[]}`)
	if err := os.Symlink(target, filepath.Join(codexBarRoot, "managed-codex-accounts.json")); err != nil {
		t.Fatal(err)
	}

	_, err := codexBarCanaryProtectionSnapshot(codexprov.NewCodexBarSource(codexBarRoot))
	if !errors.Is(err, codexprov.ErrExternalUnsafePath) {
		t.Fatalf("error = %v, want ErrExternalUnsafePath", err)
	}
	if strings.Contains(err.Error(), home) || strings.Contains(err.Error(), target) {
		t.Fatalf("unsafe manifest error contains raw path")
	}
}

func TestCodexCanaryProtectionSourceErrorDropsUnderlyingPath(t *testing.T) {
	privatePath := "/synthetic/private/source/manifest.json"
	err := codexCanaryProtectionSourceError(&os.PathError{Op: "open", Path: privatePath, Err: os.ErrPermission})
	if !errors.Is(err, codexprov.ErrExternalInvalid) {
		t.Fatalf("error = %v, want ErrExternalInvalid", err)
	}
	if strings.Contains(err.Error(), privatePath) {
		t.Fatalf("sanitised error contains source path: %v", err)
	}
}

func protectionTestTuple() proxy.CodexCanaryTuple {
	return proxy.CodexCanaryTuple{
		CQBuild: "synthetic-cq-build", ClientBuild: "synthetic-client-build",
		ParserSchema: 1, LeaseSchema: 1, SemanticsRevision: "synthetic-semantics",
		RetryBudget: 0, FixtureHash: strings.Repeat("a", 64), ReadinessFingerprint: strings.Repeat("b", 64),
	}
}

func writePrivateCanaryFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func syntheticCodexBarAuthFixture(t *testing.T, accessToken string) []byte {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	claims, err := json.Marshal(map[string]any{
		"email": "synthetic@example.test",
		"exp":   1774076490,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "synthetic-account",
			"chatgpt_user_id":    "synthetic-user",
			"chatgpt_plan_type":  "plus",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	idToken := header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic-signature"
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": accessToken, "refresh_token": "synthetic-refresh",
			"id_token": idToken, "account_id": "synthetic-account",
		},
		"cq_expires_at": int64(1774076490000),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func syntheticCodexBarManifestFixture(t *testing.T, managedHome string, auth []byte) []byte {
	t.Helper()
	fingerprint := sha256.Sum256(auth)
	data, err := json.Marshal(map[string]any{
		"version": 3,
		"accounts": []any{map[string]any{
			"id": "synthetic-record", "managedHomePath": managedHome,
			"providerAccountID": "synthetic-account", "workspaceAccountID": "synthetic-user",
			"authFingerprint": hex.EncodeToString(fingerprint[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func canaryDigestByKind(state proxy.CodexCanaryState) map[proxy.CodexCanaryProtectionKind]string {
	result := make(map[proxy.CodexCanaryProtectionKind]string, len(state.ProtectedDigests))
	for _, protected := range state.ProtectedDigests {
		result[protected.Kind] = protected.Digest
	}
	return result
}
