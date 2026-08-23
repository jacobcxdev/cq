package proxy

import (
	"context"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexNoAffinityShadowAdviceFindsAlternativeWithoutChangingActualRoute(t *testing.T) {
	t.Parallel()

	input := codexNoAffinityShadowTestInput(t, 20, 80)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	actual := plan.Accounts()[0]
	if got := actual.Choice().AccountKey; got != "account-a" {
		t.Fatalf("actual account = %q, want account-a", got)
	}

	advice := codexNoAffinityShadowAdvice(context.Background(), plan, input.DefaultAccountKey, input.BoundAccountKey, actual)

	if advice.Comparison != CodexTurnReceiptShadowAlternativeAccount || advice.AlternativeAccountKey != "account-b" {
		t.Fatalf("advice = %+v, want alternative account-b", advice)
	}
	if got := plan.Accounts()[0].Choice().AccountKey; got != "account-a" {
		t.Fatalf("actual account changed to %q", got)
	}
}

func TestCodexNoAffinityShadowAdviceReportsSameAccount(t *testing.T) {
	t.Parallel()

	input := codexNoAffinityShadowTestInput(t, 80, 20)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	advice := codexNoAffinityShadowAdvice(context.Background(), plan, input.DefaultAccountKey, input.BoundAccountKey, plan.Accounts()[0])

	if advice.Comparison != CodexTurnReceiptShadowSameAccount || advice.AlternativeAccountKey != "" {
		t.Fatalf("advice = %+v, want same account", advice)
	}
}

func TestCodexNoAffinityShadowAdviceSkipsBoundRoute(t *testing.T) {
	t.Parallel()

	input := codexNoAffinityShadowTestInput(t, 20, 80)
	input.BoundAccountKey = "account-a"
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	advice := codexNoAffinityShadowAdvice(context.Background(), plan, input.DefaultAccountKey, input.BoundAccountKey, plan.Accounts()[0])

	if advice.Comparison != CodexTurnReceiptShadowNotApplicable || advice.AlternativeAccountKey != "" {
		t.Fatalf("advice = %+v, want not applicable", advice)
	}
}

func TestCodexNoAffinityShadowAdviceClosesPolicyFailureAsUnavailable(t *testing.T) {
	t.Parallel()

	input := codexNoAffinityShadowTestInput(t, 20, 80)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	advice := codexNoAffinityShadowAdvice(ctx, plan, input.DefaultAccountKey, input.BoundAccountKey, plan.Accounts()[0])

	if advice.Comparison != CodexTurnReceiptShadowUnavailable || advice.AlternativeAccountKey != "" {
		t.Fatalf("advice = %+v, want unavailable", advice)
	}
}

func TestCodexNoAffinityShadowAdviceUsesFrozenCandidateSnapshot(t *testing.T) {
	t.Parallel()

	input := codexNoAffinityShadowTestInput(t, 20, 80)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	frozenDispatchObserveCapacity(t, input.Capacity, "account-a", CapacityBucketBase, 100, input.Now)
	frozenDispatchObserveCapacity(t, input.Capacity, "account-b", CapacityBucketBase, 0, input.Now)
	advice := codexNoAffinityShadowAdvice(context.Background(), plan, input.DefaultAccountKey, input.BoundAccountKey, plan.Accounts()[0])

	if advice.Comparison != CodexTurnReceiptShadowAlternativeAccount || advice.AlternativeAccountKey != "account-b" {
		t.Fatalf("advice after live ledger mutation = %+v, want frozen alternative account-b", advice)
	}
}

func codexNoAffinityShadowTestInput(t *testing.T, accountARemaining, accountBRemaining int) CodexFrozenDispatchInput {
	t.Helper()

	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	frozenDispatchObserveCapacity(t, ledger, "account-a", CapacityBucketBase, accountARemaining, now)
	frozenDispatchObserveCapacity(t, ledger, "account-b", CapacityBucketBase, accountBRemaining, now)
	return CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceManaged, true, now.Add(time.Hour))),
			frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceManaged, true, now.Add(time.Hour))),
		}},
		Capacity:               ledger,
		Requirements:           CodexRouteRequirements{RequestedModel: "gpt-5.6-sol"},
		AffinityAccountKey:     "account-a",
		AffinityEffectiveModel: "gpt-5.6-sol",
		DefaultAccountKey:      "account-b",
		Now:                    now,
	}
}
