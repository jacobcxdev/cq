package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexHTTPRequestPlanFactoryBuildsOnceAndBeginsDurably(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-b",
			frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceExternal, false, time.Time{}),
		),
		frozenDispatchTestLogicalAccount("account-a",
			frozenDispatchCandidate("account-a", "candidate-managed", "revision-managed", codex.SourceManaged, true, now.Add(time.Hour)),
			frozenDispatchCandidate("account-a", "candidate-accepted", "revision-accepted", codex.SourceSystem, false, now.Add(-time.Hour)),
		),
	}}
	events := make([]string, 0, 7)
	inventorySource := &codexHTTPRequestPlanTestInventory{inventory: inventory, events: &events}
	snapshotter := &codexHTTPRequestPlanTestSnapshotter{
		snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration:  13,
			Provisional:        map[codex.AccountKey]int{"account-b": 2},
			AffinityAccountKey: "account-a",
		},
		events: &events,
	}
	handle := &CodexLeaseRequestHandle{account: "account-a"}
	runtime := &codexHTTPRequestPlanTestRuntime{handle: handle, events: &events}
	headroomCalls := 0
	headroom := CodexRequestHeadroomFunc(func(_ context.Context, body []byte, mode HeadroomMode) ([]byte, int, error) {
		events = append(events, "headroom")
		headroomCalls++
		if mode != HeadroomModeCache {
			t.Fatalf("Headroom mode = %v, want cache", mode)
		}
		return body, 7, nil
	})

	factory := &CodexHTTPRequestPlanFactory{
		Inventory:         inventorySource,
		Capacity:          NewCodexCapacityLedger(func() time.Time { return now }, time.Hour),
		Routes:            snapshotter,
		Runtime:           runtime,
		DefaultAccountKey: "account-b",
		Authority: CodexLeaseAuthorityPolicy{
			ModeEpoch:                   9,
			Authoritative:               true,
			RetainedAuthoritativeEpochs: []uint64{7},
		},
		Headroom:     headroom,
		HeadroomMode: HeadroomModeCache,
		Now:          func() time.Time { return now },
	}
	var inspected *CodexFrozenRequestInspection
	var frozenByOperation *CodexFrozenRequest
	factory.operations = codexHTTPRequestPlanFactoryOperations{
		inspect: func(ctx context.Context, encoded []byte, headers http.Header) (*CodexFrozenRequestInspection, error) {
			events = append(events, "inspect")
			var err error
			inspected, err = InspectCodexNativeRequest(ctx, encoded, headers)
			return inspected, err
		},
		buildDispatch: func(ctx context.Context, input CodexFrozenDispatchInput) (CodexFrozenDispatchPlan, error) {
			events = append(events, "plan")
			return BuildCodexFrozenDispatchPlan(ctx, input)
		},
		freeze: func(ctx context.Context, inspection *CodexFrozenRequestInspection, choice RouteChoice, headroom CodexRequestHeadroom, mode HeadroomMode) (*CodexFrozenRequest, error) {
			events = append(events, "freeze")
			var err error
			frozenByOperation, err = inspection.Freeze(ctx, choice, headroom, mode)
			return frozenByOperation, err
		},
	}

	probe, err := newCodexInstalledHTTPGateProbe(sha256.Sum256([]byte("plan-listener")))
	if err != nil {
		t.Fatal(err)
	}
	trace := probe.begin(codexInstalledHTTPProbeResponses)
	ctx := withCodexInstalledHTTPTrace(context.Background(), trace)
	result, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded:          frozenRequestBody("gpt-5", CodexRequestTurn, "private-request-material"),
		Headers:          http.Header{"X-Private": {"private-header-material"}},
		AcceptedRevision: "revision-accepted",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer result.Frozen.Release()

	wantEvents := []string{"inspect", "inventory", "snapshot", "plan", "freeze", "headroom", "begin"}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if headroomCalls != 1 || inventorySource.calls != 1 || snapshotter.calls != 1 || runtime.calls != 1 {
		t.Fatalf("calls: headroom=%d inventory=%d snapshot=%d begin=%d", headroomCalls, inventorySource.calls, snapshotter.calls, runtime.calls)
	}
	if result.Frozen != frozenByOperation || result.Lifecycle == nil || result.Lifecycle.AccountKey() != handle.AccountKey() {
		t.Fatalf("ownership result = %#v", result)
	}
	if _, err := inspected.Protocol(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("inspection remains owned after Freeze: %v", err)
	}
	choices := result.Dispatch.Accounts()
	if len(choices) != 2 || choices[0].Choice().AccountKey != "account-a" || choices[1].Choice().AccountKey != "account-b" {
		t.Fatalf("dispatch choices = %#v", choices)
	}

	wantKey := LeaseKey{Lane: LaneKey{Session: "session", Thread: "thread", Namespace: CodexResponsesNamespace}, Turn: "turn"}
	if snapshotter.key != wantKey || !slices.Equal(snapshotter.accounts, []codex.AccountKey{"account-b", "account-a"}) || !reflect.DeepEqual(snapshotter.authority, factory.Authority) {
		t.Fatalf("snapshot request = key %#v accounts %v authority %#v", snapshotter.key, snapshotter.accounts, snapshotter.authority)
	}
	wantSlots := []CodexLeaseAttemptSlotPlan{
		{AccountKey: "account-a", CandidateID: "candidate-accepted", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-a", CandidateID: "candidate-managed", Kind: CodexAttemptSlotDirect},
		{AccountKey: "account-a", CandidateID: "candidate-managed", Kind: CodexAttemptSlotEligibleManagedRefresh},
		{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
	}
	wantLeasePlan := CodexLeaseRequestPlan{
		Key:             wantKey,
		Accounts:        []codex.AccountKey{"account-b", "account-a"},
		Authority:       factory.Authority,
		RequestKind:     CodexRequestTurn,
		RequestedModel:  "gpt-5",
		EffectiveModel:  "gpt-5",
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
		Slots:           wantSlots,
		InitialSlot:     1,
	}
	if !reflect.DeepEqual(runtime.plan, wantLeasePlan) {
		t.Fatalf("lease plan = %#v, want %#v", runtime.plan, wantLeasePlan)
	}
	if result.Frozen.HeadroomSavings() != 7 {
		t.Fatalf("Headroom savings = %d, want 7", result.Frozen.HeadroomSavings())
	}
	trace.mu.Lock()
	probeFacts := trace.plan
	trace.mu.Unlock()
	if probeFacts.inspectCalls != 1 || probeFacts.freezeCalls != 1 || probeFacts.transformed ||
		probeFacts.encodeCalls != 0 || probeFacts.headroomTransforms != 0 {
		t.Fatalf("unchanged Headroom probe facts = %#v", probeFacts)
	}
	choice, err := result.Frozen.Choice()
	if err != nil || !reflect.DeepEqual(choice, choices[0].Choice()) {
		t.Fatalf("frozen choice = %#v, %v", choice, err)
	}
}

func TestCodexHTTPRequestPlanFactoryRegistersReceiptAfterDurableBegin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x51}, 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account"}}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.TransportKind = "http"
	factory.TurnReceipts = store
	body := []byte(strings.TrimSuffix(string(frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-body")), "}") + `,"reasoning":{"effort":"high"}}`)

	prepared, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: body})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	receipt, found := store.lookup([]byte("session"), []byte("turn"))
	if !found {
		t.Fatal("receipt not registered")
	}
	if receipt.State != CodexTurnReceiptPlanned || receipt.Transport != CodexTurnReceiptTransportHTTP ||
		receipt.RequestKind != "turn" || receipt.RequestLineage != "previous_response_id_absent" ||
		receipt.RequestedModelClass != "gpt_5_6_sol" || receipt.RequestedReasoningEffort != "high" ||
		receipt.CompactionPhase != "not_applicable" || receipt.RouteReason != CodexTurnReceiptRouteFairnessSelect ||
		receipt.PlannedAccountHint != redactedAccountHint("codex", "account") || receipt.ActualAccountHint != "" ||
		receipt.ShadowComparison != CodexTurnReceiptShadowNotApplicable || receipt.ShadowAlternativeAccountHint != "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if prepared.receipt == nil {
		t.Fatal("prepared receipt handle unavailable")
	}
	if _, ok := prepared.Lifecycle.(*codexTurnReceiptLifecycle); !ok {
		t.Fatalf("lifecycle = %T, want receipt wrapper", prepared.Lifecycle)
	}

	failingStore, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x52}, 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	failingRuntime := &codexHTTPRequestPlanTestRuntime{err: errors.New("begin failed")}
	failingFactory := codexHTTPRequestPlanTestFactory(failingRuntime)
	failingFactory.TransportKind = "http"
	failingFactory.TurnReceipts = failingStore
	if _, err := failingFactory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: body}); err == nil {
		t.Fatal("failed begin succeeded")
	}
	if _, found := failingStore.lookup([]byte("session"), []byte("turn")); found {
		t.Fatal("failed begin registered receipt")
	}

	compaction := frozenRequestBody("gpt-5.6-sol", CodexRequestCompaction, "private-compaction")
	compactionRuntime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account"}}
	compactionFactory := codexHTTPRequestPlanTestFactory(compactionRuntime)
	compactionFactory.TransportKind = "http"
	compactionFactory.TurnReceipts = failingStore
	if prepared, err := compactionFactory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: compaction}); err == nil {
		prepared.Frozen.Release()
	}
	if _, found := failingStore.lookup([]byte("session"), []byte("turn")); found {
		t.Fatal("compaction registered root Stop receipt")
	}
}

