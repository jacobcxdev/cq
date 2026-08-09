package proxy

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestProjectCodexRoutePolicyCandidatesProjectsEveryLogicalAccount(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	observeProjectionCapacity(t, capacity, "account-pro", CapacityBucketForModel(codexSparkModel), 40)
	observeProjectionCapacity(t, capacity, "account-pro", CapacityBucketBase, 90)
	observeProjectionCapacity(t, capacity, "account-plus", CapacityBucketBase, 75)
	observeProjectionCapacity(t, capacity, "account-zero", CapacityBucketBase, 0)

	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		projectionLogicalAccount("account-pro", "pro", true, projectionReadyCandidate("account-pro", "candidate-pro")),
		projectionLogicalAccount("account-plus", "plus", true, projectionReadyCandidate("account-plus", "candidate-plus")),
		{
			Key:      "account-weak",
			Identity: codex.AccountIdentity{AccountID: "private-weak-account", PlanType: "plus"},
			Routable: true,
			Candidates: []codex.CredentialCandidate{
				projectionReadyCandidate("account-weak", "private-weak-candidate"),
			},
		},
		projectionLogicalAccount("account-logical-off", "plus", false, projectionReadyCandidate("account-logical-off", "candidate-off")),
		projectionLogicalAccount("account-blocked", "plus", true, projectionBlockedCandidate("account-blocked", "candidate-blocked")),
		projectionLogicalAccount("account-zero", "plus", true, projectionReadyCandidate("account-zero", "candidate-zero")),
	}}
	counts := map[codex.AccountKey]int{
		"account-pro":  2,
		"account-plus": 1,
	}
	inventoryBefore := cloneProjectionInventory(inventory)
	countsBefore := cloneProjectionCounts(counts)

	got, err := ProjectCodexRoutePolicyCandidates(inventory, capacity, CodexRouteRequirements{
		RequestedModel: codexSparkModel,
		RequiredModels: []string{codexFallbackModel},
	}, counts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(inventory.Accounts) {
		t.Fatalf("len(candidates) = %d, want %d", len(got), len(inventory.Accounts))
	}

	assertProjectionCandidate(t, got[0], projectionCandidateWant{
		key: "account-pro", requestedModel: codexSparkModel, effectiveModel: codexSparkModel,
		buckets: []CapacityBucket{CapacityBucketForModel(codexSparkModel), CapacityBucketBase},
		capacity: []CapacityView{
			{State: CapacityPositive, RemainingPct: 40, Source: CapacitySourceLiveRateLimits, Exact: true},
			{State: CapacityPositive, RemainingPct: 90, Source: CapacitySourceLiveRateLimits, Exact: true},
		},
		compatible: true, routable: true, provisional: 2,
	})
	assertProjectionCandidate(t, got[1], projectionCandidateWant{
		key: "account-plus", requestedModel: codexSparkModel, effectiveModel: codexFallbackModel,
		buckets: []CapacityBucket{CapacityBucketBase},
		capacity: []CapacityView{
			{State: CapacityPositive, RemainingPct: 75, Source: CapacitySourceLiveRateLimits, Exact: true},
		},
		compatible: true, routable: true, provisional: 1,
	})
	assertProjectionCandidate(t, got[2], projectionCandidateWant{
		key: "account-weak", requestedModel: codexSparkModel, effectiveModel: codexFallbackModel,
		buckets:    []CapacityBucket{CapacityBucketBase},
		capacity:   []CapacityView{{State: CapacityUnknown, Exact: true}},
		compatible: true,
	})
	assertProjectionCandidate(t, got[3], projectionCandidateWant{
		key: "account-logical-off", requestedModel: codexSparkModel, effectiveModel: codexFallbackModel,
		buckets:    []CapacityBucket{CapacityBucketBase},
		capacity:   []CapacityView{{State: CapacityUnknown, Exact: true}},
		compatible: true,
	})
	assertProjectionCandidate(t, got[4], projectionCandidateWant{
		key: "account-blocked", requestedModel: codexSparkModel, effectiveModel: codexFallbackModel,
		buckets:    []CapacityBucket{CapacityBucketBase},
		capacity:   []CapacityView{{State: CapacityUnknown, Exact: true}},
		compatible: true,
	})
	assertProjectionCandidate(t, got[5], projectionCandidateWant{
		key: "account-zero", requestedModel: codexSparkModel, effectiveModel: codexFallbackModel,
		buckets:    []CapacityBucket{CapacityBucketBase},
		capacity:   []CapacityView{{State: CapacityZero, Source: CapacitySourceLiveRateLimits, Exact: true}},
		compatible: true, routable: true,
	})

	if !reflect.DeepEqual(inventory, inventoryBefore) {
		t.Fatal("projection mutated inventory")
	}
	if !reflect.DeepEqual(counts, countsBefore) {
		t.Fatal("projection mutated provisional-count map")
	}
	serialised := fmt.Sprintf("%+v", got)
	for _, secretMetadata := range []string{"private-weak-account", "private-weak-candidate"} {
		if strings.Contains(serialised, secretMetadata) {
			t.Fatalf("projection exposed inventory metadata %q", secretMetadata)
		}
	}
}

