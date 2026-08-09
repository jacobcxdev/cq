package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type providerDoerFunc func(*http.Request) (*http.Response, error)

func (f providerDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

type providerCredentialAuthority struct {
	mu               sync.Mutex
	inventories      []Inventory
	listErr          error
	listCalls        int
	resolvePlans     []PlannedCandidate
	resolveExact     func(int, PlannedCandidate) (CredentialMaterial, error)
	resolveWeak      func(CandidateRef) (CredentialMaterial, error)
	weakResolveCalls int
}

func (a *providerCredentialAuthority) List(context.Context) (Inventory, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listCalls++
	if a.listErr != nil {
		return Inventory{}, a.listErr
	}
	index := a.listCalls - 1
	if index >= len(a.inventories) {
		index = len(a.inventories) - 1
	}
	if index < 0 {
		return Inventory{}, errors.New("unexpected inventory read")
	}
	return a.inventories[index], nil
}

func TestFetchCredentialAuthorityLossReturnsPrivacySafeFetchError(t *testing.T) {
	authorityErr := fmt.Errorf("private socket path and account metadata: %w", ErrCredentialAuthorityUnavailable)
	tests := []struct {
		name      string
		authority *providerCredentialAuthority
		broker    *providerRefreshBroker
		status    int
		wantCalls int
	}{
		{
			name:      "inventory list",
			authority: &providerCredentialAuthority{listErr: authorityErr},
		},
		{
			name: "exact resolution",
			authority: &providerCredentialAuthority{
				inventories: []Inventory{providerInventory(
					AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"},
					providerCandidate(CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}, "revision-1", SourceExternal, time.Now().Add(time.Hour)),
				)},
				resolveExact: func(int, PlannedCandidate) (CredentialMaterial, error) {
					return CredentialMaterial{}, authorityErr
				},
			},
		},
		{
			name: "managed refresh",
			authority: &providerCredentialAuthority{
				inventories: []Inventory{providerInventory(
					AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"},
					providerCandidate(CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}, "revision-1", SourceManaged, time.Time{}),
				)},
				resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
					return testCredentialMaterial(planned.Identity, "expired-secret"), nil
				},
			},
			broker: &providerRefreshBroker{refresh: func(CandidateRef, Revision) (RefreshResult, error) {
				return RefreshResult{}, authorityErr
			}},
			status:    http.StatusUnauthorized,
			wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestCalls := 0
			p := &Provider{
				client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
					requestCalls++
					return providerUsageResponse(test.status), nil
				}),
				fs: newFakeFS(), inventory: test.authority, secrets: test.authority, refreshBroker: test.broker,
			}

			results, err := p.Fetch(context.Background(), time.Now())
			if err != nil {
				t.Fatalf("Fetch error = %v, want privacy-safe result", err)
			}
			if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "fetch_error" {
				t.Fatalf("results = %+v, want one fetch_error", results)
			}
			if got := results[0].Error.Message; got != "Codex credential coordinator unavailable" {
				t.Fatalf("error message = %q, want privacy-safe coordinator error", got)
			}
			if requestCalls != test.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", requestCalls, test.wantCalls)
			}
		})
	}
}

func (a *providerCredentialAuthority) Resolve(_ context.Context, ref CandidateRef) (CredentialMaterial, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.weakResolveCalls++
	if a.resolveWeak != nil {
		return a.resolveWeak(ref)
	}
	return CredentialMaterial{}, errors.New("weak credential resolution must not be used")
}

func (a *providerCredentialAuthority) ResolveExact(_ context.Context, planned PlannedCandidate) (CredentialMaterial, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resolvePlans = append(a.resolvePlans, planned)
	if a.resolveExact == nil {
		return CredentialMaterial{}, errors.New("unexpected exact credential resolution")
	}
	return a.resolveExact(len(a.resolvePlans), planned)
}