func TestCodexHTTPRequestPlanFactoryRecordsNoAffinityAlternativeWithoutChangingRoute(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	frozenDispatchObserveCapacity(t, capacity, "account-a", CapacityBucketBase, 20, now)
	frozenDispatchObserveCapacity(t, capacity, "account-b", CapacityBucketBase, 80, now)
	accounts := []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour))),
	}
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x53}, 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: accounts}},
		Capacity:  capacity,
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration:       1,
			AffinityPresent:         true,
			AffinityAccountKey:      "account-a",
			AffinityEffectiveModel:  "gpt-5.6-sol",
			AffinityCacheAdmittedAt: now.Add(-time.Minute),
		}},
		Runtime:           &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-a"}},
		DefaultAccountKey: "account-b",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
		TransportKind:     "http",
		TurnReceipts:      store,
	}
	body := []byte(strings.TrimSuffix(string(frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-body")), "}") + `,"reasoning":{"effort":"high"}}`)

	prepared, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: body})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	if got := prepared.Dispatch.Accounts()[0].Choice().AccountKey; got != "account-a" {
		t.Fatalf("actual account = %q, want account-a", got)
	}
	receipt, found := store.lookup([]byte("session"), []byte("turn"))
	if !found {
		t.Fatal("receipt not registered")
	}
	if receipt.ShadowComparison != CodexTurnReceiptShadowAlternativeAccount || receipt.ShadowAlternativeAccountHint != redactedAccountHint("codex", "account-b") {
		t.Fatalf("shadow receipt = %+v", receipt)
	}
}

func TestCodexHTTPRequestPlanFactoryPinsUnboundRequest(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	accounts := []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-a",
			frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("account-c",
			frozenDispatchCandidate("account-c", "candidate-c", "revision-c", codex.SourceSystem, false, now.Add(time.Hour))),
	}
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-c"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory:         &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: accounts}},
		Routes:            &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{JournalGeneration: 1}},
		Runtime:           runtime,
		DefaultAccountKey: "account-a",
		PinnedAccountKey:  "account-c",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Frozen.Release()
	dispatch := result.Dispatch.Accounts()
	if len(dispatch) != 1 || dispatch[0].Choice().AccountKey != "account-c" {
		t.Fatalf("dispatch = %#v, want pinned account only", dispatch)
	}
	if len(runtime.plan.Slots) == 0 || runtime.plan.Slots[0].AccountKey != "account-c" {
		t.Fatalf("durable slots = %#v, want pinned account", runtime.plan.Slots)
	}
}

func TestCodexHTTPRequestPlanFactoryPreservesHardBoundAccountOverPin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	accounts := []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-a",
			frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("account-c",
			frozenDispatchCandidate("account-c", "candidate-c", "revision-c", codex.SourceSystem, false, now.Add(time.Hour))),
	}
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-a"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: accounts}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration: 1,
			BoundAccountKey:   "account-a",
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account-a",
		PinnedAccountKey:  "account-c",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Frozen.Release()
	dispatch := result.Dispatch.Accounts()
	if len(dispatch) != 1 || dispatch[0].Choice().AccountKey != "account-a" {
		t.Fatalf("dispatch = %#v, want hard-bound account", dispatch)
	}
}

func TestCodexHTTPRequestPlanFactoryPinOverridesSoftAffinity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	accounts := []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-a",
			frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("account-c",
			frozenDispatchCandidate("account-c", "candidate-c", "revision-c", codex.SourceSystem, false, now.Add(time.Hour))),
	}
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-c"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: accounts}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration:       1,
			AffinityPresent:         true,
			AffinityAccountKey:      "account-a",
			AffinityCacheAdmittedAt: now.Add(-time.Minute),
			AffinityEffectiveModel:  "gpt-5.6-sol",
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account-a",
		PinnedAccountKey:  "account-c",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Frozen.Release()
	dispatch := result.Dispatch.Accounts()
	if len(dispatch) != 1 || dispatch[0].Choice().AccountKey != "account-c" {
		t.Fatalf("dispatch = %#v, want pin to override soft affinity", dispatch)
	}
}

func TestCodexHTTPRequestPlanFactoryPlansPrewarmWithoutDurableAuthority(t *testing.T) {
	t.Parallel()
	runtime := &codexHTTPRequestPlanTestRuntime{}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	snapshotter := factory.Routes.(*codexHTTPRequestPlanTestSnapshotter)
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)

	dispatch, err := factory.planWebSocketPrewarm(context.Background(), CodexHTTPRequestPlanInput{Encoded: payload})
	if err != nil {
		t.Fatal(err)
	}
	accounts := dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account" {
		t.Fatalf("dispatch = %#v", accounts)
	}
	if runtime.calls != 0 || snapshotter.calls != 0 {
		t.Fatalf("durable calls = runtime %d snapshot %d", runtime.calls, snapshotter.calls)
	}
}

func TestCodexHTTPRequestPlanFactoryPinsWebSocketPrewarm(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-a",
				frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
			frozenDispatchTestLogicalAccount("account-c",
				frozenDispatchCandidate("account-c", "candidate-c", "revision-c", codex.SourceSystem, false, now.Add(time.Hour))),
		}}},
		DefaultAccountKey: "account-a",
		PinnedAccountKey:  "account-c",
		Now:               func() time.Time { return now },
	}
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)

	dispatch, err := factory.planWebSocketPrewarm(context.Background(), CodexHTTPRequestPlanInput{Encoded: payload})
	if err != nil {
		t.Fatal(err)
	}
	accounts := dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-c" {
		t.Fatalf("dispatch = %#v, want pinned account only", accounts)
	}
}

func TestCodexHTTPRequestPlanFactoryAppliesSessionPolicyToWebSocketPrewarm(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	key := []byte("01234567890123456789012345678901")
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
			frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour))),
		}}},
		DefaultAccountKey: "account-a",
		SessionPolicy: NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
			SchemaVersion: 1, RoutingGeneration: 7,
			Pools:           []AccountPoolV1{{Name: "bound", Members: []codex.AccountKey{"account-b"}}},
			SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, []byte("session")), Pool: "bound"}},
		})),
		Now: func() time.Time { return now },
	}
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	caller := RuntimeCallerAuthorityV1{Domain: NormalCallerLocal, SubjectID: "local", ConsumptionDigest: strings.Repeat("a", 64)}
	dispatch, err := factory.planWebSocketPrewarm(withRuntimeCallerAuthority(context.Background(), caller), CodexHTTPRequestPlanInput{Encoded: payload})
	if err != nil {
		t.Fatal(err)
	}
	accounts := dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-b" {
		t.Fatalf("dispatch = %#v", accounts)
	}
}

func TestCodexHTTPRequestPlanFactoryAppliesPoolValueToWebSocketPrewarm(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-valued", frozenDispatchCandidate("account-valued", "candidate-valued", "revision-valued", codex.SourceSystem, false, now.Add(time.Hour))),
			frozenDispatchTestLogicalAccount("account-ordinary", frozenDispatchCandidate("account-ordinary", "candidate-ordinary", "revision-ordinary", codex.SourceSystem, false, now.Add(time.Hour))),
		}}},
		DefaultAccountKey: "account-valued",
		SessionPolicy: NewSessionPolicyResolver(make([]byte, 32), RoutingPolicyV2{
			SchemaVersion: 2, RoutingGeneration: 7,
			Pools: []AccountPoolV2{{ID: testPoolIDA, Name: "Cyber", Value: 10, Members: []codex.AccountKey{"account-valued"}}},
		}),
		Now: func() time.Time { return now },
	}
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	dispatch, err := factory.planWebSocketPrewarm(context.Background(), CodexHTTPRequestPlanInput{Encoded: payload})
	if err != nil {
		t.Fatal(err)
	}
	accounts := dispatch.Accounts()
	if len(accounts) != 2 || accounts[0].Choice().AccountKey != "account-ordinary" || accounts[0].Value() != 0 || accounts[1].Value() != 10 {
		t.Fatalf("dispatch = %#v", accounts)
	}
}

