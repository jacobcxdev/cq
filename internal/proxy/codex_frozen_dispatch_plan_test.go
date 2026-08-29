package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestBuildCodexFrozenDispatchPlanMaterialisesPolicyChoices(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	affinity := frozenDispatchTestLogicalAccount("account-affinity",
		frozenDispatchCandidate("account-affinity", "candidate-managed", "revision-managed", codex.SourceManaged, true, now.Add(time.Hour)),
		frozenDispatchCandidate("account-affinity", "candidate-accepted", "revision-accepted", codex.SourceSystem, false, now.Add(-time.Hour)),
	)
	defaultAccount := frozenDispatchTestLogicalAccount("account-default",
		frozenDispatchCandidate("account-default", "candidate-external", "revision-external", codex.SourceExternal, false, time.Time{}),
	)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{defaultAccount, affinity}},
		Requirements: CodexRouteRequirements{
			RequestedModel: "gpt-5",
		},
		AffinityAccountKey: "account-affinity",
		DefaultAccountKey:  "account-default",
		AcceptedRevision:   "revision-accepted",
		Now:                now,
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
	accounts := plan.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("account plans = %d, want 2", len(accounts))
	}
	if accounts[0].IsDefault() || !accounts[1].IsDefault() {
		t.Fatalf("default labels = %t/%t, want false/true", accounts[0].IsDefault(), accounts[1].IsDefault())
	}
	for index, wantKey := range []codex.AccountKey{"account-affinity", "account-default"} {
		choice := accounts[index].Choice()
		if choice.AccountKey != wantKey || choice.RequestedModel != "gpt-5" || choice.EffectiveModel != "gpt-5" ||
			!reflect.DeepEqual(choice.RequiredBuckets, []CapacityBucket{CapacityBucketBase}) {
			t.Fatalf("choice[%d] = %+v", index, choice)
		}
	}
	attempts := accounts[0].Attempts()
	if len(attempts) != 2 || attempts[0].Candidate.CandidateID != "candidate-accepted" || attempts[0].Ordinal != 1 ||
		attempts[1].Candidate.CandidateID != "candidate-managed" || attempts[1].Ordinal != 2 {
		t.Fatalf("affinity attempts = %+v, want accepted then managed", attempts)
	}
	if attempts[0].Identity.Email != "" || attempts[0].Identity.AccountID == "" || attempts[0].Identity.UserID == "" {
		t.Fatalf("attempt strong identity projection = %+v", attempts[0].Identity)
	}
	refresh, ok := accounts[0].RefreshAttempt()
	if !ok || refresh.Candidate.CandidateID != "candidate-managed" || refresh.Ordinal != 2 {
		t.Fatalf("refresh attempt = %+v, %t, want managed ordinal 2", refresh, ok)
	}
	if _, ok := accounts[1].RefreshAttempt(); ok {
		t.Fatal("external-only account exposed a refresh attempt")
	}
}

