package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type sessionPolicyPermitRecorder struct {
	requests []CallerDispatchPermitRequestV1
}

func (recorder *sessionPolicyPermitRecorder) IssueAndConsume(_ context.Context, request CallerDispatchPermitRequestV1) (CallerDispatchPermitV1, error) {
	recorder.requests = append(recorder.requests, request)
	return CallerDispatchPermitV1{Digest: strings.Repeat("d", 64)}, nil
}

func TestSessionPolicyEnforcementNarrowsPoolCapabilityAndDelegationAfterContinuity(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	session := []byte("private-session")
	policy := routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:           []AccountPoolV1{{Name: "team", Members: []codex.AccountKey{"account-a", "account-b"}}},
		SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, session), Pool: "team"}},
		CapabilityEvidence: []CapabilityEvidenceV1{
			{AccountKey: "account-a", State: CapabilitySupported},
			{AccountKey: "account-b", State: CapabilityUnsupported},
		},
		Delegations: []CallerDelegationV1{{Caller: "caller-safe-id", Accounts: []codex.AccountKey{"account-a"}, ExpiresAt: time.Unix(200, 0)}},
	})
	resolver := NewSessionPolicyResolver(key, policy)
	caller := RuntimeCallerAuthorityV1{Domain: NormalCallerCodex, SubjectID: "caller-safe-id"}
	decision, err := enforceSessionPolicy(resolver, caller, session, []codex.AccountKey{"account-a", "account-b", "account-c"}, "account-a", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Status != PolicyDecisionSelected || decision.Pool != "team" || len(decision.Allowed) != 1 || decision.Allowed[0] != "account-a" {
		t.Fatalf("decision = %#v", decision)
	}
	if _, err := enforceSessionPolicy(resolver, caller, session, []codex.AccountKey{"account-a", "account-b"}, "account-b", time.Unix(100, 0)); !errors.Is(err, ErrSessionPolicyContinuity) {
		t.Fatalf("continuity error = %v", err)
	}
}

func TestSessionPolicyEnforcementPreservesUnboundParity(t *testing.T) {
	resolver := NewSessionPolicyResolver([]byte("01234567890123456789012345678901"), routingPolicyV2ForTest(RoutingPolicyV1{SchemaVersion: 1}))
	global := []codex.AccountKey{"account-b", "account-a"}
	decision, err := enforceSessionPolicy(resolver, RuntimeCallerAuthorityV1{}, []byte("unbound"), global, "", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Status != PolicyDecisionUnbound || len(decision.Allowed) != 2 || decision.Allowed[0] != "account-a" || decision.Allowed[1] != "account-b" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestSessionPolicyEnforcementAllowsCodexCallerWithoutDelegation(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	session := []byte("bound-session")
	resolver := NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:           []AccountPoolV1{{Name: "cyber", Members: []codex.AccountKey{"account-a", "account-b"}}},
		SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, session), Pool: "cyber"}},
	}))
	caller := RuntimeCallerAuthorityV1{Domain: NormalCallerCodex, SubjectID: "worker-keyed-caller"}

	decision, err := enforceSessionPolicy(resolver, caller, session, []codex.AccountKey{"account-a", "account-b", "account-c"}, "", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Status != PolicyDecisionSelected || decision.Pool != "cyber" || len(decision.Allowed) != 2 || decision.Allowed[0] != "account-a" || decision.Allowed[1] != "account-b" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestSessionPolicyResolverAllowsExplicitPoolWithoutCapabilityEvidence(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	session := []byte("explicit-session")
	resolver := NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:           []AccountPoolV1{{Name: "team", Members: []codex.AccountKey{"account-a", "account-b"}}},
		SessionBindings: []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, session), Pool: "team"}},
	}))

	decision := resolver.Resolve(session, []codex.AccountKey{"account-b", "account-c", "account-a"})
	if decision.Status != PolicyDecisionSelected || len(decision.Allowed) != 2 || decision.Allowed[0] != "account-a" || decision.Allowed[1] != "account-b" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestSessionPolicyResolverScopesCapabilityRulesToNamedPool(t *testing.T) {
	predicate := CapabilityPredicateCoreV1{SchemaVersion: 1, Capability: "model.invoke", ProductSurface: "local_token_v1", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5"}
	policy := routingPolicyV2ForTest(RoutingPolicyV1{
		RoutingGeneration: 7, CapabilityPool: "cyber",
		Pools:                     []AccountPoolV1{{Name: "paid", Members: []codex.AccountKey{"paid"}}, {Name: "cyber", Members: []codex.AccountKey{"cyber"}}},
		CapabilityPredicates:      []CapabilityPredicateCoreV1{predicate},
		CapabilityRoutingEvidence: []CapabilityRoutingEvidenceV1{{AccountKey: "cyber"}},
	})
	resolver := NewSessionPolicyResolver(nil, policy)
	if _, _, active := resolver.capabilityPolicy(policy.Pools[0].ID, 7); active {
		t.Fatal("paid pool inherited cyber capability rules")
	}
	if _, _, active := resolver.capabilityPolicy(policy.Pools[1].ID, 7); !active {
		t.Fatal("cyber pool missing capability rules")
	}
}

func TestSessionPolicyEnforcementPrecedesDurableRequestJournal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account"}}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.Inventory = &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount("account", frozenDispatchCandidate("account", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
		frozenDispatchTestLogicalAccount("other", frozenDispatchCandidate("other", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour))),
	}}}
	key := []byte("01234567890123456789012345678901")
	factory.SessionPolicy = NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 7, EffectiveGeneration: 1,
		Pools:              []AccountPoolV1{{Name: "team", Members: []codex.AccountKey{"account"}}},
		SessionBindings:    []SessionBindingV1{{SessionDigest: keyedSessionDigest(key, []byte("session")), Pool: "team"}},
		CapabilityEvidence: []CapabilityEvidenceV1{{AccountKey: "account", State: CapabilitySupported}},
	}))
	permits := &sessionPolicyPermitRecorder{}
	factory.DispatchPermits = permits
	caller := RuntimeCallerAuthorityV1{
		Domain: NormalCallerCodex, SubjectID: "worker-keyed-caller", ConsumptionDigest: strings.Repeat("a", 64),
	}
	ctx := withRuntimeCallerAuthority(context.Background(), caller)
	ctx = withRuntimeCallerIdentity(ctx, "account\x00candidate-a\x00revision-a")

	prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	if len(permits.requests) != 1 {
		t.Fatalf("permit requests = %d, want 1", len(permits.requests))
	}
	request := permits.requests[0]
	if request.Pool != "team" || request.RoutingGeneration != 7 || request.SelectedAccount != "account" || len(request.AllowedAccounts) != 1 || request.AllowedAccounts[0] != "account" {
		t.Fatalf("permit request = %#v", request)
	}
	if runtime.plan.DispatchPermitDigest != strings.Repeat("d", 64) {
		t.Fatalf("journal permit digest = %q", runtime.plan.DispatchPermitDigest)
	}
}