func TestProjectCodexRoutePolicyCandidatesNilCapacityIsUnknown(t *testing.T) {
	t.Parallel()

	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		projectionLogicalAccount("account-a", "pro", true, projectionReadyCandidate("account-a", "candidate-a")),
	}}
	got, err := ProjectCodexRoutePolicyCandidates(inventory, nil, CodexRouteRequirements{
		RequestedModel: codexSparkModel,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectionCandidate(t, got[0], projectionCandidateWant{
		key: "account-a", requestedModel: codexSparkModel, effectiveModel: codexSparkModel,
		buckets:     []CapacityBucket{CapacityBucketForModel(codexSparkModel)},
		capacity:    []CapacityView{{State: CapacityUnknown}},
		compatible:  true,
		routable:    true,
		provisional: 0,
	})
}

func TestProjectCodexRoutePolicyCandidatesEmptyModelIsIncompatibleButStructurallyValid(t *testing.T) {
	t.Parallel()

	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		projectionLogicalAccount("account-default", "pro", true, projectionReadyCandidate("account-default", "candidate-default")),
	}}
	got, err := ProjectCodexRoutePolicyCandidates(inventory, nil, CodexRouteRequirements{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectionCandidate(t, got[0], projectionCandidateWant{
		key:      "account-default",
		buckets:  []CapacityBucket{CapacityBucketBase},
		capacity: []CapacityView{{State: CapacityUnknown}},
		routable: true,
	})

	plan, err := BuildCodexRoutePlan(nil, got, CodexRoutePolicyHints{DefaultAccountKey: "account-default"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status() != CodexRoutePlanDefaultIncompatible {
		t.Fatalf("status = %q, want %q", plan.Status(), CodexRoutePlanDefaultIncompatible)
	}
	if choices := plan.Choices(); len(choices) != 0 {
		t.Fatalf("choices = %+v, want none", choices)
	}
}

func TestProjectCodexRoutePolicyCandidatesPreservesUnroutableAndZeroDefaults(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	observeProjectionCapacity(t, capacity, "account-zero", CapacityBucketBase, 0)
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		projectionLogicalAccount("account-unroutable", "plus", false, projectionReadyCandidate("account-unroutable", "candidate-unroutable")),
		projectionLogicalAccount("account-zero", "plus", true, projectionReadyCandidate("account-zero", "candidate-zero")),
	}}
	got, err := ProjectCodexRoutePolicyCandidates(inventory, capacity, CodexRouteRequirements{RequestedModel: codexFallbackModel}, nil)
	if err != nil {
		t.Fatal(err)
	}

	unroutablePlan, err := BuildCodexRoutePlan(nil, got, CodexRoutePolicyHints{DefaultAccountKey: "account-unroutable"})
	if err != nil {
		t.Fatal(err)
	}
	if unroutablePlan.Status() != CodexRoutePlanDefaultUnroutable {
		t.Fatalf("unroutable status = %q, want %q", unroutablePlan.Status(), CodexRoutePlanDefaultUnroutable)
	}
	zeroPlan, err := BuildCodexRoutePlan(nil, got, CodexRoutePolicyHints{DefaultAccountKey: "account-zero"})
	if err != nil {
		t.Fatal(err)
	}
	if zeroPlan.Status() != CodexRoutePlanReady {
		t.Fatalf("zero status = %q, want %q", zeroPlan.Status(), CodexRoutePlanReady)
	}
	choices := zeroPlan.Choices()
	if len(choices) != 1 || choices[0].AccountKey != "account-zero" {
		t.Fatalf("zero choices = %+v, want default exactly once", choices)
	}
}

func TestProjectCodexRoutePolicyCandidatesRejectsDuplicateAccountKeys(t *testing.T) {
	t.Parallel()

	privateKey := codex.AccountKey("private-duplicate-key")
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		{Key: privateKey},
		{Key: privateKey},
	}}
	got, err := ProjectCodexRoutePolicyCandidates(inventory, nil, CodexRouteRequirements{RequestedModel: codexFallbackModel}, nil)
	projectionErr := requireCodexRouteProjectionError(t, err, CodexRouteProjectionInvalidInventory)
	if got != nil {
		t.Fatalf("candidates = %+v, want nil", got)
	}
	if strings.Contains(projectionErr.Error(), string(privateKey)) {
		t.Fatalf("error exposed inventory identifier %q: %v", privateKey, projectionErr)
	}
}

