package codex

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func testCoordinator(t *testing.T) (*CredentialCoordinator, *durableFakeFS) {
	t.Helper()
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	coordinator, err := NewCredentialCoordinator(store)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, fs
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
