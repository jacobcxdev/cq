package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
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

type providerExactResolverRecorder struct {
	resolver ExactSecretResolver
	mu       sync.Mutex
	plans    []PlannedCandidate
}

func (r *providerExactResolverRecorder) ResolveExact(ctx context.Context, planned PlannedCandidate) (CredentialMaterial, error) {
	r.mu.Lock()
	r.plans = append(r.plans, planned)
	r.mu.Unlock()
	return r.resolver.ResolveExact(ctx, planned)
}

func (r *providerExactResolverRecorder) snapshot() []PlannedCandidate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PlannedCandidate(nil), r.plans...)
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
					providerRefreshEligibleCandidate(CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}, "revision-1", time.Time{}),
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

func TestFetchPartialAuthorityWiringDoesNotUseReadableFilesystemCredentials(t *testing.T) {
	identity := AccountIdentity{AccountID: "acct-1", UserID: "user-1", Email: "one@example.test"}
	fs := newFakeFS()
	fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON(
		"readable-filesystem-secret", identity.AccountID,
		fakeCodexJWT(identity.Email, identity.AccountID, identity.UserID, "plus"),
	)
	tests := []struct {
		name    string
		secrets ExactSecretResolver
		broker  CredentialRefreshBroker
	}{
		{name: "resolver only", secrets: staticSecretResolver{material: testCredentialMaterial(identity, "resolver-secret")}},
		{name: "refresh broker only", broker: &providerRefreshBroker{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestCalls := 0
			p := &Provider{
				client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
					requestCalls++
					return providerUsageResponse(http.StatusOK), nil
				}),
				fs: fs, secrets: test.secrets, refreshBroker: test.broker,
			}
			results, err := p.Fetch(context.Background(), time.Now())
			if err != nil || len(results) != 1 || results[0].Error == nil ||
				results[0].Error.Code != "fetch_error" || results[0].Error.Message != credentialAuthorityUnavailableMessage {
				t.Fatalf("Fetch results/error = %+v/%v, want typed authority unavailable", results, err)
			}
			if requestCalls != 0 {
				t.Fatalf("upstream calls = %d, want none", requestCalls)
			}
		})
	}
}

func TestFetchDegradedExternalInventoryDoesNotAuthoriseTerminalCredentialState(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	candidate := providerCandidate(
		CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"},
		"managed-revision", SourceManaged, time.Now().Add(time.Hour),
	)
	tests := []struct {
		name       string
		accounts   []LogicalAccount
		status     int
		wantUsable bool
		wantCalls  int
	}{
		{name: "no visible candidates"},
		{name: "visible candidate rejected", accounts: providerInventory(identity, candidate).Accounts, status: http.StatusUnauthorized, wantCalls: 1},
		{name: "visible candidate succeeds", accounts: providerInventory(identity, candidate).Accounts, status: http.StatusOK, wantCalls: 1, wantUsable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority := &providerCredentialAuthority{
				inventories: []Inventory{{
					Accounts:        test.accounts,
					ExternalSources: []ExternalSourceStatus{{Name: "codexbar", ErrorCode: "invalid"}},
				}},
				resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
					return testCredentialMaterial(planned.Identity, "visible-managed-secret"), nil
				},
			}
			requestCalls := 0
			p := &Provider{
				client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
					requestCalls++
					return providerUsageResponse(test.status), nil
				}),
				fs: newFakeFS(), inventory: authority, secrets: authority,
			}
			results, err := p.Fetch(context.Background(), time.Now())
			if err != nil || len(results) != 1 {
				t.Fatalf("Fetch results/error = %+v/%v", results, err)
			}
			if test.wantUsable {
				if !results[0].IsUsable() {
					t.Fatalf("results = %+v, want visible candidate success", results)
				}
			} else if results[0].Error == nil || results[0].Error.Code != "fetch_error" ||
				results[0].Error.Message != "Codex credential inventory degraded" {
				t.Fatalf("results = %+v, want typed degraded inventory", results)
			}
			if requestCalls != test.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", requestCalls, test.wantCalls)
			}
		})
	}
}

