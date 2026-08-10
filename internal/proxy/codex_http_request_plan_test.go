package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
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
		{name: "warm unresolved account fails closed", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(-time.Minute), unresolved: true, wantErr: ErrCodexLeaseAuthorityMismatch},
		{name: "unknown-policy unresolved account fails closed", model: "gpt-5.7-codex", affinityModel: "gpt-5.7-codex", cacheAdmittedAt: now.Add(-time.Hour), unresolved: true, wantErr: ErrCodexLeaseAuthorityMismatch},
		{name: "private-policy unresolved account fails closed", model: "gpt-5.6-private", affinityModel: "gpt-5.6-private", cacheAdmittedAt: now.Add(-time.Hour), unresolved: true, wantErr: ErrCodexLeaseAuthorityMismatch},
		{name: "future cache clock unresolved account fails closed", model: "gpt-5.6-sol", affinityModel: "gpt-5.6-sol", cacheAdmittedAt: now.Add(time.Minute), unresolved: true, wantErr: ErrCodexLeaseAuthorityMismatch},
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

	expected, claimed, err := factory.ProbeRetained(context.Background(), input)
	if err != nil || !claimed || expected == nil || expected.Identity != identity || expected.AccountKey != "account" || expected.RecordGeneration != 12 {
		t.Fatalf("retained probe = %#v, claimed=%t, err=%v", expected, claimed, err)
	}
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

type codexHTTPRequestPlanTestInventory struct {
	inventory codex.Inventory
	err       error
	calls     int
	events    *[]string
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
	calls        int
	plan         CodexLeaseRequestPlan
	events       *[]string
	beforeReturn func()
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
	return runtime.handle, runtime.err
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