func TestBuildCodexFrozenDispatchPlanUsesProvisionalCounts(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-a",
				frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceManaged, false, time.Time{})),
			frozenDispatchTestLogicalAccount("account-z",
				frozenDispatchCandidate("account-z", "candidate-z", "revision-z", codex.SourceManaged, false, time.Time{})),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		Provisional:       map[codex.AccountKey]int{"account-a": 1, "account-z": 0},
		DefaultAccountKey: "account-a",
		Now:               time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	accounts := plan.Accounts()
	if len(accounts) != 2 || accounts[0].Choice().AccountKey != "account-z" || accounts[1].Choice().AccountKey != "account-a" {
		t.Fatalf("account order = %+v, want lower provisional load first", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanFreezesAccountValues(t *testing.T) {
	t.Parallel()

	values := map[codex.AccountKey]PoolValue{"account-a": 10, "account-z": 0}
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceManaged, false, time.Time{})),
			frozenDispatchTestLogicalAccount("account-z", frozenDispatchCandidate("account-z", "candidate-z", "revision-z", codex.SourceManaged, false, time.Time{})),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		AccountValues:     values,
		DefaultAccountKey: "account-a",
		Now:               time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	values["account-z"] = 20
	accounts := plan.Accounts()
	if len(accounts) != 2 || accounts[0].Choice().AccountKey != "account-z" || accounts[0].Value() != 0 || accounts[1].Value() != 10 || plan.probe.selectedValue != 0 {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanRejectsNegativeProvisionalCount(t *testing.T) {
	t.Parallel()

	privateKey := codex.AccountKey("private-negative-provisional")
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount(privateKey,
				frozenDispatchCandidate(privateKey, "candidate-route", "revision-route", codex.SourceManaged, false, time.Time{})),
		}},
		Requirements: CodexRouteRequirements{RequestedModel: "gpt-5"},
		Provisional:  map[codex.AccountKey]int{privateKey: -1},
		Now:          time.Unix(1_700_000_000, 0),
	})
	var projectionErr *CodexRouteProjectionError
	if !errors.As(err, &projectionErr) || projectionErr.Code != CodexRouteProjectionInvalidProvisionalCount {
		t.Fatalf("error = %#v, want invalid-provisional projection error", err)
	}
	if strings.Contains(err.Error(), string(privateKey)) {
		t.Fatalf("error exposed provisional account key: %v", err)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanUsesInjectedTimeForCandidateOrder(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-route",
				frozenDispatchCandidate("account-route", "candidate-a-expired", "revision-expired", codex.SourceManaged, false, now.Add(-time.Hour)),
				frozenDispatchCandidate("account-route", "candidate-z-unknown", "revision-unknown", codex.SourceManaged, false, time.Time{}),
			),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-route",
		Now:               now,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempts := plan.Accounts()[0].Attempts()
	if len(attempts) != 2 || attempts[0].Candidate.CandidateID != "candidate-z-unknown" || attempts[1].Candidate.CandidateID != "candidate-a-expired" {
		t.Fatalf("attempt order = %+v, want unknown-expiry candidate before expired candidate", attempts)
	}
}

func TestBuildCodexFrozenDispatchPlanKeepsFirstCQAuthoredManagedRefresh(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-route",
				frozenDispatchCandidate("account-route", "candidate-a-first", "revision-first", codex.SourceManaged, true, time.Time{}),
				frozenDispatchCandidate("account-route", "candidate-z-second", "revision-second", codex.SourceManaged, true, time.Time{}),
			),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-route",
		Now:               time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	refresh, ok := plan.Accounts()[0].RefreshAttempt()
	if !ok || refresh.Candidate.CandidateID != "candidate-a-first" || refresh.Ordinal != 1 {
		t.Fatalf("refresh attempt = %+v, %t, want first managed CQ-authored candidate", refresh, ok)
	}
}

