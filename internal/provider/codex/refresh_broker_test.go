package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

func openRestartableAuthorityCoordinator(t *testing.T, filesystem *fsutil.MemFS, mutationBackend, ownerBackend CredentialAuthorityBackend, mutationHook, ownerHook func(string) error) *CredentialCoordinator {
	t.Helper()
	store, err := NewManagedStore(filesystem)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCredentialCoordinatorWithAuthority(store, testCQStateDir(), CredentialAuthorityCapabilities{
		Role: CredentialAuthorityPrimary, LifecycleBound: true, RootKey: bytes.Repeat([]byte{0x73}, 32),
		RefreshMutationBackend: mutationBackend, CredentialOwnerBackend: ownerBackend,
		RefreshMutationHook: mutationHook, CredentialOwnerHook: ownerHook,
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func newRestartableAuthorityCoordinator(t *testing.T, filesystem *fsutil.MemFS, mutationBackend, ownerBackend CredentialAuthorityBackend, mutationHook, ownerHook func(string) error) (*CredentialCoordinator, CandidateRef, Revision) {
	t.Helper()
	coordinator := openRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, mutationHook, ownerHook)
	record, err := coordinator.Store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, recordRef(record), record.Metadata.Revision
}

func closeAuthorityCoordinator(t *testing.T, coordinator *CredentialCoordinator) {
	t.Helper()
	if owner, ok := coordinator.CredentialOwner.(*CredentialOwnerStore); ok {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if mutation, ok := coordinator.RefreshMutations.(*RefreshMutationStore); ok {
		if err := mutation.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func testRefreshRecord(t *testing.T) (*CredentialCoordinator, *durableFakeFS, CandidateRef, Revision) {
	t.Helper()
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("refresh@test.invalid", "acct-refresh", "user-refresh", "plus")
	credential.Claims = auth.DecodeCodexClaims(credential.Tokens.IDToken)
	credential.Tokens.RefreshToken = "managed-refresh"
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	coordinator.Now = func() time.Time { return time.Unix(1_000, 0).UTC() }
	coordinator.RefreshMutations = &recoveringRefreshRecorder{}
	coordinator.CredentialOwner = &idempotentOwnerRecorder{}
	return coordinator, fs, ref, revision
}

func TestRefreshEligibleRequiresOwnedReadyCQLineage(t *testing.T) {
	base := ManagedRecord{
		Metadata: ManagedMetadata{
			Version: 1, Provenance: ProvenanceCQOAuth,
			RefreshOwnership: RefreshCQOwnedNeverExported, OperationState: OperationReady,
		},
		Credential: CredentialMaterial{RefreshToken: "synthetic"},
	}
	if !RefreshEligible(base) {
		t.Fatal("eligible lineage rejected")
	}
	tests := map[string]func(*ManagedRecord){
		"borrowed":           func(r *ManagedRecord) { r.Metadata.Provenance = ProvenanceSystemBorrowed },
		"legacy":             func(r *ManagedRecord) { r.Metadata.Provenance = ProvenanceLegacyUnknown },
		"exported":           func(r *ManagedRecord) { r.Metadata.RefreshOwnership = RefreshExportedToSystem },
		"unknown ownership":  func(r *ManagedRecord) { r.Metadata.RefreshOwnership = RefreshOwnershipUnknown },
		"activation pending": func(r *ManagedRecord) { r.Metadata.OperationState = OperationActivationPending },
		"refreshing":         func(r *ManagedRecord) { r.Metadata.OperationState = OperationRefreshing },
		"uncertain":          func(r *ManagedRecord) { r.Metadata.OperationState = OperationRotationUncertain },
		"unknown state":      func(r *ManagedRecord) { r.RefreshSuspended = true },
		"operation present":  func(r *ManagedRecord) { r.Metadata.OperationID = "pending" },
		"no refresh token":   func(r *ManagedRecord) { r.Credential.RefreshToken = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := base
			mutate(&record)
			if RefreshEligible(record) {
				t.Fatal("ineligible lineage accepted")
			}
		})
	}
}

func TestRefreshRejectsStaleRevisionBeforeExchange(t *testing.T) {
	coordinator, _, ref, _ := testRefreshRecord(t)
	var exchanges atomic.Int32
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		exchanges.Add(1)
		return nil, errors.New("unexpected")
	}
	if _, err := coordinator.Refresh(context.Background(), ref, "stale"); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Refresh error = %v, want stale revision", err)
	}
	if exchanges.Load() != 0 {
		t.Fatalf("exchange count = %d, want 0", exchanges.Load())
	}
}

type orderedRefreshMutationRecorder struct {
	events *[]string
	err    error
}

func (r orderedRefreshMutationRecorder) SelectRefreshMutation(operationID string, _ CandidateRef, _ Revision, _ RefreshMutationCapacity) (RefreshMutationSelection, error) {
	*r.events = append(*r.events, "reservation")
	return RefreshMutationSelection{ReservationDigest: operationID + "-reservation", CapacityLeaseDigest: operationID + "-lease"}, r.err
}

func (r orderedRefreshMutationRecorder) CompleteRefreshMutation(_ string, receiptDigest string) (string, error) {
	*r.events = append(*r.events, "reservation-receipt")
	if receiptDigest == "" {
		return "", errors.New("missing credential receipt binding")
	}
	return "mutation-terminal", nil
}

type orderedCredentialOwnerRecorder struct{ events *[]string }

func (r orderedCredentialOwnerRecorder) PublishCommit(_ string, reservationDigest, capacityLeaseDigest string) (string, error) {
	*r.events = append(*r.events, "owner-commit")
	if reservationDigest == "" || capacityLeaseDigest == "" {
		return "", errors.New("missing mutation binding")
	}
	return "owner-commit-digest", nil
}

type recoveringRefreshRecorder struct {
	selected         map[string]bool
	completeFailures int
	completions      int
}

func (r *recoveringRefreshRecorder) SelectRefreshMutation(operationID string, _ CandidateRef, _ Revision, _ RefreshMutationCapacity) (RefreshMutationSelection, error) {
	if r.selected == nil {
		r.selected = make(map[string]bool)
	}
	if r.selected[operationID] {
		return RefreshMutationSelection{ReservationDigest: operationID + "-reservation", CapacityLeaseDigest: operationID + "-lease"}, nil
	}
	r.selected[operationID] = true
	return RefreshMutationSelection{ReservationDigest: operationID + "-reservation", CapacityLeaseDigest: operationID + "-lease"}, nil
}

func (r *recoveringRefreshRecorder) CompleteRefreshMutation(operationID, receiptDigest string) (string, error) {
	if !r.selected[operationID] {
		return "", errors.New("unknown reservation")
	}
	if receiptDigest == "" {
		return "", errors.New("missing credential receipt binding")
	}
	r.completions++
	if r.completeFailures > 0 {
		r.completeFailures--
		return "", errors.New("terminal publication failed")
	}
	return operationID + "-terminal", nil
}

type idempotentOwnerRecorder struct {
	commits        map[string]bool
	receipts       map[string]bool
	commitFailures int
	commitAttempts int
}

func (r *idempotentOwnerRecorder) PublishCommit(operationID, reservationDigest, capacityLeaseDigest string) (string, error) {
	r.commitAttempts++
	if r.commitFailures > 0 {
		r.commitFailures--
		return "", errors.New("owner commit failed")
	}
	if reservationDigest == "" || capacityLeaseDigest == "" {
		return "", errors.New("missing mutation binding")
	}
	if r.commits == nil {
		r.commits = make(map[string]bool)
	}
	r.commits[operationID] = true
	return operationID + "-commit", nil
}

func TestRefreshRetainsSelectionWhenOwnerCommitFails(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	refreshRecorder := &recoveringRefreshRecorder{}
	ownerRecorder := &idempotentOwnerRecorder{commitFailures: 1}
	coordinator.RefreshMutations = refreshRecorder
	coordinator.CredentialOwner = ownerRecorder
	coordinator.RefreshExchange = successfulRefresh
	if _, err := coordinator.Refresh(context.Background(), ref, revision); err == nil {
		t.Fatal("owner commit failure was not returned")
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); err != nil {
		t.Fatalf("retained owner commit recovery: %v", err)
	}
	if len(refreshRecorder.selected) != 1 || ownerRecorder.commitAttempts != 2 || len(ownerRecorder.commits) != 1 {
		t.Fatalf("selection replayed or owner recovery diverged: selected=%v attempts=%d commits=%v", refreshRecorder.selected, ownerRecorder.commitAttempts, ownerRecorder.commits)
	}
}

func TestRefreshRestartRecoversDurablePostSelectionContinuation(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	mutationBackend := newMemoryCredentialAuthorityBackend()
	ownerBackend := newMemoryCredentialAuthorityBackendSharing(mutationBackend)
	crash := errors.New("crash after selection")
	coordinator, ref, revision := newRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, func(phase string) error {
		if phase == "selected_anchor_durable" {
			return crash
		}
		return nil
	}, nil)
	coordinator.RefreshExchange = successfulRefresh
	if _, err := coordinator.Refresh(context.Background(), ref, revision); !errors.Is(err, crash) {
		t.Fatalf("Refresh error = %v, want selection crash", err)
	}
	closeAuthorityCoordinator(t, coordinator)

	restarted := openRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, nil, nil)
	exchanges := 0
	restarted.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
		exchanges++
		return successfulRefresh(ctx, token)
	}
	if _, err := restarted.Refresh(context.Background(), ref, revision); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("restart exchanges = %d, want 1", exchanges)
	}
}

