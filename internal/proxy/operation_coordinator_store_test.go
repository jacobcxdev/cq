package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCredentialAuthorityIdentityRoundTripsFullFileID(t *testing.T) {
	stable := StableObjectIdentity{File: fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 1, FileID: [16]byte{15: 9}}, Size: 3, Digest: "digest"}
	if got := stableCodexAuthorityIdentity(codexAuthorityIdentity(stable)); got != stable {
		t.Fatalf("identity round trip = %#v, want %#v", got, stable)
	}
}

func TestOperationCoordinatorOrdering(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x61}, 256)), lock)
	store, err := OpenOperationCoordinatorStore(context.Background(), filesystem, directory, publisher, bytes.Repeat([]byte{0x41}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishAnchor("op", "intent"); err == nil {
		t.Fatal("anchor accepted before intent")
	}
	if err := store.PublishIntent("op", "intent"); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishReceipt("op", "receipt"); err == nil {
		t.Fatal("receipt accepted before anchor")
	}
	if err := store.PublishAnchor("op", "intent"); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishReceipt("op", "receipt"); err != nil {
		t.Fatal(err)
	}
}

func TestOperationCoordinatorCrashAfterObjectReopensWithoutSelectingIt(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x62}, 512)), lock)
	crash := errors.New("crash after object")
	store, err := OpenOperationCoordinatorStore(context.Background(), filesystem, directory, publisher, bytes.Repeat([]byte{0x42}, 32), func(phase string) error {
		if phase == "intent_object_durable" {
			return crash
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishIntent("op", "intent"); !errors.Is(err, crash) {
		t.Fatalf("PublishIntent error = %v", err)
	}
	reopened, err := OpenOperationCoordinatorStore(context.Background(), filesystem, directory, publisher, bytes.Repeat([]byte{0x42}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.PublishAnchor("op", "intent"); err == nil {
		t.Fatal("unselected object became authority after reopen")
	}
	if err := reopened.PublishIntent("op", "intent"); err != nil {
		t.Fatalf("retry intent: %v", err)
	}
}

func TestOperationCoordinatorChildSelectionMapping(t *testing.T) {
	tests := []struct {
		domain, child, proof string
		row                  int
		pre                  int
		exclusiveLifecycle   bool
	}{
		{"feature_activation", "feature_activation", "feature_activation_anchor_absent", 1, 2, false},
		{"authority_transition", "authority_journal", "authority_journal_unselected", 2, 1, false},
		{"lifecycle_action", "lifecycle_action", "lifecycle_action_anchor_absent", 3, 1, false},
		{"staged_release", "staged_release", "staged_release_anchor_absent", 4, 1, false},
		{"import_finalisation", "import_finalisation", "import_finalisation_anchor_absent", 5, 2, false},
		{"candidate_removal", "quarantine_candidate_remove", "quarantine_candidate_remove_anchor_absent", 6, 2, true},
		{"authority_reset", "quarantine_authority_reset", "quarantine_authority_reset_anchor_absent", 7, 2, true},
	}
	for _, test := range tests {
		got, err := CoordinatorChildSelectionMapping(test.domain, test.child)
		if err != nil {
			t.Fatalf("row %d: %v", test.row, err)
		}
		if got.Row != test.row || got.ProofKind != test.proof || len(got.PreTemporaryGrammars) != test.pre {
			t.Fatalf("mapping = %#v", got)
		}
		if (got.Locks[0].Mode == "exclusive") != test.exclusiveLifecycle {
			t.Fatalf("row %d lifecycle mode = %q", test.row, got.Locks[0].Mode)
		}
	}
	if _, err := CoordinatorChildSelectionMapping("feature_activation", "authority_journal"); err == nil {
		t.Fatal("accepted mixed mapping row")
	}
}

func TestOperationCoordinatorKeyBootstrapRecovery(t *testing.T) {
	seed := bytes.Repeat([]byte{0x5a}, 32)
	first, err := BootstrapOperationCoordinatorKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := BootstrapOperationCoordinatorKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	if first != recovered || len(first.Verifier) != 32 || len(first.Fingerprint) != 64 {
		t.Fatalf("unstable bootstrap: %#v %#v", first, recovered)
	}
	if _, err := BootstrapOperationCoordinatorKey(seed[:31]); err == nil {
		t.Fatal("accepted 31-byte seed")
	}
}

func TestCredentialOwnerFilesystemBackendReopensTerminalAuthority(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	refreshMutationLock, err := AcquireSelectorCASLock(filesystem, directory, "refresh-mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer refreshMutationLock.Close()
	refreshSelectorLock, err := AcquireSelectorCASLock(filesystem, directory, "refresh-selector.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer refreshSelectorLock.Close()
	refreshBackend, err := NewCredentialAuthorityFSBackend(
		filesystem,
		directory,
		bytes.NewReader(bytes.Repeat([]byte{0x5c}, 1024)),
		refreshMutationLock,
		refreshSelectorLock,
	)
	if err != nil {
		t.Fatal(err)
	}
	mutationLock, err := AcquireSelectorCASLock(filesystem, directory, "credential-mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer mutationLock.Close()
	selectorLock, err := AcquireSelectorCASLock(filesystem, directory, "credential-selector.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer selectorLock.Close()
	backend, err := NewCredentialAuthorityFSBackend(
		filesystem,
		directory,
		bytes.NewReader(bytes.Repeat([]byte{0x6c}, 1024)),
		mutationLock,
		selectorLock,
	)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x4c}, 32)
	refreshStore, err := providerCodex.OpenRefreshMutationStore(context.Background(), refreshBackend, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := refreshStore.SelectRefreshMutation(
		"refresh-1",
		providerCodex.CandidateRef{AccountKey: "account", CandidateID: "candidate"},
		providerCodex.Revision("revision"),
		providerCodex.RefreshMutationCapacity{
			MutationChildren:        256,
			OAuthChildren:           64,
			Decisions:               64,
			OperatorSlots:           2921,
			CredentialFiles:         573,
			WireFrames:              331,
			TotalUnits:              3825,
			TotalBytes:              522141696,
			CredentialBytes:         12656640,
			OuterObjectBytes:        262144,
			FixedDeltaBytes:         32768,
			PlanDeltaBytes:          98304,
			ReauthDeltaBytes:        512,
			SelectedLeaseDeltaBytes: 62,
			TerminalLeaseDeltaBytes: 62,
			DecisionDeltaBytes:      256,
			OAuthDeltaBytes:         384,
			MutationDeltaBytes:      320,
			OuterBaseObjects:        32,
			OAuthResultObjects:      64,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store, err := providerCodex.OpenCredentialOwnerStore(context.Background(), backend, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	commitDigest, err := store.PublishCommit("refresh-1", selection.ReservationDigest, selection.CapacityLeaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	receiptDigest, err := store.PublishReceipt("refresh-1", commitDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refreshStore.CompleteRefreshMutation("refresh-1", receiptDigest); err != nil {
		t.Fatal(err)
	}
	reopened, err := providerCodex.OpenCredentialOwnerStore(context.Background(), backend, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.PublishReceipt("refresh-1", commitDigest); err != nil {
		t.Fatalf("terminal authority did not survive reopen: %v", err)
	}
}