func TestProjectCodexRoutePolicyCandidatesQuarantinesBadRowsWithoutPoisoningHealthyAccounts(t *testing.T) {
	t.Parallel()

	emptyKey := projectionLogicalAccount("", "plus", true, projectionReadyCandidate("", "candidate-empty-key"))
	unstable := projectionLogicalAccount("account-unstable", "plus", true, projectionReadyCandidate("account-unstable", "candidate-unstable"))
	unstable.Unstable = true
	mismatch := projectionLogicalAccount("account-mismatch", "plus", true, projectionReadyCandidate("private-wrong-account", "candidate-mismatch"))
	emptyCandidateID := projectionLogicalAccount("account-empty-candidate", "plus", true, projectionReadyCandidate("account-empty-candidate", ""))
	emptyRevisionCandidate := projectionReadyCandidate("account-empty-revision", "candidate-empty-revision")
	emptyRevisionCandidate.Revision = ""
	emptyRevision := projectionLogicalAccount("account-empty-revision", "plus", true, emptyRevisionCandidate)
	invalidSourceCandidate := projectionReadyCandidate("account-invalid-source", "candidate-invalid-source")
	invalidSourceCandidate.Source = 0
	invalidSource := projectionLogicalAccount("account-invalid-source", "plus", true, invalidSourceCandidate)
	healthy := projectionLogicalAccount("account-healthy", "plus", true, projectionReadyCandidate("account-healthy", "candidate-healthy"))

	got, err := ProjectCodexRoutePolicyCandidates(codex.Inventory{Accounts: []codex.LogicalAccount{
		emptyKey,
		unstable,
		mismatch,
		emptyCandidateID,
		emptyRevision,
		invalidSource,
		healthy,
	}}, nil, CodexRouteRequirements{RequestedModel: codexFallbackModel}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("len(candidates) = %d, want 6 nonempty account keys", len(got))
	}
	for i := range got[:5] {
		if got[i].Routable {
			t.Fatalf("quarantined candidate %d is routable: %+v", i, got[i])
		}
	}
	if !got[5].Routable || got[5].Choice.AccountKey != "account-healthy" {
		t.Fatalf("healthy candidate = %+v, want routable account-healthy", got[5])
	}
}

func TestProjectCodexRoutePolicyCandidatesRejectsNegativeProvisionalCount(t *testing.T) {
	t.Parallel()

	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		projectionLogicalAccount("account-a", "plus", true, projectionReadyCandidate("account-a", "candidate-a")),
	}}
	counts := map[codex.AccountKey]int{"private-not-in-inventory": -1}
	got, err := ProjectCodexRoutePolicyCandidates(inventory, nil, CodexRouteRequirements{RequestedModel: codexFallbackModel}, counts)
	projectionErr := requireCodexRouteProjectionError(t, err, CodexRouteProjectionInvalidProvisionalCount)
	if got != nil {
		t.Fatalf("candidates = %+v, want nil", got)
	}
	if strings.Contains(projectionErr.Error(), "private-not-in-inventory") {
		t.Fatalf("error exposed account key: %v", projectionErr)
	}
	if counts["private-not-in-inventory"] != -1 {
		t.Fatal("projection mutated invalid count map")
	}
}

func TestProjectCodexRoutePolicyCandidatesReturnsOwnedSlices(t *testing.T) {
	t.Parallel()

	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		projectionLogicalAccount("account-a", "pro", true, projectionReadyCandidate("account-a", "candidate-a")),
	}}
	requirements := CodexRouteRequirements{
		RequestedModel: codexSparkModel,
		RequiredModels: []string{codexFallbackModel},
	}
	first, err := ProjectCodexRoutePolicyCandidates(inventory, nil, requirements, nil)
	if err != nil {
		t.Fatal(err)
	}
	first[0].Choice.RequiredBuckets[0] = "mutated"
	first[0].RequiredCapacity[0].RemainingPct = 99
	first[0].Choice.EffectiveModel = "mutated"

	second, err := ProjectCodexRoutePolicyCandidates(inventory, nil, requirements, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Choice.RequiredBuckets[0] != CapacityBucketForModel(codexSparkModel) ||
		second[0].RequiredCapacity[0].RemainingPct != 0 || second[0].Choice.EffectiveModel != codexSparkModel {
		t.Fatalf("second projection inherited caller mutations: %+v", second[0])
	}
	if requirements.RequiredModels[0] != codexFallbackModel {
		t.Fatal("projection aliased required-model input")
	}
}