func TestFetchAbsentOptionalExternalSourcePreservesVisibleTerminalState(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	candidate := providerCandidate(
		CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"},
		"managed-revision", SourceManaged, time.Now().Add(time.Hour),
	)
	tests := []struct {
		name      string
		accounts  []LogicalAccount
		status    int
		wantCode  string
		wantCalls int
	}{
		{name: "no visible candidates", wantCode: "not_configured"},
		{name: "visible candidate rejected", accounts: providerInventory(identity, candidate).Accounts, status: http.StatusUnauthorized, wantCode: "auth_expired", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority := &providerCredentialAuthority{
				inventories: []Inventory{{
					Accounts: test.accounts,
					ExternalSources: []ExternalSourceStatus{{
						Name: "codexbar", ErrorCode: "unavailable", OptionalAbsent: true,
					}},
				}},
				resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
					return testCredentialMaterial(planned.Identity, "visible-managed-secret"), nil
				},
			}
			requestCalls := 0
			p := &Provider{
				client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
					requestCalls++
					return providerUsageResponse(test.status), nil
				}),
				fs: newFakeFS(), inventory: authority, secrets: authority,
			}
			results, err := p.Fetch(context.Background(), time.Now())
			if err != nil || len(results) != 1 || results[0].Error == nil || results[0].Error.Code != test.wantCode {
				t.Fatalf("Fetch results/error = %+v/%v, want %s", results, err, test.wantCode)
			}
			if requestCalls != test.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", requestCalls, test.wantCalls)
			}
		})
	}
}

func TestFetchExternalSourceDisappearsDuringExactResolutionReturnsDegraded(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	source := &fakeExternalCredentialSource{
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
			Identity: identity, AccessExpiresAt: time.Now().Add(time.Hour), Routable: true,
		}},
		listErr: ErrExternalUnavailable, listErrAfter: 1,
	}
	coordinator, fs := testCoordinator(t)
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: fs, inventory: coordinator, secrets: coordinator,
	}

	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil || len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "fetch_error" || results[0].Error.Message != credentialInventoryDegradedMessage {
		t.Fatalf("Fetch results/error = %+v/%v, want typed degraded inventory", results, err)
	}
	if requestCalls != 0 {
		t.Fatalf("upstream calls = %d, want none after source loss", requestCalls)
	}
}

func TestFetchRetainsHealthyExternalIdentityAcrossSourceAndCoreFailures(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	source := &fakeExternalCredentialSource{
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
			Identity: identity, AccessExpiresAt: time.Now().Add(time.Hour), Routable: true,
		}},
		material: testCredentialMaterial(identity, "external-secret"),
		listErr:  ErrExternalUnavailable, listErrAfter: 2,
	}
	coordinator, fs := testCoordinator(t)
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: fs, inventory: coordinator, secrets: coordinator,
	}

	first, err := p.Fetch(context.Background(), time.Now())
	if err != nil || len(first) != 1 || !first[0].IsUsable() || first[0].AccountID != identity.AccountID {
		t.Fatalf("first Fetch results/error = %+v/%v, want healthy external identity", first, err)
	}
	second, err := p.Fetch(context.Background(), time.Now())
	if err != nil || len(second) != 1 || second[0].Error == nil || second[0].Error.Message != credentialInventoryDegradedMessage || second[0].AccountID != identity.AccountID || second[0].Email != identity.Email {
		t.Fatalf("second Fetch results/error = %+v/%v, want retained identity degraded", second, err)
	}
	fs.homeDirErr = errors.New("private core authority failure")
	third, err := p.Fetch(context.Background(), time.Now())
	if err != nil || len(third) != 1 || third[0].Error == nil || third[0].Error.Message != credentialInventoryStaleMessage || third[0].AccountID != identity.AccountID || third[0].Email != identity.Email {
		t.Fatalf("third Fetch results/error = %+v/%v, want original healthy identity stale", third, err)
	}
	if requestCalls != 1 {
		t.Fatalf("upstream calls = %d, want only healthy dispatch", requestCalls)
	}
}

