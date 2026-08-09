package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func codexBarAuthPath(root string) string {
	return filepath.Join(root, "managed-codex-homes", "synthetic-record", "auth.json")
}

func rewriteCodexBarManifest(t *testing.T, root string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(root, "managed-codex-accounts.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(manifest)
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rewriteCodexBarRecord(t *testing.T, root string, mutate func(map[string]any)) {
	t.Helper()
	rewriteCodexBarManifest(t, root, func(manifest map[string]any) {
		accounts, ok := manifest["accounts"].([]any)
		if !ok || len(accounts) != 1 {
			t.Fatalf("manifest accounts = %#v", manifest["accounts"])
		}
		record, ok := accounts[0].(map[string]any)
		if !ok {
			t.Fatalf("manifest record = %#v", accounts[0])
		}
		mutate(record)
	})
}

func writeCodexBarAuthAndFingerprint(t *testing.T, root string, data []byte) {
	t.Helper()
	if err := os.WriteFile(codexBarAuthPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(data)
	rewriteCodexBarRecord(t, root, func(record map[string]any) {
		record["authFingerprint"] = hex.EncodeToString(fingerprint[:])
	})
}

func externalInventoryCandidate(t *testing.T, source ExternalCredentialSource) CredentialCandidate {
	t.Helper()
	inventory := DiscoverInventoryWithSources(context.Background(), newFakeFS(), source)
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 1 {
		t.Fatalf("inventory = %+v, want one external candidate", inventory)
	}
	return inventory.Accounts[0].Candidates[0]
}

type replacingCodexBarReadFileSystem struct {
	osCodexBarReadFileSystem
	target      string
	replacement []byte
	replaced    bool
}

type symlinkingDirectoryReadFileSystem struct {
	osCodexBarReadFileSystem
	directory   string
	triggerPath string
	changed     bool
}

type abaSymlinkReadFileSystem struct {
	osCodexBarReadFileSystem
	target string
}

func (f *symlinkingDirectoryReadFileSystem) OpenNoFollow(root, path string) (*os.File, error) {
	if path == f.triggerPath && !f.changed {
		f.changed = true
		target := f.directory + ".prior"
		if err := os.Rename(f.directory, target); err != nil {
			return nil, err
		}
		if err := os.Symlink(target, f.directory); err != nil {
			return nil, err
		}
	}
	return f.osCodexBarReadFileSystem.OpenNoFollow(root, path)
}

func (f *abaSymlinkReadFileSystem) OpenNoFollow(root, path string) (*os.File, error) {
	if path != f.target {
		return f.osCodexBarReadFileSystem.OpenNoFollow(root, path)
	}
	realPath := path + ".real"
	if err := os.Rename(path, realPath); err != nil {
		return nil, err
	}
	if err := os.Symlink(filepath.Base(realPath), path); err != nil {
		os.Rename(realPath, path)
		return nil, err
	}
	file, openErr := f.osCodexBarReadFileSystem.OpenNoFollow(root, path)
	removeErr := os.Remove(path)
	renameErr := os.Rename(realPath, path)
	if openErr != nil {
		return nil, openErr
	}
	if removeErr != nil || renameErr != nil {
		file.Close()
		if removeErr != nil {
			return nil, removeErr
		}
		return nil, renameErr
	}
	return file, nil
}

func (f *replacingCodexBarReadFileSystem) replace(path string) error {
	if path != f.target || f.replaced {
		return nil
	}
	f.replaced = true
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := os.Rename(path, path+".prior"); err != nil {
		return err
	}
	return os.WriteFile(path, f.replacement, info.Mode().Perm())
}

func (f *replacingCodexBarReadFileSystem) OpenNoFollow(root, path string) (*os.File, error) {
	if err := f.replace(path); err != nil {
		return nil, err
	}
	return f.osCodexBarReadFileSystem.OpenNoFollow(root, path)
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

func TestExternalCandidateIDSurvivesCredentialRotation(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, nil)
	source := NewCodexBarSource(root)
	before := externalInventoryCandidate(t, source)

	rotated := inventoryAuth(
		"rotated-access",
		"acct-1",
		fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"),
		time.Now().Add(2*time.Hour).UnixMilli(),
	)
	writeCodexBarAuthAndFingerprint(t, root, rotated)
	after := externalInventoryCandidate(t, source)

	if before.Ref.CandidateID != after.Ref.CandidateID {
		t.Fatalf("candidate ID changed across rotation: %q != %q", before.Ref.CandidateID, after.Ref.CandidateID)
	}
	if before.Revision == after.Revision {
		t.Fatalf("revision did not change across rotation: %q", before.Revision)
	}
	for _, raw := range []string{"synthetic-record", string(after.Revision), root} {
		if strings.Contains(string(after.Ref.CandidateID), raw) {
			t.Fatalf("candidate ID exposed source material: %q", after.Ref.CandidateID)
		}
	}
}

func TestCodexBarRevisionChangesWhenDeclaredPathMovesWithSameBytes(t *testing.T) {
	root := t.TempDir()
	authData := writeCodexBarFixture(t, root, 0o600, nil)
	source := NewCodexBarSource(root)
	before := externalInventoryCandidate(t, source)

	movedHome := filepath.Join(root, "managed-codex-homes", "moved-record")
	if err := os.MkdirAll(movedHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movedHome, "auth.json"), authData, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteCodexBarRecord(t, root, func(record map[string]any) {
		record["managedHomePath"] = movedHome
	})
	after := externalInventoryCandidate(t, source)

	if before.Revision == after.Revision {
		t.Fatalf("revision did not change when declared path moved: %q", before.Revision)
	}
	if before.Ref.CandidateID != after.Ref.CandidateID {
		t.Fatalf("candidate ID changed when declared path moved: %q != %q", before.Ref.CandidateID, after.Ref.CandidateID)
	}
}

func TestCodexBarRevisionChangesWhenAuthFileIsReplacedWithSameBytes(t *testing.T) {
	root := t.TempDir()
	authData := writeCodexBarFixture(t, root, 0o600, nil)
	source := NewCodexBarSource(root)
	before, err := source.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	authPath := codexBarAuthPath(root)
	if err := os.Rename(authPath, authPath+".prior"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, authData, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := source.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if before[0].Ref.Revision == after[0].Ref.Revision {
		t.Fatalf("revision did not change when auth file generation changed: %q", before[0].Ref.Revision)
	}
}

func TestCodexBarSourceRejectsValidationReadRaces(t *testing.T) {
	for _, target := range []string{"manifest", "auth"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			authData := writeCodexBarFixture(t, root, 0o600, nil)
			path := filepath.Join(root, "managed-codex-accounts.json")
			replacement, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if target == "auth" {
				path = codexBarAuthPath(root)
				replacement = authData
			}
			source := NewCodexBarSource(root)
			source.fs = &replacingCodexBarReadFileSystem{target: path, replacement: replacement}

			_, err = source.List(context.Background())
			if !errors.Is(err, ErrExternalUnsafePath) {
				t.Fatalf("List error = %v, want ErrExternalUnsafePath", err)
			}
		})
	}
}

func TestCodexBarSourceRejectsManagedHomeSymlinkRace(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, nil)
	authPath := codexBarAuthPath(root)
	source := NewCodexBarSource(root)
	source.fs = &symlinkingDirectoryReadFileSystem{
		directory: filepath.Dir(authPath), triggerPath: authPath,
	}

	_, err := source.List(context.Background())
	if !errors.Is(err, ErrExternalUnsafePath) {
		t.Fatalf("List error = %v, want ErrExternalUnsafePath", err)
	}
}

func TestCodexBarSourceRejectsRootSymlinkRace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "CodexBar")
	writeCodexBarFixture(t, root, 0o600, nil)
	manifestPath := filepath.Join(root, "managed-codex-accounts.json")
	source := NewCodexBarSource(root)
	source.fs = &symlinkingDirectoryReadFileSystem{
		directory: root, triggerPath: manifestPath,
	}

	_, err := source.List(context.Background())
	if !errors.Is(err, ErrExternalUnsafePath) {
		t.Fatalf("List error = %v, want ErrExternalUnsafePath", err)
	}
}