type projectionCandidateWant struct {
	key            codex.AccountKey
	requestedModel string
	effectiveModel string
	buckets        []CapacityBucket
	capacity       []CapacityView
	compatible     bool
	routable       bool
	provisional    int
}

func assertProjectionCandidate(t *testing.T, got CodexRoutePolicyCandidate, want projectionCandidateWant) {
	t.Helper()

	if got.Choice.AccountKey != want.key || got.Choice.RequestedModel != want.requestedModel ||
		got.Choice.EffectiveModel != want.effectiveModel || got.Compatible != want.compatible ||
		got.Routable != want.routable || got.ProvisionalLeases != want.provisional ||
		!reflect.DeepEqual(got.Choice.RequiredBuckets, want.buckets) ||
		!reflect.DeepEqual(got.RequiredCapacity, want.capacity) {
		t.Fatalf("candidate = %+v, want key=%q requested=%q effective=%q buckets=%+v capacity=%+v compatible=%t routable=%t provisional=%d",
			got, want.key, want.requestedModel, want.effectiveModel, want.buckets, want.capacity, want.compatible, want.routable, want.provisional)
	}
}

func projectionLogicalAccount(key codex.AccountKey, plan string, routable bool, candidates ...codex.CredentialCandidate) codex.LogicalAccount {
	return codex.LogicalAccount{
		Key: key,
		Identity: codex.AccountIdentity{
			AccountID: "private-account-" + string(key),
			UserID:    "private-user-" + string(key),
			Email:     "private-email-" + string(key),
			PlanType:  plan,
		},
		Candidates: candidates,
		Routable:   routable,
	}
}

func projectionReadyCandidate(accountKey codex.AccountKey, candidateID codex.CandidateID) codex.CredentialCandidate {
	return codex.CredentialCandidate{
		Ref:      codex.CandidateRef{AccountKey: accountKey, CandidateID: candidateID},
		Revision: "revision",
		Source:   codex.SourceManaged,
		Routable: true,
	}
}

func projectionBlockedCandidate(accountKey codex.AccountKey, candidateID codex.CandidateID) codex.CredentialCandidate {
	candidate := projectionReadyCandidate(accountKey, candidateID)
	candidate.DispatchBlocked = true
	return candidate
}

func observeProjectionCapacity(t *testing.T, ledger *CodexCapacityLedger, account codex.AccountKey, bucket CapacityBucket, remaining int) {
	t.Helper()

	stream := ledger.NewObservationStream()
	fact := stream.Stamp(CapacityFact{
		AccountKey:   account,
		Bucket:       bucket,
		RemainingPct: remaining,
		Source:       CapacitySourceLiveRateLimits,
		ObservedAt:   time.Unix(1_700_000_000, 0),
		Confidence:   CapacityConfidenceAuthoritative,
	})
	if !ledger.Observe(fact) {
		t.Fatalf("observe capacity for opaque account failed")
	}
}

func requireCodexRouteProjectionError(t *testing.T, err error, code CodexRouteProjectionErrorCode) *CodexRouteProjectionError {
	t.Helper()

	var projectionErr *CodexRouteProjectionError
	if !errors.As(err, &projectionErr) {
		t.Fatalf("error = %v, want *CodexRouteProjectionError", err)
	}
	if projectionErr.Code != code {
		t.Fatalf("error code = %q, want %q", projectionErr.Code, code)
	}
	return projectionErr
}

func cloneProjectionInventory(inventory codex.Inventory) codex.Inventory {
	clone := inventory
	clone.Accounts = append([]codex.LogicalAccount(nil), inventory.Accounts...)
	for i := range clone.Accounts {
		clone.Accounts[i].Candidates = append([]codex.CredentialCandidate(nil), inventory.Accounts[i].Candidates...)
	}
	return clone
}

func cloneProjectionCounts(counts map[codex.AccountKey]int) map[codex.AccountKey]int {
	clone := make(map[codex.AccountKey]int, len(counts))
	for key, count := range counts {
		clone[key] = count
	}
	return clone
}
