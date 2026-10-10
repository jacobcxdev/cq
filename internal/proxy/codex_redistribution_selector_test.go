package proxy

import (
	"context"
	"slices"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexRedistributionInventoryKeepsOnlyUsableIncludedQuota(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	inventory := codex.Inventory{}
	for _, key := range []codex.AccountKey{"active", "positive", "unknown", "depleted", "blocked", "unstable", "quota-rejected"} {
		account := frozenDispatchTestLogicalAccount(key, frozenDispatchCandidate(key, codex.CandidateID("candidate-"+key), "revision", codex.SourceSystem, false, now.Add(time.Hour)))
		if key == "blocked" {
			account.Candidates[0].DispatchBlocked = true
		}
		if key == "unstable" {
			account.Unstable = true
		}
		inventory.Accounts = append(inventory.Accounts, account)
		if key != "unknown" {
			remaining := 80
			if key == "depleted" {
				remaining = 0
			}
			frozenDispatchObserveCapacity(t, ledger, key, CapacityBucketBase, remaining, now)
		}
	}
	factory := &CodexHTTPRequestPlanFactory{Capacity: ledger, Now: func() time.Time { return now }}
	selected := factory.redistributionInventory(context.Background(), CodexProtocolRequest{Model: "gpt-5"}, CodexLeaseRouteSnapshot{
		BoundAccountKey: "active", QuotaExhaustedAccountKeys: []codex.AccountKey{"quota-rejected"},
	}, inventory)
	if got := codexHTTPRequestPlanAccountKeys(selected); !slices.Equal(got, []codex.AccountKey{"active", "positive", "quota-rejected"}) {
		t.Fatalf("redistribution accounts = %v, want every usable account with fresh positive quota including historical exhaustion", got)
	}
}

func TestCodexRedistributionInventoryHonoursPinsAndSessionPool(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	inventory := codex.Inventory{}
	for _, key := range []codex.AccountKey{"outside", "inside"} {
		inventory.Accounts = append(inventory.Accounts, frozenDispatchTestLogicalAccount(key,
			frozenDispatchCandidate(key, codex.CandidateID("candidate-"+key), "revision", codex.SourceSystem, false, now.Add(time.Hour))))
		frozenDispatchObserveCapacity(t, ledger, key, CapacityBucketBase, 80, now)
	}
	protocol := CodexProtocolRequest{Model: "gpt-5", Metadata: CodexTurnMetadataResult{Metadata: CodexTurnMetadata{SessionID: "session"}}}
	key := []byte("01234567890123456789012345678901")
	factory := &CodexHTTPRequestPlanFactory{
		Capacity: ledger, Now: func() time.Time { return now },
		SessionPolicy: NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
			SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 7, EffectiveGeneration: 1,
			Pools:           []AccountPoolV1{{Name: "allowed", Members: []codex.AccountKey{"inside"}}},
			SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, []byte("session")), Pool: "allowed"}},
		})),
	}
	ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{Domain: NormalCallerLocal, SubjectID: "local"})
	selected := factory.redistributionInventory(ctx, protocol, CodexLeaseRouteSnapshot{BoundAccountKey: "outside"}, inventory)
	if got := codexHTTPRequestPlanAccountKeys(selected); !slices.Equal(got, []codex.AccountKey{"inside"}) {
		t.Fatalf("redistribution pool accounts = %v, want inside", got)
	}
	factory.PinnedAccountKey = "outside"
	if got := factory.redistributionInventory(ctx, protocol, CodexLeaseRouteSnapshot{}, inventory); len(got.Accounts) != 0 {
		t.Fatalf("pinned redistribution accounts = %v, want no automatic replacement", codexHTTPRequestPlanAccountKeys(got))
	}
}

func TestCodexRedistributionInventoryRequiresEveryModelBucket(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	inventory := codex.Inventory{}
	for _, key := range []codex.AccountKey{"scoped-empty", "complete"} {
		inventory.Accounts = append(inventory.Accounts, frozenDispatchTestLogicalAccount(key,
			frozenDispatchCandidate(key, codex.CandidateID("candidate-"+key), "revision", codex.SourceSystem, false, now.Add(time.Hour))))
		frozenDispatchObserveCapacity(t, ledger, key, CapacityBucketBase, 80, now)
	}
	frozenDispatchObserveCapacity(t, ledger, "scoped-empty", CapacityBucketForModel(codexSparkModel), 0, now)
	frozenDispatchObserveCapacity(t, ledger, "complete", CapacityBucketForModel(codexSparkModel), 25, now)
	factory := &CodexHTTPRequestPlanFactory{Capacity: ledger, Now: func() time.Time { return now }}
	protocol := CodexProtocolRequest{Model: "gpt-5", Metadata: CodexTurnMetadataResult{Metadata: CodexTurnMetadata{
		RequestKind: CodexRequestCompaction, CompactionPhase: CodexCompactionPreTurn,
	}}}
	selected := factory.redistributionInventory(context.Background(), protocol, CodexLeaseRouteSnapshot{}, inventory)
	if got := codexHTTPRequestPlanAccountKeys(selected); !slices.Equal(got, []codex.AccountKey{"complete"}) {
		t.Fatalf("redistribution required model accounts = %v, want complete", got)
	}
	protocol.Model = ""
	if got := factory.redistributionInventory(context.Background(), protocol, CodexLeaseRouteSnapshot{}, inventory); len(got.Accounts) != 0 {
		t.Fatalf("incompatible model accounts = %v, want none", codexHTTPRequestPlanAccountKeys(got))
	}
}

