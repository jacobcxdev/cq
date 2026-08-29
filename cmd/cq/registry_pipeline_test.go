package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestFirstClaudeAccessTokenFromAccountsPrefersFresherToken(t *testing.T) {
	accounts := []keyring.ClaudeOAuth{
		{AccessToken: "stale", ExpiresAt: 100},
		{AccessToken: "fresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
	}
	token, err := firstClaudeAccessTokenFromAccounts(accounts)()
	if err != nil {
		t.Fatalf("firstClaudeAccessTokenFromAccounts() error = %v", err)
	}
	if token != "fresh" {
		t.Fatalf("token = %q, want %q", token, "fresh")
	}
}

func TestFirstCodexAccessTokenPrefersFresherToken(t *testing.T) {
	accounts := []codexprov.CodexAccount{
		{AccessToken: "stale", ExpiresAt: 100},
		{AccessToken: "fresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
	}
	token, err := firstCodexAccessToken(accounts)
	if err != nil {
		t.Fatalf("firstCodexAccessToken() error = %v", err)
	}
	if token != "fresh" {
		t.Fatalf("token = %q, want %q", token, "fresh")
	}
}

func TestTokenIsFresh(t *testing.T) {
	now := time.Unix(1000, 0)
	nowMs := now.UnixMilli()

	tests := []struct {
		name      string
		expiresAt int64
		want      bool
	}{
		{"unknown expiry (0) is fresh", 0, true},
		{"strictly after now is fresh", nowMs + 1, true},
		{"exactly now is stale", nowMs, false},
		{"before now is stale", nowMs - 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenIsFresh(tt.expiresAt, now); got != tt.want {
				t.Errorf("tokenIsFresh(%d) = %v, want %v", tt.expiresAt, got, tt.want)
			}
		})
	}
}

// TestRegistryPipelinePublishConcurrency verifies that concurrent calls to
// Publish() do not race. The race detector will flag any unsynchronised shared
// state exposed by the pipeline's publish closure.
func TestRegistryPipelinePublishConcurrency(t *testing.T) {
	fsys := fsutil.NewMemFS()
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS:                 fsys,
		HomeDir:            "/home/test",
		Roots:              testCQRoots(),
		ClaudeUpstream:     "https://claude.example",
		CodexUpstream:      "https://codex.example",
		HTTPClient:         http.DefaultClient,
		CodexClientVersion: "0.124.0",
		ClaudeToken:        func() (string, error) { return "claude-token", nil },
		CodexToken:         func() (string, error) { return "codex-token", nil },
		Env:                func(string) string { return "" },
		Stderr:             io.Discard,
	})
	if err != nil {
		t.Fatalf("newRegistryPipeline() error = %v", err)
	}

	pipeline.Catalog.Replace(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderAnthropic, ID: "claude-sonnet-4-5", ContextWindow: 200000, MaxOutputTokens: 32000, Source: modelregistry.SourceNative},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.5", Source: modelregistry.SourceNative},
	}})

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			pipeline.Publish()
		}()
	}
	wg.Wait()
}

// TestNewRegistryPipelineToleratesCrossProviderDuplicateInSeed verifies that
// stale generated cache entries containing the same model ID under two
// different providers do not cause newRegistryPipeline to fail. The constructor
// must tolerate such duplicates because refresh rebuilds generated/native data
// from scratch; the duplicate fail-fast belongs at the refreshed final snapshot
// (handled by Refresher.Refresh after merge), not the seed stage.
func TestNewRegistryPipelineToleratesCrossProviderDuplicateInSeed(t *testing.T) {
	fsys := fsutil.NewMemFS()
	// Write a Codex cache with "shared-model" and a Claude capabilities cache
	// that also contains "shared-model" — cross-provider duplicate in seed.
	_ = fsys.WriteFile("/home/test/.codex/models_cache.json", []byte(`{
  "client_version":"0.124.0",
  "models":[{"slug":"shared-model","display_name":"Shared","context_window":100000}]
}`), 0o600)
	_ = fsys.WriteFile("/home/test/.claude/cache/model-capabilities.json", []byte(`{
  "timestamp": 1700000000,
  "models": [{"id":"shared-model","max_input_tokens":200000,"max_tokens":32000}]
}`), 0o600)

	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS:                 fsys,
		HomeDir:            "/home/test",
		Roots:              testCQRoots(),
		ClaudeUpstream:     "https://claude.example",
		CodexUpstream:      "https://codex.example",
		HTTPClient:         http.DefaultClient,
		CodexClientVersion: "0.124.0",
		ClaudeToken:        func() (string, error) { return "claude-token", nil },
		CodexToken:         func() (string, error) { return "codex-token", nil },
		Env:                func(string) string { return "" },
		Stderr:             io.Discard,
	})
	if err != nil {
		t.Fatalf("newRegistryPipeline() error = %v, want nil for cross-provider duplicate in seed (stale generated cache)", err)
	}
	if pipeline == nil {
		t.Fatal("newRegistryPipeline() returned nil pipeline")
	}
	// The catalog should be populated (stale seed is kept for fallback/listing).
	snap := pipeline.Catalog.Snapshot()
	if len(snap.Entries) == 0 {
		t.Error("catalog is empty; expected stale seed entries to be retained")
	}
}