func TestRefreshRestartAdoptsOwnerContinuationBeforeAnchor(t *testing.T) {
	for _, crashPhase := range []string{"continuation_durable", "selected_object_durable"} {
		t.Run(crashPhase, func(t *testing.T) {
			filesystem := fsutil.NewMemFS()
			mutationBackend := newMemoryCredentialAuthorityBackend()
			ownerBackend := newMemoryCredentialAuthorityBackendSharing(mutationBackend)
			crash := errors.New("owner publication crash")
			coordinator, ref, revision := newRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, nil, func(phase string) error {
				if phase == crashPhase {
					return crash
				}
				return nil
			})
			coordinator.RefreshExchange = successfulRefresh
			if _, err := coordinator.Refresh(context.Background(), ref, revision); !errors.Is(err, crash) {
				t.Fatalf("Refresh error = %v, want owner crash", err)
			}
			closeAuthorityCoordinator(t, coordinator)
			restarted := openRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, nil, nil)
			exchanges := 0
			restarted.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
				exchanges++
				return successfulRefresh(ctx, token)
			}
			if _, err := restarted.Refresh(context.Background(), ref, revision); err != nil {
				t.Fatalf("restart continuation adoption: %v", err)
			}
			if exchanges != 1 {
				t.Fatalf("restart exchanges = %d, want 1", exchanges)
			}
		})
	}
}

