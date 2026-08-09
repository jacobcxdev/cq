package proxy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexRoutePolicyAffinityBeatsHigherCapacity(t *testing.T) {
	t.Parallel()

	candidates := []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-b", CapacityPositive, 90),
		routePolicyCandidate("account-a", CapacityPositive, 10),
	}

	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		AffinityAccountKey: "account-a",
		DefaultAccountKey:  "account-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanReady {
		t.Fatalf("status = %q, want %q", plan.Status(), CodexRoutePlanReady)
	}
	assertRoutePolicyAccounts(t, plan, "account-a", "account-b")
}

func TestCodexRoutePolicyKnownPositivePrecedesUnknownAndZero(t *testing.T) {
	t.Parallel()

	candidates := []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-unknown", CapacityUnknown, -1),
		routePolicyCandidate("account-zero", CapacityZero, 0),
		routePolicyCandidate("account-positive", CapacityPositive, 1),
	}

	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		DefaultAccountKey: "account-positive",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-positive", "account-unknown")
}

func TestCodexRoutePolicyMaximisesMinimumRemainingPercentage(t *testing.T) {
	t.Parallel()

	candidates := []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-low", CapacityPositive, 20),
		routePolicyCandidate("account-high", CapacityPositive, 80),
	}

	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		DefaultAccountKey: "account-low",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-high", "account-low")
}

func TestCodexRoutePolicyComputesMinimumAcrossRequiredBuckets(t *testing.T) {
	t.Parallel()

	uneven := routePolicyCandidate("account-uneven", CapacityPositive, 90)
	uneven.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
	uneven.RequiredCapacity = []CapacityView{
		{State: CapacityPositive, RemainingPct: 90},
		{State: CapacityPositive, RemainingPct: 10},
	}
	balanced := routePolicyCandidate("account-balanced", CapacityPositive, 50)
	balanced.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
	balanced.RequiredCapacity = []CapacityView{
		{State: CapacityPositive, RemainingPct: 50},
		{State: CapacityPositive, RemainingPct: 50},
	}

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		uneven,
		balanced,
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-uneven"})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-balanced", "account-uneven")
}

func TestCodexRoutePolicyTreatsAnyRequiredUnknownAsUnknownAcrossBucketOrder(t *testing.T) {
	t.Parallel()

	for _, requiredCapacity := range [][]CapacityView{
		{
			{State: CapacityPositive, RemainingPct: 90},
			{State: CapacityUnknown, RemainingPct: -1},
		},
		{
			{State: CapacityUnknown, RemainingPct: -1},
			{State: CapacityPositive, RemainingPct: 90},
		},
	} {
		partlyUnknown := routePolicyCandidate("account-partly-unknown", CapacityPositive, 90)
		partlyUnknown.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
		partlyUnknown.RequiredCapacity = requiredCapacity
		known := routePolicyCandidate("account-known", CapacityPositive, 1)
		known.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
		known.RequiredCapacity = []CapacityView{
			{State: CapacityPositive, RemainingPct: 1},
			{State: CapacityPositive, RemainingPct: 1},
		}

		plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
			partlyUnknown,
			known,
		}, CodexRoutePolicyHints{DefaultAccountKey: "account-partly-unknown"})
		if err != nil {
			t.Fatal(err)
		}
		assertRoutePolicyAccounts(t, plan, "account-known", "account-partly-unknown")
	}
}

func TestCodexRoutePolicyTreatsAnyRequiredZeroAsZeroAcrossBucketOrder(t *testing.T) {
	t.Parallel()

	for _, requiredCapacity := range [][]CapacityView{
		{
			{State: CapacityPositive, RemainingPct: 90},
			{State: CapacityZero, RemainingPct: 0},
		},
		{
			{State: CapacityZero, RemainingPct: 0},
			{State: CapacityPositive, RemainingPct: 90},
		},
	} {
		depletedAffinity := routePolicyCandidate("account-affinity", CapacityPositive, 90)
		depletedAffinity.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
		depletedAffinity.RequiredCapacity = requiredCapacity
		fallback := routePolicyCandidate("account-fallback", CapacityPositive, 1)
		fallback.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
		fallback.RequiredCapacity = []CapacityView{
			{State: CapacityPositive, RemainingPct: 1},
			{State: CapacityPositive, RemainingPct: 1},
		}

		plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
			depletedAffinity,
			fallback,
		}, CodexRoutePolicyHints{
			AffinityAccountKey: "account-affinity",
			DefaultAccountKey:  "account-affinity",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertRoutePolicyAccounts(t, plan, "account-fallback", "account-affinity")
	}
}