func TestCodexBarSourceRejectsSymlinkABA(t *testing.T) {
	for _, target := range []string{"manifest", "auth"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			writeCodexBarFixture(t, root, 0o600, nil)
			path := filepath.Join(root, "managed-codex-accounts.json")
			if target == "auth" {
				path = codexBarAuthPath(root)
			}
			source := NewCodexBarSource(root)
			source.fs = &abaSymlinkReadFileSystem{target: path}

			_, err := source.List(context.Background())
			if !errors.Is(err, ErrExternalUnsafePath) {
				t.Fatalf("List error = %v, want ErrExternalUnsafePath", err)
			}
		})
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
	if !errors.Is(err, ErrExternalFingerprintMismatch) {
		t.Fatalf("List error = %v, want ErrExternalFingerprintMismatch", err)
	}
}

func TestCodexBarSourceRejectsProviderIdentityMismatch(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, func(record map[string]any) {
		record["providerAccountID"] = "different-account"
	})

	_, err := NewCodexBarSource(root).List(context.Background())
	if !errors.Is(err, ErrExternalIdentityMismatch) {
		t.Fatalf("List error = %v, want ErrExternalIdentityMismatch", err)
	}
}

func TestCodexBarSourceRejectsOuterAccountClaimMismatch(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, nil)
	authData := inventoryAuth(
		"synthetic-access",
		"acct-1",
		fakeCodexJWT("user@example.test", "acct-2", "user-1", "plus"),
		time.Now().Add(time.Hour).UnixMilli(),
	)
	writeCodexBarAuthAndFingerprint(t, root, authData)

	candidates, err := NewCodexBarSource(root).List(context.Background())
	if !errors.Is(err, ErrExternalIdentityMismatch) {
		t.Fatalf("List error = %v, want ErrExternalIdentityMismatch", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("List returned %d candidates for conflicting credential identities", len(candidates))
	}
}