func TestRefreshRestartDoesNotReplayAttemptWithoutDurableResult(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	mutationBackend := newMemoryCredentialAuthorityBackend()
	ownerBackend := newMemoryCredentialAuthorityBackendSharing(mutationBackend)
	crash := errors.New("crash after invocation attempt")
	coordinator, ref, revision := newRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, nil, func(phase string) error {
		if phase == "refresh_attempt_durable" {
			return crash
		}
		return nil
	})
	exchanges := 0
	coordinator.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
		exchanges++
		return successfulRefresh(ctx, token)
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); !errors.Is(err, crash) {
		t.Fatalf("Refresh error = %v, want attempt crash", err)
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	closeAuthorityCoordinator(t, coordinator)
	restarted := openRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, nil, nil)
	restarted.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		exchanges++
		return nil, errors.New("exchange replayed")
	}
	if _, err := restarted.Refresh(context.Background(), ref, loaded.Metadata.Revision); err == nil || err.Error() != "Codex refresh outcome indeterminate" {
		t.Fatalf("restart error = %v, want indeterminate", err)
	}
	if exchanges != 0 {
		t.Fatalf("exchange calls = %d, want 0", exchanges)
	}
}

func TestRefreshRestartDoesNotReplayDurablePostExchangeResult(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	mutationBackend := newMemoryCredentialAuthorityBackend()
	ownerBackend := newMemoryCredentialAuthorityBackendSharing(mutationBackend)
	crash := errors.New("crash after exchange completion")
	coordinator, ref, revision := newRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, nil, func(phase string) error {
		if phase == "refresh_result_durable" {
			return crash
		}
		return nil
	})
	exchanges := 0
	coordinator.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
		exchanges++
		return successfulRefresh(ctx, token)
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); !errors.Is(err, crash) {
		t.Fatalf("Refresh error = %v, want terminal crash", err)
	}
	mutationBackend.mu.Lock()
	for name, entry := range mutationBackend.entries {
		if bytes.Contains(entry.body, []byte("refreshed-access")) || bytes.Contains(entry.body, []byte("refreshed-refresh")) {
			mutationBackend.mu.Unlock()
			t.Fatalf("credential result persisted in plaintext in %q", name)
		}
	}
	mutationBackend.mu.Unlock()
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	closeAuthorityCoordinator(t, coordinator)

	restarted := openRestartableAuthorityCoordinator(t, filesystem, mutationBackend, ownerBackend, nil, nil)
	restarted.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		exchanges++
		return nil, errors.New("post-exchange effect replayed")
	}
	if _, err := restarted.Refresh(context.Background(), ref, loaded.Metadata.Revision); err != nil {
		t.Fatalf("restart terminal recovery: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("exchange replayed after restart: %d calls", exchanges)
	}
}

