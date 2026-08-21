package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestPolicyControlMutatesStoreAlreadyOwnedByWorker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := InitialiseProxyResilienceState(context.Background(), ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: root, Random: bytes.NewReader(bytes.Repeat([]byte{0x51}, 4096)), Now: time.Now,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := OpenProxyResilienceState(context.Background(), ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: root, Random: bytes.NewReader(bytes.Repeat([]byte{0x52}, 4096)), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	resolver := state.Routing.Resolver()
	handler, err := (&Server{Config: &Config{ClaudeUpstream: "https://example.test"}, RoutingPolicy: state.Routing, SessionPolicy: resolver}).handler()
	if err != nil {
		t.Fatal(err)
	}

	session := []byte("private-session")
	digestRequest := httptest.NewRequest(http.MethodPost, RuntimePolicySessionDigestPath, bytes.NewReader(session))
	digestResponse := httptest.NewRecorder()
	handler.ServeHTTP(digestResponse, digestRequest)
	if digestResponse.Code != http.StatusOK || bytes.Contains(digestResponse.Body.Bytes(), session) {
		t.Fatalf("digest response = %d %q", digestResponse.Code, digestResponse.Body.String())
	}
	var digest struct {
		SessionDigest string `json:"session_digest"`
	}
	if err := json.Unmarshal(digestResponse.Body.Bytes(), &digest); err != nil || digest.SessionDigest == "" {
		t.Fatalf("digest response = %q, error = %v", digestResponse.Body.String(), err)
	}

	policy := RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:           []AccountPoolV1{{Name: "team", Members: []codex.AccountKey{"account-a"}}},
		SessionBindings: []SessionBindingV1{{SessionDigest: digest.SessionDigest, Pool: "team"}},
	}
	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	putResponse := httptest.NewRecorder()
	handler.ServeHTTP(putResponse, httptest.NewRequest(http.MethodPut, RuntimePolicyPath, bytes.NewReader(body)))
	if putResponse.Code != http.StatusOK {
		t.Fatalf("PUT response = %d %q", putResponse.Code, putResponse.Body.String())
	}
	decision := resolver.Resolve(session, []codex.AccountKey{"account-a"})
	if decision.Status != PolicyDecisionSelected || len(decision.Allowed) != 1 || decision.Allowed[0] != "account-a" {
		t.Fatalf("updated decision = %#v", decision)
	}

	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, RuntimePolicyPath, nil))
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET response = %d %q", getResponse.Code, getResponse.Body.String())
	}
}
