package codex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
)

type projectionCatalogueStub struct {
	upsertErr error
	upserts   []RegistryAccount
	removals  []map[string]bool
}

func (s *projectionCatalogueStub) UpsertAccount(account RegistryAccount) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts = append(s.upserts, account)
	return nil
}

func (s *projectionCatalogueStub) RemoveAccounts(keys map[string]bool) error {
	copyKeys := make(map[string]bool, len(keys))
	for key, remove := range keys {
		copyKeys[key] = remove
	}
	s.removals = append(s.removals, copyKeys)
	return nil
}

func projectionCredential(email, accountID, userID, plan string, createdAt time.Time) LoginCredential {
	idToken := fakeCodexJWT(email, accountID, userID, plan)
	return LoginCredential{
		Tokens: auth.CodexTokenResponse{
			AccessToken:  "projection-access",
			RefreshToken: "projection-refresh",
			IDToken:      idToken,
		},
		Claims:    auth.DecodeCodexClaims(idToken),
		CreatedAt: createdAt,
	}
}

func installManagedDirectoryEntry(fs *durableFakeFS, path string) {
	dir := filepath.Dir(path)
	if fs.dirEntries == nil {
		fs.dirEntries = make(map[string][]fakeDirEntry)
	}
	fs.dirEntries[dir] = append(fs.dirEntries[dir], fakeDirEntry{name: filepath.Base(path)})
}

