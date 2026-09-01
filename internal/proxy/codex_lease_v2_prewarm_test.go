package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexPrewarmAdoptionCommitsRealTurnAndPreparedAttemptAtomically(t *testing.T) {
	coordinator, _, manager, reservation, request := prepareCodexPrewarmAdoptionTest(t)
	result, err := coordinator.AdoptPrewarm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	record := result.Record
	if !record.AdoptedPrewarm || record.PrewarmAdoptionJournalGeneration != result.Fence.Journal {
		t.Fatalf("adoption marker = (%t, %d), fence = %d", record.AdoptedPrewarm, record.PrewarmAdoptionJournalGeneration, result.Fence.Journal)
	}
	if record.RecordGeneration != 1 || record.LeaseGeneration != 1 {
		t.Fatalf("new record generations = record %d lease %d", record.RecordGeneration, record.LeaseGeneration)
	}
	if record.State != LeaseProvisional || !record.Authoritative || !record.NonMigratable || record.EverAdmitted {
		t.Fatalf("adopted record lifecycle = %#v", record)
	}
	if record.Generation != 1 || len(record.Attempts) != 1 || record.Attempts[0].State != CodexAttemptPrepared || record.CurrentAttemptGeneration != 1 {
		t.Fatalf("first request = %#v", record.CodexCurrentRequest)
	}
	if record.RoutingRefs != 1 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 {
		t.Fatalf("prepared request refs = R%d A%d O%d", record.RoutingRefs, record.AttemptRefs, record.ResponseObserverRefs)
	}
	if record.DownstreamSocketGeneration != 41 || record.UpstreamSocketGeneration != 43 || record.SocketLineageExtinct {
		t.Fatalf("socket fence = %#v", record)
	}
	if reservation := manager.snapshot(reservation.Lane); reservation.State != CodexPrewarmAdopted {
		t.Fatalf("published sentinel = %#v", reservation)
	}
}

func TestCodexPrewarmAdoptionRejectsIneligibleRequestAndForeignSlotsWithoutWriting(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexPrewarmAdoptionRequest)
	}{
		{name: "standalone compaction", mutate: func(request *CodexPrewarmAdoptionRequest) {
			request.RequestKind = CodexRequestCompaction
			request.CompactionPhase = CodexCompactionStandalone
		}},
		{name: "mid-turn compaction", mutate: func(request *CodexPrewarmAdoptionRequest) {
			request.RequestKind = CodexRequestCompaction
			request.CompactionPhase = CodexCompactionMidTurn
		}},
		{name: "prewarm", mutate: func(request *CodexPrewarmAdoptionRequest) { request.RequestKind = CodexRequestPrewarm }},
		{name: "foreign account slot", mutate: func(request *CodexPrewarmAdoptionRequest) { request.AttemptSlots[0].AccountKey = "foreign-account" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fsys, _, _, request := prepareCodexPrewarmAdoptionTest(t)
			before, err := fsys.ReadFile("/state/leases.json")
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&request)
			if _, err := coordinator.AdoptPrewarm(context.Background(), request); err == nil {
				t.Fatal("adoption unexpectedly succeeded")
			}
			after, err := fsys.ReadFile("/state/leases.json")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected adoption changed durable bytes")
			}
		})
	}
}

func TestCodexPrewarmAdoptionAcceptsPreTurnCompaction(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	request.RequestKind = CodexRequestCompaction
	request.CompactionPhase = CodexCompactionPreTurn
	result, err := coordinator.AdoptPrewarm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.RequestKind != CodexRequestCompaction || result.Record.CompactionPhase != CodexCompactionPreTurn {
		t.Fatalf("request metadata = kind %q phase %q", result.Record.RequestKind, result.Record.CompactionPhase)
	}
}