func TestBuildCodexFrozenDispatchPlanReturnsCanceledPolicyFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan, err := BuildCodexFrozenDispatchPlan(ctx, CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-route",
				frozenDispatchCandidate("account-route", "candidate-route", "revision-route", codex.SourceManaged, false, time.Time{})),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-route",
		Now:               time.Unix(1_700_000_000, 0),
	})
	var policyErr *CodexRoutePolicyError
	if !errors.As(err, &policyErr) || policyErr.Status != CodexRoutePlanCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %#v, want canceled policy error", err)
	}
	if got := plan.Status(); got != CodexRoutePlanCanceled {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanCanceled)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanCancellationPrecedesProjectionFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan, err := BuildCodexFrozenDispatchPlan(ctx, CodexFrozenDispatchInput{
		Provisional: map[codex.AccountKey]int{"private-account": -1},
	})
	var policyErr *CodexRoutePolicyError
	if !errors.As(err, &policyErr) || policyErr.Status != CodexRoutePlanCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %#v, want canceled policy error", err)
	}
	if got := plan.Status(); got != CodexRoutePlanCanceled {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanCanceled)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanStopsWhenCanceledDuringMaterialisation(t *testing.T) {
	t.Parallel()

	ctx := &frozenDispatchCancelOnErrCheckContext{
		Context:  context.Background(),
		cancelAt: 4,
	}
	plan, err := BuildCodexFrozenDispatchPlan(ctx, CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-route",
				frozenDispatchCandidate("account-route", "candidate-route", "revision-route", codex.SourceManaged, false, time.Time{})),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-route",
		Now:               time.Unix(1_700_000_000, 0),
	})
	var policyErr *CodexRoutePolicyError
	if !errors.As(err, &policyErr) || policyErr.Status != CodexRoutePlanCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %#v, want canceled policy error", err)
	}
	if got := plan.Status(); got != CodexRoutePlanCanceled {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanCanceled)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanFreezesOnlyKnownZeroBoundAccount(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	frozenDispatchObserveCapacity(t, capacity, "account-bound", CapacityBucketBase, 0, now)
	frozenDispatchObserveCapacity(t, capacity, "account-default", CapacityBucketBase, 100, now)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-default",
				frozenDispatchCandidate("account-default", "candidate-default", "revision-default", codex.SourceManaged, false, time.Time{})),
			frozenDispatchTestLogicalAccount("account-bound",
				frozenDispatchCandidate("account-bound", "candidate-bound", "revision-bound", codex.SourceManaged, false, time.Time{})),
		}},
		Capacity:           capacity,
		Requirements:       CodexRouteRequirements{RequestedModel: "gpt-5"},
		AffinityAccountKey: "account-default",
		DefaultAccountKey:  "account-default",
		BoundAccountKey:    "account-bound",
		Now:                now,
	})
	if err != nil {
		t.Fatal(err)
	}
	accounts := plan.Accounts()
	if len(accounts) != 1 || accounts[0].Choice().AccountKey != "account-bound" {
		t.Fatalf("accounts = %+v, want only account-bound", accounts)
	}
	if accounts[0].IsDefault() {
		t.Fatal("bound account inherited a different configured default's role")
	}
	if got := plan.Status(); got != CodexRoutePlanReady || plan.TerminalError() != nil {
		t.Fatalf("bound policy status/error = %q/%v, want ready/nil", got, plan.TerminalError())
	}
}