func TestCodexBarSourceRejectsWorkspaceIdentityMismatch(t *testing.T) {
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, func(record map[string]any) {
		record["workspaceAccountID"] = "different-user"
	})

	_, err := NewCodexBarSource(root).List(context.Background())
	if !errors.Is(err, ErrExternalIdentityMismatch) {
		t.Fatalf("List error = %v, want ErrExternalIdentityMismatch", err)
	}
}

func TestExternalSourceErrorCodesRemainDistinct(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "unavailable", err: ErrExternalUnavailable, want: "unavailable"},
		{name: "invalid", err: ErrExternalInvalid, want: "invalid"},
		{name: "unsafe path", err: ErrExternalUnsafePath, want: "unsafe_path"},
		{name: "stale revision", err: ErrStaleRevision, want: "stale_revision"},
		{name: "identity mismatch", err: ErrExternalIdentityMismatch, want: "identity_mismatch"},
		{name: "fingerprint mismatch", err: ErrExternalFingerprintMismatch, want: "fingerprint_mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := externalSourceErrorCode(tt.err); got != tt.want {
				t.Fatalf("externalSourceErrorCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCodexBarSourceValidationMatrix(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string) *CodexBarSource
		want  error
	}{
		{
			name: "valid",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				return NewCodexBarSource(root)
			},
		},
		{
			name: "missing manifest",
			setup: func(_ *testing.T, root string) *CodexBarSource {
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnavailable,
		},
		{
			name: "duplicate record ID",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				rewriteCodexBarManifest(t, root, func(manifest map[string]any) {
					accounts := manifest["accounts"].([]any)
					manifest["accounts"] = append(accounts, accounts[0])
				})
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "empty record ID",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["id"] = "" })
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "empty managed home",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["managedHomePath"] = "" })
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "relative managed home",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["managedHomePath"] = "relative" })
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "escaped managed home",
			setup: func(t *testing.T, root string) *CodexBarSource {
				outside := t.TempDir()
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["managedHomePath"] = outside })
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "manifest symlink",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				path := filepath.Join(root, "managed-codex-accounts.json")
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "managed home symlink",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				home := filepath.Dir(codexBarAuthPath(root))
				target := home + ".target"
				if err := os.Rename(home, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, home); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "auth symlink",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				path := codexBarAuthPath(root)
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "unsafe manifest permissions",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				if err := os.Chmod(filepath.Join(root, "managed-codex-accounts.json"), 0o644); err != nil {
					t.Fatal(err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "unsafe auth permissions",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o644, nil)
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "untrusted ownership",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				source := NewCodexBarSource(root)
				source.ownsFile = func(info os.FileInfo) bool { return info.Name() != "auth.json" }
				return source
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "non regular auth",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				path := codexBarAuthPath(root)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalUnsafePath,
		},
		{
			name: "malformed manifest",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				if err := os.WriteFile(filepath.Join(root, "managed-codex-accounts.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "missing manifest accounts",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				if err := os.WriteFile(filepath.Join(root, "managed-codex-accounts.json"), []byte(`{"version":3}`), 0o600); err != nil {
					t.Fatal(err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "malformed auth",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				writeCodexBarAuthAndFingerprint(t, root, []byte("{"))
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "missing auth",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				if err := os.Remove(codexBarAuthPath(root)); err != nil {
					t.Fatal(err)
				}
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "missing access token",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				data := inventoryAuth("", "acct-1", fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"), 0)
				writeCodexBarAuthAndFingerprint(t, root, data)
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "missing ID token claims",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, nil)
				writeCodexBarAuthAndFingerprint(t, root, inventoryAuth("synthetic-access", "acct-1", "", 0))
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "missing provider identity",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["providerAccountID"] = "" })
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "missing workspace identity",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["workspaceAccountID"] = "" })
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "missing fingerprint",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["authFingerprint"] = "" })
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
		{
			name: "malformed fingerprint",
			setup: func(t *testing.T, root string) *CodexBarSource {
				writeCodexBarFixture(t, root, 0o600, func(record map[string]any) { record["authFingerprint"] = strings.Repeat("z", 64) })
				return NewCodexBarSource(root)
			},
			want: ErrExternalInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := tt.setup(t, t.TempDir())
			candidates, err := source.List(context.Background())
			if tt.want == nil {
				if err != nil || len(candidates) != 1 {
					t.Fatalf("List = %d candidates, %v; want one valid candidate", len(candidates), err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("List error = %v, want %v", err, tt.want)
			}
		})
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
