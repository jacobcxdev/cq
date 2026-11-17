package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexHTTPRequestPlanFactoryKeepsIndeterminateBindingAfterQuotaRetry(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			accounts := []codex.AccountKey{"account-a", "account-b"}
			seed := codexLeaseRuntimeTestPlan("seed-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
			seed.Accounts = accounts
			seed.Key.Lane.Session = "session"
			seed.Key.Lane.Thread = "thread"
			handle, err := runtimeLease.BeginRequest(seed)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "state-a", HasTurnState: true})
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := handle.Drain(); err != nil {
				t.Fatal(err)
			}

			current := seed
			current.Key.Turn = "turn"
			current.Slots = []CodexLeaseAttemptSlotPlan{
				{AccountKey: "account-b", CandidateID: "candidate-stale", Kind: CodexAttemptSlotDirect},
				{AccountKey: "account-b", CandidateID: "candidate-current", Kind: CodexAttemptSlotDirect},
			}
			handle, err = runtimeLease.BeginRequest(current)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.RecordQuotaExhausted(2)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.Indeterminate()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := handle.Drain(); err != nil {
				t.Fatal(err)
			}

			now := time.Unix(1_700_000_000, 0).UTC()
			snapshot, err := coordinator.LoadRouteSnapshot(context.Background(), current.Key, accounts, current.Authority)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.BoundAccountKey != "account-b" || !snapshot.BoundRequiresAccount || snapshot.AffinityAccountKey != "account-a" || len(snapshot.QuotaExhaustedAccountKeys) != 1 || snapshot.QuotaExhaustedAccountKeys[0] != "account-b" {
				t.Fatalf("uncertain quota retry snapshot = %#v", snapshot)
			}
			inventory := codex.Inventory{Accounts: []codex.LogicalAccount{
				frozenDispatchTestLogicalAccount("account-a", frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour))),
				frozenDispatchTestLogicalAccount("account-b", frozenDispatchCandidate("account-b", "candidate-current", "revision-b", codex.SourceManaged, true, now.Add(time.Hour))),
			}}
			factory := &CodexHTTPRequestPlanFactory{
				Inventory: &codexHTTPRequestPlanTestInventory{inventory: inventory},
				Routes:    coordinator, Runtime: runtimeLease, Authority: seed.Authority,
				DefaultAccountKey: "account-a", TransportKind: transport,
				Now: func() time.Time { return now },
			}
			ctx := withRuntimeCallerAuthority(context.Background(), RuntimeCallerAuthorityV1{Domain: NormalCallerCodex})
			ctx = withRuntimeCallerIdentity(ctx, "account-a\x00candidate-a\x00revision-a")
			prepared, err := factory.Build(ctx, CodexHTTPRequestPlanInput{Encoded: frozenRequestBody("gpt-5", CodexRequestTurn, "retry")})
			if err != nil {
				var planErr *CodexHTTPRequestPlanError
				if errors.As(err, &planErr) {
					t.Fatalf("retry after uncertain dispatch: stage %s reason %s", planErr.Code, planErr.Reason)
				}
				t.Fatalf("retry after uncertain dispatch: %v", err)
			}
			defer prepared.Frozen.Release()
			if prepared.leaseHandle.AccountKey() != "account-b" {
				t.Fatalf("retry account = %q, want current binding account-b", prepared.leaseHandle.AccountKey())
			}
		})
	}
}