func TestBuildCodexFrozenDispatchPlanMarksBoundConfiguredDefault(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-bound",
				frozenDispatchCandidate("account-bound", "candidate-bound", "revision-bound", codex.SourceManaged, false, time.Time{})),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-bound",
		BoundAccountKey:   "account-bound",
		Now:               time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	accounts := plan.Accounts()
	if len(accounts) != 1 || !accounts[0].IsDefault() {
		t.Fatalf("accounts = %+v, want sole bound configured default", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanRejectsBoundAccountWithoutDispatchableCandidate(t *testing.T) {
	t.Parallel()

	account := frozenDispatchTestLogicalAccount("account-bound",
		frozenDispatchCandidate("account-bound", "candidate-blocked", "revision-blocked", codex.SourceManaged, false, time.Time{}),
	)
	account.Candidates[0].DispatchBlocked = true
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory:       codex.Inventory{Accounts: []codex.LogicalAccount{account}},
		Requirements:    CodexRouteRequirements{RequestedModel: "gpt-5"},
		BoundAccountKey: "account-bound",
		Now:             time.Unix(1_700_000_000, 0),
	})
	var dispatchErr *CodexFrozenDispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != CodexFrozenDispatchNoDispatchableCandidate {
		t.Fatalf("error = %#v, want no-candidate frozen-dispatch error", err)
	}
	if got := plan.Status(); got != CodexRoutePlanBoundUnroutable {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanBoundUnroutable)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanRejectsDisappearedBoundAccount(t *testing.T) {
	t.Parallel()

	privateMissingKey := codex.AccountKey("private-missing-account")
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-other",
				frozenDispatchCandidate("account-other", "candidate-other", "revision-other", codex.SourceManaged, false, time.Time{})),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-other",
		BoundAccountKey:   privateMissingKey,
		Now:               time.Unix(1_700_000_000, 0),
	})
	var dispatchErr *CodexFrozenDispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != CodexFrozenDispatchAccountDisappeared {
		t.Fatalf("error = %#v, want disappeared frozen-dispatch error", err)
	}
	if strings.Contains(err.Error(), string(privateMissingKey)) {
		t.Fatalf("error exposed bound account key: %v", err)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanRejectsIncompleteStrongIdentity(t *testing.T) {
	t.Parallel()

	weak := frozenDispatchTestLogicalAccount("account-weak",
		frozenDispatchCandidate("account-weak", "candidate-weak", "revision-weak", codex.SourceManaged, false, time.Time{}),
	)
	weak.Identity.UserID = ""
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory:        codex.Inventory{Accounts: []codex.LogicalAccount{weak}},
		Requirements:     CodexRouteRequirements{RequestedModel: "gpt-5"},
		BoundAccountKey:  "account-weak",
		AcceptedRevision: "revision-weak",
		Now:              time.Unix(1_700_000_000, 0),
	})
	wantCode := CodexFrozenDispatchStrongIdentityInvalid
	var dispatchErr *CodexFrozenDispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != wantCode {
		t.Fatalf("error = %#v, want strong-identity frozen-dispatch error", err)
	}
	if strings.Contains(err.Error(), weak.Identity.AccountID) || strings.Contains(err.Error(), weak.Identity.Email) {
		t.Fatalf("error exposed account identity: %v", err)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanRejectsUnroutableBoundAccount(t *testing.T) {
	t.Parallel()

	unroutable := frozenDispatchTestLogicalAccount("account-unroutable",
		frozenDispatchCandidate("account-unroutable", "candidate-unroutable", "revision-unroutable", codex.SourceManaged, false, time.Time{}),
	)
	unroutable.Routable = false
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory:       codex.Inventory{Accounts: []codex.LogicalAccount{unroutable}},
		Requirements:    CodexRouteRequirements{RequestedModel: "gpt-5"},
		BoundAccountKey: "account-unroutable",
		Now:             time.Unix(1_700_000_000, 0),
	})
	wantCode := CodexFrozenDispatchAccountUnroutable
	var dispatchErr *CodexFrozenDispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != wantCode {
		t.Fatalf("error = %#v, want unroutable frozen-dispatch error", err)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanReturnsBoundPolicyFailure(t *testing.T) {
	t.Parallel()

	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-bound",
				frozenDispatchCandidate("account-bound", "candidate-bound", "revision-bound", codex.SourceManaged, false, time.Time{})),
		}},
		BoundAccountKey: "account-bound",
		Now:             time.Unix(1_700_000_000, 0),
	})
	var policyErr *CodexRoutePolicyError
	if !errors.As(err, &policyErr) || policyErr.Status != CodexRoutePlanBoundIncompatible {
		t.Fatalf("error = %#v, want incompatible-bound policy error", err)
	}
	if got := plan.Status(); got != CodexRoutePlanBoundIncompatible {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanBoundIncompatible)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestBuildCodexFrozenDispatchPlanRejectsInconsistentCandidateSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		code   CodexFrozenDispatchErrorCode
		mutate func(*codex.CredentialCandidate)
	}{
		{
			name: "account reference",
			code: "candidate_account_mismatch",
			mutate: func(candidate *codex.CredentialCandidate) {
				candidate.Ref.AccountKey = "private-other-account"
			},
		},
		{
			name: "candidate reference",
			code: "candidate_reference_missing",
			mutate: func(candidate *codex.CredentialCandidate) {
				candidate.Ref.CandidateID = ""
			},
		},
		{
			name: "revision",
			code: "candidate_revision_missing",
			mutate: func(candidate *codex.CredentialCandidate) {
				candidate.Revision = ""
			},
		},
		{
			name: "source",
			code: "candidate_source_invalid",
			mutate: func(candidate *codex.CredentialCandidate) {
				candidate.Source = 0
			},
		},
		{
			name: "CQ authorship source",
			code: "candidate_source_invalid",
			mutate: func(candidate *codex.CredentialCandidate) {
				candidate.Source = codex.SourceSystem
				candidate.CQAuthored = true
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			invalid := frozenDispatchCandidate("account-route", "private-candidate-invalid", "revision-invalid", codex.SourceExternal, false, time.Time{})
			test.mutate(&invalid)
			account := frozenDispatchTestLogicalAccount("account-route",
				invalid,
				frozenDispatchCandidate("account-route", "candidate-healthy", "revision-healthy", codex.SourceManaged, false, time.Time{}),
			)
			plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
				Inventory:         codex.Inventory{Accounts: []codex.LogicalAccount{account}},
				Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
				DefaultAccountKey: "account-route",
				Now:               time.Unix(1_700_000_000, 0),
			})
			var dispatchErr *CodexFrozenDispatchError
			if !errors.As(err, &dispatchErr) || dispatchErr.Code != test.code {
				t.Fatalf("error = %#v, want code %q", err, test.code)
			}
			for _, private := range []string{"private-account-account-route", "private-email-account-route", "private-candidate-invalid"} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("error exposed private snapshot value %q: %v", private, err)
				}
			}
			if accounts := plan.Accounts(); len(accounts) != 0 {
				t.Fatalf("accounts = %+v, want none", accounts)
			}
		})
	}
}