type testCodexRefreshBroker struct {
	calls  int
	result codexprov.RefreshResult
	err    error
}

func (b *testCodexRefreshBroker) Refresh(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.RefreshResult, error) {
	b.calls++
	return b.result, b.err
}

func TestFirstCodexAccessTokenFromInventoryUsesBrokerForStaleManagedCandidate(t *testing.T) {
	ref := codexprov.CandidateRef{AccountKey: "account", CandidateID: "candidate"}
	inventory := codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
		Key: "account",
		Candidates: []codexprov.CredentialCandidate{{
			Ref: ref, Revision: "revision", Source: codexprov.SourceManaged,
			Credential: codexprov.CodexAccount{AccessToken: "stale", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()},
		}},
	}}}
	broker := &testCodexRefreshBroker{result: codexprov.RefreshResult{Material: codexprov.CredentialMaterial{AccessToken: "brokered"}}}
	token, err := firstCodexAccessTokenFromInventory(context.Background(), inventory, broker)
	if err != nil {
		t.Fatal(err)
	}
	if token != "brokered" || broker.calls != 1 {
		t.Fatalf("token = %q, broker calls = %d", token, broker.calls)
	}
}

func TestFirstCodexAccessTokenFromInventorySkipsBrokerForFreshCandidate(t *testing.T) {
	inventory := codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
		Candidates: []codexprov.CredentialCandidate{{
			Source:     codexprov.SourceManaged,
			Credential: codexprov.CodexAccount{AccessToken: "fresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		}},
	}}}
	broker := &testCodexRefreshBroker{err: errors.New("unexpected")}
	token, err := firstCodexAccessTokenFromInventory(context.Background(), inventory, broker)
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh" || broker.calls != 0 {
		t.Fatalf("token = %q, broker calls = %d", token, broker.calls)
	}
}

func TestBetterTokenCandidate(t *testing.T) {
	now := time.Unix(1000, 0)
	nowMs := now.UnixMilli()
	future := nowMs + 10000
	farFuture := nowMs + 20000

	tests := []struct {
		name           string
		currentToken   string
		currentExpires int64
		nextToken      string
		nextExpires    int64
		wantToken      string
		wantExpires    int64
	}{
		{
			name:         "empty next returns current",
			currentToken: "cur", currentExpires: future,
			nextToken: "", nextExpires: farFuture,
			wantToken: "cur", wantExpires: future,
		},
		{
			name:         "stale next is skipped",
			currentToken: "cur", currentExpires: future,
			nextToken: "next", nextExpires: nowMs - 1,
			wantToken: "cur", wantExpires: future,
		},
		{
			name:         "empty current accepts next",
			currentToken: "", currentExpires: 0,
			nextToken: "next", nextExpires: future,
			wantToken: "next", wantExpires: future,
		},
		{
			name:         "unknown current expiry prefers known-fresh next",
			currentToken: "cur", currentExpires: 0,
			nextToken: "next", nextExpires: future,
			wantToken: "next", wantExpires: future,
		},
		{
			name:         "both unknown expiry returns current",
			currentToken: "cur", currentExpires: 0,
			nextToken: "next", nextExpires: 0,
			wantToken: "cur", wantExpires: 0,
		},
		{
			name:         "later expiry wins",
			currentToken: "cur", currentExpires: future,
			nextToken: "next", nextExpires: farFuture,
			wantToken: "next", wantExpires: farFuture,
		},
		{
			name:         "current has later expiry",
			currentToken: "cur", currentExpires: farFuture,
			nextToken: "next", nextExpires: future,
			wantToken: "cur", wantExpires: farFuture,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotToken, gotExpires := betterTokenCandidate(tt.currentToken, tt.currentExpires, tt.nextToken, tt.nextExpires, now)
			if gotToken != tt.wantToken || gotExpires != tt.wantExpires {
				t.Errorf("betterTokenCandidate() = (%q, %d), want (%q, %d)", gotToken, gotExpires, tt.wantToken, tt.wantExpires)
			}
		})
	}
}