func TestCodexHTTPRequestPlanFactoryAdoptsReadyWebSocketPrewarm(t *testing.T) {
	t.Parallel()
	coordinator, _, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	account := frozenDispatchTestLogicalAccount("account-a",
		frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	factory := &CodexHTTPRequestPlanFactory{
		Inventory:         &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{account}}},
		Routes:            coordinator,
		Runtime:           runtimeLease,
		DefaultAccountKey: "account-a",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
		Now:               func() time.Time { return *now },
	}
	prewarm := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session-a\",\"thread_id\":\"thread-a\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	dispatch, err := factory.planWebSocketPrewarm(context.Background(), CodexHTTPRequestPlanInput{Encoded: prewarm})
	if err != nil || len(dispatch.Accounts()) != 1 {
		t.Fatalf("prewarm dispatch = %#v, %v", dispatch.Accounts(), err)
	}
	reservation, err := factory.beginWebSocketPrewarm(CodexHTTPRequestPlanInput{Encoded: prewarm})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err = factory.bindWebSocketPrewarm(reservation, "account-a", 41, 43)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err = factory.readyWebSocketPrewarm(reservation, "prewarm-a", "")
	if err != nil {
		t.Fatal(err)
	}
	turn := []byte(`{"type":"response.create","model":"gpt-5.6-sol","previous_response_id":"prewarm-a","client_metadata":{"x-codex-turn-metadata":{"session_id":"session-a","thread_id":"thread-a","turn_id":"turn-a","request_kind":"turn"}},"input":[]}`)
	prepared, err := factory.adoptWebSocketPrewarm(context.Background(), CodexHTTPRequestPlanInput{Encoded: turn}, reservation, func(_ context.Context, account codex.AccountKey, fence CodexPrewarmAdoptionFence) error {
		if account != "account-a" || fence.ReservationGeneration != reservation.Generation || fence.DownstreamSocketGeneration != 41 || fence.UpstreamSocketGeneration != 43 {
			t.Fatalf("adoption fence = %q/%#v", account, fence)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	if prepared.leaseHandle == nil || !prepared.leaseHandle.record.AdoptedPrewarm || prepared.leaseHandle.AccountKey() != "account-a" {
		t.Fatalf("adopted request = %#v", prepared.leaseHandle)
	}
	if prepared.leaseHandle.record.CorrelationHash != coordinator.store.hash("correlation", "prewarm-a") {
		t.Fatal("adopted response anchor was not bound")
	}
}

func TestCodexHTTPRequestPlanFactoryMakesFairnessEligibleAtGPT56CacheFloor(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		model            string
		affinityModel    string
		cacheAdmittedAt  time.Time
		bound            bool
		requiresAccount  bool
		encryptedRequest bool
		unresolved       bool
		hardZero         bool
		want             codex.AccountKey
		wantDecision     codexRuntimeDecision
		wantAccounts     int
		wantContinuity   bool
		wantErr          error
	}{
		{name: "warm before boundary", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-30*time.Minute + time.Second), want: "account-a", wantDecision: codexRuntimeDecisionAffinityReuse, wantAccounts: 2},
		{name: "normalised warm model", model: "gpt-5.6-sol[1m]", affinityModel: "gpt-5.6-sol[1m]", cacheAdmittedAt: now.Add(-30*time.Minute + time.Second), want: "account-a", wantDecision: codexRuntimeDecisionAffinityReuse, wantAccounts: 2},
		{name: "fairness eligible at boundary", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-30 * time.Minute), want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "fairness eligible after boundary", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-31 * time.Minute), want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "unknown future policy stays sticky", model: "gpt-5.7-codex", affinityModel: "gpt-5.7-codex", cacheAdmittedAt: now.Add(-24 * time.Hour), want: "account-a", wantDecision: codexRuntimeDecisionAffinityReuse, wantAccounts: 2},
		{name: "unknown dashed alias stays sticky", model: "gpt-5.6-private", affinityModel: "gpt-5.6-private", cacheAdmittedAt: now.Add(-24 * time.Hour), want: "account-a", wantDecision: codexRuntimeDecisionAffinityReuse, wantAccounts: 2},
		{name: "missing timing stays sticky", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", want: "account-a", wantDecision: codexRuntimeDecisionAffinityReuse, wantAccounts: 2},
		{name: "clock rollback stays sticky", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(time.Minute), want: "account-a", wantDecision: codexRuntimeDecisionAffinityReuse, wantAccounts: 2},
		{name: "model change has no reusable cache", model: "gpt-5.6-codex", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Minute), want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "encrypted predecessor remains pinned after floor", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Hour), requiresAccount: true, want: "account-a", wantDecision: codexRuntimeDecisionNone, wantAccounts: 1, wantContinuity: true},
		{name: "encrypted predecessor survives model change", model: "gpt-5.6-codex", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Minute), requiresAccount: true, want: "account-a", wantDecision: codexRuntimeDecisionNone, wantAccounts: 1, wantContinuity: true},
		{name: "encrypted predecessor survives hard zero", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Hour), requiresAccount: true, hardZero: true, want: "account-a", wantDecision: codexRuntimeDecisionNone, wantAccounts: 1, wantContinuity: true},
		{name: "encrypted request remains pinned after floor", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Hour), encryptedRequest: true, want: "account-a", wantDecision: codexRuntimeDecisionNone, wantAccounts: 1, wantContinuity: true},
		{name: "exact turn remains bound after floor", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Hour), bound: true, want: "account-a", wantDecision: codexRuntimeDecisionNone, wantAccounts: 1},
		{name: "warm unresolved account permits fairness", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Minute), unresolved: true, want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "unknown-policy unresolved account permits fairness", model: "gpt-5.7-codex", affinityModel: "gpt-5.7-codex", cacheAdmittedAt: now.Add(-time.Hour), unresolved: true, want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "private-policy unresolved account permits fairness", model: "gpt-5.6-private", affinityModel: "gpt-5.6-private", cacheAdmittedAt: now.Add(-time.Hour), unresolved: true, want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "future cache clock unresolved account permits fairness", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(time.Minute), unresolved: true, want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "floor reached unresolved account permits fairness", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-30 * time.Minute), unresolved: true, want: "account-b", wantDecision: codexRuntimeDecisionFairnessSelect, wantAccounts: 2},
		{name: "hard unresolved account fails closed", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Hour), unresolved: true, requiresAccount: true, wantErr: ErrCodexLeaseAuthorityMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
				frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
				frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour))),
			}}
			snapshot := CodexLeaseRouteSnapshot{
				JournalGeneration:       1,
				AffinityPresent:         true,
				AffinityCacheAdmittedAt: test.cacheAdmittedAt,
				AffinityEffectiveModel:  test.affinityModel,
				AffinityRequiresAccount: test.requiresAccount,
				Provisional:             map[codex.AccountKey]int{"account-a": 1},
			}
			if !test.unresolved {
				snapshot.AffinityAccountKey = "account-a"
			}
			if test.bound {
				snapshot.Classification = CodexRestoredLaneCurrent
				snapshot.BoundAccountKey = "account-a"
			}
			capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
			if test.hardZero {
				capacity.Observe(CapacityFact{AccountKey: "account-a", Bucket: CapacityBucketBase, Source: CapacitySourceHardLimit, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative})
			}
			handleAccount := test.want
			if handleAccount == "" {
				handleAccount = "account-b"
			}
			runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: handleAccount}}
			factory := &CodexHTTPRequestPlanFactory{
				Inventory:         &codexHTTPRequestPlanTestInventory{inventory: inventory},
				Capacity:          capacity,
				Routes:            &codexHTTPRequestPlanTestSnapshotter{snapshot: snapshot},
				Runtime:           runtime,
				DefaultAccountKey: "account-b",
				Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
				Now:               func() time.Time { return now },
			}

			encoded := frozenRequestBody(test.model, CodexRequestTurn, "private-body")
			if test.encryptedRequest {
				encoded = []byte(strings.TrimSuffix(string(encoded), "}") + `,"encrypted_content":"opaque"}`)
			}
			result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: encoded})
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Build error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer result.Frozen.Release()
			accounts := result.Dispatch.Accounts()
			if len(accounts) == 0 || accounts[0].Choice().AccountKey != test.want {
				t.Fatalf("first dispatch account = %#v, want %q", accounts, test.want)
			}
			if len(accounts) != test.wantAccounts {
				t.Fatalf("dispatch account count = %d, want %d", len(accounts), test.wantAccounts)
			}
			if len(accounts) != 0 && accounts[0].decision != test.wantDecision {
				t.Fatalf("first dispatch decision = %q, want %q", accounts[0].decision, test.wantDecision)
			}
			if runtime.plan.RequiresAccountContinuity != test.wantContinuity {
				t.Fatalf("durable account continuity = %v, want %v", runtime.plan.RequiresAccountContinuity, test.wantContinuity)
			}
		})
	}
}

func TestCodexGPT56PromptCacheModelRecognisesOnlyInstalledPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		want  bool
	}{
		{model: "gpt-5.6-sol", want: true},
		{model: "gpt-5.6-sol[1m]", want: true},
		{model: "gpt-5.6"},
		{model: "gpt-5.6-private"},
		{model: "gpt-5.6-sol-extra"},
		{model: "GPT-5.6-SOL"},
		{model: " gpt-5.6-sol"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()
			if got := codexGPT56PromptCacheModel(test.model); got != test.want {
				t.Fatalf("codexGPT56PromptCacheModel(%q) = %v, want %v", test.model, got, test.want)
			}
		})
	}
}

func TestCodexHTTPRequestPlanFactoryPrefersExactBoundForRawContinuity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour))),
	}}
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-b"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: inventory},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			Classification:          CodexRestoredLaneCurrent,
			BoundAccountKey:         "account-b",
			AffinityPresent:         true,
			AffinityAccountKey:      "account-a",
			AffinityCacheAdmittedAt: now.Add(-time.Minute),
			AffinityEffectiveModel:  "gpt-5.6-sol",
			JournalGeneration:       1,
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account-a",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}
	encoded := []byte(strings.TrimSuffix(string(frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-body")), "}") + `,"encrypted_content":"opaque"}`)
	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: encoded})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Frozen.Release()
	accounts := result.Dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-b" || accounts[0].decision != codexRuntimeDecisionNone {
		t.Fatalf("hard exact-turn dispatch = %#v, want bound account-b only", accounts)
	}
	if !runtime.plan.RequiresAccountContinuity {
		t.Fatal("raw hard continuity was not carried into the durable request plan")
	}
}