func TestBuildCodexFrozenDispatchPlanDoesNotReturnPartialAccountsOnLateFailure(t *testing.T) {
	t.Parallel()

	invalid := frozenDispatchCandidate("account-z-invalid", "candidate-a-invalid", "", codex.SourceManaged, false, time.Time{})
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
			frozenDispatchTestLogicalAccount("account-a-healthy",
				frozenDispatchCandidate("account-a-healthy", "candidate-healthy", "revision-healthy", codex.SourceManaged, false, time.Time{})),
			frozenDispatchTestLogicalAccount("account-z-invalid",
				invalid,
				frozenDispatchCandidate("account-z-invalid", "candidate-z-healthy", "revision-healthy", codex.SourceManaged, false, time.Time{}),
			),
		}},
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-z-invalid",
		Now:               time.Unix(1_700_000_000, 0),
	})
	var dispatchErr *CodexFrozenDispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != CodexFrozenDispatchCandidateRevisionMissing {
		t.Fatalf("error = %#v, want missing-revision frozen-dispatch error", err)
	}
	if got := plan.Status(); got != CodexRoutePlanReady {
		t.Fatalf("status = %q, want %q", got, CodexRoutePlanReady)
	}
	if accounts := plan.Accounts(); len(accounts) != 0 {
		t.Fatalf("partial accounts escaped failed build: %+v", accounts)
	}
}

func TestFreezeCodexDispatchAccountRejectsNoDispatchableCandidate(t *testing.T) {
	t.Parallel()

	logical := frozenDispatchTestLogicalAccount("account-route",
		frozenDispatchCandidate("account-route", "candidate-off", "revision-off", codex.SourceManaged, false, time.Time{}),
	)
	logical.Candidates[0].Routable = false
	_, err := freezeCodexDispatchAccount(logical, RouteChoice{
		AccountKey:      "account-route",
		RequestedModel:  "gpt-5",
		EffectiveModel:  "gpt-5",
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
	}, "", time.Unix(1_700_000_000, 0), false)
	wantCode := CodexFrozenDispatchNoDispatchableCandidate
	var dispatchErr *CodexFrozenDispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != wantCode {
		t.Fatalf("error = %#v, want no-candidate frozen-dispatch error", err)
	}
}

