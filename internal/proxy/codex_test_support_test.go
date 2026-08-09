package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type testSecretResolver struct {
	mu        sync.Mutex
	materials map[codex.CandidateID]codex.CredentialMaterial
	calls     int
}

type testExactSecretResolver struct {
	mu        sync.Mutex
	materials map[codex.Revision]codex.CredentialMaterial
	errors    map[codex.Revision]error
	plans     []codex.PlannedCandidate
}

func (r *testExactSecretResolver) ResolveExact(_ context.Context, planned codex.PlannedCandidate) (codex.CredentialMaterial, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans = append(r.plans, planned)
	if err := r.errors[planned.Revision]; err != nil {
		return codex.CredentialMaterial{}, err
	}
	material, ok := r.materials[planned.Revision]
	if !ok {
		return codex.CredentialMaterial{}, fmt.Errorf("credential revision unavailable")
	}
	return material, nil
}

func testExactCredentialMaterial(identity codex.AccountIdentity, accessToken string) codex.CredentialMaterial {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": identity.AccountID,
			"chatgpt_user_id":    identity.UserID,
		},
	})
	return codex.CredentialMaterial{
		AccessToken: accessToken,
		AccountID:   identity.AccountID,
		IDToken:     "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature",
	}
}

func testRejectedReplanInventories(ref codex.CandidateRef, identity codex.AccountIdentity) map[string]codex.Inventory {
	newCandidate := func(source codex.CredentialSource, routable bool) codex.CredentialCandidate {
		return codex.CredentialCandidate{Ref: ref, Revision: "revision-new", Source: source, Routable: routable}
	}
	return map[string]codex.Inventory{
		"missing candidate": {},
		"account switch": {Accounts: []codex.LogicalAccount{{
			Key: "other", Identity: identity, Routable: true,
			Candidates: []codex.CredentialCandidate{{
				Ref:      codex.CandidateRef{AccountKey: "other", CandidateID: ref.CandidateID},
				Revision: "revision-new", Source: codex.SourceSystem, Routable: true,
			}},
		}}},
		"identity switch": {Accounts: []codex.LogicalAccount{{
			Key: ref.AccountKey, Identity: codex.AccountIdentity{AccountID: identity.AccountID, UserID: "other-user"}, Routable: true,
			Candidates: []codex.CredentialCandidate{newCandidate(codex.SourceSystem, true)},
		}}},
		"source switch": {Accounts: []codex.LogicalAccount{{
			Key: ref.AccountKey, Identity: identity, Routable: true,
			Candidates: []codex.CredentialCandidate{newCandidate(codex.SourceManaged, true)},
		}}},
		"unroutable candidate": {Accounts: []codex.LogicalAccount{{
			Key: ref.AccountKey, Identity: identity, Routable: true,
			Candidates: []codex.CredentialCandidate{newCandidate(codex.SourceSystem, false)},
		}}},
		"unroutable account": {Accounts: []codex.LogicalAccount{{
			Key: ref.AccountKey, Identity: identity, Routable: false,
			Candidates: []codex.CredentialCandidate{newCandidate(codex.SourceSystem, true)},
		}}},
	}
}

func (r *testSecretResolver) Resolve(_ context.Context, ref codex.CandidateRef) (codex.CredentialMaterial, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	material, ok := r.materials[ref.CandidateID]
	if !ok {
		return codex.CredentialMaterial{}, fmt.Errorf("credential unavailable")
	}
	return material, nil
}

func (r *testSecretResolver) ResolveExact(_ context.Context, planned codex.PlannedCandidate) (codex.CredentialMaterial, error) {
	material, err := r.Resolve(context.Background(), planned.Ref)
	if err != nil || material.IDToken != "" {
		return material, err
	}
	resolved := testExactCredentialMaterial(planned.Identity, material.AccessToken)
	resolved.RefreshToken = material.RefreshToken
	return resolved, nil
}