func TestFetchPartialDegradationRetainsSameAccountDifferentUserIdentity(t *testing.T) {
	firstIdentity := AccountIdentity{AccountID: "shared-account", UserID: "user-1", Email: "one@example.test"}
	secondIdentity := AccountIdentity{AccountID: "shared-account", UserID: "user-2", Email: "two@example.test"}
	firstInventory := providerInventory(firstIdentity, providerCandidate(
		CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}, "revision-1", SourceManaged, time.Now().Add(time.Hour),
	))
	secondAccount := providerInventory(secondIdentity, providerCandidate(
		CandidateRef{AccountKey: "logical-2", CandidateID: "managed-2"}, "revision-2", SourceManaged, time.Now().Add(time.Hour),
	)).Accounts[0]
	secondAccount.Key = "logical-2"
	firstInventory.Accounts = append(firstInventory.Accounts, secondAccount)
	partial := Inventory{
		Accounts:        firstInventory.Accounts[:1],
		ExternalSources: []ExternalSourceStatus{{Name: "codexbar", ErrorCode: "unavailable"}},
	}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{firstInventory, partial},
		resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
			return testCredentialMaterial(planned.Identity, "visible-secret"), nil
		},
	}
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}
	if first, err := p.Fetch(context.Background(), time.Now()); err != nil || len(first) != 2 || !first[0].IsUsable() || !first[1].IsUsable() {
		t.Fatalf("first Fetch results/error = %+v/%v, want two healthy identities", first, err)
	}
	second, err := p.Fetch(context.Background(), time.Now())
	if err != nil || len(second) != 2 {
		t.Fatalf("second Fetch results/error = %+v/%v, want visible plus missing identity", second, err)
	}
	if !second[0].IsUsable() || second[0].Email != firstIdentity.Email {
		t.Fatalf("visible result = %+v, want first user success", second[0])
	}
	if second[1].Error == nil || second[1].Error.Message != credentialInventoryDegradedMessage || second[1].Email != secondIdentity.Email {
		t.Fatalf("missing result = %+v, want second user degraded", second[1])
	}
}

func TestFetchRetainsLastLogicalInventoryAsStaleOnRealCoordinatorFailure(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.AccessToken = "managed-readable-secret"
	credential.Tokens.IDToken = fakeCodexJWT("one@example.test", "acct-1", "user-1", "plus")
	credential.Claims = auth.DecodeCodexClaims(credential.Tokens.IDToken)
	ref, _, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("/fake/home/.codex/accounts", string(ref.CandidateID)+".auth.json")
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	before, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: fs, inventory: coordinator, secrets: coordinator,
	}
	first, err := p.Fetch(context.Background(), time.Unix(1_700_000_000, 0))
	if err != nil || len(first) != 1 || !first[0].IsUsable() {
		t.Fatalf("initial Fetch results/error = %+v/%v, want one usable result", first, err)
	}

	const sensitive = "private managed directory and token-secret"
	fs.readDirErr = map[string]error{"/fake/home/.codex/accounts": errors.New(sensitive)}
	second, err := p.Fetch(context.Background(), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("degraded Fetch error = %v, want typed result", err)
	}
	if len(second) != 1 || second[0].Error == nil || second[0].Error.Code != "fetch_error" ||
		second[0].Error.Message != "Codex credential inventory stale" || second[0].AccountID != "acct-1" || second[0].Email != "one@example.test" {
		t.Fatalf("degraded results = %+v, want last logical account explicitly stale", second)
	}
	if strings.Contains(fmt.Sprintf("%+v", second), sensitive) {
		t.Fatalf("degraded results exposed coordinator detail: %+v", second)
	}
	if requestCalls != 1 {
		t.Fatalf("upstream calls = %d, want only initial dispatch", requestCalls)
	}
	after, err := fs.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("managed credential bytes changed after authority failure: err=%v", err)
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