func TestSaveLoginProjectionFailureRecoversAfterRestart(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	catalogueFailure := errors.New("synthetic catalogue failure")
	coordinator.Registry = &projectionCatalogueStub{upsertErr: catalogueFailure}

	_, _, err := coordinator.SaveLogin(context.Background(), projectionCredential(
		"recovery@test.com", "acct-recovery", "user-recovery", "plus", time.Unix(123, 0),
	))
	var projectionErr *ManagedProjectionError
	if !errors.As(err, &projectionErr) || !errors.Is(err, catalogueFailure) {
		t.Fatalf("SaveLogin error = %v, want typed projection failure", err)
	}

	paths := managedCredentialPaths(fs)
	if len(paths) != 1 {
		t.Fatalf("managed paths = %v, want one committed record", paths)
	}
	managedPath := paths[0]
	managedBefore := append([]byte(nil), fs.files[managedPath]...)
	installManagedDirectoryEntry(fs, managedPath)

	restarted, err := NewCredentialCoordinator(testManagedStore(t, fs), testCQStateDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("RecoverCredentialState: %v", err)
	}
	if got := string(fs.files[managedPath]); got != string(managedBefore) {
		t.Fatal("projection recovery rewrote managed credential")
	}

	record, err := restarted.Store.Load(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	registryDoc := readRegistryDocument(t, fs)
	accounts, _ := registryDoc["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("registry accounts = %#v, want one repaired row", accounts)
	}
	row, _ := accounts[0].(map[string]any)
	if row["account_key"] != string(record.Metadata.AccountKey) ||
		row["chatgpt_account_id"] != "acct-recovery" ||
		row["chatgpt_user_id"] != "user-recovery" ||
		row["email"] != "recovery@test.com" || row["plan"] != "plus" {
		t.Fatalf("repaired row = %#v", row)
	}
	if _, exists := fs.files["/fake/home/.codex/auth.json"]; exists {
		t.Fatal("projection recovery changed system auth")
	}
}

func TestExistingSaveLoginProjectionFailureRecoversRotatedRecord(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	initial := projectionCredential("old@test.com", "acct-rotate", "user-rotate", "plus", time.Unix(123, 0))
	ref, _, err := coordinator.SaveLogin(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	installManagedDirectoryEntry(fs, ref.path)
	registryPath := coordinator.Registry.(Registry).path()
	delete(fs.files, registryPath)
	delete(fs.modes, registryPath)

	catalogueFailure := errors.New("synthetic rotated catalogue failure")
	coordinator.Registry = &projectionCatalogueStub{upsertErr: catalogueFailure}
	rotated := projectionCredential("new@test.com", "acct-rotate", "user-rotate", "pro", time.Unix(124, 0))
	rotated.Tokens.AccessToken = "rotated-access"
	_, _, err = coordinator.SaveLogin(context.Background(), rotated)
	var projectionErr *ManagedProjectionError
	if !errors.As(err, &projectionErr) || !errors.Is(err, catalogueFailure) {
		t.Fatalf("SaveLogin error = %v, want typed projection failure", err)
	}
	managedBefore := append([]byte(nil), fs.files[ref.path]...)

	restarted, err := NewCredentialCoordinator(testManagedStore(t, fs), testCQStateDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("RecoverCredentialState: %v", err)
	}
	if got := string(fs.files[ref.path]); got != string(managedBefore) {
		t.Fatal("projection recovery rewrote rotated managed record")
	}
	loaded, err := restarted.Store.Load(ref.path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Credential.AccessToken != "rotated-access" {
		t.Fatalf("access token = %q, want durable rotation", loaded.Credential.AccessToken)
	}
	accounts := readRegistryDocument(t, fs)["accounts"].([]any)
	row := accounts[0].(map[string]any)
	if len(accounts) != 1 || row["email"] != "new@test.com" || row["plan"] != "pro" {
		t.Fatalf("repaired rotated row = %#v", accounts)
	}
}

func TestProjectionRecoveryPreservesActiveAndUnknownFields(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	record, err := coordinator.Store.SaveNew(projectionCredential(
		"preserve@test.com", "acct-preserve", "user-preserve", "pro", time.Unix(456, 0),
	))
	if err != nil {
		t.Fatal(err)
	}
	installManagedDirectoryEntry(fs, record.Path)

	registryPath := coordinator.Registry.(Registry).path()
	fs.files[registryPath] = []byte(`{
		"schema_version": 3,
		"active_account_key": "existing::active",
		"future_top": {"keep": true},
		"accounts": [
			{
				"account_key": "` + string(record.Metadata.AccountKey) + `",
				"email": "old@test.com",
				"alias": "work",
				"created_at": 7,
				"future_row": 42
			},
			{"account_key": "unrelated::account", "future_unrelated": "keep"}
		]
	}`)
	fs.modes[registryPath] = 0o600

	if err := coordinator.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("RecoverCredentialState: %v", err)
	}
	doc := readRegistryDocument(t, fs)
	if doc["active_account_key"] != "existing::active" {
		t.Fatalf("active_account_key = %#v", doc["active_account_key"])
	}
	if _, ok := doc["future_top"]; !ok {
		t.Fatal("future top-level field was lost")
	}
	accounts := doc["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v, want repaired plus unrelated", accounts)
	}
	repaired := accounts[0].(map[string]any)
	if repaired["alias"] != "work" || repaired["created_at"] != float64(7) || repaired["future_row"] != float64(42) {
		t.Fatalf("stable fields changed: %#v", repaired)
	}
	if repaired["email"] != "preserve@test.com" || repaired["plan"] != "pro" {
		t.Fatalf("known fields not repaired: %#v", repaired)
	}
	unrelated := accounts[1].(map[string]any)
	if unrelated["account_key"] != "unrelated::account" || unrelated["future_unrelated"] != "keep" {
		t.Fatalf("unrelated row changed: %#v", unrelated)
	}
}

func TestAdoptProjectionFailureRecoversAfterRestart(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	catalogueFailure := errors.New("synthetic adoption catalogue failure")
	coordinator.Registry = &projectionCatalogueStub{upsertErr: catalogueFailure}
	idToken := fakeCodexJWT("adopt@test.com", "acct-adopt", "user-adopt", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	fs.files[systemPath] = codexAuthJSON("adopt-access", "acct-adopt", idToken)
	fs.modes[systemPath] = 0o600
	snapshot, err := coordinator.Activator.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = coordinator.Adopt(context.Background(), snapshot)
	var projectionErr *ManagedProjectionError
	if !errors.As(err, &projectionErr) || !errors.Is(err, catalogueFailure) {
		t.Fatalf("Adopt error = %v, want typed projection failure", err)
	}
	paths := managedCredentialPaths(fs)
	if len(paths) != 1 {
		t.Fatalf("managed paths = %v, want committed borrowed record", paths)
	}
	managedPath := paths[0]
	managedBefore := append([]byte(nil), fs.files[managedPath]...)
	installManagedDirectoryEntry(fs, managedPath)

	restarted, err := NewCredentialCoordinator(testManagedStore(t, fs), testCQStateDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("RecoverCredentialState: %v", err)
	}
	if got := string(fs.files[managedPath]); got != string(managedBefore) {
		t.Fatal("adoption recovery rewrote borrowed managed credential")
	}
	accounts := readRegistryDocument(t, fs)["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want one repaired adoption row", accounts)
	}
}

func TestExistingBorrowedAdoptRepairsMissingProjection(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	idToken := fakeCodexJWT("borrowed@test.com", "acct-borrowed", "user-borrowed", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	fs.files[systemPath] = codexAuthJSON("borrowed-one", "acct-borrowed", idToken)
	fs.modes[systemPath] = 0o600
	snapshot, _ := coordinator.Activator.Active(context.Background())
	ref, revision, err := coordinator.Adopt(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	installManagedDirectoryEntry(fs, ref.path)
	delete(fs.files, coordinator.Registry.(Registry).path())
	delete(fs.modes, coordinator.Registry.(Registry).path())

	fs.files[systemPath] = codexAuthJSON("borrowed-two", "acct-borrowed", idToken)
	fs.modes[systemPath] = 0o600
	snapshot, _ = coordinator.Activator.Active(context.Background())
	returnedRef, returnedRevision, err := coordinator.Adopt(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if returnedRef != ref || returnedRevision == revision {
		t.Fatalf("returned ref/revision = %+v %q, want same candidate new revision", returnedRef, returnedRevision)
	}
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if record.Credential.AccessToken != "borrowed-two" {
		t.Fatalf("access token = %q, want rolled forward", record.Credential.AccessToken)
	}
	accounts := readRegistryDocument(t, fs)["accounts"].([]any)
	if len(accounts) != 1 || accounts[0].(map[string]any)["account_key"] != string(record.Metadata.AccountKey) {
		t.Fatalf("repaired accounts = %#v", accounts)
	}
}

func TestExistingCQOwnedAdoptRepairsProjectionWithoutOverwrite(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := projectionCredential("owned@test.com", "acct-owned", "user-owned", "plus", time.Unix(700, 0))
	ref, _, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	installManagedDirectoryEntry(fs, ref.path)
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	record.Document["future_managed"] = map[string]any{"keep": true}
	if err := coordinator.Store.Commit(&record, record.Metadata.Revision); err != nil {
		t.Fatal(err)
	}
	managedBefore := append([]byte(nil), fs.files[record.Path]...)
	metadataBefore := record.Metadata
	delete(fs.files, coordinator.Registry.(Registry).path())
	delete(fs.modes, coordinator.Registry.(Registry).path())

	systemPath := "/fake/home/.codex/auth.json"
	fs.files[systemPath] = codexAuthJSON("must-not-overwrite", "acct-owned", credential.Tokens.IDToken)
	fs.modes[systemPath] = 0o600
	snapshot, _ := coordinator.Activator.Active(context.Background())
	returnedRef, returnedRevision, err := coordinator.Adopt(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if returnedRef != recordRef(record) || returnedRevision != metadataBefore.Revision {
		t.Fatalf("returned ref/revision = %+v %q", returnedRef, returnedRevision)
	}
	if got := string(fs.files[record.Path]); got != string(managedBefore) {
		t.Fatal("CQ-owned adoption rewrote managed bytes")
	}
	loaded, err := coordinator.Store.Load(record.Path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.LineageID != metadataBefore.LineageID || loaded.Metadata.RefreshOwnership != metadataBefore.RefreshOwnership || loaded.Metadata.Revision != metadataBefore.Revision || loaded.Document["future_managed"] == nil {
		t.Fatalf("managed authority changed: %+v %#v", loaded.Metadata, loaded.Document)
	}
	accounts := readRegistryDocument(t, fs)["accounts"].([]any)
	if len(accounts) != 1 || accounts[0].(map[string]any)["account_key"] != string(record.Metadata.AccountKey) {
		t.Fatalf("repaired accounts = %#v", accounts)
	}
}

func TestLegacyAdoptRepairsProjectionWithoutCredentialWrite(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	idToken := fakeCodexJWT("legacy@test.com", "acct-legacy", "user-legacy", "plus")
	legacyPath := "/fake/home/.codex/accounts/legacy.auth.json"
	legacyBefore := codexAuthJSON("legacy-access", "acct-legacy", idToken)
	fs.files[legacyPath] = append([]byte(nil), legacyBefore...)
	fs.modes[legacyPath] = 0o600
	installManagedDirectoryEntry(fs, legacyPath)
	systemPath := "/fake/home/.codex/auth.json"
	fs.files[systemPath] = codexAuthJSON("system-copy", "acct-legacy", idToken)
	fs.modes[systemPath] = 0o600
	snapshot, _ := coordinator.Activator.Active(context.Background())

	if _, _, err := coordinator.Adopt(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if got := string(fs.files[legacyPath]); got != string(legacyBefore) {
		t.Fatal("legacy adoption rewrote credential")
	}
	accounts := readRegistryDocument(t, fs)["accounts"].([]any)
	if len(accounts) != 1 || accounts[0].(map[string]any)["account_key"] != "user-legacy::acct-legacy" {
		t.Fatalf("legacy projection = %#v", accounts)
	}
}

func TestProjectionRecoveryRunsAfterRemovalRecovery(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	record, err := coordinator.Store.SaveNew(projectionCredential(
		"removed@test.com", "acct-removed", "user-removed", "plus", time.Unix(800, 0),
	))
	if err != nil {
		t.Fatal(err)
	}
	installManagedDirectoryEntry(fs, record.Path)
	if err := coordinator.RecoverCredentialState(context.Background()); err != nil {
		t.Fatal(err)
	}
	plan := RemovalPlan{
		Version: 1, OperationID: "projection-removal", AccountKey: record.Metadata.AccountKey,
		Candidates:   []RemovalCandidate{{CandidateID: record.Metadata.CandidateID, Revision: record.Metadata.Revision}},
		RegistryKeys: []string{string(record.Metadata.AccountKey)},
	}
	if err := coordinator.Journal.Save(plan); err != nil {
		t.Fatal(err)
	}
	fs.dirEntries[filepath.Dir(record.Path)] = nil

	if err := coordinator.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("RecoverCredentialState: %v", err)
	}
	if _, exists := fs.files[record.Path]; exists {
		t.Fatal("pending removal did not remove managed record")
	}
	doc := readRegistryDocument(t, fs)
	if accounts := doc["accounts"].([]any); len(accounts) != 0 {
		t.Fatalf("deleted record resurrected into catalogue: %#v", accounts)
	}
	if _, exists := fs.files[coordinator.Journal.path()]; exists {
		t.Fatal("pending removal journal was not cleared")
	}
}

type projectionPanicExternalSource struct{}

func (projectionPanicExternalSource) Name() string { return "projection-panic" }
func (projectionPanicExternalSource) List(context.Context) ([]ExternalCandidate, error) {
	panic("projection recovery scanned external source")
}
func (projectionPanicExternalSource) Resolve(context.Context, ExternalCandidateRef) (CredentialMaterial, error) {
	panic("projection recovery resolved external source")
}

func TestProjectionRecoveryNeverProjectsExternalOrSystem(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	catalogue := &projectionCatalogueStub{}
	coordinator.Registry = catalogue
	coordinator.ExternalSources = []ExternalCredentialSource{projectionPanicExternalSource{}}
	idToken := fakeCodexJWT("system@test.com", "acct-system", "user-system", "plus")
	systemPath := "/fake/home/.codex/auth.json"
	systemBefore := codexAuthJSON("system-access", "acct-system", idToken)
	fs.files[systemPath] = append([]byte(nil), systemBefore...)
	fs.modes[systemPath] = 0o600

	if err := coordinator.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("RecoverCredentialState: %v", err)
	}
	if len(catalogue.upserts) != 0 || len(catalogue.removals) != 0 {
		t.Fatalf("catalogue mutations = upserts %#v removals %#v", catalogue.upserts, catalogue.removals)
	}
	if got := string(fs.files[systemPath]); got != string(systemBefore) {
		t.Fatal("projection recovery rewrote system auth")
	}
}

func TestProjectionRecoveryCrashMatrix(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*CredentialCoordinator, *durableFakeFS)
		wantRecord bool
	}{
		{
			name: "managed pre-rename",
			configure: func(_ *CredentialCoordinator, fs *durableFakeFS) {
				fs.failRenameAt = 1
			},
		},
		{
			name: "managed post-rename",
			configure: func(_ *CredentialCoordinator, fs *durableFakeFS) {
				fs.failStep = "directory sync"
			},
			wantRecord: true,
		},
		{
			name: "catalogue write",
			configure: func(coordinator *CredentialCoordinator, _ *durableFakeFS) {
				coordinator.Registry = &projectionCatalogueStub{upsertErr: errors.New("synthetic catalogue failure")}
			},
			wantRecord: true,
		},
		{
			name:       "process return",
			configure:  func(_ *CredentialCoordinator, _ *durableFakeFS) {},
			wantRecord: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fs := testCoordinator(t)
			test.configure(coordinator, fs)
			_, _, _ = coordinator.SaveLogin(context.Background(), projectionCredential(
				"matrix@test.com", "acct-matrix", "user-matrix", "plus", time.Unix(900, 0),
			))
			fs.failStep = ""
			fs.failRenameAt = 0
			fs.renameCount = 0
			paths := managedCredentialPaths(fs)
			if test.wantRecord && len(paths) != 1 {
				t.Fatalf("managed paths = %v, want one durable record", paths)
			}
			if !test.wantRecord && len(paths) != 0 {
				t.Fatalf("managed paths = %v, want no committed record", paths)
			}
			var managedBefore []byte
			if len(paths) == 1 {
				managedBefore = append([]byte(nil), fs.files[paths[0]]...)
				installManagedDirectoryEntry(fs, paths[0])
			} else {
				fs.dirEntries = map[string][]fakeDirEntry{"/fake/home/.codex/accounts": nil}
			}

			restarted, err := NewCredentialCoordinator(testManagedStore(t, fs), testCQStateDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.RecoverCredentialState(context.Background()); err != nil {
				t.Fatalf("first RecoverCredentialState: %v", err)
			}
			if err := restarted.RecoverCredentialState(context.Background()); err != nil {
				t.Fatalf("second RecoverCredentialState: %v", err)
			}
			if len(paths) == 1 && string(fs.files[paths[0]]) != string(managedBefore) {
				t.Fatal("restart recovery rewrote managed authority")
			}
			var accounts []any
			if _, exists := fs.files["/fake/home/.codex/accounts/registry.json"]; exists {
				doc := readRegistryDocument(t, fs)
				accounts, _ = doc["accounts"].([]any)
			}
			wantAccounts := 0
			if test.wantRecord {
				wantAccounts = 1
			}
			if len(accounts) != wantAccounts {
				t.Fatalf("accounts = %#v, want %d repaired rows", accounts, wantAccounts)
			}
		})
	}
}

func TestRegistryAccountFromManagedRecordRequiresStrongIdentity(t *testing.T) {
	validToken := fakeCodexJWT("strong@test.com", "acct-strong", "user-strong", "plus")
	tests := []struct {
		name   string
		record ManagedRecord
		wantOK bool
	}{
		{
			name: "strong version one",
			record: ManagedRecord{
				Metadata:   ManagedMetadata{Version: 1, AccountKey: "opaque-managed-key"},
				Credential: CredentialMaterial{AccountID: "acct-strong", IDToken: validToken},
				Document:   map[string]any{"auth_mode": "chatgpt"},
			},
			wantOK: true,
		},
		{
			name: "missing user identity",
			record: ManagedRecord{
				Metadata:   ManagedMetadata{Version: 1, AccountKey: "opaque-managed-key"},
				Credential: CredentialMaterial{AccountID: "acct-strong", IDToken: "invalid"},
			},
		},
		{
			name: "mismatched account identity",
			record: ManagedRecord{
				Metadata:   ManagedMetadata{Version: 1, AccountKey: "opaque-managed-key"},
				Credential: CredentialMaterial{AccountID: "different", IDToken: validToken},
			},
		},
		{
			name: "missing managed key",
			record: ManagedRecord{
				Metadata:   ManagedMetadata{Version: 1},
				Credential: CredentialMaterial{AccountID: "acct-strong", IDToken: validToken},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, gotOK := registryAccountFromManagedRecord(test.record)
			if gotOK != test.wantOK {
				t.Fatalf("registryAccountFromManagedRecord ok = %v, want %v", gotOK, test.wantOK)
			}
		})
	}
}

func TestProjectionRecoveryUsesDeterministicManagedPathOrder(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	first, err := coordinator.Store.SaveNew(projectionCredential(
		"first@test.com", "acct-first", "user-first", "plus", time.Unix(1000, 0),
	))
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.Store.SaveNew(projectionCredential(
		"second@test.com", "acct-second", "user-second", "plus", time.Unix(1001, 0),
	))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(first.Path)
	fs.dirEntries = map[string][]fakeDirEntry{dir: {
		{name: filepath.Base(second.Path)},
		{name: filepath.Base(first.Path)},
	}}
	catalogue := &projectionCatalogueStub{}
	coordinator.Registry = catalogue

	if err := coordinator.RecoverCredentialState(context.Background()); err != nil {
		t.Fatalf("RecoverCredentialState: %v", err)
	}
	if len(catalogue.upserts) != 2 {
		t.Fatalf("upserts = %#v, want two", catalogue.upserts)
	}
	records := []ManagedRecord{first, second}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	if catalogue.upserts[0].AccountKey != string(records[0].Metadata.AccountKey) ||
		catalogue.upserts[1].AccountKey != string(records[1].Metadata.AccountKey) {
		t.Fatalf("upsert order = %#v, want managed path order %q then %q", catalogue.upserts, records[0].Path, records[1].Path)
	}
	if len(catalogue.removals) != 0 {
		t.Fatalf("projection recovery performed broad deletion: %#v", catalogue.removals)
	}
}

func managedCredentialPaths(fs *durableFakeFS) []string {
	var paths []string
	for path := range fs.files {
		if filepath.Ext(path) == ".json" && filepath.Base(path) != "registry.json" && filepath.Dir(path) == "/fake/home/.codex/accounts" {
			paths = append(paths, path)
		}
	}
	return paths
}

func readRegistryDocument(t *testing.T, fs *durableFakeFS) map[string]any {
	t.Helper()
	data := fs.files["/fake/home/.codex/accounts/registry.json"]
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode registry: %v", err)
	}
	return doc
}
