package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func testCoordinator(t *testing.T) (*CredentialCoordinator, *durableFakeFS) {
	t.Helper()
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	coordinator, err := NewCredentialCoordinator(store, testCQStateDir())
	if err != nil {
		t.Fatal(err)
	}
	coordinator.RefreshMutations = &recoveringRefreshRecorder{}
	coordinator.CredentialOwner = &idempotentOwnerRecorder{}
	return coordinator, fs
}

func testCQStateDir() string { return "/cq/state" }

func TestNewCredentialCoordinatorDoesNotCreateCredentialOwnerAuthority(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	store, err := NewManagedStore(filesystem)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCredentialCoordinator(store, testCQStateDir())
	if err != nil {
		t.Fatal(err)
	}
	if coordinator.RefreshMutations != nil || coordinator.CredentialOwner != nil {
		t.Fatal("ordinary constructor acquired credential-owner authority")
	}
	if _, err := filesystem.Stat(filepath.Join(store.Home, ".cq", "credential-authority", "authority.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary constructor created credential-owner key: %v", err)
	}
}

func TestNewCredentialCoordinatorRequiresStrictStateDirectory(t *testing.T) {
	tests := map[string]string{
		"empty":    "",
		"relative": "cq/state",
		"unclean":  "/cq/state/../state",
	}
	for name, stateDir := range tests {
		t.Run(name, func(t *testing.T) {
			filesystem := fsutil.NewMemFS()
			store, err := NewManagedStore(filesystem)
			if err != nil {
				t.Fatal(err)
			}
			_, err = NewCredentialCoordinator(store, stateDir)
			if err == nil {
				t.Fatalf("NewCredentialCoordinator accepted %q", stateDir)
			}
			if _, statErr := filesystem.Stat(filepath.Join(store.Home, ".codex", "auth.json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid state root mutated provider auth: %v", statErr)
			}
		})
	}
}

