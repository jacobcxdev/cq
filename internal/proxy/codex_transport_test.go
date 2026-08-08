package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// multiCodexSelector supports exclusion filtering in legacy handler tests.
type multiCodexSelector struct {
	accounts []codex.CodexAccount
}

func (s *multiCodexSelector) Select(_ context.Context, exclude ...codex.SelectionExclusion) (*codex.CodexAccount, error) {
	excludedAccounts := make(map[codex.AccountKey]bool, len(exclude))
	excludedCandidates := make(map[codex.CandidateID]bool, len(exclude))
	for _, exclusion := range exclude {
		excludedAccounts[exclusion.AccountKey] = true
		excludedCandidates[exclusion.CandidateID] = true
	}
	for i := range s.accounts {
		a := &s.accounts[i]
		if codexAcctExcluded(a, excludedAccounts, excludedCandidates) || a.AccessToken == "" {
			continue
		}
		result := *a
		return &result, nil
	}
	return nil, fmt.Errorf("no codex accounts available")
}

func makeCodexRequest(body string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	return req
}

func TestCodexTokenTransportInjectsExplicitMaterial(t *testing.T) {
	var gotAuth, gotAccount, gotAPIKey string
	transport := &CodexTokenTransport{Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		gotAccount = req.Header.Get("ChatGPT-Account-ID")
		gotAPIKey = req.Header.Get("x-api-key")
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	req := makeCodexRequest(`{"model":"gpt-5.4"}`)
	req.Header.Set("Authorization", "Bearer caller")
	req.Header.Set("x-api-key", "caller-key")
	choice := RouteChoice{AccountKey: "identity", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"}
	_, err := transport.Do(req, choice, codex.CredentialMaterial{AccessToken: "secret", AccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || gotAccount != "account" || gotAPIKey != "" {
		t.Fatalf("headers auth=%q account=%q api-key=%q", gotAuth, gotAccount, gotAPIKey)
	}
	if req.Header.Get("Authorization") != "Bearer caller" || req.Header.Get("x-api-key") != "caller-key" {
		t.Fatal("caller request was mutated")
	}
}

func TestCodexTokenTransportConsumesRouteModel(t *testing.T) {
	var body string
	transport := &CodexTokenTransport{Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(req.Body)
		body = string(data)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	choice := RouteChoice{AccountKey: "identity", RequestedModel: codexSparkModel, EffectiveModel: codexFallbackModel}
	_, err := transport.Do(makeCodexRequest(`{"model":"gpt-5.3-codex-spark","input":[]}`), choice, codex.CredentialMaterial{AccessToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"model":"gpt-5.3-codex"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestCodexAttemptExecutorRejectsIdentityMismatchBeforeSecretResolution(t *testing.T) {
	resolver := &testSecretResolver{materials: map[codex.CandidateID]codex.CredentialMaterial{"candidate": {AccessToken: "secret"}}}
	executor := &CodexAttemptExecutor{Secrets: resolver, Transport: &CodexTokenTransport{}}
	_, err := executor.Do(context.Background(), RouteChoice{AccountKey: "one"}, CandidateAttempt{
		AccountKey: "two",
		Candidate:  codex.CandidateRef{AccountKey: "two", CandidateID: "candidate"},
	}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("err = %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("secret resolutions = %d", resolver.calls)
	}
}

func TestRewriteCodexModelNamePreservesSuffix(t *testing.T) {
	got, ok := rewriteCodexModelName("gpt-5.3-codex-spark-high")
	if !ok || got != "gpt-5.3-codex-high" {
		t.Fatalf("rewrite = %q, %v", got, ok)
	}
}
