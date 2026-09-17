package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
)

type staticResetInventory struct {
	inventory Inventory
	err       error
	calls     int
}

func (s *staticResetInventory) List(context.Context) (Inventory, error) {
	s.calls++
	return s.inventory, s.err
}

type recordingResetResolver struct {
	material CredentialMaterial
	err      error
	plans    []PlannedCandidate
}

func (r *recordingResetResolver) ResolveExact(_ context.Context, planned PlannedCandidate) (CredentialMaterial, error) {
	r.plans = append(r.plans, planned)
	return r.material, r.err
}

type recordingResetRefresh struct {
	result RefreshResult
	err    error
	calls  int
}

func resetResolvedMaterial(accountID, userID string) CredentialMaterial {
	return CredentialMaterial{
		AccessToken: "access-token",
		AccountID:   accountID,
		IDToken:     fakeCodexJWT("", accountID, userID, ""),
	}
}

func (r *recordingResetRefresh) Refresh(context.Context, CandidateRef, Revision) (RefreshResult, error) {
	r.calls++
	return r.result, r.err
}

func resetAccountInventory() Inventory {
	return Inventory{Accounts: []LogicalAccount{
		{
			Key:      "account-a",
			Identity: AccountIdentity{AccountID: "acct-a", UserID: "user-a", Email: "a@example.com", PlanType: "pro"},
			Active:   true,
			Routable: true,
			Candidates: []CredentialCandidate{{
				Ref:      CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"},
				Revision: "revision-a", Source: SourceSystem, Routable: true,
				AccessExpiresAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			}},
		},
		{
			Key:      "account-b",
			Identity: AccountIdentity{AccountID: "acct-b", UserID: "user-b", Email: "b@example.com", PlanType: "team"},
			Routable: false,
			Candidates: []CredentialCandidate{{
				Ref:      CandidateRef{AccountKey: "account-b", CandidateID: "candidate-b"},
				Revision: "revision-b", Source: SourceManaged, CQAuthored: true, RefreshEligible: true,
				AccessExpiresAt: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
			}},
		},
		{
			Key:      "account-blocked",
			Identity: AccountIdentity{AccountID: "acct-blocked", UserID: "user-blocked", Email: "blocked@example.com"},
			Candidates: []CredentialCandidate{{
				Ref:      CandidateRef{AccountKey: "account-blocked", CandidateID: "candidate-blocked"},
				Revision: "revision-blocked", Source: SourceManaged, DispatchBlocked: true,
			}},
		},
	}}
}

func TestProjectVisibleAccountsMatchesAccountsDiscoverInventory(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	inventory := resetAccountInventory()
	visible := ProjectVisibleAccounts(inventory, now)
	if len(visible) != 2 {
		t.Fatalf("visible accounts = %+v", visible)
	}
	if visible[0].Email != "a@example.com" || visible[1].Email != "b@example.com" {
		t.Fatalf("visible order = %+v", visible)
	}
	if !visible[1].refreshable {
		t.Fatal("managed CQ-owned account not marked refreshable")
	}
	if visible[1].planned.Ref.CandidateID != "candidate-b" {
		t.Fatalf("planned candidate = %+v", visible[1].planned)
	}

	manager := &Accounts{Inventory: &staticResetInventory{inventory: inventory}, Now: func() time.Time { return now }}
	listed, err := manager.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Account{
		{AccountID: "acct-a", Email: "a@example.com", Label: "pro", Active: true, SwitchID: "a@example.com"},
		{AccountID: "acct-b", Email: "b@example.com", Label: "team", SwitchID: "b@example.com"},
	}
	if !reflect.DeepEqual(listed, want) {
		t.Fatalf("listed = %+v, want %+v", listed, want)
	}
}