func TestCodexHTTPRequestPlanFactoryAdoptsAuthenticatedCallerContinuity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	account := frozenDispatchTestLogicalAccount(
		"account-a",
		frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-a"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{account}}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration: 1,
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account-a",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}
	body := []byte(strings.Replace(
		string(frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-body")),
		`,"input":`,
		`,"previous_response_id":"rescue-response","input":`,
		1,
	))
	bound, err := bindRuntimeCallerCredentials(bytes.Repeat([]byte{0x42}, sha256.Size), []NormalCallerCredentialV1{{
		Domain:    NormalCallerCodex,
		Bearer:    "caller-token",
		SubjectID: "account-a\x00candidate-a\x00revision-a",
	}})
	if err != nil {
		t.Fatal(err)
	}
	caller := RuntimeCallerAuthorityV1{
		SchemaVersion:     1,
		Kind:              "provider_branch_admission_consumed_v1",
		Domain:            NormalCallerCodex,
		SubjectID:         bound[0].SubjectID,
		BearerFingerprint: "fingerprint",
		IndexEpoch:        1,
		AdmissionID:       strings.Repeat("a", 32),
		SingleUseNonce:    strings.Repeat("b", 32),
		RequestNonce:      strings.Repeat("c", 32),
		Method:            http.MethodPost,
		RequestURI:        "/responses",
		ValidUntil:        time.Now().Add(time.Hour),
		ConsumptionDigest: "consumption",
		MAC:               "mac",
	}
	var prepared CodexPreparedHTTPRequest
	var buildErr error
	handler := normalWorkerHandler(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		prepared, buildErr = factory.Build(request.Context(), CodexHTTPRequestPlanInput{Encoded: body})
	}), bound)
	request := httptest.NewRequest(http.MethodPost, "http://cq.test/responses", nil)
	request = request.WithContext(withRuntimeCallerAuthority(request.Context(), caller))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if buildErr != nil {
		t.Fatalf("Build through bound normal caller: %v", buildErr)
	}
	defer prepared.Frozen.Release()
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-a" {
		t.Fatalf("dispatch accounts = %#v, want authenticated account-a", accounts)
	}
	if !runtime.plan.RequiresAccountContinuity {
		t.Fatal("adopted rescue continuation was not durably account-bound")
	}
	if !runtime.plan.authenticatedCallerContinuity {
		t.Fatal("authenticated caller adoption was not carried into the durable request plan")
	}
}

func TestCodexHTTPRequestPlanFactoryCarriesAuthenticatedBoundRetryContinuity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 27, 5, 0, 0, 0, time.UTC)
	callerAccount := frozenDispatchTestLogicalAccount(
		"account-a",
		frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	boundAccount := frozenDispatchTestLogicalAccount(
		"account-b",
		frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-b"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{callerAccount, boundAccount}}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			Classification:    CodexRestoredLaneCurrent,
			BoundAccountKey:   "account-b",
			JournalGeneration: 1,
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account-a",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}
	body := []byte(strings.Replace(
		string(frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-body")),
		`,"input":`,
		`,"previous_response_id":"response-a","encrypted_content":"opaque","input":`,
		1,
	))
	bound, err := bindRuntimeCallerCredentials(bytes.Repeat([]byte{0x43}, sha256.Size), []NormalCallerCredentialV1{{
		Domain:    NormalCallerCodex,
		Bearer:    "caller-token",
		SubjectID: "account-a\x00candidate-a\x00revision-a",
	}})
	if err != nil {
		t.Fatal(err)
	}
	caller := RuntimeCallerAuthorityV1{
		SchemaVersion:     1,
		Kind:              "provider_branch_admission_consumed_v1",
		Domain:            NormalCallerCodex,
		SubjectID:         bound[0].SubjectID,
		BearerFingerprint: "fingerprint",
		IndexEpoch:        1,
		AdmissionID:       strings.Repeat("a", 32),
		SingleUseNonce:    strings.Repeat("b", 32),
		RequestNonce:      strings.Repeat("c", 32),
		Method:            http.MethodPost,
		RequestURI:        "/responses",
		ValidUntil:        time.Now().Add(time.Hour),
		ConsumptionDigest: "consumption",
		MAC:               "mac",
	}
	var prepared CodexPreparedHTTPRequest
	var buildErr error
	handler := normalWorkerHandler(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		prepared, buildErr = factory.Build(request.Context(), CodexHTTPRequestPlanInput{Encoded: body})
	}), bound)
	request := httptest.NewRequest(http.MethodPost, "http://cq.test/responses", nil)
	request = request.WithContext(withRuntimeCallerAuthority(request.Context(), caller))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if buildErr != nil {
		t.Fatalf("Build through bound normal caller: %v", buildErr)
	}
	defer prepared.Frozen.Release()
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-b" {
		t.Fatalf("bound retry dispatch = %#v, want account-b", accounts)
	}
	if !runtime.plan.RequiresAccountContinuity || !runtime.plan.authenticatedCallerContinuity {
		t.Fatalf("bound retry continuity = required %t authenticated %t, want true/true", runtime.plan.RequiresAccountContinuity, runtime.plan.authenticatedCallerContinuity)
	}
}

func TestCodexHTTPRequestPlanFactoryPreservesAuthenticatedContinuationAcrossSessionPool(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	privateSubject := "private-system-account"
	privateCandidate := "private-system-candidate"
	privateRevision := "private-system-revision"
	callerAccount := frozenDispatchTestLogicalAccount(
		codex.AccountKey(privateSubject),
		frozenDispatchCandidate(codex.AccountKey(privateSubject), codex.CandidateID(privateCandidate), codex.Revision(privateRevision), codex.SourceSystem, false, now.Add(time.Hour)),
	)
	boundAccount := frozenDispatchTestLogicalAccount(
		"account-b",
		frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceManaged, false, now.Add(time.Hour)),
	)
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-b"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{callerAccount, boundAccount}}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			Classification:        CodexRestoredLaneCurrent,
			BoundAccountKey:       "account-b",
			BoundIdentity:         CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 1, Authoritative: true},
			BoundRecordGeneration: 1,
			JournalGeneration:     1,
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account-b",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}
	key := []byte("01234567890123456789012345678901")
	factory.SessionPolicy = NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 7, EffectiveGeneration: 1,
		Pools:           []AccountPoolV1{{Name: "pool-b", Members: []codex.AccountKey{"account-b"}}},
		SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, []byte("session")), Pool: "pool-b"}},
	}))
	factory.DispatchPermits = &sessionPolicyPermitRecorder{}
	caller := RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: privateSubject, IndexEpoch: 42, ConsumptionDigest: strings.Repeat("a", 64),
	}
	ctx, diagnostics := withRouteDiagnostics(context.Background())
	ctx = withRuntimeCallerAuthority(ctx, caller)
	ctx = withRuntimeCallerIdentity(ctx, privateSubject+"\x00"+privateCandidate+"\x00"+privateRevision)

	prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "tool result without repeated turn state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-b" {
		t.Fatalf("continuation dispatch = %#v, want pool-bound account-b", accounts)
	}
	if !runtime.plan.RequiresAccountContinuity || !runtime.plan.authenticatedCallerContinuity {
		t.Fatalf("pool continuation = required %t authenticated %t, want true/true", runtime.plan.RequiresAccountContinuity, runtime.plan.authenticatedCallerContinuity)
	}
	event := RouteEvent{Provider: "codex"}
	event.applyRouteDiagnostics(diagnostics)
	if event.CallerDomain != string(NormalCallerCodex) || event.CallerIdentityPresent == nil || !*event.CallerIdentityPresent {
		t.Fatalf("caller trace = %#v, want Codex caller with identity", event)
	}
	if event.CallerContinuityMapped == nil || !*event.CallerContinuityMapped || event.CallerRoutingMapped == nil || *event.CallerRoutingMapped {
		t.Fatalf("caller mapping trace = %#v, want continuity=true routing=false", event)
	}
	if event.CallerIndexEpoch != 42 {
		t.Fatalf("caller index epoch = %d, want 42", event.CallerIndexEpoch)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{privateSubject, privateCandidate, privateRevision} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("caller trace exposed private value %q: %s", private, encoded)
		}
	}
}

func TestCodexHTTPRequestPlanFactoryRoutesAuthenticatedUnstableCallerThroughPinnedAccount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	callerAccount := frozenDispatchTestLogicalAccount(
		"unstable-caller",
		frozenDispatchCandidate("unstable-caller", "system-candidate", "system-revision", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	callerAccount.Unstable = true
	routingAccount := frozenDispatchTestLogicalAccount(
		"managed-route",
		frozenDispatchCandidate("managed-route", "managed-candidate", "managed-revision", codex.SourceManaged, true, now.Add(time.Hour)),
	)
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "managed-route"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{callerAccount, routingAccount}}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration: 1,
		}},
		Runtime:           runtime,
		DefaultAccountKey: "managed-route",
		PinnedAccountKey:  "managed-route",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}
	ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{Domain: NormalCallerCodex})
	ctx = withRuntimeCallerIdentity(ctx, "unstable-caller\x00system-candidate\x00system-revision")

	prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "managed-route" {
		t.Fatalf("dispatch = %#v, want pinned managed route", accounts)
	}
	if runtime.calls != 1 || runtime.plan.InitialSlot == 0 || runtime.plan.Slots[runtime.plan.InitialSlot-1].AccountKey != "managed-route" {
		t.Fatalf("lease begin = calls %d plan %#v, want pinned managed route", runtime.calls, runtime.plan)
	}
}

