package proxy

import (
	"bytes"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2LoadLaneClassifiesCurrentAndReturnsLaneRecords(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)

	restored, err := coordinator.Store().LoadLane(
		LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"},
		[]codex.AccountKey{"account-one"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Classification != CodexRestoredLaneCurrent {
		t.Fatalf("classification = %q, want %q", restored.Classification, CodexRestoredLaneCurrent)
	}
	if len(restored.Records) != 4 || len(restored.ResolvedRecords) != 4 {
		t.Fatalf("matching lane records = %d/%d, want 4/4", len(restored.Records), len(restored.ResolvedRecords))
	}
	if restored.Fence.Journal != 8 || restored.Fence.Lane != 3 || restored.Fence.Current != restored.RequestedIdentity || restored.Fence.Last != restored.RequestedIdentity {
		t.Fatalf("restored fence = %#v requested=%#v", restored.Fence, restored.RequestedIdentity)
	}
	current := findCodexLeaseV2LaneTestRecord(t, restored, "current")
	if current.AccountKey != "account-one" || current.Choice.AccountKey != "account-one" || current.Choice.RequestedModel != "" || current.Choice.EffectiveModel != "gpt-effective" || !reflect.DeepEqual(current.Choice.RequiredBuckets, []CapacityBucket{CapacityBucketBase}) {
		t.Fatalf("resolved current choice = account %q choice %#v", current.AccountKey, current.Choice)
	}
	if current.Identity != restored.RequestedIdentity || current.Fence.Revision != 4 || current.Fence.Lease != 2 || current.Fence.RequestGeneration != 1 || current.Fence.CurrentAttempt != 1 || len(current.Fence.TouchedAttempts) != 1 || current.Fence.TouchedAttempts[0] != (CodexAttemptFence{RequestGeneration: 1, Generation: 1, Revision: 2}) {
		t.Fatalf("resolved current identity/fence = %#v %#v", current.Identity, current.Fence)
	}
	if current.Record.Generation != 1 || current.Record.Attempts[0].State != CodexAttemptAbandonedBeforeDispatch {
		t.Fatalf("restored bounded request = %#v", current.Record.CodexCurrentRequest)
	}
	if current.Record.SessionHash == "session" || current.Record.ThreadHash == "thread" || current.Record.TurnHash == "current" || current.Record.AccountHash == "account-one" || current.Record.PredecessorTurnHash == "predecessor" {
		t.Fatalf("restored record exposed raw identity: %#v", current.Record)
	}
}

func TestCodexLeaseV2LoadLaneClassifiesHistoryUnseenShadowAndRetainedEpochs(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	accounts := []codex.AccountKey{"account-one"}
	basePolicy := CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}}

	tests := []struct {
		name           string
		turn           string
		policy         CodexLeaseAuthorityPolicy
		classification CodexRestoredLaneClassification
		err            error
	}{
		{name: "current", turn: "current", policy: basePolicy, classification: CodexRestoredLaneCurrent},
		{name: "historical", turn: "predecessor", policy: basePolicy, classification: CodexRestoredLaneHistorical},
		{name: "unseen", turn: "unseen", policy: basePolicy, classification: CodexRestoredLaneUnseen},
		{name: "shadow only", turn: "shadow", policy: basePolicy, classification: CodexRestoredLaneShadowOnly},
		{name: "retained authoritative epoch", turn: "retained", policy: basePolicy, classification: CodexRestoredLaneHistorical},
		{name: "unrecognised authoritative epoch", turn: "retained", policy: CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true}, classification: CodexRestoredLaneRecoveryBlocked, err: ErrCodexLeaseAuthorityMismatch},
		{name: "observe retains current authority", turn: "current", policy: CodexLeaseAuthorityPolicy{ModeEpoch: 10, RetainedAuthoritativeEpochs: []uint64{8, 9}}, classification: CodexRestoredLaneCurrent},
		{name: "observe retains historical authority", turn: "predecessor", policy: CodexLeaseAuthorityPolicy{ModeEpoch: 10, RetainedAuthoritativeEpochs: []uint64{8, 9}}, classification: CodexRestoredLaneHistorical},
		{name: "observe rejects unlisted authority", turn: "current", policy: CodexLeaseAuthorityPolicy{ModeEpoch: 10, RetainedAuthoritativeEpochs: []uint64{8}}, classification: CodexRestoredLaneRecoveryBlocked, err: ErrCodexLeaseAuthorityMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restored, err := coordinator.Store().LoadLane(LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: test.turn}, accounts, test.policy)
			if !errors.Is(err, test.err) {
				t.Fatalf("LoadLane error = %T %v, want %v", err, err, test.err)
			}
			if restored.Classification != test.classification {
				t.Fatalf("classification = %q, want %q", restored.Classification, test.classification)
			}
			if test.err == nil && len(restored.Records) != 4 {
				t.Fatalf("matching records = %d, want 4", len(restored.Records))
			}
		})
	}
}