func TestCodexPrewarmAdoptionRequiresExactReservationFenceAndIsOneShot(t *testing.T) {
	fields := []struct {
		name   string
		mutate func(*CodexPrewarmAdoptionRequest)
	}{
		{name: "correlation", mutate: func(request *CodexPrewarmAdoptionRequest) { request.Correlation += "-stale" }},
		{name: "response anchor", mutate: func(request *CodexPrewarmAdoptionRequest) { request.ResponseAnchor += "-stale" }},
		{name: "turn state", mutate: func(request *CodexPrewarmAdoptionRequest) { request.TurnState += "-stale" }},
		{name: "reservation generation", mutate: func(request *CodexPrewarmAdoptionRequest) { request.ReservationGeneration++ }},
		{name: "downstream generation", mutate: func(request *CodexPrewarmAdoptionRequest) { request.DownstreamSocketGeneration++ }},
		{name: "upstream generation", mutate: func(request *CodexPrewarmAdoptionRequest) { request.UpstreamSocketGeneration++ }},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			coordinator, fsys, _, _, request := prepareCodexPrewarmAdoptionTest(t)
			before, _ := fsys.ReadFile("/state/leases.json")
			field.mutate(&request)
			if _, err := coordinator.AdoptPrewarm(context.Background(), request); !errors.Is(err, ErrCodexContinuity) {
				t.Fatalf("error = %v", err)
			}
			after, _ := fsys.ReadFile("/state/leases.json")
			if !bytes.Equal(before, after) {
				t.Fatal("stale adoption changed durable bytes")
			}
		})
	}

	coordinator, fsys, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	if _, err := coordinator.AdoptPrewarm(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	before, _ := fsys.ReadFile("/state/leases.json")
	if _, err := coordinator.AdoptPrewarm(context.Background(), request); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("duplicate error = %v", err)
	}
	after, _ := fsys.ReadFile("/state/leases.json")
	if !bytes.Equal(before, after) {
		t.Fatal("duplicate adoption changed durable bytes")
	}
}

func TestCodexPrewarmAdoptionRequiresFreshExternalAccountAndSocketRevalidation(t *testing.T) {
	tests := []struct {
		name       string
		revalidate CodexPrewarmAdoptionRevalidator
		cancel     bool
	}{
		{name: "missing callback"},
		{name: "removed account", revalidate: func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error {
			return errors.New("account no longer routable")
		}},
		{name: "dead socket", revalidate: func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error {
			return errors.New("socket lineage replaced")
		}},
		{name: "callback cancellation", cancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fsys, manager, reservation, request := prepareCodexPrewarmAdoptionTest(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request.Revalidate = test.revalidate
			if test.cancel {
				request.Revalidate = func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error {
					cancel()
					return nil
				}
			}
			before, err := fsys.ReadFile("/state/leases.json")
			if err != nil {
				t.Fatal(err)
			}
			generation := coordinator.store.Generation()
			if _, err := coordinator.AdoptPrewarm(ctx, request); err == nil {
				t.Fatal("adoption unexpectedly passed failed revalidation")
			}
			after, err := fsys.ReadFile("/state/leases.json")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || coordinator.store.Generation() != generation {
				t.Fatal("failed revalidation changed durable authority")
			}
			if got := manager.snapshot(reservation.Lane); got.State != CodexPrewarmReady || got.Generation != reservation.Generation {
				t.Fatalf("failed revalidation published sentinel: %#v", got)
			}
		})
	}
}

func TestCodexPrewarmAdoptionRevalidatesExactGenerationFence(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	var got CodexPrewarmAdoptionFence
	var gotAccount codex.AccountKey
	request.Revalidate = func(_ context.Context, account codex.AccountKey, fence CodexPrewarmAdoptionFence) error {
		gotAccount = account
		got = fence
		return nil
	}
	if _, err := coordinator.AdoptPrewarm(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := CodexPrewarmAdoptionFence{
		ReservationGeneration:      request.ReservationGeneration,
		DownstreamSocketGeneration: request.DownstreamSocketGeneration,
		UpstreamSocketGeneration:   request.UpstreamSocketGeneration,
	}
	if gotAccount != request.Choice.AccountKey || got != want {
		t.Fatalf("revalidation = account %q fence %#v, want account %q fence %#v", gotAccount, got, request.Choice.AccountKey, want)
	}
}

func TestCodexPrewarmAdoptionCommitsDetachedValidatedPlan(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	request.Revalidate = func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error {
		request.Choice.RequiredBuckets[0] = CapacityBucket("model:mutated")
		request.AttemptSlots[0].AccountKey = "mutated-account"
		request.AttemptSlots[0].CandidateID = "mutated-candidate"
		request.AttemptSlots[0].Kind = "mutated-kind"
		return nil
	}
	result, err := coordinator.AdoptPrewarm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Record.RequiredBuckets) != 1 || result.Record.RequiredBuckets[0] != CapacityBucketBase {
		t.Fatalf("stored mutated buckets: %#v", result.Record.RequiredBuckets)
	}
	slot := result.Record.AttemptEnvelope.Slots[0]
	if slot.Kind != CodexAttemptSlotDirect || !constantTimeCodexLeaseDigestEqual(slot.AccountHash, result.Record.AccountHash) || !constantTimeCodexLeaseDigestEqual(slot.CandidateHash, coordinator.store.hash("candidate", "candidate-raw")) {
		t.Fatalf("stored mutated attempt slot: %#v", slot)
	}
}