func TestNewCredentialCoordinatorKeepsCQStateSeparateFromProviderHome(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	store, err := NewManagedStore(filesystem)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := "/cq/state"
	coordinator, err := NewCredentialCoordinator(store, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator.StateDir != stateDir || coordinator.Journal.StateDir != stateDir {
		t.Fatalf("state roots = coordinator %q, journal %q", coordinator.StateDir, coordinator.Journal.StateDir)
	}
	if _, err := filesystem.Stat(stateDir); err != nil {
		t.Fatalf("state directory: %v", err)
	}
	legacyState := filepath.Join(store.Home, ".config", "cq", "state")
	if _, err := filesystem.Stat(legacyState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy provider-derived state exists: %v", err)
	}
}

func TestNewCredentialCoordinatorWithAuthorityRejectsCandidate(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	store, err := NewManagedStore(filesystem)
	if err != nil {
		t.Fatal(err)
	}
	mutationBackend := newMemoryCredentialAuthorityBackend()
	ownerBackend := newMemoryCredentialAuthorityBackendSharing(mutationBackend)
	_, err = NewCredentialCoordinatorWithAuthority(store, testCQStateDir(), CredentialAuthorityCapabilities{
		Role: CredentialAuthorityCandidate, LifecycleBound: true, RootKey: make([]byte, 32),
		RefreshMutationBackend: mutationBackend, CredentialOwnerBackend: ownerBackend,
	})
	if err == nil {
		t.Fatal("candidate acquired credential-owner authority")
	}
	occupancy, occupancyErr := mutationBackend.CredentialAuthorityOccupancy(context.Background())
	if occupancyErr != nil {
		t.Fatal(occupancyErr)
	}
	if occupancy.Files != 0 {
		t.Fatalf("candidate created %d credential-owner objects", occupancy.Files)
	}
}

func TestNewCredentialCoordinatorWithAuthorityDerivesSeparatePurposeKeys(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	store, err := NewManagedStore(filesystem)
	if err != nil {
		t.Fatal(err)
	}
	mutationBackend := newMemoryCredentialAuthorityBackend()
	ownerBackend := newMemoryCredentialAuthorityBackendSharing(mutationBackend)
	coordinator, err := NewCredentialCoordinatorWithAuthority(store, testCQStateDir(), CredentialAuthorityCapabilities{
		Role: CredentialAuthorityPrimary, LifecycleBound: true, RootKey: bytes.Repeat([]byte{0x61}, 32),
		RefreshMutationBackend: mutationBackend, CredentialOwnerBackend: ownerBackend,
	})
	if err != nil {
		t.Fatal(err)
	}
	mutation := coordinator.RefreshMutations.(*RefreshMutationStore)
	owner := coordinator.CredentialOwner.(*CredentialOwnerStore)
	if mutation.chain.key == owner.chain.key {
		t.Fatal("refresh mutation and credential owner shared one raw key")
	}
	closeAuthorityCoordinator(t, coordinator)
}

type cancellingInventorySource struct {
	cancel context.CancelFunc
}

func (s cancellingInventorySource) Name() string { return "external-cancelling" }

func (s cancellingInventorySource) List(ctx context.Context) ([]ExternalCandidate, error) {
	s.cancel()
	return nil, ctx.Err()
}

func (cancellingInventorySource) Resolve(context.Context, ExternalCandidateRef) (CredentialMaterial, error) {
	return CredentialMaterial{}, errors.New("unexpected external resolution")
}

func TestCredentialCoordinatorSaveLoginCreatesOwnedManagedRecord(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	ref, revision, err := coordinator.SaveLogin(context.Background(), testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if record.Metadata.Revision != revision || record.Metadata.Provenance != ProvenanceCQOAuth || record.Metadata.RefreshOwnership != RefreshCQOwnedNeverExported {
		t.Fatalf("record = %+v", record.Metadata)
	}
}

func TestCredentialCoordinatorListNeverReturnsCredentialMaterial(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	ref, _, err := coordinator.SaveLogin(context.Background(), testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 1 {
		t.Fatalf("inventory = %+v", inventory)
	}
	credential := inventory.Accounts[0].Candidates[0].Credential
	if credential.AccessToken != "" || credential.RefreshToken != "" || credential.IDToken != "" || credential.AccountID != "" {
		t.Fatalf("inventory exposed credential material: %+v", credential)
	}
}

func TestCredentialCoordinatorPreservesCancellationDuringInventoryDiscovery(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *CredentialCoordinator) error
	}{
		{
			name: "list",
			call: func(ctx context.Context, coordinator *CredentialCoordinator) error {
				_, err := coordinator.List(ctx)
				return err
			},
		},
		{
			name: "exact resolution",
			call: func(ctx context.Context, coordinator *CredentialCoordinator) error {
				_, err := coordinator.ResolveExact(ctx, PlannedCandidate{
					Ref:      CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"},
					Revision: "revision-1", Source: SourceExternal,
					Identity: AccountIdentity{AccountID: "account-1", UserID: "user-1"},
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _ := testCoordinator(t)
			ctx, cancel := context.WithCancel(context.Background())
			coordinator.ExternalSources = []ExternalCredentialSource{cancellingInventorySource{cancel: cancel}}

			if err := test.call(ctx, coordinator); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestCredentialCoordinatorListFailsClosedOnCoreInventoryErrors(t *testing.T) {
	const sensitive = "private credential path and token-secret"
	tests := []struct {
		name  string
		setup func(*testing.T, *CredentialCoordinator, *durableFakeFS)
	}{
		{
			name: "home unavailable",
			setup: func(_ *testing.T, _ *CredentialCoordinator, fs *durableFakeFS) {
				fs.homeDirErr = errors.New(sensitive)
			},
		},
		{
			name: "managed directory unreadable",
			setup: func(_ *testing.T, _ *CredentialCoordinator, fs *durableFakeFS) {
				path := "/fake/home/.codex/accounts/readable.auth.json"
				fs.files[path] = codexAuthJSON("readable-secret", "acct-1", fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"))
				fs.modes[path] = 0o600
				fs.readDirErr = map[string]error{"/fake/home/.codex/accounts": errors.New(sensitive)}
			},
		},
		{
			name: "managed credential malformed",
			setup: func(_ *testing.T, _ *CredentialCoordinator, fs *durableFakeFS) {
				path := "/fake/home/.codex/accounts/malformed.auth.json"
				fs.files[path] = []byte(`{"tokens":`)
				fs.modes[path] = 0o600
				fs.dirEntries = map[string][]fakeDirEntry{"/fake/home/.codex/accounts": {{name: "malformed.auth.json"}}}
			},
		},
		{
			name: "managed credential unsafe",
			setup: func(t *testing.T, coordinator *CredentialCoordinator, fs *durableFakeFS) {
				ref, _, err := coordinator.SaveLogin(context.Background(), testLoginCredential())
				if err != nil {
					t.Fatal(err)
				}
				path := coordinator.candidatePath(ref.CandidateID)
				fs.modes[path] = 0o644
				fs.dirEntries = map[string][]fakeDirEntry{"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}}}
			},
		},
		{
			name: "system credential malformed",
			setup: func(_ *testing.T, _ *CredentialCoordinator, fs *durableFakeFS) {
				fs.files["/fake/home/.codex/auth.json"] = []byte(`{"tokens":`)
				fs.modes["/fake/home/.codex/auth.json"] = 0o600
			},
		},
		{
			name: "system credential unsafe",
			setup: func(_ *testing.T, _ *CredentialCoordinator, fs *durableFakeFS) {
				path := "/fake/home/.codex/auth.json"
				fs.files[path] = codexAuthJSON("system-secret", "acct-1", fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"))
				fs.modes[path] = 0o644
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fs := testCoordinator(t)
			test.setup(t, coordinator, fs)

			inventory, err := coordinator.List(context.Background())
			if !errors.Is(err, ErrCredentialAuthorityUnavailable) {
				t.Fatalf("List inventory/error = %+v/%v, want typed authority unavailable", inventory, err)
			}
			if err != nil && strings.Contains(err.Error(), sensitive) {
				t.Fatalf("List exposed raw authority failure: %v", err)
			}
		})
	}
}

func TestCredentialCoordinatorListRejectsUnsafeCoreCredentialPaths(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "system credential symlink",
			setup: func(t *testing.T, home string) {
				authDir := filepath.Join(home, ".codex")
				if err := os.MkdirAll(authDir, 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "auth.json")
				if err := os.WriteFile(target, codexAuthJSON("system-secret", "acct-1", fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus")), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(authDir, "auth.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty core directory symlink",
			setup: func(t *testing.T, home string) {
				target := filepath.Join(t.TempDir(), "codex")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(home, ".codex")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "managed directory permissive",
			setup: func(t *testing.T, home string) {
				accountsDir := filepath.Join(home, ".codex", "accounts")
				if err := os.MkdirAll(accountsDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(accountsDir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "managed directory symlink",
			setup: func(t *testing.T, home string) {
				authDir := filepath.Join(home, ".codex")
				if err := os.MkdirAll(authDir, 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "accounts")
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(authDir, "accounts")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "system directory symlink",
			setup: func(t *testing.T, home string) {
				target := filepath.Join(t.TempDir(), "codex")
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "auth.json"), codexAuthJSON("system-secret", "acct-1", fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus")), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(home, ".codex")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			test.setup(t, home)
			store, err := NewManagedStore(fixedHomeDurableFS{home: home})
			if err != nil {
				t.Fatal(err)
			}
			coordinator, err := NewCredentialCoordinator(store, filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			coordinator.ExternalSources = nil

			inventory, err := coordinator.List(context.Background())
			if !errors.Is(err, ErrCredentialAuthorityUnavailable) {
				t.Fatalf("List inventory/error = %+v/%v, want typed authority unavailable", inventory, err)
			}
		})
	}
}

func TestCredentialCoordinatorListAcceptsStandardCodexCoreDirectoryPermissions(t *testing.T) {
	home := t.TempDir()
	coreDir := filepath.Join(home, ".codex")
	if err := os.Mkdir(coreDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coreDir, "auth.json"), codexAuthJSON("system-secret", "acct-1", fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus")), 0o600); err != nil {
		t.Fatal(err)
	}
	accountsDir := filepath.Join(coreDir, "accounts")
	if err := os.Mkdir(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := NewManagedStore(fixedHomeDurableFS{home: home})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCredentialCoordinator(store, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ExternalSources = nil

	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want standard Codex directory accepted", err)
	}
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 1 {
		t.Fatalf("List() inventory = %+v, want one system credential", inventory)
	}
}

func TestCredentialCoordinatorRejectsCredentialSuffixDirectoryButLegacyIgnoresIt(t *testing.T) {
	home := t.TempDir()
	credentialDir := filepath.Join(home, ".codex", "accounts", "decoy.auth.json")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fsys := fixedHomeDurableFS{home: home}
	if inventory := DiscoverInventory(fsys); len(inventory.Accounts) != 0 {
		t.Fatalf("legacy inventory = %+v, want credential-shaped directory ignored", inventory)
	}
	store, err := NewManagedStore(fsys)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCredentialCoordinator(store, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ExternalSources = nil

	inventory, err := coordinator.List(context.Background())
	if !errors.Is(err, ErrCredentialAuthorityUnavailable) {
		t.Fatalf("List inventory/error = %+v/%v, want typed authority unavailable", inventory, err)
	}
}

func TestCredentialCoordinatorListKeepsOptionalExternalFailureExplicit(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	coordinator.ExternalSources = []ExternalCredentialSource{&fakeExternalCredentialSource{
		listErr: errors.New("external private path and token-secret"),
	}}

	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want optional source degradation", err)
	}
	if len(inventory.Accounts) != 0 || len(inventory.ExternalSources) != 1 || inventory.ExternalSources[0].ErrorCode != "fetch_error" || inventory.ExternalSources[0].OptionalAbsent {
		t.Fatalf("inventory = %+v, want no external candidates and explicit degraded source", inventory)
	}
}

func TestCredentialCoordinatorListAllowsAbsentSystemAndCodexBar(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want absent optional sources", err)
	}
	if len(inventory.Accounts) != 0 || len(inventory.ExternalSources) != 1 || inventory.ExternalSources[0].ErrorCode != "unavailable" || !inventory.ExternalSources[0].OptionalAbsent {
		t.Fatalf("inventory = %+v, want empty accounts and unavailable optional CodexBar status", inventory)
	}
}

func TestCredentialCoordinatorMarksPreviouslyPresentCodexBarDisappearanceDegraded(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	root := t.TempDir()
	writeCodexBarFixture(t, root, 0o600, nil)
	coordinator.ExternalSources = []ExternalCredentialSource{NewCodexBarSource(root)}

	first, err := coordinator.List(context.Background())
	if err != nil || len(first.ExternalSources) != 1 || first.ExternalSources[0].ErrorCode != "" || first.ExternalSources[0].OptionalAbsent {
		t.Fatalf("first List inventory/error = %+v/%v, want present CodexBar", first, err)
	}
	manifestPath := filepath.Join(root, "managed-codex-accounts.json")
	backupPath := manifestPath + ".backup"
	if err := os.Rename(manifestPath, backupPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(backupPath, manifestPath) })

	second, err := coordinator.List(context.Background())
	if err != nil || len(second.ExternalSources) != 1 || second.ExternalSources[0].ErrorCode != "unavailable" || second.ExternalSources[0].OptionalAbsent {
		t.Fatalf("second List inventory/error = %+v/%v, want previously-present CodexBar degradation", second, err)
	}
}

func TestCredentialCoordinatorBoundsExternalPlanStateToLatestList(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	source := &fakeExternalCredentialSource{candidates: []ExternalCandidate{{
		Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
		Identity: identity, Routable: true,
	}}}
	coordinator, _ := testCoordinator(t)
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	first, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstPlan := PlanCandidate(first.Accounts[0], first.Accounts[0].Candidates[0])
	source.candidates[0].Ref.Revision = "revision-2"
	second, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondPlan := PlanCandidate(second.Accounts[0], second.Accounts[0].Candidates[0])

	coordinator.externalStateMu.Lock()
	defer coordinator.externalStateMu.Unlock()
	if len(coordinator.externalPlans) != 1 || coordinator.externalPlans[externalPlanKey{Ref: secondPlan.Ref, Revision: secondPlan.Revision}] != "external-test" {
		t.Fatalf("external plan state = %+v, want latest generation only", coordinator.externalPlans)
	}
	if _, retained := coordinator.externalPlans[externalPlanKey{Ref: firstPlan.Ref, Revision: firstPlan.Revision}]; retained {
		t.Fatalf("obsolete external revision retained: %+v", firstPlan)
	}
}

func TestCredentialCoordinatorResolveReadsSystemCandidateWithoutWriting(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	jwt := fakeCodexJWT("system@test.com", "acct-system", "user-system", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	before := codexAuthJSON("system-access", "acct-system", jwt)
	fs.files[systemPath] = append([]byte(nil), before...)
	fs.modes[systemPath] = 0o600
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 1 {
		t.Fatalf("inventory = %+v", inventory)
	}
	material, err := coordinator.Resolve(context.Background(), inventory.Accounts[0].Candidates[0].Ref)
	if err != nil {
		t.Fatal(err)
	}
	if material.AccessToken != "system-access" || material.AccountID != "acct-system" {
		t.Fatalf("material = %+v", material)
	}
	if string(fs.files[systemPath]) != string(before) {
		t.Fatal("Resolve mutated system credential")
	}
}

func TestCredentialCoordinatorExternalResolvePanicFailsClosedPrivately(t *testing.T) {
	for _, operation := range []string{"resolve_panic", "resolve_error"} {
		t.Run(operation, func(t *testing.T) {
			externalRef := ExternalCandidateRef{
				Source: "panicking-external", RecordID: "synthetic-record", Revision: "synthetic-revision",
			}
			coordinator, _ := testCoordinator(t)
			coordinator.ExternalSources = []ExternalCredentialSource{panickingExternalCredentialSource{
				operation: operation,
				candidates: []ExternalCandidate{{
					Ref: externalRef,
					Identity: AccountIdentity{
						AccountID: "synthetic-account", UserID: "synthetic-user", RecordKey: "synthetic-user::synthetic-account",
					},
					Routable: true,
				}},
			}}
			inventory, err := coordinator.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 1 {
				t.Fatalf("inventory = %+v", inventory)
			}

			_, err = coordinator.Resolve(context.Background(), inventory.Accounts[0].Candidates[0].Ref)
			if !errors.Is(err, ErrExternalInvalid) {
				t.Fatalf("resolve error = %v, want ErrExternalInvalid", err)
			}
			if strings.Contains(err.Error(), "private external source") {
				t.Fatalf("resolve error disclosed external detail: %v", err)
			}
		})
	}
}

func TestCredentialCoordinatorRepeatedLoginRotatesSameCandidateAndPreservesUnknownFields(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	record.Document["unknown"] = map[string]any{"keep": true}
	if err := coordinator.Store.Commit(&record, revision); err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	credential.Tokens.AccessToken = "rotated"
	ref2, revision2, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if ref2.AccountKey != ref.AccountKey || ref2.CandidateID != ref.CandidateID || revision2 == record.Metadata.Revision {
		t.Fatalf("repeated login ref/revision = %+v %q", ref2, revision2)
	}
	loaded, err := coordinator.loadRef(ref2)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Credential.AccessToken != "rotated" || loaded.Document["unknown"] == nil {
		t.Fatalf("repeated login record = %+v", loaded)
	}
}

func TestCredentialCoordinatorActivateMakesOwnershipPermanentlyExported(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Activate(context.Background(), ref, revision)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SystemCommitted {
		t.Fatalf("result = %+v", result)
	}
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if record.Metadata.RefreshOwnership != RefreshExportedToSystem || record.Metadata.OperationState != OperationReady {
		t.Fatalf("metadata = %+v", record.Metadata)
	}
	var system map[string]any
	_ = json.Unmarshal(fs.files["/fake/home/.codex/auth.json"], &system)
	if _, ok := system["_cq"]; ok {
		t.Fatal("system auth contains _cq metadata")
	}
}

func TestCredentialCoordinatorAdoptsAndRollsForwardBorrowedSystem(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	jwt := fakeCodexJWT("system@test.com", "acct-system", "user-system", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	fs.files[systemPath] = codexAuthJSON("system-one", "acct-system", jwt)
	fs.modes[systemPath] = 0o600
	snapshot, err := coordinator.Activator.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref, firstRevision, err := coordinator.Adopt(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	fs.files[systemPath] = codexAuthJSON("system-two", "acct-system", jwt)
	fs.modes[systemPath] = 0o600
	snapshot, _ = coordinator.Activator.Active(context.Background())
	ref2, secondRevision, err := coordinator.Adopt(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if ref2.CandidateID != ref.CandidateID || secondRevision == firstRevision {
		t.Fatalf("roll forward ref/revision = %+v %q, want same candidate new revision", ref2, secondRevision)
	}
	record, _ := coordinator.loadRef(ref)
	if record.Credential.AccessToken != "system-two" || record.Metadata.Provenance != ProvenanceSystemBorrowed {
		t.Fatalf("record = %+v", record)
	}
}

func TestCredentialCoordinatorMigratesLegacyManagedRoutingIdentity(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	jwt := fakeCodexJWT("legacy@test.com", "acct-legacy", "user-legacy", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	managedPath := "/fake/home/.codex/accounts/legacy.auth.json"
	systemBefore := codexAuthJSON("system", "acct-legacy", jwt)
	managedBefore := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"managed","id_token":"` + jwt + `","account_id":"acct-legacy"},"unknown":{"kept":true}}`)
	fs.files[systemPath] = append([]byte(nil), systemBefore...)
	fs.files[managedPath] = append([]byte(nil), managedBefore...)
	fs.modes[systemPath] = 0o600
	fs.modes[managedPath] = 0o600
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "legacy.auth.json"}},
	}

	before := DiscoverInventory(fs)
	if len(before.Accounts) != 1 || !before.Accounts[0].Unstable {
		t.Fatalf("legacy inventory = %+v, want one unstable account", before.Accounts)
	}
	result, err := coordinator.MigrateLegacyManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1", result.Migrated)
	}
	if got := string(fs.files[systemPath]); got != string(systemBefore) {
		t.Fatal("legacy migration changed system auth")
	}
	after := DiscoverInventory(fs)
	if len(after.Accounts) != 1 || after.Accounts[0].Unstable || !after.Accounts[0].Routable {
		t.Fatalf("migrated inventory = %+v, want one stable routable account", after.Accounts)
	}
	record, err := coordinator.Store.Load(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.Metadata.Version != 1 || record.Metadata.Provenance != ProvenanceLegacyUnknown || record.Metadata.RefreshOwnership != RefreshOwnershipUnknown || !record.RefreshSuspended {
		t.Fatalf("migrated metadata = %+v suspended=%t", record.Metadata, record.RefreshSuspended)
	}
	if _, ok := record.Document["unknown"]; !ok {
		t.Fatal("legacy migration dropped unknown field")
	}
	second, err := coordinator.MigrateLegacyManaged(context.Background())
	if err != nil || second.Migrated != 0 {
		t.Fatalf("repeat migration = %+v, %v", second, err)
	}
}

func TestCredentialCoordinatorResumesLegacyManagedMigration(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	jwts := []string{
		fakeCodexJWT("legacy-a@test.com", "acct-legacy-a", "user-legacy-a", "plus"),
		fakeCodexJWT("legacy-b@test.com", "acct-legacy-b", "user-legacy-b", "plus"),
	}
	paths := []string{
		"/fake/home/.codex/accounts/legacy-a.auth.json",
		"/fake/home/.codex/accounts/legacy-b.auth.json",
	}
	for index, path := range paths {
		fs.files[path] = []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":"managed-%d","id_token":"%s","account_id":"acct-legacy-%c"}}`, index, jwts[index], 'a'+index))
		fs.modes[path] = 0o600
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "legacy-a.auth.json"}, {name: "legacy-b.auth.json"}},
	}
	before := DiscoverInventory(fs)
	if len(before.Accounts) != 2 {
		t.Fatalf("legacy accounts = %d, want 2", len(before.Accounts))
	}
	fs.failRenameAt = 2
	if _, err := coordinator.MigrateLegacyManaged(context.Background()); err == nil {
		t.Fatal("partial migration error = nil")
	}
	versions := make([]int, len(paths))
	for index, path := range paths {
		record, err := coordinator.Store.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		versions[index] = record.Metadata.Version
	}
	if !((versions[0] == 1 && versions[1] == 0) || (versions[0] == 0 && versions[1] == 1)) {
		t.Fatalf("partially migrated versions = %v, want one v1 and one legacy", versions)
	}

	fs.failRenameAt = 0
	result, err := coordinator.MigrateLegacyManaged(context.Background())
	if err != nil || result.Migrated != 1 {
		t.Fatalf("resumed migration = %+v, %v", result, err)
	}
	for _, path := range paths {
		record, err := coordinator.Store.Load(path)
		if err != nil || record.Metadata.Version != 1 || record.Metadata.AccountKey == "" {
			t.Fatalf("resumed record %q = %+v, %v", path, record.Metadata, err)
		}
	}
}

func TestCredentialCoordinatorAdoptionNeverOverwritesCQOwnedRecord(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("external", "acct-1", credential.Tokens.IDToken)
	fs.modes["/fake/home/.codex/auth.json"] = 0o600
	snapshot, _ := coordinator.Activator.Active(context.Background())
	returnedRef, returnedRevision, err := coordinator.Adopt(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if returnedRef.CandidateID != ref.CandidateID || returnedRevision != revision {
		t.Fatalf("adoption changed CQ-owned reference: %+v %q", returnedRef, returnedRevision)
	}
	record, _ := coordinator.loadRef(ref)
	if record.Credential.AccessToken != "access" {
		t.Fatalf("CQ-owned token overwritten: %q", record.Credential.AccessToken)
	}
}

func TestCredentialCoordinatorRemoveUsesExactJournaledRevision(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	ref, revision, err := coordinator.SaveLogin(context.Background(), testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	result, err := coordinator.RemoveManaged(context.Background(), ref.AccountKey, RevisionSet{ref.CandidateID: revision}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.ManagedDeleted != 1 || result.PendingRecovery {
		t.Fatalf("result = %+v", result)
	}
}

func TestCredentialCoordinatorRemovalRejectsPartialCandidateSetBeforeJournal(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	ref, _, err := coordinator.SaveLogin(context.Background(), testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	if _, err := coordinator.RemoveManaged(context.Background(), ref.AccountKey, RevisionSet{}, false); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("RemoveManaged error = %v", err)
	}
	if _, ok, err := coordinator.Journal.Load(); err != nil || ok {
		t.Fatalf("journal created for partial set: ok %v, err %v", ok, err)
	}
}

func TestCredentialCoordinatorRemovingInactiveAccountKeepsOtherSystemAccount(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	activeJWT := fakeCodexJWT("active@test.com", "acct-active", "user-active", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	fs.files[systemPath] = codexAuthJSON("active", "acct-active", activeJWT)
	fs.modes[systemPath] = 0o600
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("inactive@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	result, err := coordinator.RemoveManaged(context.Background(), ref.AccountKey, RevisionSet{ref.CandidateID: revision}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.SystemDeactivated {
		t.Fatal("inactive removal deactivated unrelated system account")
	}
	if _, ok := fs.files[systemPath]; !ok {
		t.Fatal("unrelated system auth was removed")
	}
}

func TestCredentialCoordinatorSystemRevisionRaceLeavesRemovalRecoverable(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("active@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	if _, err := coordinator.Activate(context.Background(), ref, revision); err != nil {
		t.Fatal(err)
	}
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	plan := RemovalPlan{
		Version: 1, OperationID: "race", AccountKey: ref.AccountKey,
		Candidates: []RemovalCandidate{{CandidateID: ref.CandidateID, Revision: record.Metadata.Revision}},
	}
	active, err := coordinator.Activator.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan.ExpectedSystemRevision = active.Revision
	if err := coordinator.Journal.Save(plan); err != nil {
		t.Fatal(err)
	}
	fs.files["/fake/home/.codex/auth.json"] = append(fs.files["/fake/home/.codex/auth.json"], ' ')
	result, err := coordinator.RecoverRemoval(context.Background())
	if !errors.Is(err, ErrStaleRevision) || !result.PendingRecovery {
		t.Fatalf("RecoverRemoval = %+v, %v", result, err)
	}
	if _, ok, loadErr := coordinator.Journal.Load(); loadErr != nil || !ok {
		t.Fatalf("journal lost after race: ok %v, err %v", ok, loadErr)
	}
}

func TestCredentialCoordinatorRecoveryTreatsAlreadyRemovedSystemAsComplete(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("active@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	if _, err := coordinator.Activate(context.Background(), ref, revision); err != nil {
		t.Fatal(err)
	}
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	active, err := coordinator.Activator.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := RemovalPlan{
		Version: 1, OperationID: "removed-before-restart", AccountKey: ref.AccountKey,
		Candidates:             []RemovalCandidate{{CandidateID: ref.CandidateID, Revision: record.Metadata.Revision}},
		ExpectedSystemRevision: active.Revision,
	}
	if err := coordinator.Journal.Save(plan); err != nil {
		t.Fatal(err)
	}
	delete(fs.files, ref.path)
	delete(fs.modes, ref.path)
	delete(fs.files, "/fake/home/.codex/auth.json")
	delete(fs.modes, "/fake/home/.codex/auth.json")
	result, err := coordinator.RecoverRemoval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.SystemDeactivated || result.PendingRecovery {
		t.Fatalf("result = %+v", result)
	}
}

func TestCredentialCoordinatorSerialisesActivateAndRemove(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := coordinator.Activate(context.Background(), ref, revision)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := coordinator.RemoveManaged(context.Background(), ref.AccountKey, RevisionSet{ref.CandidateID: revision}, true)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful conflicting operations = %d, want 1", successes)
	}
}

func TestCredentialCoordinatorRecoveryFinishesExactPendingRemoval(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	ref, revision, err := coordinator.SaveLogin(context.Background(), testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	plan := RemovalPlan{Version: 1, OperationID: "pending", AccountKey: ref.AccountKey, Candidates: []RemovalCandidate{{CandidateID: ref.CandidateID, Revision: revision}}}
	if err := coordinator.Journal.Save(plan); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.RecoverRemoval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ManagedDeleted != 1 || result.PendingRecovery {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := fs.files[coordinator.Journal.path()]; ok {
		t.Fatal("removal journal still present")
	}
}

func TestCredentialCoordinatorListDoesNotWrite(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	before := len(fs.files)
	if _, err := coordinator.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fs.files) != before {
		t.Fatal("read-only inventory wrote state")
	}
}
