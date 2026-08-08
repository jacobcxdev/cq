package proxy

import (
	"context"
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
	s.resolver.mu.Lock()
	s.resolver.materials[candidateID] = codex.CredentialMaterial{
		AccessToken:  account.AccessToken,
		RefreshToken: account.RefreshToken,
		IDToken:      account.IDToken,
		AccountID:    account.AccountID,
	}
	s.resolver.mu.Unlock()
	effective := requirements.RequestedModel
	if s.effective != "" {
		effective = s.effective
	}
	choice := RouteChoice{AccountKey: key, RequestedModel: requirements.RequestedModel, EffectiveModel: effective}
	attempt := CandidateAttempt{AccountKey: key, Candidate: ref, Revision: account.Revision, Ordinal: 1}
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
	return NewCodexWebSocketAttemptExecutor(scope.resolver)
}

func setTestCodexInner(router *CodexRequestRouter, inner http.RoundTripper) {
	executor, ok := router.Executor.(*CodexAttemptExecutor)
	if !ok {
		panic("test Codex router has unexpected executor")
	}
	executor.Transport.Inner = inner
}