func TestCodexLeaseV2LoadLaneAllowsDefinitelyAbandonedUnadmittedAccountToBeUnresolvedAndReturnsDetachedCopies(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	key := LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"}
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}}

	unresolved, err := coordinator.Store().LoadLane(key, nil, policy)
	if err != nil || unresolved.Classification != CodexRestoredLaneCurrent {
		t.Fatalf("definitely abandoned unresolved route = (%#v, %T %v)", unresolved, err, err)
	}
	unresolvedCurrent := findCodexLeaseV2LaneTestRecord(t, unresolved, "current")
	if unresolvedCurrent.AccountKey != "" || !reflect.DeepEqual(unresolvedCurrent.Choice, RouteChoice{}) || unresolvedCurrent.Record.EverAdmitted || unresolvedCurrent.Record.NonMigratable || unresolvedCurrent.Record.State != LeaseProvisional || codexLeaseCurrentAttemptState(unresolvedCurrent.Record) != CodexAttemptAbandonedBeforeDispatch {
		t.Fatalf("definitely abandoned unresolved record = %#v", unresolvedCurrent)
	}

	restored, err := coordinator.Store().LoadLane(key, []codex.AccountKey{"account-one"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	current := findCodexLeaseV2LaneTestRecord(t, restored, "current")
	storeBefore := cloneCodexLeaseV2Envelope(*coordinator.Store().v2)
	originalSlotAccount := current.Record.AttemptEnvelope.Slots[0].AccountHash
	originalBucket := current.Record.RequiredBuckets[0]
	originalAttemptState := current.Record.Attempts[0].State
	originalAttemptRevision := current.Fence.TouchedAttempts[0].Revision
	current.Record.AttemptEnvelope.Slots[0].AccountHash = "changed"
	current.Record.RequiredBuckets[0] = "changed"
	current.Record.Attempts[0].State = CodexAttemptStreaming
	current.Choice.RequiredBuckets[0] = "changed"
	current.Fence.TouchedAttempts[0].Revision = 99
	for index := range restored.Records {
		if restored.Records[index].Identity() != current.Identity {
			continue
		}
		restored.Records[index].AttemptEnvelope.Slots[0].AccountHash = "changed-again"
		restored.Records[index].RequiredBuckets[0] = "changed-again"
		restored.Records[index].Attempts[0].State = CodexAttemptDispatched
	}
	restored.Fence.TouchedRecords[0].Revision = 99
	if !reflect.DeepEqual(*coordinator.Store().v2, storeBefore) {
		t.Fatal("mutating restored lane changed in-memory store state")
	}

	again, err := coordinator.Store().LoadLane(key, []codex.AccountKey{"account-one"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	againCurrent := findCodexLeaseV2LaneTestRecord(t, again, "current")
	if againCurrent.Record.AttemptEnvelope.Slots[0].AccountHash != originalSlotAccount || againCurrent.Record.RequiredBuckets[0] != originalBucket || againCurrent.Record.Attempts[0].State != originalAttemptState || againCurrent.Choice.RequiredBuckets[0] != originalBucket || againCurrent.Fence.TouchedAttempts[0].Revision != originalAttemptRevision || again.Fence.TouchedRecords[0].Revision == 99 {
		t.Fatal("mutating restored lane aliased store state")
	}
}

func TestCodexLeaseV2LoadLaneCanReselectAfterDefinitelyAbandonedUnadmittedAccountDisappears(t *testing.T) {
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	key := LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"}
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}}

	restored, err := store.LoadLane(key, []codex.AccountKey{"replacement-account"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	current := findCodexLeaseV2LaneTestRecord(t, restored, "current")
	if current.AccountKey != "" || codexLeaseCurrentAttemptState(current.Record) != CodexAttemptAbandonedBeforeDispatch || current.Record.EverAdmitted || current.Record.NonMigratable {
		t.Fatalf("reselection source = %#v", current)
	}

	mutation := nextCodexLeaseV2CASTestRequestMutation(store, current.Record, "replacement-request")
	mutation.State = LeaseProvisional
	mutation.AccountHash = store.hash("account", "replacement-account")
	mutation.AttemptEnvelope.Slots[0].AccountHash = mutation.AccountHash
	mutation.AttemptEnvelope.PlanDigest = codexLeaseAttemptPlanDigest(store.key, mutation.AttemptEnvelope.Slots)
	predecessor := CodexJournalRecordIdentity{
		LaneDigest:    current.Identity.LaneDigest,
		TurnDigest:    current.Record.PredecessorTurnHash,
		ModeEpoch:     current.Record.PredecessorModeEpoch,
		Authoritative: current.Record.PredecessorAuthoritative,
	}
	fence, err := restored.MutationFence(current.Identity, predecessor)
	if err != nil {
		t.Fatal(err)
	}
	for index := range fence.TouchedRecords {
		if fence.TouchedRecords[index].Record == current.Identity {
			fence.TouchedRecords[index].TouchedAttempts = []CodexAttemptFence{{RequestGeneration: 0, Generation: 0}}
		}
	}
	identity := current.Identity
	if _, err := store.CommitLane(fence, CodexLaneMutation{BeginRequest: &identity, UpsertRecords: []CodexJournalRecordV2{mutation}}); err != nil {
		t.Fatal(err)
	}

	next := findCodexLeaseV2LaneTestRecord(t, mustLoadCodexLeaseV2LaneTest(t, store, key, []codex.AccountKey{"replacement-account"}, policy), "current")
	if next.AccountKey != "replacement-account" || next.Record.Generation != current.Record.Generation+1 || next.Record.State != LeaseProvisional || next.Record.NonMigratable || next.Record.EverAdmitted || codexLeaseCurrentAttemptState(next.Record) != CodexAttemptPrepared {
		t.Fatalf("reselected request = %#v", next)
	}
}

func mustLoadCodexLeaseV2LaneTest(t *testing.T, store *CodexLeaseStore, key LeaseKey, accounts []codex.AccountKey, policy CodexLeaseAuthorityPolicy) CodexRestoredLane {
	t.Helper()
	restored, err := store.LoadLane(key, accounts, policy)
	if err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestCloneCodexRestoredLaneDetachesEveryBoundedCurrentRequest(t *testing.T) {
	t.Parallel()
	request := CodexCurrentRequest{
		Generation:      2,
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
		AttemptEnvelope: CodexAttemptEnvelope{Slots: []CodexAttemptSlot{{Index: 1, AccountHash: "account"}}},
		Attempts:        []CodexJournalAttempt{{Generation: 1, Revision: 1, State: CodexAttemptPrepared}},
	}
	restored := CodexRestoredLane{
		RequestedRecord: CodexJournalRecordV2{CodexCurrentRequest: cloneCodexCurrentRequest(request)},
		Records:         []CodexJournalRecordV2{{CodexCurrentRequest: cloneCodexCurrentRequest(request)}},
		ResolvedRecords: []CodexRestoredRecord{{Record: CodexJournalRecordV2{CodexCurrentRequest: cloneCodexCurrentRequest(request)}}},
	}

	clone := cloneCodexRestoredLane(restored)
	clone.RequestedRecord.RequiredBuckets[0] = "requested-changed"
	clone.RequestedRecord.AttemptEnvelope.Slots[0].AccountHash = "requested-changed"
	clone.RequestedRecord.Attempts[0].State = CodexAttemptStreaming
	clone.Records[0].RequiredBuckets[0] = "record-changed"
	clone.ResolvedRecords[0].Record.RequiredBuckets[0] = "resolved-changed"

	if restored.RequestedRecord.RequiredBuckets[0] != CapacityBucketBase || restored.RequestedRecord.AttemptEnvelope.Slots[0].AccountHash != "account" || restored.RequestedRecord.Attempts[0].State != CodexAttemptPrepared {
		t.Fatal("requested record bounded request aliases its clone")
	}
	if restored.Records[0].RequiredBuckets[0] != CapacityBucketBase || restored.ResolvedRecords[0].Record.RequiredBuckets[0] != CapacityBucketBase {
		t.Fatal("hydrated record bounded request aliases its clone")
	}
}

func TestCodexLeaseV2LoadLaneRequiresHealthyGuardedStoreWithoutWriting(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	key := LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"}
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()

	beginErr := errors.New("owner revoked")
	store.mu.Lock()
	originalOwner := store.owner
	store.owner = &cutoverTestOwner{beginErr: beginErr}
	store.mu.Unlock()
	restored, err := store.LoadLane(key, []codex.AccountKey{"account-one"}, policy)
	if !errors.Is(err, ErrCodexLeaseWriterUnavailable) || !errors.Is(err, beginErr) || restored.Classification != CodexRestoredLaneRecoveryBlocked {
		t.Fatalf("revoked-owner LoadLane = (%#v, %T %v)", restored, err, err)
	}
	store.mu.Lock()
	store.owner = originalOwner
	store.mu.Unlock()
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) {
		t.Fatal("rejected owner changed lease authority")
	}

	store.mu.Lock()
	store.poisoned = errors.New("indeterminate commit")
	store.mu.Unlock()
	restored, err = store.LoadLane(key, []codex.AccountKey{"account-one"}, policy)
	if !errors.Is(err, ErrCodexLeaseStorePoisoned) || restored.Classification != CodexRestoredLaneRecoveryBlocked {
		t.Fatalf("poisoned LoadLane = (%#v, %T %v)", restored, err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) {
		t.Fatal("poisoned read changed lease authority")
	}
}

func TestCodexLeaseV2LoadLaneSeparatesLanesAndValidatesAuthorityPolicy(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()

	other, err := store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "other-session", Thread: "other-thread", Namespace: CodexResponsesNamespace}, Turn: "other-turn"},
		nil,
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if other.Classification != CodexRestoredLaneHistorical || len(other.Records) != 1 || other.Lane.SessionHash == "" {
		t.Fatalf("other lane = %#v", other)
	}

	absent, err := store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "absent-session", Thread: "absent-thread", Namespace: CodexResponsesNamespace}, Turn: "absent-turn"},
		nil,
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if absent.Classification != CodexRestoredLaneUnseen || len(absent.Records) != 0 || absent.Fence.Journal != store.Generation() || absent.Fence.Lane != 0 || len(absent.Fence.TouchedRecords) != 1 || absent.Fence.TouchedRecords[0].Record != absent.RequestedIdentity {
		t.Fatalf("absent lane = %#v", absent)
	}

	key := LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"}
	for _, policy := range []CodexLeaseAuthorityPolicy{
		{},
		{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8, 8}},
		{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8, 7}},
		{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{9}},
		{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{7}},
	} {
		restored, loadErr := store.LoadLane(key, []codex.AccountKey{"account-one"}, policy)
		if !errors.Is(loadErr, ErrCodexLeaseAuthorityMismatch) || restored.Classification != CodexRestoredLaneRecoveryBlocked {
			t.Fatalf("invalid policy %#v = (%#v, %T %v)", policy, restored, loadErr, loadErr)
		}
	}
}