func (r *idempotentOwnerRecorder) PublishReceipt(operationID, commitDigest string) (string, error) {
	if !r.commits[operationID] {
		return "", errors.New("unknown owner commit")
	}
	if commitDigest != operationID+"-commit" {
		return "", errors.New("owner commit binding mismatch")
	}
	if r.receipts == nil {
		r.receipts = make(map[string]bool)
	}
	r.receipts[operationID] = true
	return operationID + "-receipt", nil
}

func (r orderedCredentialOwnerRecorder) PublishReceipt(_ string, commitDigest string) (string, error) {
	*r.events = append(*r.events, "owner-receipt")
	if commitDigest == "" {
		return "", errors.New("missing owner commit digest")
	}
	return "owner-receipt-digest", nil
}

func TestRefreshMutationReservationPrecedesCredentialEffect(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	events := []string{}
	coordinator.RefreshMutations = orderedRefreshMutationRecorder{events: &events}
	coordinator.CredentialOwner = orderedCredentialOwnerRecorder{events: &events}
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		events = append(events, "exchange")
		return successfulRefresh(context.Background(), "")
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); err != nil {
		t.Fatal(err)
	}
	want := []string{"reservation", "owner-commit", "exchange", "owner-receipt", "reservation-receipt"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRefreshMutationReservationFailurePreventsEffect(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	wantErr := errors.New("capacity refused")
	events := []string{}
	coordinator.RefreshMutations = orderedRefreshMutationRecorder{events: &events, err: wantErr}
	coordinator.CredentialOwner = orderedCredentialOwnerRecorder{events: &events}
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		events = append(events, "exchange")
		return successfulRefresh(context.Background(), "")
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); !errors.Is(err, wantErr) {
		t.Fatalf("Refresh error = %v, want %v", err, wantErr)
	}
	if !slices.Equal(events, []string{"reservation"}) {
		t.Fatalf("events = %v, want reservation only", events)
	}
}