func TestResetAccountSnapshotResolvesExistingReferenceForms(t *testing.T) {
	backend := ResetBackend{
		Inventory: &staticResetInventory{inventory: resetAccountInventory()},
		Aliases: func() (AccountAliasIndex, error) {
			return AccountAliasIndex{rows: []accountAliasRow{{Alias: "secondary", AccountKey: "account-b"}}}, nil
		},
		Now: func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
	}
	snapshot, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"account-b", "B@example.com", "SECONDARY"} {
		got, err := snapshot.ResolveReference(reference)
		if err != nil || got.AccountKey != "account-b" {
			t.Fatalf("ResolveReference(%q) = %+v, %v", reference, got, err)
		}
	}
	if _, err := snapshot.ResolveReference("blocked@example.com"); err == nil {
		t.Fatal("blocked invisible account resolved")
	}
}

func TestResetBackendListCreditsResolvesExactPlannedGeneration(t *testing.T) {
	inventory := resetAccountInventory()
	resolver := &recordingResetResolver{material: resetResolvedMaterial("acct-a", "user-a")}
	backend := ResetBackend{
		Inventory: &staticResetInventory{inventory: inventory},
		Resolver:  resolver,
		Credits: ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
			return resetJSONResponse(http.StatusOK, `{"credits":[],"available_count":0}`), nil
		})},
		Now: func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
	}
	snapshot, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ListCredits(context.Background(), snapshot.Accounts[0]); err != nil {
		t.Fatal(err)
	}
	if len(resolver.plans) != 1 || resolver.plans[0].Ref.CandidateID != "candidate-a" || resolver.plans[0].Revision != "revision-a" {
		t.Fatalf("resolved plans = %+v", resolver.plans)
	}
}

func TestResetBackendRefreshesOnlyEligibleManagedAfterUnauthorized(t *testing.T) {
	inventory := resetAccountInventory()
	resolver := &recordingResetResolver{material: resetResolvedMaterial("acct-b", "user-b")}
	refresh := &recordingResetRefresh{result: RefreshResult{
		Ref: CandidateRef{AccountKey: "account-b", CandidateID: "candidate-b"}, Revision: "revision-b2",
	}}
	statuses := []int{http.StatusUnauthorized, http.StatusOK}
	backend := ResetBackend{
		Inventory: &staticResetInventory{inventory: inventory}, Resolver: resolver, Refresh: refresh,
		Credits: ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
			status := statuses[0]
			statuses = statuses[1:]
			if status == http.StatusOK {
				return resetJSONResponse(status, `{"credits":[],"available_count":0}`), nil
			}
			return resetJSONResponse(status, `{}`), nil
		})},
		Now: func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
	}
	snapshot, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ListCredits(context.Background(), snapshot.Accounts[1]); err != nil {
		t.Fatal(err)
	}
	if refresh.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh.calls)
	}
	if len(resolver.plans) != 2 || resolver.plans[1].Revision != "revision-b2" {
		t.Fatalf("resolved plans = %+v", resolver.plans)
	}
}

func TestResetBackendNeverRefreshesIneligibleSources(t *testing.T) {
	tests := []struct {
		name     string
		account  int
		eligible bool
	}{
		{name: "system", account: 0},
		{name: "managed exported", account: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := resetAccountInventory()
			inventory.Accounts[1].Candidates[0].RefreshEligible = tt.eligible
			refresh := &recordingResetRefresh{}
			backend := ResetBackend{
				Inventory: &staticResetInventory{inventory: inventory},
				Resolver: &recordingResetResolver{material: resetResolvedMaterial(
					inventory.Accounts[tt.account].Identity.AccountID,
					inventory.Accounts[tt.account].Identity.UserID,
				)},
				Refresh: refresh,
				Credits: ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
					return resetJSONResponse(http.StatusUnauthorized, `{}`), nil
				})},
				Now: func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
			}
			snapshot, err := backend.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.ListCredits(context.Background(), snapshot.Accounts[tt.account])
			var statusErr *ResetHTTPError
			if !errors.As(err, &statusErr) || statusErr.Status != http.StatusUnauthorized {
				t.Fatalf("error = %T %v", err, err)
			}
			if refresh.calls != 0 {
				t.Fatalf("refresh calls = %d", refresh.calls)
			}
		})
	}
}