func TestCodexLeaseV2LoadLaneBuildsExactSubsetFenceForCommit(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	store := coordinator.Store()
	restored, err := store.LoadLane(
		LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "current"},
		[]codex.AccountKey{"account-one"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{8}},
	)
	if err != nil {
		t.Fatal(err)
	}
	current := findCodexLeaseV2LaneTestRecord(t, restored, "current")
	predecessor := findCodexLeaseV2LaneTestRecord(t, restored, "predecessor")
	fence, err := restored.MutationFence(current.Identity, predecessor.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(fence.TouchedRecords) != 2 {
		t.Fatalf("subset fences = %d, want 2", len(fence.TouchedRecords))
	}
	mutationRecord := codexLeaseV2CASTestMutationRecord(current.Record)
	mutationRecord.NonMigratable = true
	post, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutationRecord}})
	if err != nil {
		t.Fatal(err)
	}
	if post.Journal != fence.Journal+1 {
		t.Fatalf("post journal generation = %d, want %d", post.Journal, fence.Journal+1)
	}
	if len(store.v2.Records) != 5 || !findCodexLeaseV2LaneTestStoredRecord(t, store.v2.Records, current.Identity).NonMigratable {
		t.Fatal("subset mutation erased unrelated history or failed to update current")
	}
	beforeStale := append([]byte(nil), store.journalBytes...)
	if _, err := store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{mutationRecord}}); !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale subset mutation error = %T %v", err, err)
	}
	if !bytes.Equal(beforeStale, store.journalBytes) {
		t.Fatal("stale subset mutation changed durable bytes")
	}
	if _, err := restored.MutationFence(CodexJournalRecordIdentity{TurnDigest: "outside"}); !errors.Is(err, ErrCodexLeaseInvalidMutation) {
		t.Fatalf("outside subset error = %T %v", err, err)
	}
}

