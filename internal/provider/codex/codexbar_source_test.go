package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCodexBarFixture(t *testing.T, root string, authMode os.FileMode, mutateRecord func(map[string]any)) []byte {
	t.Helper()
	managedHome := filepath.Join(root, "managed-codex-homes", "synthetic-record")
	if err := os.MkdirAll(managedHome, 0o755); err != nil {
		t.Fatal(err)
	}
	authData := inventoryAuth(
		"synthetic-access",
		"acct-1",
		fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"),
		time.Now().Add(time.Hour).UnixMilli(),
	)
	if err := os.WriteFile(filepath.Join(managedHome, "auth.json"), authData, authMode); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(authData)
	record := map[string]any{
		"id":                 "synthetic-record",
		"managedHomePath":    managedHome,
		"providerAccountID":  "acct-1",
		"workspaceAccountID": "user-1",
		"authFingerprint":    hex.EncodeToString(fingerprint[:]),
	}
	if mutateRecord != nil {
		mutateRecord(record)
	}
	manifest, err := json.Marshal(map[string]any{"version": 3, "accounts": []any{record}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "managed-codex-accounts.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return authData
}

func TestCodexBarSourceListsMetadataAndResolvesExactRevision(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, nil)
	source := NewCodexBarSource(root)

	candidates, err := source.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}
	candidate := candidates[0]
	if candidate.Identity.AccountID != "acct-1" || candidate.Identity.UserID != "user-1" || candidate.Ref.RecordID != "synthetic-record" || candidate.Ref.Revision == "" {
		t.Fatalf("candidate metadata = %+v", candidate)
	}

	material, err := source.Resolve(context.Background(), candidate.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if material.AccessToken != "synthetic-access" || material.AccountID != "acct-1" {
		t.Fatalf("resolved material identity = access:%t account:%q", material.AccessToken != "", material.AccountID)
	}

	writeCodexBarFixture(t, root, 0o600, nil)
	updated := inventoryAuth(
		"rotated-access",
		"acct-1",
		fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"),
		time.Now().Add(2*time.Hour).UnixMilli(),
	)
	if err := os.WriteFile(filepath.Join(root, "managed-codex-homes", "synthetic-record", "auth.json"), updated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Resolve(context.Background(), candidate.Ref); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Resolve after rotation error = %v, want ErrStaleRevision", err)
	}
}

func TestCodexBarSourceRejectsEscapedManagedHome(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, func(record map[string]any) {
		record["managedHomePath"] = outside
	})

	_, err := NewCodexBarSource(root).List(context.Background())
	if !errors.Is(err, ErrExternalUnsafePath) {
		t.Fatalf("List error = %v, want ErrExternalUnsafePath", err)
	}
}

func TestCodexBarSourceRejectsUnsafeAuthPermissions(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o644, nil)

	_, err := NewCodexBarSource(root).List(context.Background())
	if !errors.Is(err, ErrExternalUnsafePath) {
		t.Fatalf("List error = %v, want ErrExternalUnsafePath", err)
	}
}

func TestCodexBarSourceRejectsFingerprintMismatch(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, func(record map[string]any) {
		record["authFingerprint"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	})

	_, err := NewCodexBarSource(root).List(context.Background())
	if !errors.Is(err, ErrExternalInvalid) {
		t.Fatalf("List error = %v, want ErrExternalInvalid", err)
	}
}

func TestCodexBarSourceRejectsProviderIdentityMismatch(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, func(record map[string]any) {
		record["providerAccountID"] = "different-account"
	})

	_, err := NewCodexBarSource(root).List(context.Background())
	if !errors.Is(err, ErrExternalInvalid) {
		t.Fatalf("List error = %v, want ErrExternalInvalid", err)
	}
}

func TestCodexBarSourceLiveReadOnly(t *testing.T) {
	root := os.Getenv("CQ_CODEXBAR_LIVE_ROOT")
	if root == "" {
		t.Skip("CQ_CODEXBAR_LIVE_ROOT not set")
	}
	source := NewCodexBarSource(root)
	candidates, err := source.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		t.Fatal("live CodexBar source returned no candidates")
	}
	for _, candidate := range candidates {
		material, err := source.Resolve(context.Background(), candidate.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if material.AccessToken == "" || material.AccountID == "" {
			t.Fatal("live CodexBar candidate resolved incomplete material")
		}
	}
}