func TestCodexFrozenDispatchPlanReturnsDetachedMemoryOnlyViews(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	candidate := frozenDispatchCandidate("account-route", "candidate-route", "revision-route", codex.SourceManaged, true, time.Time{})
	candidate.Credential = codex.CodexAccount{
		AccessToken: "private-secret-token",
		AccountID:   "private-credential-account-id",
		Email:       "private-credential-email@test.invalid",
	}
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account-route", candidate),
	}}
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory:         inventory,
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-route",
		Now:               now,
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory.Accounts[0].Identity.AccountID = "mutated-input-account"
	inventory.Accounts[0].Candidates[0].Ref.CandidateID = "mutated-input-candidate"

	accounts := plan.Accounts()
	choice := accounts[0].Choice()
	choice.RequiredBuckets[0] = "mutated-choice"
	attempts := accounts[0].Attempts()
	attempts[0].Ordinal = 99
	attempts[0].Identity.AccountID = "mutated-attempt"
	accounts[0] = CodexFrozenDispatchAccount{}

	again := plan.Accounts()
	if len(again) != 1 {
		t.Fatalf("account count after caller mutation = %d, want 1", len(again))
	}
	if got := again[0].Choice().RequiredBuckets; !reflect.DeepEqual(got, []CapacityBucket{CapacityBucketBase}) {
		t.Fatalf("choice buckets mutated through alias: %+v", got)
	}
	if got := again[0].Attempts(); len(got) != 1 || got[0].Ordinal != 1 || got[0].Identity.AccountID == "mutated-attempt" ||
		got[0].Identity.AccountID == "mutated-input-account" || got[0].Candidate.CandidateID == "mutated-input-candidate" {
		t.Fatalf("attempts mutated through alias: %+v", got)
	}

	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	encodedAccounts, err := json.Marshal(plan.Accounts())
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{encodedPlan, encodedAccounts} {
		for _, private := range []string{
			"private-secret-token",
			"private-credential-account-id",
			"private-credential-email@test.invalid",
			"private-account-account-route",
			"private-user-account-route",
		} {
			if strings.Contains(string(encoded), private) {
				t.Fatalf("durable DTO exposed private value %q: %s", private, encoded)
			}
		}
	}
}

func frozenDispatchTestLogicalAccount(key codex.AccountKey, candidates ...codex.CredentialCandidate) codex.LogicalAccount {
	return codex.LogicalAccount{
		Key: key,
		Identity: codex.AccountIdentity{
			AccountID: "private-account-" + string(key),
			UserID:    "private-user-" + string(key),
			Email:     "private-email-" + string(key) + "@test.invalid",
			PlanType:  "pro",
		},
		Candidates: candidates,
		Routable:   true,
	}
}

func frozenDispatchCandidate(accountKey codex.AccountKey, candidateID codex.CandidateID, revision codex.Revision, source codex.CredentialSource, cqAuthored bool, expires time.Time) codex.CredentialCandidate {
	return codex.CredentialCandidate{
		Ref:             codex.CandidateRef{AccountKey: accountKey, CandidateID: candidateID},
		Revision:        revision,
		Source:          source,
		AccessExpiresAt: expires,
		CQAuthored:      cqAuthored,
		Routable:        true,
	}
}

func frozenDispatchObserveCapacity(t *testing.T, ledger *CodexCapacityLedger, account codex.AccountKey, bucket CapacityBucket, remaining int, now time.Time) {
	t.Helper()

	fact := ledger.NewObservationStream().Stamp(CapacityFact{
		AccountKey:   account,
		Bucket:       bucket,
		RemainingPct: remaining,
		Source:       CapacitySourceLiveRateLimits,
		ObservedAt:   now,
		Confidence:   CapacityConfidenceAuthoritative,
	})
	if !ledger.Observe(fact) {
		t.Fatal("observe frozen dispatch capacity")
	}
}

type frozenDispatchCancelOnErrCheckContext struct {
	context.Context
	cancelAt int
	checks   int
}

func (c *frozenDispatchCancelOnErrCheckContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