func TestResetBackendConsumeUsesExactCredential(t *testing.T) {
	resolver := &recordingResetResolver{material: resetResolvedMaterial("acct-a", "user-a")}
	backend := ResetBackend{
		Inventory: &staticResetInventory{inventory: resetAccountInventory()}, Resolver: resolver,
		Credits: ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
			return resetJSONResponse(http.StatusOK, `{"code":"reset","windows_reset":2}`), nil
		})},
		Now: func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
	}
	snapshot, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Consume(context.Background(), snapshot.Accounts[0], "credit-1", "request-1")
	if err != nil || result.Outcome != ConsumeReset {
		t.Fatalf("Consume() = %+v, %v", result, err)
	}
	if len(resolver.plans) != 1 || resolver.plans[0].Ref.CandidateID != "candidate-a" {
		t.Fatalf("plans = %+v", resolver.plans)
	}
}

func TestResetCandidateRefreshEligibilityRequiresOwnedReadyLineage(t *testing.T) {
	base := rawCandidate{
		source:     SourceManaged,
		cqAuthored: true,
		account:    CodexAccount{RefreshToken: "refresh"},
		metadata: &ManagedMetadata{
			Version: 1, Provenance: ProvenanceCQOAuth,
			RefreshOwnership: RefreshCQOwnedNeverExported, OperationState: OperationReady,
		},
	}
	if !resetCandidateRefreshEligible(base) {
		t.Fatal("owned ready lineage is not eligible")
	}

	tests := []struct {
		name   string
		mutate func(*rawCandidate)
	}{
		{name: "system", mutate: func(c *rawCandidate) { c.source = SourceSystem }},
		{name: "borrowed", mutate: func(c *rawCandidate) { c.metadata.Provenance = ProvenanceSystemBorrowed }},
		{name: "exported", mutate: func(c *rawCandidate) { c.metadata.RefreshOwnership = RefreshExportedToSystem }},
		{name: "uncertain", mutate: func(c *rawCandidate) { c.metadata.OperationState = OperationRotationUncertain }},
		{name: "operation pending", mutate: func(c *rawCandidate) { c.metadata.OperationID = "operation" }},
		{name: "no refresh token", mutate: func(c *rawCandidate) { c.account.RefreshToken = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := base
			metadata := *base.metadata
			candidate.metadata = &metadata
			tt.mutate(&candidate)
			if resetCandidateRefreshEligible(candidate) {
				t.Fatal("ineligible lineage accepted")
			}
		})
	}
}

func TestResetBackendListCandidateFallback(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			inventory := resetAccountInventory()
			logical := &inventory.Accounts[0]
			second := logical.Candidates[0]
			second.Ref.CandidateID = "candidate-second"
			second.Revision = "second"
			second.Source = SourceManaged
			second.RefreshEligible = true
			logical.Candidates = append(logical.Candidates, second)
			resolver := &recordingResetResolver{material: resetResolvedMaterial("acct-a", "user-a")}
			refresh := &recordingResetRefresh{}
			calls := 0
			backend := ResetBackend{Inventory: &staticResetInventory{inventory: inventory}, Resolver: resolver, Refresh: refresh, Now: func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) }, Credits: ResetCreditClient{HTTP: resetDoerFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("ChatGPT-Account-Id") != "acct-a" {
					t.Error("identity changed")
				}
				if calls == 1 {
					return resetJSONResponse(status, `{}`), nil
				}
				return resetJSONResponse(200, `{"credits":[],"available_count":0}`), nil
			})}}
			snapshot, err := backend.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.ListCredits(context.Background(), snapshot.Accounts[0])
			want := 1
			if status == 401 || status == 403 {
				want = 2
				if err != nil {
					t.Errorf("existing credential should succeed: %v", err)
				}
			}
			if calls != want || refresh.calls != 0 {
				t.Fatalf("HTTP=%d refresh=%d, want HTTP=%d refresh=0", calls, refresh.calls, want)
			}
		})
	}
}