func findCodexLeaseV2LaneTestStoredRecord(t *testing.T, records []CodexJournalRecordV2, identity CodexJournalRecordIdentity) CodexJournalRecordV2 {
	t.Helper()
	for _, record := range records {
		if record.Identity() == identity {
			return record
		}
	}
	t.Fatalf("stored record %#v not found", identity)
	return CodexJournalRecordV2{}
}

func TestCodexLeaseV2LoadLaneDurableBytesContainNoRawIdentifiers(t *testing.T) {
	t.Parallel()
	coordinator := openCodexLeaseV2LaneTestCoordinator(t)
	for _, raw := range []string{"session", "thread", "current", "predecessor", "shadow", "retained", "account-one", "candidate-one", "gpt-requested"} {
		if bytes.Contains(coordinator.Store().journalBytes, []byte(strconv.Quote(raw))) {
			t.Fatalf("durable v2 journal contains raw fixture value %q", raw)
		}
	}
}

func findCodexLeaseV2LaneTestRecord(t *testing.T, restored CodexRestoredLane, rawTurn string) CodexRestoredRecord {
	t.Helper()
	for _, record := range restored.ResolvedRecords {
		if constantTimeCodexLeaseDigestEqual(record.Record.TurnHash, restoredRecordHashForLaneTest(rawTurn)) {
			return record
		}
	}
	t.Fatalf("restored record %q not found", rawTurn)
	return CodexRestoredRecord{}
}