func TestCodexRedistributionDeltaResyncRequiresPositiveDifferentAccount(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		active      int
		alternative int
		wantResync  bool
	}{
		{name: "active still has included quota", active: 80, alternative: 0},
		{name: "both accounts have included quota", active: 80, alternative: 75},
		{name: "alternative has unknown quota", alternative: -1},
		{name: "different account has included quota", alternative: 75, wantResync: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0).UTC()
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
			inventory := codex.Inventory{}
			for _, key := range []codex.AccountKey{"active", "alternative"} {
				inventory.Accounts = append(inventory.Accounts, frozenDispatchTestLogicalAccount(key,
					frozenDispatchCandidate(key, codex.CandidateID("candidate-"+key), "revision", codex.SourceSystem, false, now.Add(time.Hour))))
			}
			frozenDispatchObserveCapacity(t, ledger, "active", CapacityBucketBase, test.active, now)
			if test.alternative >= 0 {
				frozenDispatchObserveCapacity(t, ledger, "alternative", CapacityBucketBase, test.alternative, now)
			}
			factory := &CodexHTTPRequestPlanFactory{
				Inventory: &codexHTTPRequestPlanTestInventory{inventory: inventory}, Capacity: ledger,
				Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{RedistributionGeneration: 1, BoundAccountKey: "active"}},
				Now:    func() time.Time { return now },
			}
			protocol := CodexProtocolRequest{Model: "gpt-5", Metadata: CodexTurnMetadataResult{Strong: true, Metadata: CodexTurnMetadata{
				SessionID: "session", ThreadID: "thread", TurnID: "turn", RequestKind: CodexRequestTurn,
			}}}
			for _, socketAccount := range []codex.AccountKey{"active", ""} {
				if got := factory.ShouldRedistribute(context.Background(), protocol, socketAccount); got != test.wantResync {
					t.Fatalf("delta resync with socket account %q = %t, want %t", socketAccount, got, test.wantResync)
				}
			}
		})
	}
}

func TestCodexRedistributionRejectsPartialResetDespiteAdvisoryCreditCapacity(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	inventory := codex.Inventory{}
	for _, key := range []codex.AccountKey{"active", "alternative"} {
		inventory.Accounts = append(inventory.Accounts, frozenDispatchTestLogicalAccount(key,
			frozenDispatchCandidate(key, codex.CandidateID("candidate-"+key), "revision", codex.SourceSystem, false, now.Add(time.Hour))))
	}
	frozenDispatchObserveCapacity(t, ledger, "active", CapacityBucketBase, 0, now)
	positive := resetTestSnapshot(now, quota.Window7Day, 100)
	fiveHourRemaining := 100.0
	positive.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 100, RemainingPctExact: &fiveHourRemaining, ResetAtUnix: now.Add(time.Hour).Unix()}
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "alternative", positive)
	protocol := CodexProtocolRequest{Model: "gpt-5", Metadata: CodexTurnMetadataResult{Strong: true, Metadata: CodexTurnMetadata{
		SessionID: "session", ThreadID: "thread", TurnID: "turn", RequestKind: CodexRequestTurn,
	}}}
	snapshot := CodexLeaseRouteSnapshot{RedistributionGeneration: 1, BoundAccountKey: "active"}
	factory := &CodexHTTPRequestPlanFactory{
		Inventory: &codexHTTPRequestPlanTestInventory{inventory: inventory}, Capacity: ledger,
		Routes: &codexHTTPRequestPlanTestSnapshotter{snapshot: snapshot}, Now: func() time.Time { return now },
	}
	now = now.Add(time.Second)
	depleted := resetTestSnapshot(now, quota.Window7Day, 0)
	zero := 0.0
	depleted.Result.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 0, RemainingPctExact: &zero, ResetAtUnix: now.Add(time.Hour).Unix()}
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "alternative", depleted)
	for _, stage := range []string{"fresh exhaustion", "only weekly quota reset"} {
		if stage == "only weekly quota reset" {
			now = now.Add(time.Second)
			partial := resetTestSnapshot(now, quota.Window7Day, 100)
			partial.Result.Windows[quota.Window5Hour] = depleted.Result.Windows[quota.Window5Hour]
			ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "alternative", partial)
		}
		if view := ledger.Capacity("alternative", CapacityBucketBase); view.State != CapacityPositive {
			t.Fatalf("%s changed ordinary extra-credit capacity: %+v", stage, view)
		}
		if selected := factory.redistributionInventory(context.Background(), protocol, snapshot, inventory); len(selected.Accounts) != 0 {
			t.Errorf("%s selected extra-credit account %v", stage, codexHTTPRequestPlanAccountKeys(selected))
		}
		if factory.ShouldRedistribute(context.Background(), protocol, "active") {
			t.Errorf("%s requested delta resync towards an account with exhausted included quota", stage)
		}
	}
	now = now.Add(time.Second)
	positive.FetchedAt = now
	recoveredFive := positive.Result.Windows[quota.Window5Hour]
	recoveredFive.ResetAtUnix = depleted.Result.Windows[quota.Window5Hour].ResetAtUnix
	positive.Result.Windows[quota.Window5Hour] = recoveredFive
	ledger.ObserveLivePositiveQuotaSnapshot(ledger.NewObservationStream(), "alternative", positive)
	if selected := factory.redistributionInventory(context.Background(), protocol, snapshot, inventory); !slices.Equal(codexHTTPRequestPlanAccountKeys(selected), []codex.AccountKey{"alternative"}) {
		t.Fatalf("complete reset accounts = %v, want alternative", codexHTTPRequestPlanAccountKeys(selected))
	}
	if !factory.ShouldRedistribute(context.Background(), protocol, "active") {
		t.Fatal("complete included-quota reset did not enable delta resync")
	}
}
