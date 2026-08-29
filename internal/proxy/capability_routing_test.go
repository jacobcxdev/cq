package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCapabilityRoutingNarrowsAuthorityAndSortsFiniteEvidenceBeforeNull(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	finite := now.Add(time.Hour)
	predicate := CapabilityPredicateCoreV1{SchemaVersion: 1, Capability: "model.invoke", ProductSurface: "*", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5.1"}
	policy := RoutingPolicySnapshotV1{
		SchemaVersion: 1, Active: true, RoutingGeneration: 9,
		PoolID:     testPoolIDA,
		Pool:       AccountPoolV1{Name: "capability-model-invoke", Members: []codex.AccountKey{"account-a", "account-b"}},
		Predicates: []CapabilityPredicateCoreV1{predicate},
	}
	request := CallerRequestAuthorityV1{
		SchemaVersion: 1, AllowedAccounts: []codex.AccountKey{"account-c", "account-a"}, PreferredAccount: "account-a", AccountWorkspaces: []CapabilityAccountWorkspaceV1{{AccountKey: "account-a", Workspace: "workspace-a"}}, EvaluatedAt: now,
		FinalScope: CapabilityFinalScopeCoreV1{SchemaVersion: 1, RouteID: "responses", Provider: "codex", TransportKind: "http", ProductSurface: "desktop", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5.1", OutboundModel: "gpt-5.1", TransformationDigest: digestBytes([]byte("transform")), EncodedRequestDigest: digestBytes([]byte("request")), NormalCredentialOriginBindingDigest: digestBytes([]byte("origin"))},
	}
	evidence := []CapabilityRoutingEvidenceV1{
		capabilityEvidenceForTest("account-c", "workspace-a", predicate, "source-c", nil, CapabilityEvidenceEligible, 9, now),
		capabilityEvidenceForTest("account-a", "workspace-a", predicate, "source-null", nil, CapabilityEvidenceEligible, 9, now),
		capabilityEvidenceForTest("account-b", "workspace-a", predicate, "source-b", nil, CapabilityEvidenceEligible, 9, now),
		capabilityEvidenceForTest("account-a", "workspace-a", predicate, "source-finite", &finite, CapabilityEvidenceEligible, 9, now),
	}
	choice, err := ResolveCapabilityRoute(policy, evidence, request)
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "account-a" || choice.PoolID != testPoolIDA || choice.Pool != "capability-model-invoke" || choice.FinalScope.EffectiveModel != choice.FinalScope.OutboundModel {
		t.Fatalf("choice = %#v", choice)
	}
	if len(choice.EvidenceUsed) != 2 || choice.EvidenceUsed[0].Source != "source-finite" || choice.EvidenceUsed[1].Source != "source-null" {
		t.Fatalf("evidence order = %#v", choice.EvidenceUsed)
	}
	for _, account := range choice.AllowedAccounts {
		if account != "account-a" {
			t.Fatalf("choice escaped pool/caller authority: %q", account)
		}
	}
}

func TestCapabilityRoutingExcludesConflictingStaleAndDifferentlyScopedEvidence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	predicate := CapabilityPredicateCoreV1{SchemaVersion: 1, Capability: "model.invoke", ProductSurface: "desktop", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5"}
	policy := RoutingPolicySnapshotV1{SchemaVersion: 1, Active: true, RoutingGeneration: 4, PoolID: testPoolIDA, Pool: AccountPoolV1{Name: "derived", Members: []codex.AccountKey{"account"}}, Predicates: []CapabilityPredicateCoreV1{predicate}}
	request := CallerRequestAuthorityV1{SchemaVersion: 1, AllowedAccounts: []codex.AccountKey{"account"}, PreferredAccount: "account", AccountWorkspaces: []CapabilityAccountWorkspaceV1{{AccountKey: "account", Workspace: "workspace"}}, EvaluatedAt: now, FinalScope: CapabilityFinalScopeCoreV1{SchemaVersion: 1, RouteID: "responses", Provider: "codex", TransportKind: "http", ProductSurface: "desktop", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5", OutboundModel: "gpt-5", TransformationDigest: digestBytes([]byte("t")), EncodedRequestDigest: digestBytes([]byte("r")), NormalCredentialOriginBindingDigest: digestBytes([]byte("o"))}}
	expired := now.Add(-time.Second)
	evidence := []CapabilityRoutingEvidenceV1{
		capabilityEvidenceForTest("account", "workspace", predicate, "eligible", nil, CapabilityEvidenceEligible, 4, now),
		capabilityEvidenceForTest("account", "workspace", predicate, "conflict", nil, CapabilityEvidenceUnknown, 4, now),
		capabilityEvidenceForTest("account", "workspace", predicate, "stale", &expired, CapabilityEvidenceEligible, 3, now),
		capabilityEvidenceForTest("account", "other-workspace", predicate, "foreign", nil, CapabilityEvidenceEligible, 4, now),
	}
	if _, err := ResolveCapabilityRoute(policy, evidence, request); !errors.Is(err, ErrCapabilityRouteUnavailable) {
		t.Fatalf("ResolveCapabilityRoute error = %v", err)
	}
}

func TestCapabilityRoutingPrefersOrdinaryEligibleSelection(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	predicate := CapabilityPredicateCoreV1{SchemaVersion: 1, Capability: "model.invoke", ProductSurface: "desktop", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5"}
	policy := RoutingPolicySnapshotV1{SchemaVersion: 1, Active: true, RoutingGeneration: 4, PoolID: testPoolIDA, Pool: AccountPoolV1{Name: "derived", Members: []codex.AccountKey{"account-a", "account-b"}}, Predicates: []CapabilityPredicateCoreV1{predicate}}
	request := CallerRequestAuthorityV1{
		SchemaVersion: 1, AllowedAccounts: []codex.AccountKey{"account-a", "account-b"}, PreferredAccount: "account-b", EvaluatedAt: now,
		AccountWorkspaces: []CapabilityAccountWorkspaceV1{{AccountKey: "account-a", Workspace: "workspace-a"}, {AccountKey: "account-b", Workspace: "workspace-b"}},
		FinalScope:        CapabilityFinalScopeCoreV1{SchemaVersion: 1, RouteID: "responses", Provider: "codex", TransportKind: "http", ProductSurface: "desktop", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5", OutboundModel: "gpt-5", TransformationDigest: digestBytes([]byte("t")), EncodedRequestDigest: digestBytes([]byte("r")), NormalCredentialOriginBindingDigest: digestBytes([]byte("o"))},
	}
	evidence := []CapabilityRoutingEvidenceV1{
		capabilityEvidenceForTest("account-a", "workspace-a", predicate, "a", nil, CapabilityEvidenceEligible, 4, now),
		capabilityEvidenceForTest("account-b", "workspace-b", predicate, "b", nil, CapabilityEvidenceEligible, 4, now),
	}
	choice, err := ResolveCapabilityRoute(policy, evidence, request)
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "account-b" || len(choice.AllowedAccounts) != 2 {
		t.Fatalf("choice = %#v", choice)
	}
}

func TestCapabilityRoutingInactiveReturnsBeforeEvidenceEffects(t *testing.T) {
	source := &panicCapabilityEvidenceSource{}
	_, err := ResolveCapabilityRouteFromSource(context.Background(), RoutingPolicySnapshotV1{SchemaVersion: 1, Active: false}, source, CallerRequestAuthorityV1{})
	if !errors.Is(err, ErrCapabilityRoutingInactive) {
		t.Fatalf("error = %v", err)
	}
	if source.calls != 0 {
		t.Fatalf("inactive resolver loaded evidence %d times", source.calls)
	}
}

type panicCapabilityEvidenceSource struct{ calls int }

func (source *panicCapabilityEvidenceSource) Load(context.Context) ([]CapabilityRoutingEvidenceV1, error) {
	source.calls++
	panic("inactive resolver performed evidence work")
}

func capabilityEvidenceForTest(account codex.AccountKey, workspace string, predicate CapabilityPredicateCoreV1, source string, expires *time.Time, state CapabilityRoutingEvidenceState, generation uint64, observed time.Time) CapabilityRoutingEvidenceV1 {
	productSurface := predicate.ProductSurface
	if productSurface == "*" {
		productSurface = "desktop"
	}
	return CapabilityRoutingEvidenceV1{
		SchemaVersion: 1, AccountKey: account, AccountKeyHMAC: digestBytes([]byte(account)), Workspace: workspace, Capability: predicate.Capability, ProductSurface: productSurface,
		AccessPath: predicate.AccessPath, AuthMode: predicate.AuthMode, RequestedModel: predicate.RequestedModel, EffectiveModel: predicate.EffectiveModel,
		State: state, Source: source, ObservedAt: observed, ExpiresAt: expires, RoutingGeneration: generation, Authenticated: true,
	}
}