func restoredRecordHashForLaneTest(raw string) string {
	store := &CodexLeaseStore{key: bytes.Repeat([]byte{0x35}, codexLeaseHMACKeyBytes)}
	return store.hash("turn", raw)
}

func openCodexLeaseV2LaneTestCoordinator(t *testing.T) *CodexContinuityCoordinator {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x35}, codexLeaseHMACKeyBytes)
	if err := fsys.WriteFile("/state/leases.key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &CodexLeaseStore{key: append([]byte(nil), key...)}
	cutoverAt := time.Date(2026, 8, 9, 7, 0, 0, 0, time.UTC)
	createdAt := cutoverAt.Add(time.Minute)
	observedAt := createdAt.Add(time.Minute)
	sessionHash := store.hash("session", "session")
	threadHash := store.hash("thread", "thread")
	namespaceHash := store.hash("namespace", CodexResponsesNamespace)
	predecessorHash := store.hash("turn", "predecessor")
	currentHash := store.hash("turn", "current")
	shadowHash := store.hash("turn", "shadow")
	retainedHash := store.hash("turn", "retained")
	otherSessionHash := store.hash("session", "other-session")
	otherThreadHash := store.hash("thread", "other-thread")
	otherTurnHash := store.hash("turn", "other-turn")
	accountHash := store.hash("account", "account-one")
	slots := []CodexAttemptSlot{{
		Index:         1,
		AccountHash:   accountHash,
		CandidateHash: store.hash("candidate", "candidate-one"),
		Kind:          CodexAttemptSlotDirect,
	}}
	envelope := codexLeaseJournalEnvelopeV2{
		Version:     codexLeaseJournalVersionV2,
		HashVersion: codexLeaseHashVersion,
		Generation:  7,
		Cutover: CodexLeaseCutover{
			SourceVersion:        0,
			CompatibilityEpoch:   3,
			State:                CodexLeaseCutoverComplete,
			At:                   cutoverAt,
			JournalGeneration:    1,
			CompletedAt:          cutoverAt,
			CompletionGeneration: 1,
			NoLegacyAuthority:    true,
		},
		Lanes: []CodexJournalLane{
			{
				SessionHash:          sessionHash,
				ThreadHash:           threadHash,
				NamespaceHash:        namespaceHash,
				Generation:           3,
				CurrentTurnHash:      currentHash,
				CurrentModeEpoch:     9,
				CurrentAuthoritative: true,
				LastTurnHash:         currentHash,
				LastModeEpoch:        9,
				LastAuthoritative:    true,
				LastObservedAt:       observedAt,
			},
			{
				SessionHash:       otherSessionHash,
				ThreadHash:        otherThreadHash,
				NamespaceHash:     namespaceHash,
				Generation:        1,
				LastTurnHash:      otherTurnHash,
				LastModeEpoch:     9,
				LastAuthoritative: true,
				LastObservedAt:    observedAt,
			},
		},
		Records: []CodexJournalRecordV2{
			{
				SessionHash:          sessionHash,
				ThreadHash:           threadHash,
				NamespaceHash:        namespaceHash,
				TurnHash:             predecessorHash,
				RecordGeneration:     2,
				LaneGeneration:       2,
				LeaseGeneration:      2,
				ModeEpoch:            9,
				State:                LeaseFailedUnadmitted,
				ProtocolSchema:       CurrentCodexLeaseSchema,
				Authoritative:        true,
				SocketLineageExtinct: true,
				CreatedAt:            createdAt,
				LastObservedAt:       observedAt,
			},
			{
				SessionHash:              sessionHash,
				ThreadHash:               threadHash,
				NamespaceHash:            namespaceHash,
				TurnHash:                 currentHash,
				AccountHash:              accountHash,
				PredecessorTurnHash:      predecessorHash,
				PredecessorModeEpoch:     9,
				PredecessorAuthoritative: true,
				PredecessorGeneration:    2,
				RecordGeneration:         3,
				LaneGeneration:           3,
				LeaseGeneration:          2,
				ModeEpoch:                9,
				State:                    LeaseProvisional,
				ProtocolSchema:           CurrentCodexLeaseSchema,
				Authoritative:            true,
				CodexCurrentRequest: CodexCurrentRequest{
					Generation:               1,
					CurrentAttemptGeneration: 1,
					AttemptEnvelope: CodexAttemptEnvelope{
						PolicyVersion: CodexLeaseAttemptPolicyVersion,
						AttemptLimit:  1,
						Slots:         slots,
					},
					RequestKind:        CodexRequestTurn,
					RequestedModelHash: store.hash("requested-model", "gpt-requested"),
					EffectiveModel:     "gpt-effective",
					RequiredBuckets:    []CapacityBucket{CapacityBucketBase},
					Attempts: []CodexJournalAttempt{{
						Generation:     1,
						Revision:       1,
						Slot:           1,
						State:          CodexAttemptPrepared,
						CreatedAt:      createdAt,
						LastObservedAt: createdAt,
					}},
				},
				SocketLineageExtinct: true,
				CreatedAt:            createdAt,
				LastObservedAt:       observedAt,
			},
			{
				SessionHash:          sessionHash,
				ThreadHash:           threadHash,
				NamespaceHash:        namespaceHash,
				TurnHash:             shadowHash,
				RecordGeneration:     2,
				LaneGeneration:       2,
				LeaseGeneration:      2,
				ModeEpoch:            9,
				State:                LeaseFailedUnadmitted,
				ProtocolSchema:       CurrentCodexLeaseSchema,
				Authoritative:        false,
				SocketLineageExtinct: true,
				CreatedAt:            createdAt,
				LastObservedAt:       observedAt,
			},
			{
				SessionHash:          sessionHash,
				ThreadHash:           threadHash,
				NamespaceHash:        namespaceHash,
				TurnHash:             retainedHash,
				RecordGeneration:     2,
				LaneGeneration:       2,
				LeaseGeneration:      2,
				ModeEpoch:            8,
				State:                LeaseFailedUnadmitted,
				ProtocolSchema:       CurrentCodexLeaseSchema,
				Authoritative:        true,
				SocketLineageExtinct: true,
				CreatedAt:            createdAt,
				LastObservedAt:       observedAt,
			},
			{
				SessionHash:          otherSessionHash,
				ThreadHash:           otherThreadHash,
				NamespaceHash:        namespaceHash,
				TurnHash:             otherTurnHash,
				RecordGeneration:     1,
				LaneGeneration:       1,
				LeaseGeneration:      1,
				ModeEpoch:            9,
				State:                LeaseFailedUnadmitted,
				ProtocolSchema:       CurrentCodexLeaseSchema,
				Authoritative:        true,
				SocketLineageExtinct: true,
				CreatedAt:            createdAt,
				LastObservedAt:       observedAt,
			},
		},
	}
	for index := range envelope.Records {
		if envelope.Records[index].TurnHash == currentHash {
			envelope.Records[index].AttemptEnvelope.PlanDigest = codexLeaseAttemptPlanDigest(key, slots)
		}
	}
	payload, err := store.marshalV2Envelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.json", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy:      CodexLeasePolicy{Retention: 24 * time.Hour, Now: func() time.Time { return observedAt }},
		Modes:       CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{8, 9}},
	}, codexLeaseV2CASTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator
}