func TestCodexHTTPRequestPlanFactorySurvivesCallerRevisionRotation(t *testing.T) {
	t.Parallel()
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	factory := codexHTTPRequestPlanTestFactory(runtimeLease)
	factory.Routes = coordinator
	factory.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true}
	ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{Domain: NormalCallerCodex})
	ctx = withRuntimeCallerIdentity(ctx, "account\x00candidate\x00revision")

	first, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "first request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	first.Frozen.Release()
	admitted, err := first.leaseHandle.MarkDispatchedContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err = admitted.AdmitWebSocketContext(ctx, CodexWebSocketAdmissionEvidence{
		DownstreamGeneration: 1,
		UpstreamGeneration:   1,
		TurnState:            "private-websocket-turn-state",
		HasTurnState:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err = admitted.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{
			ResponseAnchor:    "response-a",
			HasResponseAnchor: true,
		},
		EndTurn: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitted.Drain(); err != nil {
		t.Fatal(err)
	}
	factory.Inventory.(*codexHTTPRequestPlanTestInventory).inventory.Accounts[0].Candidates[0].Revision = "refreshed-revision"

	continuationBody := frozenRequestBody("gpt-5", CodexRequestTurn, "tool result")
	continuation, err := factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: continuationBody})
	if err != nil {
		var planErr *CodexHTTPRequestPlanError
		if errors.As(err, &planErr) {
			t.Fatalf("continuation after WebSocket turn-state admission = stage %s reason %s", planErr.Code, planErr.Reason)
		}
		t.Fatalf("continuation after WebSocket turn-state admission = %T %v", err, err)
	}
	defer continuation.Frozen.Release()
	if continuation.leaseHandle.AccountKey() != "account" || continuation.leaseHandle.RequestGeneration() != 2 {
		t.Fatalf("continuation authority = account %q generation %d, want account/2", continuation.leaseHandle.AccountKey(), continuation.leaseHandle.RequestGeneration())
	}
	dispatched, err := continuation.leaseHandle.MarkDispatchedContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := dispatched.AdmitWebSocketContext(ctx, CodexWebSocketAdmissionEvidence{
		DownstreamGeneration: 1,
		UpstreamGeneration:   1,
		ResponseID:           "response-b",
		ResponseCreated:      true,
	})
	if err != nil {
		t.Fatalf("continuation admission on reconnected socket: %v", err)
	}
	continued, err = continued.ProviderCompleted(CodexHTTPCompletionEvidence{
		CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-b", HasResponseAnchor: true},
		EndTurn:                   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := continued.Drain(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHTTPRequestPlanFactoryRejectsAuthenticatedCallerWithoutRoutableCandidate(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	available := frozenDispatchTestLogicalAccount(
		"account-b",
		frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	blocked := frozenDispatchTestLogicalAccount(
		"account-a",
		frozenDispatchCandidate("account-a", "candidate-a", "revision-b", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	blocked.Candidates[0].DispatchBlocked = true

	for _, test := range []struct {
		name      string
		inventory codex.Inventory
	}{
		{name: "candidate absent", inventory: codex.Inventory{Accounts: []codex.LogicalAccount{available}}},
		{name: "candidate blocked", inventory: codex.Inventory{Accounts: []codex.LogicalAccount{blocked, available}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-b"}}
			factory := &CodexHTTPRequestPlanFactory{
				Inventory: &codexHTTPRequestPlanTestInventory{inventory: test.inventory},
				Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
					JournalGeneration: 1,
				}},
				Runtime:           runtime,
				DefaultAccountKey: "account-b",
				Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
				Now:               func() time.Time { return now },
			}
			ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{Domain: NormalCallerCodex})
			ctx = withRuntimeCallerIdentity(ctx, "account-a\x00candidate-a\x00revision-a")

			result, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
				Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
			})
			if !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
				t.Fatalf("Build error = %v, want authority mismatch", err)
			}
			if runtime.calls != 0 || result.Frozen != nil || result.Lifecycle != nil || result.leaseHandle != nil {
				t.Fatalf("rejected caller retained ownership: begin calls %d result %#v", runtime.calls, result)
			}
		})
	}
}

func TestCodexHTTPRequestPlanFactoryRoutesFreshAuthenticatedCallerWithinSessionPool(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-b"}}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
			frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour))),
		}}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration: 1,
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account-a",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}
	key := []byte("01234567890123456789012345678901")
	factory.SessionPolicy = NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 7, EffectiveGeneration: 1,
		Pools:           []AccountPoolV1{{Name: "pool-b", Members: []codex.AccountKey{"account-b"}}},
		SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, []byte("session")), Pool: "pool-b"}},
	}))
	factory.DispatchPermits = &sessionPolicyPermitRecorder{}
	caller := RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: "account-a", ConsumptionDigest: strings.Repeat("a", 64),
	}
	ctx := withRuntimeCallerAuthority(context.Background(), caller)
	ctx = withRuntimeCallerIdentity(ctx, "account-a\x00candidate-a\x00revision-a")

	prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-b" {
		t.Fatalf("dispatch = %#v, want account-b", accounts)
	}
	if runtime.calls != 1 || runtime.plan.RequiresAccountContinuity || runtime.plan.authenticatedCallerContinuity {
		t.Fatalf("fresh pool dispatch = calls %d required %t authenticated %t", runtime.calls, runtime.plan.RequiresAccountContinuity, runtime.plan.authenticatedCallerContinuity)
	}
}

func TestCodexHTTPRequestPlanFactoryRejectsUnverifiedCallerContinuity(t *testing.T) {
	t.Parallel()
	factory := codexHTTPRequestPlanTestFactory(&codexHTTPRequestPlanTestRuntime{})
	body := []byte(strings.Replace(
		string(frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")),
		`,"input":`,
		`,"previous_response_id":"rescue-response","input":`,
		1,
	))
	caller := RuntimeCallerAuthorityV1{Domain: NormalCallerCodex, SubjectID: "account\x00wrong-candidate\x00wrong-revision"}

	_, err := factory.Build(withRuntimeCallerAuthority(context.Background(), caller), CodexHTTPRequestPlanInput{Encoded: body})
	if !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("Build error = %v, want authority mismatch", err)
	}
}

func TestCodexHTTPRequestPlanFactoryReleasesInspectionBeforeFreeze(t *testing.T) {
	t.Parallel()

	privateCause := errors.New("private-inventory-cause")
	var inspected *CodexFrozenRequestInspection
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{err: privateCause},
		Routes:    &codexHTTPRequestPlanTestSnapshotter{},
		Runtime:   &codexHTTPRequestPlanTestRuntime{},
		Authority: CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
	}
	factory.operations.inspect = func(ctx context.Context, encoded []byte, headers http.Header) (*CodexFrozenRequestInspection, error) {
		var err error
		inspected, err = InspectCodexNativeRequest(ctx, encoded, headers)
		return inspected, err
	}

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if result.Frozen != nil || result.Lifecycle != nil {
		t.Fatalf("result retained ownership: %#v", result)
	}
	assertCodexHTTPRequestPlanError(t, err, CodexHTTPRequestPlanInventory, privateCause.Error())
	if _, protocolErr := inspected.Protocol(); !errors.Is(protocolErr, ErrCodexFrozenRequestReleased) {
		t.Fatalf("inspection was not released: %v", protocolErr)
	}
}

func TestCodexHTTPRequestPlanFactoryDerivesPreTurnCompactionCapacity(t *testing.T) {
	t.Parallel()

	handle := &CodexLeaseRequestHandle{account: "account"}
	runtime := &codexHTTPRequestPlanTestRuntime{handle: handle}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	body := []byte(strings.Replace(
		string(frozenRequestBody("gpt-5", CodexRequestCompaction, "private-body")),
		`"compaction":"standalone_turn"`,
		`"compaction":"pre_turn"`,
		1,
	))

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: body})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer result.Frozen.Release()

	wantBuckets := []CapacityBucket{CapacityBucketBase, CapacityBucketForModel(codexSparkModel)}
	if runtime.plan.RequestKind != CodexRequestCompaction || runtime.plan.CompactionPhase != CodexCompactionPreTurn || !slices.Equal(runtime.plan.RequiredBuckets, wantBuckets) {
		t.Fatalf("compaction lease plan = kind %q phase %q buckets %v, want pre-turn %v", runtime.plan.RequestKind, runtime.plan.CompactionPhase, runtime.plan.RequiredBuckets, wantBuckets)
	}
}