func TestRefreshRetainedRecoveryTerminalisesOriginalReservation(t *testing.T) {
	coordinator, fs, ref, revision := testRefreshRecord(t)
	refreshRecorder := &recoveringRefreshRecorder{completeFailures: 1}
	ownerRecorder := &idempotentOwnerRecorder{}
	coordinator.RefreshMutations = refreshRecorder
	coordinator.CredentialOwner = ownerRecorder
	coordinator.RefreshExchange = successfulRefresh
	fs.renameCount = 0
	fs.failRenameAt = 2
	if _, err := coordinator.Refresh(context.Background(), ref, revision); err == nil {
		t.Fatal("Refresh error = nil, want final commit failure")
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	fs.failRenameAt = 0
	if _, err := coordinator.Refresh(context.Background(), ref, loaded.Metadata.Revision); err == nil {
		t.Fatal("retained recovery succeeded despite terminal publication failure")
	}
	loaded, err = coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Refresh(context.Background(), ref, loaded.Metadata.Revision); err != nil {
		t.Fatalf("second retained recovery: %v", err)
	}
	if refreshRecorder.completions != 2 || len(ownerRecorder.commits) != 1 || len(ownerRecorder.receipts) != 1 {
		t.Fatalf("recovery state: completions=%d commits=%v receipts=%v", refreshRecorder.completions, ownerRecorder.commits, ownerRecorder.receipts)
	}
}

func TestRefreshEqualSystemTokenPermanentlyExportsLineage(t *testing.T) {
	coordinator, fs, ref, revision := testRefreshRecord(t)
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	fs.files["/fake/home/.codex/auth.json"] = codexAuthWithRefresh(record.Credential.AccessToken, record.Credential.AccountID, record.Credential.IDToken, record.Credential.RefreshToken)
	fs.modes["/fake/home/.codex/auth.json"] = 0o600
	var exchanges atomic.Int32
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		exchanges.Add(1)
		return nil, errors.New("unexpected")
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); !errors.Is(err, ErrRefreshIneligible) {
		t.Fatalf("Refresh error = %v, want ineligible", err)
	}
	if exchanges.Load() != 0 {
		t.Fatalf("exchange count = %d, want 0", exchanges.Load())
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.RefreshOwnership != RefreshExportedToSystem {
		t.Fatalf("ownership = %q", loaded.Metadata.RefreshOwnership)
	}
	fs.files["/fake/home/.codex/auth.json"] = codexAuthWithRefresh("other", "other", fakeCodexJWT("other@test.invalid", "other", "other", "plus"), "different")
	if _, err := coordinator.Refresh(context.Background(), ref, loaded.Metadata.Revision); !errors.Is(err, ErrRefreshIneligible) {
		t.Fatalf("second Refresh error = %v, want permanently ineligible", err)
	}
}

func TestRefreshUnequalSystemTokenKeepsIndependentOwnership(t *testing.T) {
	coordinator, fs, ref, revision := testRefreshRecord(t)
	systemPath := "/fake/home/.codex/auth.json"
	systemBefore := codexAuthWithRefresh("other", "other", fakeCodexJWT("other@test.invalid", "other", "other", "plus"), "different")
	fs.files[systemPath] = systemBefore
	fs.modes["/fake/home/.codex/auth.json"] = 0o600
	coordinator.RefreshExchange = successfulRefresh
	if _, err := coordinator.Refresh(context.Background(), ref, revision); err != nil {
		t.Fatal(err)
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.RefreshOwnership != RefreshCQOwnedNeverExported {
		t.Fatalf("ownership = %q", loaded.Metadata.RefreshOwnership)
	}
	if got := string(fs.files[systemPath]); got != string(systemBefore) {
		t.Fatal("managed refresh rewrote system auth")
	}
}

func TestCredentialCoordinatorRefreshReplacesHigherStoredExpiry(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	record.Document["cq_expires_at"] = int64(9_999_999_999_999)
	if err := coordinator.Store.Commit(&record, revision); err != nil {
		t.Fatal(err)
	}

	coordinator.RefreshExchange = successfulRefresh
	if _, err := coordinator.Refresh(context.Background(), ref, record.Metadata.Revision); err != nil {
		t.Fatal(err)
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	const wantExpiresAt = int64(4_600_000)
	got, ok := loaded.Document["cq_expires_at"].(float64)
	if !ok || int64(got) != wantExpiresAt {
		t.Fatalf("cq_expires_at = %#v, want %d", loaded.Document["cq_expires_at"], wantExpiresAt)
	}
}

func TestRefreshConcurrentRequestsExchangeOnce(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var exchanges atomic.Int32
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		if exchanges.Add(1) == 1 {
			close(started)
		}
		<-release
		return successfulRefresh(context.Background(), "")
	}
	type outcome struct {
		result RefreshResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	go func() {
		result, err := coordinator.Refresh(context.Background(), ref, revision)
		outcomes <- outcome{result: result, err: err}
	}()
	<-started
	go func() {
		result, err := coordinator.Refresh(context.Background(), ref, revision)
		outcomes <- outcome{result: result, err: err}
	}()
	waitForRefreshWaiter(t, coordinator, ref, revision)
	close(release)
	first, second := <-outcomes, <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("errors = %v, %v", first.err, second.err)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("exchange count = %d, want 1", exchanges.Load())
	}
	if first.result.Revision != second.result.Revision {
		t.Fatalf("revisions = %q, %q", first.result.Revision, second.result.Revision)
	}
}

func waitForRefreshWaiter(t *testing.T, coordinator *CredentialCoordinator, ref CandidateRef, revision Revision) {
	t.Helper()
	key := string(ref.CandidateID) + ":" + string(revision)
	deadline := time.Now().Add(time.Second)
	for {
		coordinator.refreshMu.Lock()
		flight := coordinator.refreshFlights[key]
		joined := flight != nil && flight.waiters > 0
		coordinator.refreshMu.Unlock()
		if joined {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("second refresh did not join in-flight exchange")
		}
		runtime.Gosched()
	}
}

func TestRefreshAndActivateHaveOneRevisionFencedOutcome(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	started := make(chan struct{})
	release := make(chan struct{})
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		close(started)
		<-release
		return successfulRefresh(context.Background(), "")
	}
	refreshErr := make(chan error, 1)
	activateErr := make(chan error, 1)
	go func() {
		_, err := coordinator.Refresh(context.Background(), ref, revision)
		refreshErr <- err
	}()
	<-started
	go func() {
		_, err := coordinator.Activate(context.Background(), ref, revision)
		activateErr <- err
	}()
	close(release)
	if err := <-refreshErr; err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := <-activateErr; !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Activate error = %v, want stale revision", err)
	}
}