func (a *providerCredentialAuthority) snapshot() (int, int, []PlannedCandidate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listCalls, a.weakResolveCalls, append([]PlannedCandidate(nil), a.resolvePlans...)
}

type providerRefreshBroker struct {
	mu        sync.Mutex
	calls     int
	refs      []CandidateRef
	revisions []Revision
	refresh   func(CandidateRef, Revision) (RefreshResult, error)
}

func (b *providerRefreshBroker) Refresh(_ context.Context, ref CandidateRef, revision Revision) (RefreshResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.refs = append(b.refs, ref)
	b.revisions = append(b.revisions, revision)
	if b.refresh == nil {
		return RefreshResult{}, errors.New("unexpected managed refresh")
	}
	return b.refresh(ref, revision)
}

func (b *providerRefreshBroker) snapshot() (int, []CandidateRef, []Revision) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, append([]CandidateRef(nil), b.refs...), append([]Revision(nil), b.revisions...)
}

func providerInventory(identity AccountIdentity, candidates ...CredentialCandidate) Inventory {
	return Inventory{Accounts: []LogicalAccount{{
		Key: "logical-1", Identity: identity, Candidates: candidates, Routable: true,
	}}}
}

func providerCandidate(ref CandidateRef, revision Revision, source CredentialSource, expires time.Time) CredentialCandidate {
	return CredentialCandidate{Ref: ref, Revision: revision, Source: source, AccessExpiresAt: expires, Routable: true}
}