func TestCodexRoutePolicyRejectsMismatchedRequiredCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		buckets    []CapacityBucket
		capacities []CapacityView
	}{
		{
			name:       "missing view",
			buckets:    []CapacityBucket{CapacityBucketBase, "model:spark"},
			capacities: []CapacityView{{State: CapacityPositive, RemainingPct: 50}},
		},
		{
			name:       "extra view",
			buckets:    []CapacityBucket{CapacityBucketBase},
			capacities: []CapacityView{{State: CapacityPositive, RemainingPct: 50}, {State: CapacityZero}},
		},
		{
			name: "empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := routePolicyCandidate("account-default", CapacityPositive, 50)
			candidate.Choice.RequiredBuckets = tt.buckets
			candidate.RequiredCapacity = tt.capacities
			plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
				candidate,
			}, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
			var policyErr *CodexRoutePolicyError
			if !errors.As(err, &policyErr) {
				t.Fatalf("error = %v, want *CodexRoutePolicyError", err)
			}
			if policyErr.Status != CodexRoutePlanInvalidCandidate || plan.Status() != policyErr.Status {
				t.Fatalf("statuses = %q/%q, want invalid_candidate", policyErr.Status, plan.Status())
			}
			assertRoutePolicyAccounts(t, plan)
		})
	}
}

func TestCodexRoutePolicyRejectsInvalidPositiveCapacityPercentages(t *testing.T) {
	t.Parallel()

	for _, remaining := range []int{-1, 0, 101} {
		remaining := remaining
		t.Run(fmt.Sprintf("remaining_%d", remaining), func(t *testing.T) {
			t.Parallel()

			candidate := routePolicyCandidate("account-a", CapacityPositive, remaining)
			plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{candidate}, CodexRoutePolicyHints{
				DefaultAccountKey: "account-a",
			})
			var policyErr *CodexRoutePolicyError
			if !errors.As(err, &policyErr) {
				t.Fatalf("error = %T, want *CodexRoutePolicyError", err)
			}
			if policyErr.Status != CodexRoutePlanInvalidCandidate || plan.Status() != CodexRoutePlanInvalidCandidate {
				t.Fatalf("statuses = %q/%q, want invalid_candidate", policyErr.Status, plan.Status())
			}
			assertRoutePolicyAccounts(t, plan)
		})
	}
}

func TestCodexRoutePolicyPrefersNativeEffectiveModel(t *testing.T) {
	t.Parallel()

	rewritten := routePolicyCandidate("account-rewritten", CapacityPositive, 50)
	rewritten.Choice.EffectiveModel = "gpt-5-fallback"
	candidates := []CodexRoutePolicyCandidate{
		rewritten,
		routePolicyCandidate("account-native", CapacityPositive, 50),
	}

	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		DefaultAccountKey: "account-native",
	})
	if err != nil {
		t.Fatal(err)
	}
	choices := plan.Choices()
	if len(choices) == 0 || choices[0].AccountKey != "account-native" {
		t.Fatalf("first choice = %#v, want native account", choices)
	}
}

func TestCodexRoutePolicyPrefersFewerProvisionalLeases(t *testing.T) {
	t.Parallel()

	busy := routePolicyCandidate("account-busy", CapacityPositive, 50)
	busy.ProvisionalLeases = 4
	idle := routePolicyCandidate("account-idle", CapacityPositive, 50)
	candidates := []CodexRoutePolicyCandidate{busy, idle}

	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		DefaultAccountKey: "account-busy",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-idle", "account-busy")
}