func TestCapabilityRoutingSelectsFinalAccountBeforeDurableRequestJournal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	runtime := &codexHTTPRequestPlanTestRuntime{handle: &CodexLeaseRequestHandle{account: "account-b"}}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.DefaultAccountKey = "account-a"
	accountA := frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour)))
	accountA.Identity.AccountID = "workspace-a"
	accountB := frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-b", "revision-b", codex.SourceSystem, false, now.Add(time.Hour)))
	accountB.Identity.AccountID = "workspace-b"
	factory.Inventory = &codexHTTPRequestPlanTestInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
		accountA, accountB,
	}}}
	key := []byte("01234567890123456789012345678901")
	sessionDigest := keyedSessionDigest(key, []byte("session"))
	predicate := CapabilityPredicateCoreV1{SchemaVersion: 1, Capability: "model.invoke", ProductSurface: string(NormalCallerLocal), AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5"}
	factory.SessionPolicy = NewSessionPolicyResolver(key, routingPolicyV2ForTest(RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 7, EffectiveGeneration: 1,
		CapabilityPool:       "team",
		Pools:                []AccountPoolV1{{Name: "team", Members: []codex.AccountKey{"account-a", "account-b"}}},
		SessionBindings:      []SessionBindingV1{{SessionDigest: sessionDigest, Pool: "team"}},
		CapabilityEvidence:   []CapabilityEvidenceV1{{AccountKey: "account-a", State: CapabilitySupported}, {AccountKey: "account-b", State: CapabilitySupported}},
		CapabilityPredicates: []CapabilityPredicateCoreV1{predicate},
		CapabilityRoutingEvidence: []CapabilityRoutingEvidenceV1{
			capabilityEvidenceForTest("account-a", "workspace-a", predicate, "probe-a", nil, CapabilityEvidenceIneligible, 7, now),
			capabilityEvidenceForTest("account-b", "workspace-b", predicate, "probe-b", nil, CapabilityEvidenceEligible, 7, now),
		},
	}))
	permits := &sessionPolicyPermitRecorder{}
	factory.DispatchPermits = permits
	caller := RuntimeCallerAuthorityV1{Domain: NormalCallerLocal, SubjectID: "local-caller", ConsumptionDigest: strings.Repeat("a", 64)}

	prepared, err := factory.Build(withRuntimeCallerAuthority(context.Background(), caller), CodexHTTPRequestPlanInput{Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	if got := prepared.Dispatch.Accounts(); len(got) != 1 || got[0].Choice().AccountKey != "account-b" {
		t.Fatalf("final dispatch = %#v", got)
	}
	if len(permits.requests) != 1 || permits.requests[0].SelectedAccount != "account-b" || len(permits.requests[0].AllowedAccounts) != 1 || permits.requests[0].AllowedAccounts[0] != "account-b" {
		t.Fatalf("permit requests = %#v", permits.requests)
	}
	if runtime.calls != 1 || runtime.plan.DispatchPermitDigest == "" {
		t.Fatalf("durable begin = calls %d digest %q", runtime.calls, runtime.plan.DispatchPermitDigest)
	}
}