func providerRefreshEligibleCandidate(ref CandidateRef, revision Revision, expires time.Time) CredentialCandidate {
	candidate := providerCandidate(ref, revision, SourceManaged, expires)
	candidate.CQAuthored = true
	candidate.RefreshEligible = true
	return candidate
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

func TestFetchKnownExpiredExternalCredentialDoesNotDispatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(
			identity,
			providerCandidate(ref, "revision-1", SourceExternal, now.Add(-time.Minute)),
		)},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), now)
	if err != nil || len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "auth_expired" {
		t.Fatalf("Fetch results/error = %+v/%v, want one auth_expired result", results, err)
	}
	if got := results[0].Error.Message; got != "access expired — credential owner must refresh" {
		t.Fatalf("error message = %q, want credential-owner refresh guidance", got)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 1 || weakCalls != 0 || len(plans) != 0 || requestCalls != 0 {
		t.Fatalf("list/weak/plans/upstream calls = %d/%d/%d/%d, want 1/0/0/0", listCalls, weakCalls, len(plans), requestCalls)
	}
}

func TestFetchProviderAuthEvidenceOutranksLaterLocalExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorised", status: http.StatusUnauthorized},
		{
			name:   "forbidden authentication error",
			status: http.StatusForbidden,
			body:   `{"error":{"type":"authentication_error"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			readyRef := CandidateRef{AccountKey: "logical-1", CandidateID: "external-ready"}
			expiredRef := CandidateRef{AccountKey: "logical-1", CandidateID: "external-expired"}
			authority := &providerCredentialAuthority{
				inventories: []Inventory{providerInventory(
					identity,
					providerCandidate(readyRef, "ready-revision", SourceExternal, now.Add(time.Hour)),
					providerCandidate(expiredRef, "expired-revision", SourceExternal, now.Add(-time.Minute)),
				)},
				resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
					return testCredentialMaterial(planned.Identity, "rejected-secret"), nil
				},
			}
			requestCalls := 0
			p := &Provider{
				client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
					requestCalls++
					return &http.Response{
						StatusCode: test.status,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(test.body)),
					}, nil
				}),
				fs: newFakeFS(), inventory: authority, secrets: authority,
			}

			results, err := p.Fetch(context.Background(), now)
			if err != nil || len(results) != 1 || results[0].Error == nil {
				t.Fatalf("Fetch results/error = %+v/%v, want one provider rejection", results, err)
			}
			if got := results[0].Error.Code; got != "auth_expired" {
				t.Fatalf("error code = %q, want auth_expired", got)
			}
			if got := results[0].Error.HTTPStatus; got != test.status {
				t.Fatalf("HTTP status = %d, want %d", got, test.status)
			}
			if got := results[0].Error.Message; got != credentialRejectedMessage {
				t.Fatalf("error message = %q, want provider rejection evidence", got)
			}
			listCalls, weakCalls, plans := authority.snapshot()
			if listCalls != 1 || weakCalls != 0 || len(plans) != 1 || requestCalls != 1 {
				t.Fatalf("list/weak/plans/upstream calls = %d/%d/%d/%d, want 1/0/1/1", listCalls, weakCalls, len(plans), requestCalls)
			}
		})
	}
}

func TestFetchProviderAuthEvidenceOutranksLaterManagedRefreshRequirement(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	readyRef := CandidateRef{AccountKey: "logical-1", CandidateID: "external-ready"}
	expiredRef := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-expired"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(
			identity,
			providerCandidate(readyRef, "ready-revision", SourceExternal, now.Add(time.Hour)),
			providerRefreshEligibleCandidate(expiredRef, "expired-revision", now.Add(-time.Minute)),
		)},
		resolveExact: func(_ int, planned PlannedCandidate) (CredentialMaterial, error) {
			return testCredentialMaterial(planned.Identity, "rejected-secret"), nil
		},
	}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
			requestCalls++
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority,
	}

	results, err := p.Fetch(context.Background(), now)
	if err != nil || len(results) != 1 || results[0].Error == nil {
		t.Fatalf("Fetch results/error = %+v/%v, want one provider rejection", results, err)
	}
	if got := results[0].Error.Code; got != "auth_expired" {
		t.Fatalf("error code = %q, want auth_expired", got)
	}
	if got := results[0].Error.HTTPStatus; got != http.StatusUnauthorized {
		t.Fatalf("HTTP status = %d, want 401", got)
	}
	if got := results[0].Error.Message; got != credentialRejectedMessage {
		t.Fatalf("error message = %q, want provider rejection evidence", got)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	if listCalls != 1 || weakCalls != 0 || len(plans) != 1 || requestCalls != 1 {
		t.Fatalf("list/weak/plans/upstream calls = %d/%d/%d/%d, want 1/0/1/1", listCalls, weakCalls, len(plans), requestCalls)
	}
}

func TestFetchKnownExpiredManagedCredentialRefreshesBeforeDispatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	candidate := providerCandidate(ref, "revision-1", SourceManaged, now.Add(-time.Minute))
	candidate.CQAuthored = true
	candidate.RefreshEligible = true
	authority := &providerCredentialAuthority{
		inventories: []Inventory{providerInventory(identity, candidate)},
	}
	broker := &providerRefreshBroker{refresh: func(gotRef CandidateRef, gotRevision Revision) (RefreshResult, error) {
		if gotRef != ref || gotRevision != "revision-1" {
			t.Fatalf("refresh target = %+v/%q, want %+v/revision-1", gotRef, gotRevision, ref)
		}
		return RefreshResult{
			Ref: ref, Revision: "revision-2",
			Material: testCredentialMaterial(identity, "refreshed-secret"),
		}, nil
	}}
	requestCalls := 0
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			requestCalls++
			if got := req.Header.Get("Authorization"); got != "Bearer refreshed-secret" {
				t.Fatalf("Authorization = %q, want refreshed credential", got)
			}
			return providerUsageResponse(http.StatusOK), nil
		}),
		fs: newFakeFS(), inventory: authority, secrets: authority, refreshBroker: broker,
	}

	results, err := p.Fetch(context.Background(), now)
	if err != nil || len(results) != 1 || !results[0].IsUsable() {
		t.Fatalf("Fetch results/error = %+v/%v, want refreshed success", results, err)
	}
	listCalls, weakCalls, plans := authority.snapshot()
	refreshCalls, _, _ := broker.snapshot()
	if listCalls != 1 || weakCalls != 0 || len(plans) != 0 || refreshCalls != 1 || requestCalls != 1 {
		t.Fatalf("list/weak/plans/refresh/upstream calls = %d/%d/%d/%d/%d, want 1/0/0/1/1", listCalls, weakCalls, len(plans), refreshCalls, requestCalls)
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
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "fetch_error" {
		t.Fatalf("results = %+v, want one inventory failure result", results)
	}
	if results[0].Error.Message != credentialInventoryDegradedMessage {
		t.Fatalf("error message = %q, want credential-inventory guidance without unproven expiry", results[0].Error.Message)
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
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "fetch_error" || results[0].Error.Message != credentialInventoryDegradedMessage {
		t.Fatalf("results = %+v, want one credential inventory failure", results)
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
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "fetch_error" || results[0].Error.Message != credentialInventoryDegradedMessage {
		t.Fatalf("results = %+v, want one credential inventory failure", results)
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
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "fetch_error" {
		t.Fatalf("results = %+v, want one credential inventory failure", results)
	}
	if results[0].Error.Message != credentialInventoryDegradedMessage {
		t.Fatalf("error message = %q, want credential-inventory guidance without unproven expiry", results[0].Error.Message)
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

func TestFetchStaleManaged401UsesExactFreshCodexBarRevisionWithoutWrites(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	credential := testLoginCredential()
	credential.Tokens.AccessToken = "synthetic-stale-managed-token"
	credential.Tokens.IDToken = fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus")
	credential.Claims = auth.DecodeCodexClaims(credential.Tokens.IDToken)
	ref, revision, err := coordinator.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	record, err := coordinator.loadRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	record.Document["cq_expires_at"] = time.Now().Add(2 * time.Hour).UnixMilli()
	if err := coordinator.Store.Commit(&record, revision); err != nil {
		t.Fatal(err)
	}

	codexBarRoot := filepath.Join(t.TempDir(), "CodexBar")
	writeCodexBarFixture(t, codexBarRoot, 0o600, nil)
	coordinator.ExternalSources = []ExternalCredentialSource{NewCodexBarSource(codexBarRoot)}
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 2 {
		t.Fatalf("inventory = %+v, want one identity with managed and CodexBar candidates", inventory)
	}
	expected := make(map[CredentialSource]PlannedCandidate)
	for _, candidate := range inventory.Accounts[0].Candidates {
		expected[candidate.Source] = PlanCandidate(inventory.Accounts[0], candidate)
	}
	if expected[SourceManaged].Revision == "" || expected[SourceExternal].Revision == "" {
		t.Fatalf("candidate plans = %+v, want exact managed and external revisions", expected)
	}

	managedPath := record.Path
	externalPath := codexBarAuthPath(codexBarRoot)
	manifestPath := filepath.Join(codexBarRoot, "managed-codex-accounts.json")
	managedBefore, err := fs.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	externalBefore, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	resolver := &providerExactResolverRecorder{resolver: coordinator}
	broker := &providerRefreshBroker{}
	var requestTokens []string
	p := &Provider{
		client: providerDoerFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			requestTokens = append(requestTokens, token)
			if token == credential.Tokens.AccessToken {
				return providerUsageResponse(http.StatusUnauthorized), nil
			}
			if token == "synthetic-access" {
				return providerUsageResponse(http.StatusOK), nil
			}
			return providerUsageResponse(http.StatusForbidden), nil
		}),
		fs: fs, inventory: coordinator, secrets: resolver, refreshBroker: broker,
	}
	results, err := p.Fetch(context.Background(), time.Now())
	if err != nil || len(results) != 1 || !results[0].IsUsable() {
		t.Fatalf("Fetch results/error = %+v/%v, want fresh external success", results, err)
	}
	if !reflect.DeepEqual(requestTokens, []string{credential.Tokens.AccessToken, "synthetic-access"}) {
		t.Fatalf("request tokens = %q, want stale managed then fresh CodexBar", requestTokens)
	}
	plans := resolver.snapshot()
	if len(plans) != 2 || plans[0] != expected[SourceManaged] || plans[1] != expected[SourceExternal] {
		t.Fatalf("exact plans = %+v, want exact managed then external revisions %+v", plans, expected)
	}
	refreshCalls, _, _ := broker.snapshot()
	if refreshCalls != 0 {
		t.Fatalf("managed refresh calls = %d, want none before external candidate succeeds", refreshCalls)
	}

	managedAfter, managedErr := fs.ReadFile(managedPath)
	externalAfter, externalErr := os.ReadFile(externalPath)
	manifestAfter, manifestErr := os.ReadFile(manifestPath)
	if managedErr != nil || externalErr != nil || manifestErr != nil ||
		!bytes.Equal(managedAfter, managedBefore) || !bytes.Equal(externalAfter, externalBefore) || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatalf("credential stores changed: managed=%v external=%v manifest=%v", managedErr, externalErr, manifestErr)
	}
}

func TestFetchManagedRefreshUsesReplannedRevision(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1", Email: "one@example.test"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "managed-1"}
	authority := &providerCredentialAuthority{
		inventories: []Inventory{
			providerInventory(identity, providerRefreshEligibleCandidate(ref, "revision-1", time.Time{})),
			providerInventory(identity, providerRefreshEligibleCandidate(ref, "revision-2", time.Time{})),
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
			providerRefreshEligibleCandidate(ref, "revision-1", time.Time{}),
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
			providerRefreshEligibleCandidate(ref, "revision-1", time.Time{}),
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