func TestCodexRoutePolicyBreaksTiesLexicallyAcrossInventoryPermutations(t *testing.T) {
	t.Parallel()

	base := []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-c", CapacityUnknown, -1),
		routePolicyCandidate("account-a", CapacityUnknown, -1),
		routePolicyCandidate("account-b", CapacityUnknown, -1),
	}
	permutations := [][]int{
		{0, 1, 2},
		{0, 2, 1},
		{1, 0, 2},
		{1, 2, 0},
		{2, 0, 1},
		{2, 1, 0},
	}

	for _, permutation := range permutations {
		candidates := []CodexRoutePolicyCandidate{
			base[permutation[0]],
			base[permutation[1]],
			base[permutation[2]],
		}
		plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
			DefaultAccountKey: "account-b",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertRoutePolicyAccounts(t, plan, "account-a", "account-b", "account-c")
	}
}

func TestCodexRoutePolicyFreezesFirstEffectiveModel(t *testing.T) {
	t.Parallel()

	rewritten := routePolicyCandidate("account-rewritten", CapacityPositive, 100)
	rewritten.Choice.EffectiveModel = "gpt-5-fallback"
	candidates := []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-affinity", CapacityPositive, 10),
		rewritten,
		routePolicyCandidate("account-compatible", CapacityPositive, 5),
	}

	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		AffinityAccountKey: "account-affinity",
		DefaultAccountKey:  "account-compatible",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.EffectiveModel() != "gpt-5" {
		t.Fatalf("effective model = %q, want gpt-5", plan.EffectiveModel())
	}
	assertRoutePolicyAccounts(t, plan, "account-affinity", "account-compatible")
}