type legacySelectorScope struct {
	selector  CodexSelector
	resolver  *testSecretResolver
	effective string
}

type legacyCodexTokenTransport struct {
	Selector CodexSelector
	Quota    codexQuotaReader
	Inner    http.RoundTripper
}

func (t *legacyCodexTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.Selector == nil {
		return nil, fmt.Errorf("legacy Codex test transport unavailable")
	}
	response, _, _, err := testCodexRequestRouter(t.Selector, t.Inner).Do(req.Context(), CodexRouteRequirements{
		RequestedModel: extractModelFromRequest(req),
	}, req)
	return response, err
}

func (t *legacyCodexTokenTransport) codexWebSocketRouting() (*CodexRequestRouter, ExplicitWebSocketExecutor) {
	router := testCodexRequestRouter(t.Selector, t.Inner)
	return router, testCodexWebSocketExecutor(router)
}

func extractModelFromRequest(req *http.Request) string {
	if req == nil || req.GetBody == nil {
		return ""
	}
	body, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	return extractModel(data)
}

func (s *legacySelectorScope) Plan(ctx context.Context, requirements CodexRouteRequirements, _ codex.Revision, exclude ...codex.SelectionExclusion) (CodexRequestPlan, error) {
	account, err := s.selector.Select(ctx, exclude...)
	if err != nil {
		return CodexRequestPlan{}, err
	}
	key := codexRoutingAccountKey(account)
	if key == "" {
		key = "test-account"
	}
	candidateID := account.CandidateID
	if candidateID == "" {
		candidateID = codex.CandidateID("test:" + key)
	}
	ref := codex.CandidateRef{AccountKey: key, CandidateID: candidateID}
	identity := codex.AccountIdentity{AccountID: account.AccountID, UserID: account.UserID}
	if identity.AccountID == "" {
		identity.AccountID = "test-account:" + string(key)
	}
	if identity.UserID == "" {
		identity.UserID = "test-user:" + string(key)
	}
	revision := account.Revision
	if revision == "" {
		revision = codex.Revision("test-revision:" + string(candidateID))
	}
	material := testExactCredentialMaterial(identity, account.AccessToken)
	material.RefreshToken = account.RefreshToken
	s.resolver.mu.Lock()
	s.resolver.materials[candidateID] = material
	s.resolver.mu.Unlock()
	effective := requirements.RequestedModel
	if s.effective != "" {
		effective = s.effective
	}
	choice := RouteChoice{AccountKey: key, RequestedModel: requirements.RequestedModel, EffectiveModel: effective}
	attempt := CandidateAttempt{
		AccountKey: key, Candidate: ref, Revision: revision,
		Source: codex.SourceSystem, Identity: identity, Ordinal: 1,
	}
	return CodexRequestPlan{Choice: choice, Attempts: []CandidateAttempt{attempt}}, nil
}

func testCodexRequestRouter(selector CodexSelector, inner http.RoundTripper) *CodexRequestRouter {
	if inner == nil {
		inner = http.DefaultTransport
	}
	resolver := &testSecretResolver{materials: make(map[codex.CandidateID]codex.CredentialMaterial)}
	return &CodexRequestRouter{
		Scope:    &legacySelectorScope{selector: selector, resolver: resolver},
		Executor: &CodexAttemptExecutor{Secrets: resolver, Transport: &CodexTokenTransport{Inner: inner}},
	}
}

func testCodexWebSocketExecutor(router *CodexRequestRouter) ExplicitWebSocketExecutor {
	scope, ok := router.Scope.(*legacySelectorScope)
	if !ok {
		panic("test Codex router has unexpected scope")
	}
	return NewCodexWebSocketAttemptExecutor(nil, scope.resolver)
}

func setTestCodexInner(router *CodexRequestRouter, inner http.RoundTripper) {
	executor, ok := router.Executor.(*CodexAttemptExecutor)
	if !ok {
		panic("test Codex router has unexpected executor")
	}
	executor.Transport.Inner = inner
}
