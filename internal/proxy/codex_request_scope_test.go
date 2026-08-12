package proxy

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type staticRouteChooser struct {
	choice RouteChoice
	err    error
}

func (c staticRouteChooser) Choose(context.Context, CodexRouteRequirements, ...codex.SelectionExclusion) (RouteChoice, error) {
	return c.choice, c.err
}

type staticCredentialInventory struct {
	inventory codex.Inventory
	err       error
}

func (i staticCredentialInventory) List(context.Context) (codex.Inventory, error) {
	return i.inventory, i.err
}

func TestCodexRequestScopeOrdersCandidatesWithoutSecrets(t *testing.T) {
	now := time.Unix(100, 0)
	accountKey := codex.AccountKey("account")
	scope := &CodexRequestScope{
		Chooser: staticRouteChooser{choice: RouteChoice{AccountKey: accountKey, RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"}},
		Inventory: staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{
			Key:      accountKey,
			Identity: codex.AccountIdentity{AccountID: "strong-account", UserID: "strong-user", Email: "private@example.test"},
			Routable: true,
			Candidates: []codex.CredentialCandidate{
				{Ref: codex.CandidateRef{AccountKey: accountKey, CandidateID: "managed"}, Revision: "new", Source: codex.SourceManaged, CQAuthored: true, AccessExpiresAt: now.Add(time.Hour), Routable: true},
				{Ref: codex.CandidateRef{AccountKey: accountKey, CandidateID: "system"}, Revision: "accepted", Source: codex.SourceSystem, AccessExpiresAt: now.Add(2 * time.Hour), Routable: true},
				{Ref: codex.CandidateRef{AccountKey: accountKey, CandidateID: "blocked"}, Revision: "blocked", Source: codex.SourceManaged, DispatchBlocked: true, Routable: true},
			},
		}}}},
		Now: func() time.Time { return now },
	}
	plan, err := scope.Plan(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Attempts) != 2 || plan.Attempts[0].Candidate.CandidateID != "system" || plan.Attempts[1].Candidate.CandidateID != "managed" {
		t.Fatalf("attempts = %+v", plan.Attempts)
	}
	if plan.refreshAttempt == nil || plan.refreshAttempt.Candidate.CandidateID != "managed" {
		t.Fatalf("refresh attempt = %+v", plan.refreshAttempt)
	}
	if plan.Attempts[0].Source != codex.SourceSystem || plan.Attempts[0].Identity != (codex.AccountIdentity{AccountID: "strong-account", UserID: "strong-user"}) {
		t.Fatalf("attempt authority = %+v", plan.Attempts[0])
	}
	for _, field := range []string{"AccessToken", "RefreshToken", "IDToken", "Credential", "Material"} {
		if _, ok := reflect.TypeOf(plan.Attempts[0]).FieldByName(field); ok {
			t.Fatalf("CandidateAttempt exposes secret field %q", field)
		}
	}
}

func TestCodexRequestScopeSkipsUnroutableCandidate(t *testing.T) {
	accountKey := codex.AccountKey("account")
	identity := codex.AccountIdentity{AccountID: "strong-account", UserID: "strong-user"}
	scope := &CodexRequestScope{
		Chooser: staticRouteChooser{choice: RouteChoice{AccountKey: accountKey}},
		Inventory: staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{
			Key: accountKey, Identity: identity, Routable: true,
			Candidates: []codex.CredentialCandidate{
				{Ref: codex.CandidateRef{AccountKey: accountKey, CandidateID: "unroutable"}, Revision: "bad", Source: codex.SourceExternal},
				{Ref: codex.CandidateRef{AccountKey: accountKey, CandidateID: "ready"}, Revision: "good", Source: codex.SourceExternal, Routable: true},
			},
		}}}},
	}
	plan, err := scope.Plan(context.Background(), CodexRouteRequirements{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Attempts) != 1 || plan.Attempts[0].Candidate.CandidateID != "ready" {
		t.Fatalf("attempts = %+v", plan.Attempts)
	}
}

func TestCodexRequestScopeRejectsUnroutableAccount(t *testing.T) {
	accountKey := codex.AccountKey("account")
	scope := &CodexRequestScope{
		Chooser: staticRouteChooser{choice: RouteChoice{AccountKey: accountKey}},
		Inventory: staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{
			Key: accountKey, Identity: codex.AccountIdentity{AccountID: "strong-account", UserID: "strong-user"}, Routable: false,
			Candidates: []codex.CredentialCandidate{{
				Ref: codex.CandidateRef{AccountKey: accountKey, CandidateID: "candidate"}, Revision: "revision",
				Source: codex.SourceSystem, Routable: true,
			}},
		}}}},
	}
	if _, err := scope.Plan(context.Background(), CodexRouteRequirements{}, ""); err == nil {
		t.Fatal("unroutable selected account produced a dispatch plan")
	}
}

func TestCodexRequestScopeRejectsIncompleteStrongIdentity(t *testing.T) {
	accountKey := codex.AccountKey("account")
	scope := &CodexRequestScope{
		Chooser: staticRouteChooser{choice: RouteChoice{AccountKey: accountKey}},
		Inventory: staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{
			Key: accountKey, Identity: codex.AccountIdentity{AccountID: "strong-account"}, Routable: true,
			Candidates: []codex.CredentialCandidate{{
				Ref: codex.CandidateRef{AccountKey: accountKey, CandidateID: "candidate"}, Revision: "revision",
				Source: codex.SourceSystem, Routable: true,
			}},
		}}}},
	}
	if _, err := scope.Plan(context.Background(), CodexRouteRequirements{}, ""); err == nil {
		t.Fatal("selected account without a complete strong identity produced a dispatch plan")
	}
}

func TestCodexRequestScopeFailsWhenSelectedIdentityDisappears(t *testing.T) {
	scope := &CodexRequestScope{
		Chooser:   staticRouteChooser{choice: RouteChoice{AccountKey: "missing"}},
		Inventory: staticCredentialInventory{},
	}
	_, err := scope.Plan(context.Background(), CodexRouteRequirements{}, "")
	if err == nil || err.Error() != "selected Codex account disappeared from inventory" {
		t.Fatalf("err = %v", err)
	}
}

func TestCodexRequestScopePropagatesInventoryBoundaryError(t *testing.T) {
	scope := &CodexRequestScope{
		Chooser:   staticRouteChooser{choice: RouteChoice{AccountKey: "account"}},
		Inventory: staticCredentialInventory{err: errors.New("offline")},
	}
	_, err := scope.Plan(context.Background(), CodexRouteRequirements{}, "")
	if err == nil || err.Error() != "list Codex credential inventory: offline" {
		t.Fatalf("err = %v", err)
	}
}