func TestCodexPrewarmAdoptionRejectsNilContextWithoutWriting(t *testing.T) {
	coordinator, fsys, manager, reservation, request := prepareCodexPrewarmAdoptionTest(t)
	before, _ := fsys.ReadFile("/state/leases.json")
	if _, err := coordinator.AdoptPrewarm(nil, request); err == nil {
		t.Fatal("nil context adoption succeeded")
	}
	after, _ := fsys.ReadFile("/state/leases.json")
	if !bytes.Equal(before, after) || manager.snapshot(reservation.Lane).State != CodexPrewarmReady {
		t.Fatal("nil context changed adoption authority")
	}
}

func TestCodexPrewarmAdoptionRejectsPhysicalSocketDeathBeforeQueuedDisconnect(t *testing.T) {
	coordinator, fsys, manager, reservation, request := prepareCodexPrewarmAdoptionTest(t)
	before, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	callbackEntered := make(chan struct{})
	continueCallback := make(chan struct{})
	socketLive := true
	request.Revalidate = func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error {
		close(callbackEntered)
		<-continueCallback
		if !socketLive {
			return errors.New("physical socket died")
		}
		return nil
	}
	adoptionDone := make(chan error, 1)
	go func() {
		_, err := coordinator.AdoptPrewarm(context.Background(), request)
		adoptionDone <- err
	}()
	<-callbackEntered

	socketLive = false
	disconnectStarted := make(chan struct{})
	disconnectDone := make(chan error, 1)
	go func() {
		close(disconnectStarted)
		disconnectDone <- manager.Disconnect(reservation.Lane)
	}()
	<-disconnectStarted
	close(continueCallback)
	if err := <-adoptionDone; err == nil {
		t.Fatal("adoption committed dead socket lineage")
	}
	if err := <-disconnectDone; err != nil {
		t.Fatal(err)
	}
	after, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || coordinator.store.Generation() != 1 {
		t.Fatal("dead socket adoption changed durable authority")
	}
	if got := manager.snapshot(reservation.Lane); got.State != CodexPrewarmDisconnected {
		t.Fatalf("queued disconnect state = %#v", got)
	}
}

func TestCodexPrewarmAdoptionPersistsOnlyKeyedDigests(t *testing.T) {
	coordinator, fsys, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	if _, err := coordinator.AdoptPrewarm(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"session-raw", "thread-raw", "turn-raw", "account-raw", "candidate-raw", "correlation-raw", "response-anchor-raw", "turn-state-raw"} {
		if bytes.Contains(data, []byte(raw)) {
			t.Fatalf("journal contains raw value %q", raw)
		}
	}
}

func TestCodexPrewarmAdoptionMarkerOwnsRemovalAuthority(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	if _, err := coordinator.AdoptPrewarm(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), request.Choice.AccountKey)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if summary.BoundCount != 1 || summary.AdoptedPrewarm != 1 || summary.BoundActive != 0 || summary.ContinuationPending != 0 || summary.BoundQuiescent != 0 || summary.OrphanedOrRestored != 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestCodexPrewarmAdoptionRestartExtinguishesSocketButPreservesMarker(t *testing.T) {
	coordinator, _, manager, reservation, request := prepareCodexPrewarmAdoptionTest(t)
	result, err := coordinator.AdoptPrewarm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.store.mu.Lock()
	err = coordinator.store.normaliseRestoredV2Locked()
	coordinator.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	stored := findCodexLeaseV2CASTestRecord(t, coordinator.store.v2.Records, result.Record.Identity())
	if !stored.AdoptedPrewarm || stored.PrewarmAdoptionJournalGeneration != result.Record.PrewarmAdoptionJournalGeneration || !stored.SocketLineageExtinct || stored.Attempts[0].State != CodexAttemptAbandonedBeforeDispatch {
		t.Fatalf("restored adoption = %#v", stored)
	}

	restarted := NewCodexPrewarmManager(manager.leases, manager.now)
	restarted.Restore([]CodexPrewarmReservation{manager.snapshot(reservation.Lane)})
	if inherited := restarted.snapshot(reservation.Lane); inherited.State == CodexPrewarmAdopted {
		t.Fatalf("adopted sentinel inherited across restart: %#v", inherited)
	}
}