func TestRefreshAndRemoveHaveOneRevisionFencedOutcome(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	started := make(chan struct{})
	release := make(chan struct{})
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		close(started)
		<-release
		return successfulRefresh(context.Background(), "")
	}
	refreshErr := make(chan error, 1)
	removeErr := make(chan error, 1)
	go func() {
		_, err := coordinator.Refresh(context.Background(), ref, revision)
		refreshErr <- err
	}()
	<-started
	go func() {
		_, err := coordinator.RemoveManaged(context.Background(), ref.AccountKey, RevisionSet{ref.CandidateID: revision}, false)
		removeErr <- err
	}()
	close(release)
	if err := <-refreshErr; err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := <-removeErr; !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Remove error = %v, want stale revision", err)
	}
}

func TestRefreshUncertainFailureBlocksOldCredentialAndRestart(t *testing.T) {
	coordinator, fs, ref, revision := testRefreshRecord(t)
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		return nil, os.ErrDeadlineExceeded
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Refresh error = %v", err)
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.OperationState != OperationRotationUncertain {
		t.Fatalf("state = %q", loaded.Metadata.OperationState)
	}
	inventory := DiscoverInventory(fs)
	if len(inventory.Accounts) != 1 || len(ResolveCandidate(inventory.Accounts[0], "", time.Now())) != 0 {
		t.Fatal("uncertain credential remained dispatchable")
	}
	restarted, err := NewCredentialCoordinator(coordinator.Store, coordinator.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	restarted.RefreshExchange = successfulRefresh
	if _, err := restarted.Refresh(context.Background(), ref, loaded.Metadata.Revision); !errors.Is(err, ErrRefreshIneligible) {
		t.Fatalf("restart Refresh error = %v, want ineligible", err)
	}
}

func TestRefreshFinalCommitFailureRetriesOnlyRetainedResponse(t *testing.T) {
	coordinator, fs, ref, revision := testRefreshRecord(t)
	coordinator.RefreshExchange = successfulRefresh
	fs.renameCount = 0
	fs.failRenameAt = 2
	if _, err := coordinator.Refresh(context.Background(), ref, revision); err == nil {
		t.Fatal("Refresh error = nil, want final commit failure")
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.OperationState != OperationRotationUncertain {
		t.Fatalf("state = %q, want uncertain", loaded.Metadata.OperationState)
	}
	fs.failRenameAt = 0
	result, err := coordinator.Refresh(context.Background(), ref, loaded.Metadata.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if result.Material.AccessToken != "refreshed-access" {
		t.Fatalf("access token was not retained response")
	}
	final, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if final.Metadata.OperationState != OperationReady || final.Metadata.OperationID != "" {
		t.Fatalf("final metadata = %+v", final.Metadata)
	}
}

func TestDefinitiveRefreshRejectionReturnsLineageReady(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		return nil, &auth.CodexTokenHTTPError{StatusCode: 400}
	}
	if _, err := coordinator.Refresh(context.Background(), ref, revision); err == nil {
		t.Fatal("Refresh error = nil")
	}
	loaded, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.OperationState != OperationReady || loaded.Metadata.OperationID != "" {
		t.Fatalf("metadata = %+v", loaded.Metadata)
	}
}

func TestNewLoginAfterActivationCreatesIndependentLineage(t *testing.T) {
	coordinator, fs, ref, revision := testRefreshRecord(t)
	before, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Activate(context.Background(), ref, revision); err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("refresh@test.invalid", "acct-refresh", "user-refresh", "plus")
	credential.Claims = auth.DecodeCodexClaims(credential.Tokens.IDToken)
	credential.Tokens.RefreshToken = "new-login-refresh"
	refreshedRef, _, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	after, err := coordinator.loadRef(refreshedRef)
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata.LineageID == before.Metadata.LineageID {
		t.Fatal("new login reused exported lineage")
	}
	if after.Metadata.RefreshOwnership != RefreshCQOwnedNeverExported {
		t.Fatalf("new lineage ownership = %q", after.Metadata.RefreshOwnership)
	}
}

func successfulRefresh(context.Context, string) (*auth.CodexTokenResponse, error) {
	return &auth.CodexTokenResponse{
		AccessToken: "refreshed-access", RefreshToken: "refreshed-refresh",
		IDToken:   fakeCodexJWT("refresh@test.invalid", "acct-refresh", "user-refresh", "plus"),
		ExpiresIn: 3600,
	}, nil
}

func codexAuthWithRefresh(access, accountID, idToken, refresh string) []byte {
	doc := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": access, "refresh_token": refresh,
			"id_token": idToken, "account_id": accountID,
		},
	}
	data, _ := json.Marshal(doc)
	return data
}