func TestCodexHTTPRequestPlanFactoryProjectsPrivateContinuityEvidence(t *testing.T) {
	t.Parallel()

	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account"}}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	snapshotter := factory.Routes.(*codexHTTPRequestPlanTestSnapshotter)
	snapshotter.snapshot.Classification = CodexRestoredLaneCurrent
	snapshotter.snapshot.BoundAccountKey = "account"
	body := []byte(strings.Replace(
		string(frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")),
		`,"input":`,
		`,"previous_response_id":"private-response","encrypted_content":"private-encrypted","input":`,
		1,
	))

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded: body,
		Headers: http.Header{"X-Codex-Turn-State": {"private-turn-state"}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer result.Frozen.Release()

	want := CodexLeaseRequestEvidence{
		PreviousResponseID: "private-response",
		TurnState:          "private-turn-state",
		HasTurnState:       true,
		HasEncryptedState:  true,
	}
	if runtime.plan.Evidence != want {
		t.Fatalf("evidence = %#v, want private continuity projection", runtime.plan.Evidence)
	}
	if !runtime.plan.RequiresAccountContinuity {
		t.Fatal("private continuity evidence did not freeze the durable account plan")
	}
}

func TestCodexHTTPRequestPlanFactoryReleasesFrozenOnBeginFailure(t *testing.T) {
	t.Parallel()

	privateCause := errors.New("private-runtime-cause")
	var frozen *CodexFrozenRequest
	factory := codexHTTPRequestPlanTestFactory(&codexHTTPRequestPlanTestRuntime{err: privateCause})
	factory.operations.freeze = func(ctx context.Context, inspection *CodexFrozenRequestInspection, choice RouteChoice, headroom CodexRequestHeadroom, mode HeadroomMode) (*CodexFrozenRequest, error) {
		var err error
		frozen, err = inspection.Freeze(ctx, choice, headroom, mode)
		return frozen, err
	}

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if result.Frozen != nil || result.Lifecycle != nil {
		t.Fatalf("result retained ownership: %#v", result)
	}
	assertCodexHTTPRequestPlanError(t, err, CodexHTTPRequestPlanBegin, privateCause.Error())
	if _, protocolErr := frozen.Protocol(); !errors.Is(protocolErr, ErrCodexFrozenRequestReleased) {
		t.Fatalf("frozen request was not released: %v", protocolErr)
	}
}

func TestCodexHTTPRequestPlanFactoryWaitsForLongRunningPredecessor(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	runtime := &codexHTTPRequestPlanBlockingRuntime{
		release: release,
		began:   make(chan struct{}, 1),
		handle:  &CodexLeaseRequestHandle{account: "account"},
	}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	key := []byte("01234567890123456789012345678901")
	factory.SessionPolicy = NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:           []AccountPoolV1{{Name: "team", Members: []codex.AccountKey{"account"}}},
		SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, []byte("session")), Pool: "team"}},
	}))
	permits := &sessionPolicyPermitRecorder{}
	factory.DispatchPermits = permits
	inspectCalls := 0
	freezeCalls := 0
	factory.operations.inspect = func(ctx context.Context, encoded []byte, headers http.Header) (*CodexFrozenRequestInspection, error) {
		inspectCalls++
		return InspectCodexNativeRequest(ctx, encoded, headers)
	}
	factory.operations.freeze = func(ctx context.Context, inspection *CodexFrozenRequestInspection, choice RouteChoice, headroom CodexRequestHeadroom, mode HeadroomMode) (*CodexFrozenRequest, error) {
		freezeCalls++
		return inspection.Freeze(ctx, choice, headroom, mode)
	}

	type buildResult struct {
		prepared CodexPreparedHTTPRequest
		err      error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = withRuntimeCallerAuthority(ctx, RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: "caller", ConsumptionDigest: strings.Repeat("a", 64),
	})
	ctx = withRuntimeCallerIdentity(ctx, "account\x00candidate\x00revision")
	result := make(chan buildResult, 1)
	go func() {
		prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
			Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
		})
		result <- buildResult{prepared: prepared, err: err}
	}()

	select {
	case <-runtime.began:
	case <-ctx.Done():
		t.Fatal("successor never began waiting for predecessor")
	}
	<-time.After(300 * time.Millisecond)
	select {
	case got := <-result:
		close(release)
		t.Fatalf("successor returned before predecessor completed: %v", got.err)
	default:
	}
	close(release)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("build after predecessor completed: %v", got.err)
		}
		if got.prepared.Frozen == nil || got.prepared.Lifecycle == nil {
			t.Fatalf("prepared result = %#v", got.prepared)
		}
		got.prepared.Frozen.Release()
		inventory := factory.Inventory.(*codexHTTPRequestPlanTestInventory)
		snapshotter := factory.Routes.(*codexHTTPRequestPlanTestSnapshotter)
		if inspectCalls != 1 || inventory.calls != 1 || snapshotter.calls != 1 || freezeCalls != 1 || len(permits.requests) != 1 {
			t.Fatalf("one-shot planning calls = inspect %d inventory %d snapshot %d freeze %d permits %d", inspectCalls, inventory.calls, snapshotter.calls, freezeCalls, len(permits.requests))
		}
		if runtime.calls != 1 {
			t.Fatalf("begin calls = %d, want one after predecessor drains", runtime.calls)
		}
	case <-ctx.Done():
		t.Fatal("successor did not begin after predecessor completed")
	}
}

func TestCodexHTTPRequestPlanFactoryPreservesQueuedContinuityAcrossWebSocketRotation(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}})
	plan.Key.Lane.Session = "session"
	plan.Key.Lane.Thread = "thread"
	plan.RequestedModel = "gpt-5"
	plan.EffectiveModel = "gpt-5"

	turn, err := runtime.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turn.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turn.AdmitHTTP2xx()
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turn.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: false})
	if err != nil {
		t.Fatal(err)
	}
	if turn, err = turn.Drain(); err != nil {
		t.Fatal(err)
	}

	midTurn := plan
	midTurn.RequestKind = CodexRequestCompaction
	midTurn.CompactionPhase = CodexCompactionMidTurn
	predecessor, err := runtime.BeginRequest(midTurn)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err = predecessor.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	webSocketLifecycle, err := newCodexWSLifecycle(predecessor, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	const queuedTurnState = "private-ws-state-a"
	if err := webSocketLifecycle.ObserveUpstreamUpgrade(context.Background(), 1, queuedTurnState); err != nil {
		t.Fatal(err)
	}

	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.Routes = coordinator
	factory.Authority = plan.Authority
	inventoryEntered := make(chan struct{})
	inventoryRelease := make(chan struct{})
	factory.Inventory = &codexHTTPRequestPlanBlockingInventory{
		inner:   factory.Inventory,
		entered: inventoryEntered,
		release: inventoryRelease,
	}
	body := bytes.Replace(
		frozenRequestBody("gpt-5", CodexRequestCompaction, "private-body"),
		[]byte(`"compaction":"standalone_turn"`),
		[]byte(`"compaction":"pre_turn"`),
		1,
	)
	type buildResult struct {
		prepared CodexPreparedHTTPRequest
		err      error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = withRuntimeCallerAuthority(ctx, RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: "caller", ConsumptionDigest: strings.Repeat("a", 64),
	})
	ctx = withRuntimeCallerIdentity(ctx, "account\x00candidate\x00revision")
	result := make(chan buildResult, 1)
	go func() {
		prepared, buildErr := factory.Build(ctx, CodexHTTPRequestPlanInput{
			Encoded: body,
			Headers: http.Header{"X-Codex-Turn-State": {queuedTurnState}},
		})
		result <- buildResult{prepared: prepared, err: buildErr}
	}()

	select {
	case <-inventoryEntered:
	case <-ctx.Done():
		t.Fatal("HTTP successor did not reach credential inventory")
	}
	select {
	case got := <-result:
		t.Fatalf("HTTP successor returned before credential inventory resumed: %v", got.err)
	default:
	}
	if _, err := webSocketLifecycle.ObserveFrame(
		context.Background(),
		1,
		[]byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"private-ws-state-b"}}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := webSocketLifecycle.ObserveFrame(
		context.Background(),
		1,
		[]byte(`{"type":"response.completed","response":{"id":"response-a","end_turn":false}}`),
	); err != nil {
		t.Fatal(err)
	}
	if err := webSocketLifecycle.Drain(); err != nil {
		t.Fatal(err)
	}
	close(inventoryRelease)

	select {
	case got := <-result:
		if got.err != nil {
			var planErr *CodexHTTPRequestPlanError
			if !errors.As(got.err, &planErr) || planErr.Reason != CodexRequestFailureReason(codexContinuityTurnStateMismatch) {
				t.Fatalf("build after durable predecessor drained = %T %v, want success or current turn-state mismatch", got.err, got.err)
			}
			t.Fatalf("queued successor state was invalidated while waiting: %s", planErr.Reason)
		}
		if got.prepared.Frozen == nil || got.prepared.Lifecycle == nil {
			t.Fatalf("prepared result = %#v", got.prepared)
		}
		if got.prepared.leaseHandle == nil || got.prepared.leaseHandle.RequestGeneration() != 3 {
			t.Fatalf("successor generation = %v, want 3", got.prepared.leaseHandle)
		}
		if _, err := got.prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		got.prepared.Frozen.Release()
	case <-ctx.Done():
		t.Fatal("HTTP successor did not begin after WebSocket predecessor drained")
	}
}

func TestCodexLeaseRuntimePlanningGateSerialisesExactLaneOnly(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn-a", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account", CandidateID: "candidate", Kind: CodexAttemptSlotDirect,
	}})
	first := plan.Key
	first.Lane = LaneKey{Session: "session-a", Thread: "thread-a", Namespace: CodexResponsesNamespace}
	firstDigest := sha256.Sum256([]byte(first.Lane.Session + "\x00" + first.Lane.Thread + "\x00" + first.Lane.Namespace))
	second := first
	second.Turn = "turn-b"
	for index := 0; ; index++ {
		second.Lane = LaneKey{Session: "session-b", Thread: fmt.Sprintf("thread-%d", index), Namespace: CodexResponsesNamespace}
		digest := sha256.Sum256([]byte(second.Lane.Session + "\x00" + second.Lane.Thread + "\x00" + second.Lane.Namespace))
		if digest[0] == firstDigest[0] {
			break
		}
	}

	releaseFirst, err := runtime.AcquireRequestPlanningContext(context.Background(), first, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	secondContext, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	defer cancelSecond()
	releaseSecond, err := runtime.AcquireRequestPlanningContext(secondContext, second, plan.Accounts, plan.Authority)
	if err != nil {
		t.Fatalf("unrelated colliding lane blocked: %v", err)
	}
	releaseSecond()

	sameLane := first
	sameLane.Turn = "turn-c"
	type acquisition struct {
		release func()
		err     error
	}
	acquired := make(chan acquisition, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		release, acquireErr := runtime.AcquireRequestPlanningContext(ctx, sameLane, plan.Accounts, plan.Authority)
		acquired <- acquisition{release: release, err: acquireErr}
	}()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for codexHTTPRequestPlanPlanningGateReferences(runtime, first.Lane) != 2 {
		select {
		case <-time.After(time.Millisecond):
		case <-deadline.C:
			t.Fatal("same-lane waiter did not queue")
		}
	}
	select {
	case got := <-acquired:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("same lane acquired concurrently: %v", got.err)
	default:
	}
	releaseFirst()

	select {
	case got := <-acquired:
		if got.err != nil {
			t.Fatal(got.err)
		}
		got.release()
	case <-ctx.Done():
		t.Fatal("same lane did not acquire after release")
	}
	if refs := codexHTTPRequestPlanPlanningGateReferences(runtime, first.Lane); refs != 0 {
		t.Fatalf("released lane retained %d planning references", refs)
	}
}