func TestCodexRestartedPrewarmAdoptionAgesOutAfterRetention(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	result, err := coordinator.AdoptPrewarm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.store
	store.mu.Lock()
	err = store.normaliseRestoredV2Locked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.compactV2(); err != nil {
		t.Fatal(err)
	}
	stored := findCodexLeaseV2CASTestRecord(t, store.v2.Records, result.Record.Identity())
	if stored.State != LeaseProvisional || !stored.AdoptedPrewarm {
		t.Fatalf("within-horizon restored adoption = %#v", stored)
	}

	expireAt := stored.LastObservedAt.Add(2 * store.policy.Retention)
	store.policy.Now = func() time.Time { return expireAt }
	if err := store.compactV2(); err != nil {
		t.Fatal(err)
	}
	stored = findCodexLeaseV2CASTestRecord(t, store.v2.Records, result.Record.Identity())
	if stored.State != LeaseExpired || !stored.AdoptedPrewarm {
		t.Fatalf("expired restored adoption = %#v", stored)
	}
	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), request.Choice.AccountKey)
	if err != nil {
		t.Fatal(err)
	}
	guard.Release()
	if summary.BoundCount != 0 || summary.AdoptedPrewarm != 0 {
		t.Fatalf("expired restored summary = %#v", summary)
	}

	deleteAt := stored.LastObservedAt.Add(2 * store.policy.Retention)
	store.policy.Now = func() time.Time { return deleteAt }
	if err := store.compactV2(); err != nil {
		t.Fatal(err)
	}
	for _, retained := range store.v2.Records {
		if retained.Identity() == result.Record.Identity() {
			t.Fatalf("expired restored adoption retained: %#v", retained)
		}
	}
}

func TestCodexCompletedPrewarmAdoptionUsesBoundStateAndExpiresNormally(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	result, err := coordinator.AdoptPrewarm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.store
	terminalAt := store.policy.Now().UTC().Add(time.Minute)
	store.mu.Lock()
	next := cloneCodexLeaseV2Envelope(*store.v2)
	record := &next.Records[0]
	record.State = LeaseBoundQuiescent
	record.SocketLineageExtinct = true
	record.RoutingRefs = 0
	record.Attempts[0].State = CodexAttemptProviderCompleted
	record.Attempts[0].Revision++
	record.Attempts[0].LastObservedAt = terminalAt
	record.RecordGeneration++
	record.LeaseGeneration++
	record.EverAdmitted = true
	record.AdmissionJournalGeneration = next.Generation + 1
	record.AdmissionRequestGeneration = record.Generation
	record.AdmissionRequestKind = record.RequestKind
	record.AdmittedAt = terminalAt
	record.LastObservedAt = terminalAt
	lane := &next.Lanes[0]
	lane.LastAdmittedAccountHash = record.AccountHash
	lane.LastAdmittedTurnHash = record.TurnHash
	lane.LastAdmittedModeEpoch = record.ModeEpoch
	lane.LastAdmittedAuthoritative = true
	lane.LastAdmissionJournalGeneration = record.AdmissionJournalGeneration
	lane.LastAdmittedAt = terminalAt
	lane.LastObservedAt = terminalAt
	err = store.commitV2Locked(store.v2.Generation, next)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	guard, summary, err := coordinator.BeginAccountRemoval(context.Background(), request.Choice.AccountKey)
	if err != nil {
		t.Fatal(err)
	}
	guard.Release()
	if summary.BoundCount != 1 || summary.BoundQuiescent != 1 || summary.AdoptedPrewarm != 0 {
		t.Fatalf("completed adoption summary = %#v", summary)
	}

	retentionNow := terminalAt.Add(2 * store.policy.Retention)
	store.policy.Now = func() time.Time { return retentionNow }
	if err := store.compactV2(); err != nil {
		t.Fatal(err)
	}
	guard, summary, err = coordinator.BeginAccountRemoval(context.Background(), request.Choice.AccountKey)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if summary.BoundCount != 0 || summary.AdoptedPrewarm != 0 {
		t.Fatalf("expired adoption summary = %#v", summary)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, retained := range store.v2.Records {
		if retained.Identity() == result.Record.Identity() {
			t.Fatalf("terminal adoption marker retained past horizon: %#v", retained)
		}
	}
}

func TestCodexPrewarmAdoptionWaitsBehindAccountRemovalGate(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	guard, _, err := coordinator.BeginAccountRemoval(context.Background(), request.Choice.AccountKey)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := coordinator.AdoptPrewarm(context.Background(), request)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("adoption crossed removal gate: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	guard.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("adoption did not resume after removal gate")
	}
}

func TestCodexPrewarmAdoptionMarkerSchemaIsAllOrNoneAndBounded(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	if _, err := coordinator.AdoptPrewarm(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*CodexJournalRecordV2)
	}{
		{name: "boolean only", mutate: func(record *CodexJournalRecordV2) { record.PrewarmAdoptionJournalGeneration = 0 }},
		{name: "generation only", mutate: func(record *CodexJournalRecordV2) { record.AdoptedPrewarm = false }},
		{name: "future generation", mutate: func(record *CodexJournalRecordV2) {
			record.PrewarmAdoptionJournalGeneration += 100
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator.store.mu.Lock()
			defer coordinator.store.mu.Unlock()
			next := cloneCodexLeaseV2Envelope(*coordinator.store.v2)
			test.mutate(&next.Records[0])
			if err := coordinator.store.validateCodexLeaseV2CandidateLocked(next); err == nil {
				t.Fatal("invalid adoption marker accepted")
			}
		})
	}
}