func TestCodexRoutePolicyAppendsKnownZeroDefaultLast(t *testing.T) {
	t.Parallel()

	candidates := []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-default", CapacityZero, 0),
		routePolicyCandidate("account-ordinary", CapacityPositive, 30),
	}
	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		DefaultAccountKey: "account-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanReady {
		t.Fatalf("status = %q, want %q", plan.Status(), CodexRoutePlanReady)
	}
	if got := plan.DefaultAccountKey(); got != "account-default" {
		t.Fatalf("default account key = %q, want account-default", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-ordinary", "account-default")
}

func TestCodexRoutePolicyReportsMissingDefaultWithoutDiscardingOrdinaryPlan(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-ordinary", CapacityPositive, 30),
	}, CodexRoutePolicyHints{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanDefaultMissing {
		t.Fatalf("status = %q, want %q", plan.Status(), CodexRoutePlanDefaultMissing)
	}
	var policyErr *CodexRoutePolicyError
	if !errors.As(plan.TerminalError(), &policyErr) {
		t.Fatalf("terminal error = %T, want *CodexRoutePolicyError", plan.TerminalError())
	}
	if policyErr.Status != CodexRoutePlanDefaultMissing {
		t.Fatalf("terminal error status = %q, want %q", policyErr.Status, CodexRoutePlanDefaultMissing)
	}
	if got := plan.DefaultAccountKey(); got != "" {
		t.Fatalf("default account key = %q, want empty", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-ordinary")
}

func TestCodexRoutePolicyReportsUnresolvedDefault(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-ordinary", CapacityPositive, 30),
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-not-in-inventory"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanDefaultUnresolved {
		t.Fatalf("status = %q, want default_unresolved", plan.Status())
	}
	var policyErr *CodexRoutePolicyError
	if !errors.As(plan.TerminalError(), &policyErr) {
		t.Fatalf("terminal error = %T, want *CodexRoutePolicyError", plan.TerminalError())
	}
	if policyErr.Status != CodexRoutePlanDefaultUnresolved {
		t.Fatalf("terminal error status = %q, want %q", policyErr.Status, CodexRoutePlanDefaultUnresolved)
	}
	if got := plan.DefaultAccountKey(); got != "" {
		t.Fatalf("default account key = %q, want empty", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-ordinary")
}

func TestCodexRoutePolicyReportsIncompatibleDefault(t *testing.T) {
	t.Parallel()

	defaultCandidate := routePolicyCandidate("account-default", CapacityPositive, 90)
	defaultCandidate.Compatible = false
	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-ordinary", CapacityPositive, 30),
		defaultCandidate,
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanDefaultIncompatible {
		t.Fatalf("status = %q, want default_incompatible", plan.Status())
	}
	var policyErr *CodexRoutePolicyError
	if !errors.As(plan.TerminalError(), &policyErr) {
		t.Fatalf("terminal error = %T, want *CodexRoutePolicyError", plan.TerminalError())
	}
	if policyErr.Status != CodexRoutePlanDefaultIncompatible {
		t.Fatalf("terminal error status = %q, want %q", policyErr.Status, CodexRoutePlanDefaultIncompatible)
	}
	if got := plan.DefaultAccountKey(); got != "" {
		t.Fatalf("default account key = %q, want empty", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-ordinary")
}

func TestCodexRoutePolicyReportsUnroutableDefault(t *testing.T) {
	t.Parallel()

	defaultCandidate := routePolicyCandidate("account-default", CapacityPositive, 90)
	defaultCandidate.Routable = false
	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-ordinary", CapacityPositive, 30),
		defaultCandidate,
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanDefaultUnroutable {
		t.Fatalf("status = %q, want default_unroutable", plan.Status())
	}
	var policyErr *CodexRoutePolicyError
	if !errors.As(plan.TerminalError(), &policyErr) {
		t.Fatalf("terminal error = %T, want *CodexRoutePolicyError", plan.TerminalError())
	}
	if policyErr.Status != CodexRoutePlanDefaultUnroutable {
		t.Fatalf("terminal error status = %q, want %q", policyErr.Status, CodexRoutePlanDefaultUnroutable)
	}
	if got := plan.DefaultAccountKey(); got != "" {
		t.Fatalf("default account key = %q, want empty", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-ordinary")
}

func TestCodexRoutePolicyNeverPlansEmptyAccountKey(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("", CapacityPositive, 100),
		routePolicyCandidate("account-default", CapacityPositive, 10),
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-default")
}

func TestCodexRoutePolicyPlansEachAccountKeyOnce(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-default", CapacityPositive, 80),
		routePolicyCandidate("account-default", CapacityPositive, 20),
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-default")
}

func TestCodexRoutePolicyUsesCanonicalModelIdentityForNativePreference(t *testing.T) {
	t.Parallel()

	canonicalNative := routePolicyCandidate("account-z-native", CapacityPositive, 50)
	canonicalNative.Choice.RequestedModel = "GPT-5[1M]"
	canonicalNative.Choice.EffectiveModel = "gpt-5"
	rewritten := routePolicyCandidate("account-a-rewritten", CapacityPositive, 50)
	rewritten.Choice.EffectiveModel = "gpt-5-fallback"

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		rewritten,
		canonicalNative,
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-z-native"})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-z-native")
}

func TestCodexRoutePolicyCancellationReturnsNoPlan(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan, err := BuildCodexRoutePlan(ctx, []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-default", CapacityPositive, 50),
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if plan.Status() != CodexRoutePlanCanceled {
		t.Fatalf("status = %q, want canceled", plan.Status())
	}
	var policyErr *CodexRoutePolicyError
	if !errors.As(err, &policyErr) || policyErr.Status != CodexRoutePlanCanceled {
		t.Fatalf("typed error = %#v, want canceled policy error", policyErr)
	}
	assertRoutePolicyAccounts(t, plan)
}

func TestCodexRoutePolicyKeepsOnlyFrozenBucketCompatibleAlternatives(t *testing.T) {
	t.Parallel()

	incompatibleBuckets := routePolicyCandidate("account-other-buckets", CapacityPositive, 100)
	incompatibleBuckets.Choice.RequiredBuckets = []CapacityBucket{"model:other"}
	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-affinity", CapacityPositive, 10),
		incompatibleBuckets,
	}, CodexRoutePolicyHints{
		AffinityAccountKey: "account-affinity",
		DefaultAccountKey:  "account-affinity",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-affinity")
}

func TestCodexRoutePolicyDeterministicallyCoalescesDuplicateRouteVariants(t *testing.T) {
	t.Parallel()

	variantZ := routePolicyCandidate("account-default", CapacityPositive, 50)
	variantZ.Choice.EffectiveModel = "gpt-5-fallback-z"
	variantA := routePolicyCandidate("account-default", CapacityPositive, 50)
	variantA.Choice.EffectiveModel = "gpt-5-fallback-a"
	for _, candidates := range [][]CodexRoutePolicyCandidate{
		{variantZ, variantA},
		{variantA, variantZ},
	} {
		plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
			DefaultAccountKey: "account-default",
		})
		if err != nil {
			t.Fatal(err)
		}
		if plan.EffectiveModel() != "gpt-5-fallback-a" {
			t.Fatalf("effective model = %q, want gpt-5-fallback-a", plan.EffectiveModel())
		}
		assertRoutePolicyAccounts(t, plan, "account-default")
	}
}

func TestCodexRoutePolicyNormalisesCanonicalAlternativesToFrozenChoice(t *testing.T) {
	t.Parallel()

	affinity := routePolicyCandidate("account-affinity", CapacityPositive, 10)
	affinity.Choice.RequestedModel = "GPT-5[1M]"
	affinity.Choice.EffectiveModel = "gpt-5"
	alternative := routePolicyCandidate("account-alternative", CapacityPositive, 90)
	alternative.Choice.RequestedModel = "gpt-5"
	alternative.Choice.EffectiveModel = "GPT-5[1M]"
	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		affinity,
		alternative,
	}, CodexRoutePolicyHints{
		AffinityAccountKey: "account-affinity",
		DefaultAccountKey:  "account-alternative",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyAccounts(t, plan, "account-affinity", "account-alternative")
	for _, choice := range plan.Choices() {
		if choice.RequestedModel != "GPT-5[1M]" || choice.EffectiveModel != "gpt-5" {
			t.Fatalf("choice models = %q/%q, want frozen GPT-5[1M]/gpt-5", choice.RequestedModel, choice.EffectiveModel)
		}
	}
}

func TestCodexRoutePolicyDeterministicallyCoalescesKnownZeroDefaultVariants(t *testing.T) {
	t.Parallel()

	variantZ := routePolicyCandidate("account-default", CapacityZero, 0)
	variantZ.Choice.EffectiveModel = "gpt-5-fallback-z"
	variantA := routePolicyCandidate("account-default", CapacityZero, 0)
	variantA.Choice.EffectiveModel = "gpt-5-fallback-a"
	for _, candidates := range [][]CodexRoutePolicyCandidate{
		{variantZ, variantA},
		{variantA, variantZ},
	} {
		plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
			DefaultAccountKey: "account-default",
		})
		if err != nil {
			t.Fatal(err)
		}
		if plan.EffectiveModel() != "gpt-5-fallback-a" {
			t.Fatalf("effective model = %q, want gpt-5-fallback-a", plan.EffectiveModel())
		}
		assertRoutePolicyAccounts(t, plan, "account-default")
	}
}

func TestCodexRoutePolicyReportsFrozenModelIncompatibleDefault(t *testing.T) {
	t.Parallel()

	defaultCandidate := routePolicyCandidate("account-default", CapacityPositive, 90)
	defaultCandidate.Choice.EffectiveModel = "gpt-5-fallback"
	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-ordinary", CapacityPositive, 30),
		defaultCandidate,
	}, CodexRoutePolicyHints{
		AffinityAccountKey: "account-ordinary",
		DefaultAccountKey:  "account-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanDefaultIncompatible {
		t.Fatalf("status = %q, want %q", plan.Status(), CodexRoutePlanDefaultIncompatible)
	}
	if got := plan.DefaultAccountKey(); got != "" {
		t.Fatalf("default account key = %q, want empty", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-ordinary")
}

func TestCodexRoutePolicyDoesNotDuplicateDefaultAlreadyInOrdinaryPlan(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-default", CapacityPositive, 40),
		routePolicyCandidate("account-ordinary", CapacityPositive, 80),
	}, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.DefaultAccountKey(); got != "account-default" {
		t.Fatalf("default account key = %q, want account-default", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-ordinary", "account-default")
}

func TestCodexRoutePolicyBoundAccountIsSoleChoiceAtKnownZero(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-bound", CapacityZero, 0),
		routePolicyCandidate("account-ordinary", CapacityPositive, 100),
		routePolicyCandidate("account-default", CapacityPositive, 100),
	}, CodexRoutePolicyHints{
		AffinityAccountKey: "account-ordinary",
		DefaultAccountKey:  "account-default",
		BoundAccountKey:    "account-bound",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Status(); got != CodexRoutePlanReady {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanReady)
	}
	if err := plan.TerminalError(); err != nil {
		t.Fatalf("terminal error = %v, want nil", err)
	}
	if got := plan.DefaultAccountKey(); got != "" {
		t.Fatalf("default account key = %q, want empty", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-bound")
}

func TestCodexRoutePolicyBoundAccountRetainsConfiguredDefaultRole(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-bound", CapacityZero, 0),
	}, CodexRoutePolicyHints{
		DefaultAccountKey: "account-bound",
		BoundAccountKey:   "account-bound",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.DefaultAccountKey(); got != "account-bound" {
		t.Fatalf("default account key = %q, want account-bound", got)
	}
	assertRoutePolicyAccounts(t, plan, "account-bound")
}

func TestCodexRoutePolicyBoundAccountVariantIsPermutationDeterministic(t *testing.T) {
	t.Parallel()

	variantZ := routePolicyCandidate("account-bound", CapacityPositive, 50)
	variantZ.Choice.EffectiveModel = "gpt-5-fallback-z"
	variantA := routePolicyCandidate("account-bound", CapacityPositive, 50)
	variantA.Choice.EffectiveModel = "gpt-5-fallback-a"
	for _, candidates := range [][]CodexRoutePolicyCandidate{
		{variantZ, variantA},
		{variantA, variantZ},
	} {
		plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
			BoundAccountKey: "account-bound",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.EffectiveModel(); got != "gpt-5-fallback-a" {
			t.Fatalf("effective model = %q, want gpt-5-fallback-a", got)
		}
		assertRoutePolicyAccounts(t, plan, "account-bound")
	}
}

func TestCodexRoutePolicyBoundAccountClonesInputAndOutput(t *testing.T) {
	t.Parallel()

	candidate := routePolicyCandidate("account-bound", CapacityZero, 0)
	candidate.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
	candidate.RequiredCapacity = []CapacityView{
		{State: CapacityZero, RemainingPct: 0},
		{State: CapacityZero, RemainingPct: 0},
	}
	candidates := []CodexRoutePolicyCandidate{candidate}
	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		BoundAccountKey: "account-bound",
	})
	if err != nil {
		t.Fatal(err)
	}

	candidates[0].Choice.RequiredBuckets[0] = "mutated-input"
	returned := plan.Choices()
	returned[0].RequiredBuckets[1] = "mutated-output"

	again := plan.Choices()
	if !reflect.DeepEqual(again[0].RequiredBuckets, []CapacityBucket{CapacityBucketBase, "model:spark"}) {
		t.Fatalf("frozen bound buckets mutated through alias: %#v", again[0].RequiredBuckets)
	}
}

func TestCodexRoutePolicyReportsUnresolvedBoundAccount(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		routePolicyCandidate("account-default", CapacityPositive, 100),
	}, CodexRoutePolicyHints{
		DefaultAccountKey: "account-default",
		BoundAccountKey:   "account-missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Status(); got != CodexRoutePlanBoundUnresolved {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanBoundUnresolved)
	}
	var policyErr *CodexRoutePolicyError
	if terminal := plan.TerminalError(); !errors.As(terminal, &policyErr) || policyErr.Status != CodexRoutePlanBoundUnresolved {
		t.Fatalf("terminal error = %#v, want unresolved-bound policy error", terminal)
	}
	if got := plan.DefaultAccountKey(); got != "" {
		t.Fatalf("default account key = %q, want empty", got)
	}
	assertRoutePolicyAccounts(t, plan)
}

func TestCodexRoutePolicyReportsIncompatibleBoundAccount(t *testing.T) {
	t.Parallel()

	bound := routePolicyCandidate("account-bound", CapacityPositive, 100)
	bound.Compatible = false
	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		bound,
		routePolicyCandidate("account-default", CapacityPositive, 100),
	}, CodexRoutePolicyHints{
		DefaultAccountKey: "account-default",
		BoundAccountKey:   "account-bound",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := CodexRoutePlanBoundIncompatible
	if got := plan.Status(); got != wantStatus {
		t.Fatalf("status = %q, want %q", got, wantStatus)
	}
	var policyErr *CodexRoutePolicyError
	if terminal := plan.TerminalError(); !errors.As(terminal, &policyErr) || policyErr.Status != wantStatus {
		t.Fatalf("terminal error = %#v, want incompatible-bound policy error", terminal)
	}
	assertRoutePolicyAccounts(t, plan)
}

func TestCodexRoutePolicyReportsUnroutableBoundAccount(t *testing.T) {
	t.Parallel()

	bound := routePolicyCandidate("account-bound", CapacityPositive, 100)
	bound.Routable = false
	plan, err := BuildCodexRoutePlan(context.Background(), []CodexRoutePolicyCandidate{
		bound,
		routePolicyCandidate("account-default", CapacityPositive, 100),
	}, CodexRoutePolicyHints{
		DefaultAccountKey: "account-default",
		BoundAccountKey:   "account-bound",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := CodexRoutePlanBoundUnroutable
	if got := plan.Status(); got != wantStatus {
		t.Fatalf("status = %q, want %q", got, wantStatus)
	}
	var policyErr *CodexRoutePolicyError
	if terminal := plan.TerminalError(); !errors.As(terminal, &policyErr) || policyErr.Status != wantStatus {
		t.Fatalf("terminal error = %#v, want unroutable-bound policy error", terminal)
	}
	assertRoutePolicyAccounts(t, plan)
}

func TestCodexRoutePolicyClonesInputsAndReturnedChoices(t *testing.T) {
	t.Parallel()

	candidate := routePolicyCandidate("account-default", CapacityPositive, 50)
	candidate.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase, "model:spark"}
	candidate.RequiredCapacity = []CapacityView{
		{State: CapacityPositive, RemainingPct: 50},
		{State: CapacityPositive, RemainingPct: 25},
	}
	candidates := []CodexRoutePolicyCandidate{candidate}
	before := cloneCodexRoutePolicyCandidate(candidate)
	plan, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		DefaultAccountKey: "account-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidates[0], before) {
		t.Fatalf("input candidate mutated: %#v", candidates[0])
	}

	candidates[0].Choice.EffectiveModel = "mutated-input"
	candidates[0].Choice.RequiredBuckets[0] = "mutated-input"
	candidates[0].RequiredCapacity[0] = CapacityView{State: CapacityZero}
	returned := plan.Choices()
	returned[0].EffectiveModel = "mutated-output"
	returned[0].RequiredBuckets[0] = "mutated-output"

	again := plan.Choices()
	if again[0].EffectiveModel != "gpt-5" || again[0].RequiredBuckets[0] != CapacityBucketBase {
		t.Fatalf("frozen choice mutated through alias: %#v", again[0])
	}
}

func routePolicyCandidate(key codex.AccountKey, state CapacityState, remaining int) CodexRoutePolicyCandidate {
	return CodexRoutePolicyCandidate{
		Choice: RouteChoice{
			AccountKey:      key,
			RequestedModel:  "gpt-5",
			EffectiveModel:  "gpt-5",
			RequiredBuckets: []CapacityBucket{CapacityBucketBase},
		},
		RequiredCapacity: []CapacityView{{
			State:        state,
			RemainingPct: remaining,
		}},
		Compatible: true,
		Routable:   true,
	}
}

func assertRoutePolicyAccounts(t *testing.T, plan CodexRoutePlan, want ...codex.AccountKey) {
	t.Helper()
	choices := plan.Choices()
	if len(choices) != len(want) {
		t.Fatalf("account count = %d, want %d: %#v", len(choices), len(want), choices)
	}
	for i := range want {
		if choices[i].AccountKey != want[i] {
			t.Fatalf("account[%d] = %q, want %q", i, choices[i].AccountKey, want[i])
		}
	}
}