func TestCodexHTTPRequestPlanFactoryStopsWaitingForCancelledSuccessor(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	runtime := &codexHTTPRequestPlanBlockingRuntime{
		release: release,
		began:   make(chan struct{}, 1),
		handle:  &CodexLeaseRequestHandle{account: "account"},
	}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
			Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
		})
		result <- err
	}()

	select {
	case <-runtime.began:
	case <-time.After(time.Second):
		t.Fatal("successor never began waiting for predecessor")
	}
	cancel()

	select {
	case err := <-result:
		assertCodexHTTPRequestPlanError(t, err, CodexHTTPRequestPlanBegin, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
		if runtime.calls != 0 {
			t.Fatalf("begin calls = %d, want none after cancelled wait", runtime.calls)
		}
	case <-time.After(time.Second):
		t.Fatal("successor wait ignored context cancellation")
	}
}

func TestCodexHTTPRequestPlanFactoryAbandonsCommittedHandleAfterCancellation(t *testing.T) {
	t.Parallel()

	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	preparedPlan := codexLeaseRuntimeTestPlan("factory-cleanup", []CodexLeaseAttemptSlotPlan{{
		AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect,
	}})
	prepared, err := runtimeLease.BeginRequest(preparedPlan)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	privateCause := errors.New("private-runtime-cause")
	runtime := &codexHTTPRequestPlanTestRuntime{
		handle:       prepared,
		err:          privateCause,
		beforeReturn: cancel,
	}
	factory := codexHTTPRequestPlanTestFactory(runtime)

	result, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if result.Frozen != nil || result.Lifecycle != nil {
		t.Fatalf("result retained ownership: %#v", result)
	}
	assertCodexHTTPRequestPlanError(t, err, CodexHTTPRequestPlanBegin, privateCause.Error())
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("build context = %v, want cancellation before cleanup", ctx.Err())
	}

	nextPlan := preparedPlan
	nextPlan.Accounts = []codex.AccountKey{"account-b"}
	nextPlan.Slots = []CodexLeaseAttemptSlotPlan{{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect}}
	next, beginErr := runtimeLease.BeginRequest(nextPlan)
	if beginErr != nil {
		t.Fatalf("BeginRequest after factory cleanup: %v", beginErr)
	}
	if next.RequestGeneration() != 2 || next.AccountKey() != "account-b" {
		t.Fatalf("request after cleanup = account %q generation %d", next.AccountKey(), next.RequestGeneration())
	}
}

func TestCodexHTTPRequestPlanFactoryErrorsAreTypedPrivateAndPreserveCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	factory := codexHTTPRequestPlanTestFactory(&codexHTTPRequestPlanTestRuntime{})
	_, err := factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")})
	assertCodexHTTPRequestPlanError(t, err, CodexHTTPRequestPlanInspect, "private-body")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not preserve cancellation: %v", err)
	}
}

func TestCodexHTTPRequestPlanFailureProjectsSafeContinuityReason(t *testing.T) {
	t.Parallel()
	private := "private-continuity-cause"
	err := newCodexHTTPRequestPlanError(
		CodexHTTPRequestPlanBegin,
		fmt.Errorf("%w: %s", &codexContinuityError{reason: codexContinuityPreviousResponseMismatch}, private),
	)
	var planErr *CodexHTTPRequestPlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("error = %T, want plan error", err)
	}
	want := CodexRequestFailureReason(codexContinuityPreviousResponseMismatch)
	if planErr.Reason != want {
		t.Fatalf("reason = %q, want %q", planErr.Reason, want)
	}
	if strings.Contains(planErr.Error(), private) {
		t.Fatal("plan error exposed private continuity cause")
	}
}

func TestCodexHTTPRequestPlanFactoryEnrichesOnlyHTTPDiagnosticsWithoutEmitting(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		transportKind string
		wantLineage   string
		wantModel     string
	}{
		{name: "HTTP", transportKind: "http", wantLineage: "previous_response_id_present", wantModel: "gpt_5_6_sol"},
		{name: "WebSocket", transportKind: "websocket"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			factory := codexHTTPRequestPlanTestFactory(&codexHTTPRequestPlanTestRuntime{})
			factory.TransportKind = test.transportKind
			factory.Routes = &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{}}
			ctx, diagnostics := withRouteDiagnostics(context.Background())
			body := []byte(strings.TrimSuffix(string(frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-prompt")), "}") + `,"previous_response_id":"private-response","reasoning":{"effort":"high"}}`)

			_, _ = factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: body})

			event := RouteEvent{}
			event.applyRouteDiagnostics(diagnostics)
			if event.RequestLineage != test.wantLineage || event.RequestedModelClass != test.wantModel {
				t.Fatalf("request shape = %+v, want lineage %q model %q", event, test.wantLineage, test.wantModel)
			}
			if event.RequestedReasoningEffort != map[bool]string{true: "high"}[test.transportKind == "http"] {
				t.Fatalf("reasoning effort = %q", event.RequestedReasoningEffort)
			}
		})
	}
}

func TestCodexHTTPRequestPlanFactoryPreparationRetryOverwritesShapeWithoutEmission(t *testing.T) {
	t.Parallel()

	factory := codexHTTPRequestPlanTestFactory(&codexHTTPRequestPlanTestRuntime{})
	factory.TransportKind = "http"
	factory.Routes = &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{}}
	inspectCalls := 0
	factory.operations.inspect = func(ctx context.Context, encoded []byte, header http.Header) (*CodexFrozenRequestInspection, error) {
		inspectCalls++
		inspection, err := InspectCodexNativeRequest(ctx, encoded, header)
		if err == nil && inspectCalls == 2 {
			inspection.Release()
		}
		return inspection, err
	}
	ctx, diagnostics := withRouteDiagnostics(context.Background())
	noteCodexObservation(ctx, codexObservationFields{LeasePhase: "prepared"})
	first := []byte(strings.TrimSuffix(string(frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-first")), "}") + `,"reasoning":{"effort":"high"}}`)
	second := []byte(strings.TrimSuffix(string(frozenRequestBody("gpt-5.6-terra", CodexRequestTurn, "private-second")), "}") + `,"previous_response_id":"private-id","reasoning":{"effort":"low"}}`)

	_, _ = factory.buildOnce(ctx, CodexHTTPRequestPlanInput{Encoded: first})
	_, _ = factory.buildOnce(ctx, CodexHTTPRequestPlanInput{Encoded: second})

	event := RouteEvent{}
	event.applyRouteDiagnostics(diagnostics)
	if event.RequestKind != "" || event.RequestLineage != "unknown" || event.RequestedReasoningEffort != "unknown" || event.RequestedModelClass != "unknown" || event.CompactionPhase != "unknown" || event.LeasePhase != "prepared" {
		t.Fatalf("retried request shape = %+v", event)
	}
}

func TestCodexHTTPRequestPlanFactoryAppliesUnknownShapeOnProtocolError(t *testing.T) {
	t.Parallel()
	factory := codexHTTPRequestPlanTestFactory(&codexHTTPRequestPlanTestRuntime{})
	factory.TransportKind = "http"
	factory.operations.inspect = func(ctx context.Context, encoded []byte, header http.Header) (*CodexFrozenRequestInspection, error) {
		inspection, err := InspectCodexNativeRequest(ctx, encoded, header)
		if err == nil {
			inspection.Release()
		}
		return inspection, err
	}
	ctx, diagnostics := withRouteDiagnostics(context.Background())
	_, _ = factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: frozenRequestBody("gpt-5.6-sol", CodexRequestTurn, "private-body")})
	event := RouteEvent{}
	event.applyRouteDiagnostics(diagnostics)
	if event.RequestLineage != "unknown" || event.RequestedReasoningEffort != "unknown" || event.RequestedModelClass != "unknown" || event.CompactionPhase != "unknown" {
		t.Fatalf("protocol error shape = %+v", event)
	}
}

func TestCodexHTTPRequestPlanFactoryRejectsInvalidRouteSnapshotBeforeFreeze(t *testing.T) {
	t.Parallel()

	routes := &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{JournalGeneration: 0}}
	runtime := &codexHTTPRequestPlanTestRuntime{}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.Routes = routes
	freezeCalls := 0
	factory.operations.freeze = func(context.Context, *CodexFrozenRequestInspection, RouteChoice, CodexRequestHeadroom, HeadroomMode) (*CodexFrozenRequest, error) {
		freezeCalls++
		return nil, errors.New("unexpected freeze")
	}

	_, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")})
	assertCodexHTTPRequestPlanError(t, err, CodexHTTPRequestPlanRouteSnapshot, "private-body")
	if freezeCalls != 0 || runtime.calls != 0 || routes.calls != 1 {
		t.Fatalf("calls: snapshot=%d freeze=%d begin=%d", routes.calls, freezeCalls, runtime.calls)
	}
}

func TestCodexHTTPRequestPlanFactoryProbeRetainedClaimsOnlyExactAuthoritativeBinding(t *testing.T) {
	t.Parallel()

	factory := codexHTTPRequestPlanTestFactory(&codexHTTPRequestPlanTestRuntime{})
	factory.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 9, RetainedAuthoritativeEpochs: []uint64{7}}
	identity := CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true}
	routes := &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
		Classification:        CodexRestoredLaneCurrent,
		BoundAccountKey:       "account",
		BoundIdentity:         identity,
		BoundRecordGeneration: 12,
		JournalGeneration:     13,
	}}
	factory.Routes = routes
	input := CodexHTTPRequestPlanInput{Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")}

	claim, claimed, err := factory.ProbeRetained(context.Background(), input)
	if err != nil || !claimed || claim == nil || claim.ExpectedBound.Identity != identity || claim.ExpectedBound.AccountKey != "account" || claim.ExpectedBound.RecordGeneration != 12 {
		t.Fatalf("retained probe = %#v, claimed=%t, err=%v", claim, claimed, err)
	}
	claim.release()
	if routes.calls != 1 {
		t.Fatalf("route snapshot calls = %d, want 1", routes.calls)
	}

	routes.snapshot.BoundIdentity.Authoritative = false
	if expected, claimed, err := factory.ProbeRetained(context.Background(), input); err != nil || claimed || expected != nil {
		t.Fatalf("shadow probe = %#v, claimed=%t, err=%v", expected, claimed, err)
	}

	routes.snapshot.Classification = CodexRestoredLaneHistorical
	routes.snapshot.BoundIdentity = CodexJournalRecordIdentity{}
	routes.snapshot.HistoricalAuthoritative = true
	if expected, claimed, err := factory.ProbeRetained(context.Background(), input); expected != nil || !claimed || !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("historical probe = %#v, claimed=%t, err=%v", expected, claimed, err)
	}
	routes.snapshot.HistoricalAuthoritative = false
	if expected, claimed, err := factory.ProbeRetained(context.Background(), input); expected != nil || claimed || err != nil {
		t.Fatalf("historical shadow probe = %#v, claimed=%t, err=%v", expected, claimed, err)
	}
}