func TestCodexPrewarmAdoptionMarkerCannotBeForgedOrChangedByGenericCAS(t *testing.T) {
	coordinator, _, _, _, request := prepareCodexPrewarmAdoptionTest(t)
	result, err := coordinator.AdoptPrewarm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	forged := reservingCodexLeaseV2CASTestRecord(coordinator.store, "forged-session", "forged-thread", "forged-turn")
	forged.AdoptedPrewarm = true
	forged.PrewarmAdoptionJournalGeneration = 2
	if _, _, _, err := coordinator.store.buildCodexLeaseRecordAfterImage(CodexJournalRecordV2{}, false, forged, false, false, CodexLeaseRecordFence{}, coordinator.store.policy.Now()); err == nil {
		t.Fatal("generic CAS forged adoption marker")
	}

	changed := cloneCodexJournalRecordV2(result.Record)
	changed.PrewarmAdoptionJournalGeneration++
	fence := CodexLeaseRecordFence{Record: result.Record.Identity(), Revision: result.Record.RecordGeneration, Lease: result.Record.LeaseGeneration, RequestGeneration: result.Record.Generation, CurrentAttempt: result.Record.CurrentAttemptGeneration}
	if _, _, _, err := coordinator.store.buildCodexLeaseRecordAfterImage(result.Record, true, changed, false, false, fence, coordinator.store.policy.Now()); err == nil {
		t.Fatal("generic CAS changed adoption marker")
	}
}

func prepareCodexPrewarmAdoptionTest(t *testing.T) (*CodexContinuityCoordinator, *fsutil.MemFS, *CodexPrewarmManager, CodexPrewarmReservation, CodexPrewarmAdoptionRequest) {
	t.Helper()
	coordinator, fsys := openCodexLeaseV2RemovalTestCoordinator(t)
	manager := coordinator.prewarms
	metadata := CodexTurnMetadata{
		SessionID:   "session-raw",
		ThreadID:    "thread-raw",
		RequestKind: CodexRequestPrewarm,
	}
	reservation, err := manager.Create(metadata, "correlation-raw")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err = manager.BindSockets(reservation.Lane, "account-raw", 41, 43)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err = manager.Ready(reservation.Lane, "response-anchor-raw", "turn-state-raw")
	if err != nil {
		t.Fatal(err)
	}

	request := CodexPrewarmAdoptionRequest{
		Key:                        LeaseKey{Lane: reservation.Lane, Turn: "turn-raw"},
		Policy:                     CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
		Choice:                     RouteChoice{AccountKey: "account-raw", RequestedModel: "gpt-5", EffectiveModel: "gpt-5", RequiredBuckets: []CapacityBucket{CapacityBucketBase}},
		AttemptSlots:               []CodexPrewarmAttemptSlot{{AccountKey: "account-raw", CandidateID: codex.CandidateID("candidate-raw"), Kind: CodexAttemptSlotDirect}},
		RequestKind:                CodexRequestTurn,
		Correlation:                "correlation-raw",
		ResponseAnchor:             "response-anchor-raw",
		TurnState:                  "turn-state-raw",
		ReservationGeneration:      reservation.Generation,
		DownstreamSocketGeneration: 41,
		UpstreamSocketGeneration:   43,
		Revalidate: func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error {
			return nil
		},
	}
	return coordinator, fsys, manager, reservation, request
}