func TestResetBackendListRefreshesAfterAllCandidates(t *testing.T) {
	inventory := resetAccountInventory()
	logical := &inventory.Accounts[0]
	second := logical.Candidates[0]
	second.Ref.CandidateID = "managed-second"
	second.Revision = "managed-old"
	second.Source = SourceManaged
	second.RefreshEligible = true
	logical.Candidates = append(logical.Candidates, second)
	resolver := &recordingResetResolver{material: resetResolvedMaterial("acct-a", "user-a")}
	refresh := &recordingResetRefresh{result: RefreshResult{Ref: second.Ref, Revision: "managed-new"}}
	calls := 0
	backend := ResetBackend{Inventory: &staticResetInventory{inventory: inventory}, Resolver: resolver, Refresh: refresh, Now: func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) }, Credits: ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return resetJSONResponse(401, `{}`), nil
		}
		return resetJSONResponse(200, `{"credits":[],"available_count":0}`), nil
	})}}
	snapshot, _ := backend.Snapshot(context.Background())
	_, err := backend.ListCredits(context.Background(), snapshot.Accounts[0])
	if err != nil || calls != 3 || refresh.calls != 1 || len(resolver.plans) != 3 || resolver.plans[1].Ref.CandidateID != "managed-second" || resolver.plans[2].Revision != "managed-new" {
		t.Fatalf("calls=%d refresh=%d plans=%+v err=%v", calls, refresh.calls, resolver.plans, err)
	}
}

func TestProjectVisibleAccountsMarksUnselectableReferences(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			inventory := resetAccountInventory()
			if duplicate {
				inventory.Accounts = append(inventory.Accounts, inventory.Accounts[0])
			} else {
				inventory.Accounts[0].Unstable = true
			}
			rows := ProjectVisibleAccounts(inventory, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
			if len(rows) == 0 || !rows[0].Unselectable {
				t.Fatalf("unselectable identity advertised as selector: %+v", rows)
			}
		})
	}
}

type resetResolverFunc func(context.Context, PlannedCandidate) (CredentialMaterial, error)

func (f resetResolverFunc) ResolveExact(ctx context.Context, p PlannedCandidate) (CredentialMaterial, error) {
	return f(ctx, p)
}
func TestResetBackendListRefreshRetainsReplannedRevision(t *testing.T) {
	inventory := resetAccountInventory()
	logical := &inventory.Accounts[1]
	logical.Routable = true
	logical.Candidates[0].Routable = true
	backendInventory := &staticResetInventory{inventory: inventory}
	refresh := &recordingResetRefresh{result: RefreshResult{Ref: logical.Candidates[0].Ref, Revision: "after-refresh"}}
	oldPlan := ProjectVisibleAccounts(inventory, time.Now())[1]
	logical.Candidates[0].Revision = "replanned"
	calls := 0
	resolver := resetResolverFunc(func(_ context.Context, plan PlannedCandidate) (CredentialMaterial, error) {
		if plan.Revision == "revision-b" {
			return CredentialMaterial{}, ErrStaleRevision
		}
		return resetResolvedMaterial("acct-b", "user-b"), nil
	})
	backend := ResetBackend{Inventory: backendInventory, Resolver: resolver, Refresh: refresh, Credits: ResetCreditClient{HTTP: resetDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return resetJSONResponse(401, `{}`), nil
		}
		return resetJSONResponse(200, `{"credits":[],"available_count":0}`), nil
	})}}
	// Broker receives the refreshed snapshot revision, never the stale caller plan.
	expected := Revision("")
	backend.Refresh = resetRefreshFunc(func(ctx context.Context, ref CandidateRef, rev Revision) (RefreshResult, error) {
		expected = rev
		return refresh.Refresh(ctx, ref, rev)
	})
	_, err := backend.ListCredits(context.Background(), oldPlan)
	if err != nil || expected != "replanned" {
		t.Fatalf("refresh revision=%q error=%v", expected, err)
	}
}

type resetRefreshFunc func(context.Context, CandidateRef, Revision) (RefreshResult, error)

func (f resetRefreshFunc) Refresh(ctx context.Context, ref CandidateRef, rev Revision) (RefreshResult, error) {
	return f(ctx, ref, rev)
}