func providerUsageResponse(status int) *http.Response {
	body := ""
	if status == http.StatusOK {
		body = happyUsageBody
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestFetchReplansOneSameIdentityCredentialRevisionBeforeDispatch(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	first := providerInventory(identity, providerCandidate(ref, "revision-1", SourceExternal, time.Now().Add(time.Hour)))
	second := providerInventory(identity, providerCandidate(ref, "revision-2", SourceExternal, time.Now().Add(time.Hour)))
	authority := &providerCredentialAuthority{
		inventories: []Inventory{first, second},
		resolveExact: func(call int, planned PlannedCandidate) (CredentialMaterial, error) {
			if call == 1 {
				return CredentialMaterial{}, ErrStaleRevision
			}
			return testCredentialMaterial(planned.Identity, "rotated-secret"), nil
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			requestCalls++
			if got := req.Header.Get("Authorization"); got != "Bearer rotated-secret" {
				t.Fatalf("Authorization = %q, want rotated revision", got)
			}
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsUsable() {
		t.Fatalf("results = %+v, want one usable result", results)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 2 || weakCalls != 0 || requestCalls != 1 {
		t.Fatalf("list/weak/upstream calls = %d/%d/%d, want 2/0/1", listCalls, weakCalls, requestCalls)
	}
	wantPlans := []PlannedCandidate{
		{Ref: ref, Revision: "revision-1", Source: SourceExternal, Identity: AccountIdentity{AccountID: identity.AccountID, UserID: identity.UserID}},
		{Ref: ref, Revision: "revision-2", Source: SourceExternal, Identity: AccountIdentity{AccountID: identity.AccountID, UserID: identity.UserID}},
	}
	if !reflect.DeepEqual(plans, wantPlans) {
		t.Fatalf("exact plans = %+v, want %+v", plans, wantPlans)
	}
}

func TestFetchStaleCredentialIdentitySwitchFailsBeforeDispatch(t *testing.T) {
	firstIdentity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	secondIdentity := AccountIdentity{AccountID: "account-2", UserID: "user-2", Email: "two@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "system-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{
			providerInventory(firstIdentity, providerCandidate(ref, "revision-1", SourceSystem, time.Time{})),
			providerInventory(secondIdentity, providerCandidate(ref, "revision-2", SourceSystem, time.Time{})),
		},
		resolveExact: func(int, PlannedCandidate) (CredentialMaterial, error) {
			return CredentialMaterial{}, ErrStaleRevision
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("results = %+v, want one auth_expired result", results)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 2 || weakCalls != 0 || len(plans) != 1 || requestCalls != 0 {
		t.Fatalf("list/weak/exact/upstream calls = %d/%d/%d/%d, want 2/0/1/0", listCalls, weakCalls, len(plans), requestCalls)
	}
}

func TestFetchSkipsUnroutableCandidateWithinRoutableLogicalAccount(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	unroutableRef := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	routableRef := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	unroutable := providerCandidate(unroutableRef, "external-revision", SourceExternal, time.Now().Add(time.Hour))
	unroutable.Routable = false
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(identity,
			unroutable,
			providerCandidate(routableRef, "managed-revision", SourceManaged, time.Time{}),
		)},
		resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
			if planned.Ref != routableRef {
				t.Fatalf("resolved unroutable candidate: %+v", planned)
			}
			return testCredentialMaterial(planned.Identity, "managed-secret"), nil
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			requestCalls++
			if got := req.Header.Get("Authorization"); got != "Bearer managed-secret" {
				t.Fatalf("Authorization = %q, want routable sibling", got)
			}
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsUsable() || requestCalls != 1 {
		t.Fatalf("results/upstream calls = %+v/%d, want one routable dispatch", results, requestCalls)
	}
	_, _, plans := authority.snapshot()
	if len(plans) != 1 || plans[0].Ref != routableRef {
		t.Fatalf("exact plans = %+v, want routable sibling only", plans)
	}
}

func TestFetchRejectsUnroutableLogicalAccountBeforeResolution(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	inventory := providerInventory(identity, providerCandidate(ref, "revision-1", SourceExternal, time.Now().Add(time.Hour)))
	inventory.Accounts[0].Routable = false
	authority := &providerCredentialAuthority{
		inventories: []Inventory{inventory},
		resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
			return testCredentialMaterial(planned.Identity, "unexpected-secret"), nil
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "no_token" {
		t.Fatalf("results = %+v, want one no_token result", results)
	}
	_, _, plans := authority.snapshot()
	if len(plans) != 0 || requestCalls != 0 {
		t.Fatalf("exact/upstream calls = %d/%d, want 0/0", len(plans), requestCalls)
	}
}

func TestFetchResolvedCredentialIdentityMismatchFailsBeforeDispatch(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(identity,
			providerCandidate(ref, "revision-1", SourceExternal, time.Now().Add(time.Hour)),
		)},
		resolveExact: func(int, PlannedCandidate) (CredentialMaterial, error) {
			return CredentialMaterial{AccessToken: "wrong-account-secret", AccountID: "account-2"}, nil
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("results = %+v, want one auth_expired result", results)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 1 || weakCalls != 0 || len(plans) != 1 || requestCalls != 0 {
		t.Fatalf("list/weak/exact/upstream calls = %d/%d/%d/%d, want 1/0/1/0", listCalls, weakCalls, len(plans), requestCalls)
	}
}

func TestFetchResolvedCredentialUserIdentityMismatchFailsBeforeDispatch(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(identity,
			providerCandidate(ref, "revision-1", SourceExternal, time.Now().Add(time.Hour)),
		)},
		resolveExact: func(int, PlannedCandidate) (CredentialMaterial, error) {
			return CredentialMaterial{
				AccessToken: "wrong-user-secret", AccountID: identity.AccountID,
				IDToken: fakeCodexJWT("other@example.test", identity.AccountID, "user-2", "plus"),
			}, nil
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("results = %+v, want one auth_expired result", results)
	}
	if requestCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", requestCalls)
	}
}

func TestFetchRepeatedStaleCredentialStopsAfterOneReplan(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{
			providerInventory(identity, providerCandidate(ref, "revision-1", SourceExternal, time.Time{})),
			providerInventory(identity, providerCandidate(ref, "revision-2", SourceExternal, time.Time{})),
		},
		resolveExact: func(int, PlannedCandidate) (CredentialMaterial, error) {
			return CredentialMaterial{}, ErrStaleRevision
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("results = %+v, want one auth_expired result", results)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 2 || weakCalls != 0 || len(plans) != 2 || requestCalls != 0 {
		t.Fatalf("list/weak/exact/upstream calls = %d/%d/%d/%d, want 2/0/2/0", listCalls, weakCalls, len(plans), requestCalls)
	}
}

func TestFetchEmptyReplannedCredentialAdvancesWithinFrozenOrder(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	externalRef := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	managedRef := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	first := providerInventory(identity,
		providerCandidate(externalRef, "external-revision-1", SourceExternal, time.Now().Add(time.Hour)),
		providerCandidate(managedRef, "managed-revision-1", SourceManaged, time.Time{}),
	)
	second := providerInventory(identity,
		providerCandidate(externalRef, "external-revision-2", SourceExternal, time.Now().Add(time.Hour)),
		providerCandidate(managedRef, "managed-revision-1", SourceManaged, time.Time{}),
	)
	authority := &providerCredentialAuthority{
		inventories: []Inventory{first, second},
		resolveExact: func(call int, planned PlannedCandidate) (CredentialMaterial, error) {
			switch call {
			case 1:
				return CredentialMaterial{}, ErrStaleRevision
			case 2:
				return testCredentialMaterial(planned.Identity, ""), nil
			default:
				return testCredentialMaterial(planned.Identity, "managed-secret"), nil
			}
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			requestCalls++
			if got := req.Header.Get("Authorization"); got != "Bearer managed-secret" {
				t.Fatalf("Authorization = %q, want remaining frozen candidate", got)
			}
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsUsable() {
		t.Fatalf("results = %+v, want remaining same-identity candidate", results)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 2 || weakCalls != 0 || len(plans) != 3 || requestCalls != 1 {
		t.Fatalf("list/weak/exact/upstream calls = %d/%d/%d/%d, want 2/0/3/1", listCalls, weakCalls, len(plans), requestCalls)
	}
}

func TestFetchExternal401AdvancesToManagedCandidateBeforeRefresh(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	externalRef := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	managedRef := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(identity,
			providerCandidate(externalRef, "external-revision", SourceExternal, time.Now().Add(time.Hour)),
			providerCandidate(managedRef, "managed-revision", SourceManaged, time.Time{}),
		)},
		resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
			token := "managed-secret"
			if planned.Source == SourceExternal {
				token = "external-secret"
			}
			return testCredentialMaterial(planned.Identity, token), nil
		},
	}
	broker := &providerRefreshBroker{}
	var requestTokens []string
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			requestTokens = append(requestTokens, token)
			if token == "external-secret" {
				return providerUsageResponse(http.StatusUnauthorized), nil
			}
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority, refreshBroker: broker,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsUsable() {
		t.Fatalf("results = %+v, want one usable result", results)
	}
	if !reflect.DeepEqual(requestTokens, []string{"external-secret", "managed-secret"}) {
		t.Fatalf("request tokens = %v, want external then managed", requestTokens)
	}
	_, weakCalls, plans := authority.snapshot()
	if weakCalls != 0 || len(plans) != 2 {
		t.Fatalf("weak/exact calls = %d/%d, want 0/2", weakCalls, len(plans))
	}
	refreshCalls, _, _ := broker.snapshot()
	if refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0 before same-identity candidates finish", refreshCalls)
	}
}

func TestFetchManagedRefreshUsesReplannedRevision(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{
			providerInventory(identity, providerCandidate(ref, "revision-1", SourceManaged, time.Time{})),
			providerInventory(identity, providerCandidate(ref, "revision-2", SourceManaged, time.Time{})),
		},
		resolveExact: func(call int, planned PlannedCandidate) (CredentialMaterial, error) {
			if call == 1 {
				return CredentialMaterial{}, ErrStaleRevision
			}
			return testCredentialMaterial(planned.Identity, "expired-secret"), nil
		},
		resolveWeak: func(CandidateRef) (CredentialMaterial, error) {
			return testCredentialMaterial(identity, "expired-secret"), nil
		},
	}
	broker := &providerRefreshBroker{refresh: func(refreshedRef CandidateRef, revision Revision) (RefreshResult, error) {
		if refreshedRef != ref || revision != "revision-2" {
			return RefreshResult{}, ErrStaleRevision
		}
		return RefreshResult{
			Ref: ref, Revision: "revision-3",
			Material: testCredentialMaterial(identity, "refreshed-secret"),
		}, nil
	}}
	var requestTokens []string
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			requestTokens = append(requestTokens, token)
			if token == "refreshed-secret" {
				return providerUsageResponse(http.StatusOK), nil
			}
			return providerUsageResponse(http.StatusUnauthorized), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority, refreshBroker: broker,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsUsable() {
		t.Fatalf("results = %+v, want one usable result", results)
	}
	if !reflect.DeepEqual(requestTokens, []string{"expired-secret", "refreshed-secret"}) {
		t.Fatalf("request tokens = %v, want expired then refreshed", requestTokens)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 2 || weakCalls != 0 || len(plans) != 2 {
		t.Fatalf("list/weak/exact calls = %d/%d/%d, want 2/0/2", listCalls, weakCalls, len(plans))
	}
	refreshCalls, refs, revisions := broker.snapshot()
	if refreshCalls != 1 || !reflect.DeepEqual(refs, []CandidateRef{ref}) || !reflect.DeepEqual(revisions, []Revision{"revision-2"}) {
		t.Fatalf("refresh = %d refs=%v revisions=%v, want exact replanned revision", refreshCalls, refs, revisions)
	}
}

func TestFetchManagedRefreshStrongIdentityMismatchFailsBeforeDispatch(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(identity,
			providerCandidate(ref, "revision-1", SourceManaged, time.Time{}),
		)},
		resolveExact: func(int, PlannedCandidate) (CredentialMaterial, error) {
			return testCredentialMaterial(identity, "expired-secret"), nil
		},
	}
	broker := &providerRefreshBroker{refresh: func(CandidateRef, Revision) (RefreshResult, error) {
		return RefreshResult{
			Ref: ref, Revision: "revision-2",
			Material: CredentialMaterial{
				AccessToken: "wrong-user-secret", AccountID: identity.AccountID,
				IDToken: fakeCodexJWT("other@example.test", identity.AccountID, "user-2", "plus"),
			},
		}, nil
	}}
	var requestTokens []string
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			requestTokens = append(requestTokens, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
			if len(requestTokens) == 1 {
				return providerUsageResponse(http.StatusUnauthorized), nil
			}
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority, refreshBroker: broker,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("results = %+v, want original auth_expired result", results)
	}
	if !reflect.DeepEqual(requestTokens, []string{"expired-secret"}) {
		t.Fatalf("request tokens = %v, want no dispatch for mismatched refreshed identity", requestTokens)
	}
}

func TestFetchManagedRefreshCancellationStopsImmediately(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(identity,
			providerCandidate(ref, "revision-1", SourceManaged, time.Time{}),
		)},
		resolveExact: func(int, PlannedCandidate) (CredentialMaterial, error) {
			return testCredentialMaterial(identity, "expired-secret"), nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	broker := &providerRefreshBroker{refresh: func(CandidateRef, Revision) (RefreshResult, error) {
		cancel()
		return RefreshResult{}, context.Canceled
	}}
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			return providerUsageResponse(http.StatusUnauthorized), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority, refreshBroker: broker,
	}

	results, err := p.Fetch(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "transport_error" {
		t.Fatalf("results = %+v, want cancellation transport_error", results)
	}
	refreshCalls, _, _ := broker.snapshot()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
}