func TestCodexHTTPRequestPlanFactoryRejectsChangedExpectedBoundBeforeDecision(t *testing.T) {
	t.Parallel()

	runtime := &codexHTTPRequestPlanTestRuntime{}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	routes := &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
		Classification:        CodexRestoredLaneCurrent,
		BoundAccountKey:       "account",
		BoundIdentity:         CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true},
		BoundRecordGeneration: 13,
		JournalGeneration:     14,
	}}
	factory.Routes = routes
	planCalls := 0
	factory.operations.buildDispatch = func(context.Context, CodexFrozenDispatchInput) (CodexFrozenDispatchPlan, error) {
		planCalls++
		return CodexFrozenDispatchPlan{}, errors.New("unexpected route decision")
	}
	expected := &CodexLeaseBoundExpectation{
		Identity:         routes.snapshot.BoundIdentity,
		AccountKey:       "account",
		RecordGeneration: 12,
	}

	_, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded:       frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
		ExpectedBound: expected,
	})
	assertCodexHTTPRequestPlanError(t, err, CodexHTTPRequestPlanRouteSnapshot, "private-body")
	if planCalls != 0 || runtime.calls != 0 {
		t.Fatalf("route decision/begin calls = %d/%d, want 0/0", planCalls, runtime.calls)
	}
}

func TestCodexHTTPRequestPlanFactoryCarriesMatchingExpectedBound(t *testing.T) {
	t.Parallel()

	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account"}}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	identity := CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true}
	expected := &CodexLeaseBoundExpectation{Identity: identity, AccountKey: "account", RecordGeneration: 12}
	factory.Routes = &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
		Classification:        CodexRestoredLaneCurrent,
		BoundAccountKey:       "account",
		BoundIdentity:         identity,
		BoundRecordGeneration: 12,
		BoundChoice: RouteChoice{
			AccountKey:      "account",
			EffectiveModel:  "gpt-5",
			RequiredBuckets: []CapacityBucket{CapacityBucketBase},
		},
		JournalGeneration: 13,
	}}

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded:       frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
		ExpectedBound: expected,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Frozen.Release()
	if runtime.plan.ExpectedBound == nil || *runtime.plan.ExpectedBound != *expected || runtime.plan.ExpectedBound == expected {
		t.Fatalf("runtime expected bound = %#v, want detached %#v", runtime.plan.ExpectedBound, expected)
	}
}

func TestCodexHTTPRequestPlanFactoryCarriesExpectedBoundAcrossRequestBuckets(t *testing.T) {
	t.Parallel()

	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account"}}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	identity := CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true}
	expected := &CodexLeaseBoundExpectation{Identity: identity, AccountKey: "account", RecordGeneration: 12}
	factory.Routes = &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
		Classification:        CodexRestoredLaneCurrent,
		BoundAccountKey:       "account",
		BoundIdentity:         identity,
		BoundRecordGeneration: 12,
		BoundChoice: RouteChoice{
			AccountKey:      "account",
			EffectiveModel:  "gpt-5",
			RequiredBuckets: []CapacityBucket{CapacityBucketBase, CapacityBucketForModel(codexSparkModel)},
		},
		JournalGeneration: 13,
	}}

	result, err := factory.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded:       frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
		ExpectedBound: expected,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Frozen.Release()
	if runtime.plan.ExpectedBound == nil || *runtime.plan.ExpectedBound != *expected {
		t.Fatalf("runtime expected bound = %#v, want %#v", runtime.plan.ExpectedBound, expected)
	}
}

type codexHTTPRequestPlanTestInventory struct {
	inventory codex.Inventory
	err       error
	calls     int
	events    *[]string
}

type codexHTTPRequestPlanBlockingInventory struct {
	inner   codex.CredentialInventory
	entered chan struct{}
	release <-chan struct{}
}

func (inventory *codexHTTPRequestPlanBlockingInventory) List(ctx context.Context) (codex.Inventory, error) {
	close(inventory.entered)
	select {
	case <-inventory.release:
	case <-ctx.Done():
		return codex.Inventory{}, ctx.Err()
	}
	return inventory.inner.List(ctx)
}

func (inventory *codexHTTPRequestPlanTestInventory) List(context.Context) (codex.Inventory, error) {
	inventory.calls++
	if inventory.events != nil {
		*inventory.events = append(*inventory.events, "inventory")
	}
	return inventory.inventory, inventory.err
}

type codexHTTPRequestPlanTestSnapshotter struct {
	snapshot  CodexLeaseRouteSnapshot
	err       error
	calls     int
	key       LeaseKey
	accounts  []codex.AccountKey
	authority CodexLeaseAuthorityPolicy
	events    *[]string
}

func (snapshotter *codexHTTPRequestPlanTestSnapshotter) LoadRouteSnapshot(_ context.Context, key LeaseKey, accounts []codex.AccountKey, authority CodexLeaseAuthorityPolicy) (CodexLeaseRouteSnapshot, error) {
	snapshotter.calls++
	if snapshotter.events != nil {
		*snapshotter.events = append(*snapshotter.events, "snapshot")
	}
	snapshotter.key = key
	snapshotter.accounts = append([]codex.AccountKey(nil), accounts...)
	snapshotter.authority = cloneCodexLeaseAuthorityPolicy(authority)
	return snapshotter.snapshot, snapshotter.err
}

type codexHTTPRequestPlanTestRuntime struct {
	handle       *CodexLeaseRequestHandle
	err          error
	errs         []error
	calls        int
	plan         CodexLeaseRequestPlan
	events       *[]string
	beforeReturn func()
}

type codexHTTPRequestPlanBlockingRuntime struct {
	release <-chan struct{}
	began   chan struct{}
	handle  *CodexLeaseRequestHandle
	calls   int
}

func (runtime *codexHTTPRequestPlanBlockingRuntime) AcquireRequestPlanningContext(ctx context.Context, _ LeaseKey, _ []codex.AccountKey, _ CodexLeaseAuthorityPolicy) (func(), error) {
	select {
	case runtime.began <- struct{}{}:
	default:
	}
	select {
	case <-runtime.release:
		return func() {}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (runtime *codexHTTPRequestPlanBlockingRuntime) BeginRequestContext(_ context.Context, _ CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error) {
	runtime.calls++
	return runtime.handle, nil
}

func (runtime *codexHTTPRequestPlanTestRuntime) BeginRequestContext(_ context.Context, plan CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error) {
	runtime.calls++
	if runtime.events != nil {
		*runtime.events = append(*runtime.events, "begin")
	}
	runtime.plan = plan
	if runtime.beforeReturn != nil {
		runtime.beforeReturn()
	}
	err := runtime.err
	sequenced := len(runtime.errs) > 0
	if len(runtime.errs) > 0 {
		err = runtime.errs[0]
		runtime.errs = runtime.errs[1:]
	}
	if sequenced && err != nil {
		return nil, err
	}
	return runtime.handle, err
}

func codexHTTPRequestPlanTestFactory(runtime CodexHTTPRequestPlanRuntime) *CodexHTTPRequestPlanFactory {
	now := time.Unix(1_700_000_000, 0).UTC()
	account := frozenDispatchTestLogicalAccount("account",
		frozenDispatchCandidate("account", "candidate", "revision", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	return &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{account}}},
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
			JournalGeneration: 1,
		}},
		Runtime:           runtime,
		DefaultAccountKey: "account",
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               func() time.Time { return now },
	}
}

func codexHTTPRequestPlanPlanningGateHeld(runtime *CodexLeaseRuntime, lane LaneKey) bool {
	if runtime == nil || runtime.planningGates == nil {
		return false
	}
	runtime.planningGates.mu.Lock()
	defer runtime.planningGates.mu.Unlock()
	entry := runtime.planningGates.entries[lane]
	return entry != nil && len(entry.token) == 0
}

func codexHTTPRequestPlanPlanningGateReferences(runtime *CodexLeaseRuntime, lane LaneKey) int {
	if runtime == nil || runtime.planningGates == nil {
		return 0
	}
	runtime.planningGates.mu.Lock()
	defer runtime.planningGates.mu.Unlock()
	entry := runtime.planningGates.entries[lane]
	if entry == nil {
		return 0
	}
	return entry.refs
}

func assertCodexHTTPRequestPlanError(t *testing.T, err error, code CodexHTTPRequestPlanErrorCode, private string) {
	t.Helper()
	var planErr *CodexHTTPRequestPlanError
	if !errors.As(err, &planErr) || planErr.Code != code {
		t.Fatalf("error = %#v, want code %q", err, code)
	}
	if private != "" {
		for _, formatted := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
			if strings.Contains(formatted, private) {
				t.Fatalf("error exposed private material: %s", formatted)
			}
		}
	}
}
