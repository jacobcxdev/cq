package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

	policy := RoutingPolicyDocument{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:           []AccountPoolDocument{{Name: "Team", Value: 10, Members: []codex.AccountKey{"account-a"}}},
		SessionBindings: []SessionBindingDocument{{SessionDigest: digest.SessionDigest, Pool: "Team"}},
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
	poolID := state.Routing.Current().Pools[0].ID

	renameBody, _ := json.Marshal(PoolMutationRequest{Operation: "rename", Name: "team", NewName: "Security Research"})
	renameResponse := httptest.NewRecorder()
	handler.ServeHTTP(renameResponse, httptest.NewRequest(http.MethodPost, RuntimePolicyPoolPath, bytes.NewReader(renameBody)))
	if renameResponse.Code != http.StatusOK {
		t.Fatalf("rename response = %d %q", renameResponse.Code, renameResponse.Body.String())
	}
	valueBody, _ := json.Marshal(PoolMutationRequest{Operation: "value", Name: "SECURITY RESEARCH", Value: 12})
	valueResponse := httptest.NewRecorder()
	handler.ServeHTTP(valueResponse, httptest.NewRequest(http.MethodPost, RuntimePolicyPoolPath, bytes.NewReader(valueBody)))
	if valueResponse.Code != http.StatusOK {
		t.Fatalf("value response = %d %q", valueResponse.Code, valueResponse.Body.String())
	}
	current := state.Routing.Current()
	if current.Pools[0].ID != poolID || current.Pools[0].Name != "Security Research" || current.Pools[0].Value != 12 || current.SessionBindings[0].PoolID != poolID {
		t.Fatalf("mutated internal policy = %#v", current)
	}

	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, RuntimePolicyPath, nil))
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET response = %d %q", getResponse.Code, getResponse.Body.String())
	}
	if bytes.Contains(getResponse.Body.Bytes(), []byte(poolID)) || bytes.Contains(getResponse.Body.Bytes(), []byte(`"pool_id"`)) || bytes.Contains(getResponse.Body.Bytes(), []byte(`"mac"`)) {
		t.Fatalf("GET leaked internal policy: %s", getResponse.Body.String())
	}
	var got RoutingPolicyDocument
	if err := json.Unmarshal(getResponse.Body.Bytes(), &got); err != nil || got.Pools[0].Name != "Security Research" || got.Pools[0].Value != 12 || got.SessionBindings[0].Pool != "Security Research" {
		t.Fatalf("GET document = %#v, error = %v", got, err)
	}
}

func TestPolicyPoolControlReturnsSafeFailureCodes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := InitialiseProxyResilienceState(context.Background(), ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: root, Random: bytes.NewReader(bytes.Repeat([]byte{0x61}, 4096)), Now: time.Now,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := OpenProxyResilienceState(context.Background(), ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.Routing.PublishDocument(RoutingPolicyDocument{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools: []AccountPoolDocument{
			{Name: "Cyber", Members: []codex.AccountKey{"account-a"}},
			{Name: "Research", Members: []codex.AccountKey{"account-b"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	handler, err := (&Server{
		Config: &Config{ClaudeUpstream: "https://example.test"}, RoutingPolicy: state.Routing, SessionPolicy: state.Routing.Resolver(),
	}).handler()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		mutation PoolMutationRequest
		want     string
	}{
		{name: "missing pool", mutation: PoolMutationRequest{Operation: "rename", Name: "Missing", NewName: "Other"}, want: "pool_not_found"},
		{name: "duplicate name", mutation: PoolMutationRequest{Operation: "rename", Name: "Cyber", NewName: "Research"}, want: "pool_name_conflict"},
		{name: "invalid name", mutation: PoolMutationRequest{Operation: "rename", Name: "Cyber", NewName: "Bad\nName"}, want: "invalid_pool_name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.mutation)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, RuntimePolicyPoolPath, bytes.NewReader(body)))
			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
			}
			var failure struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error != test.want {
				t.Fatalf("failure = %q, error = %v, want %q", response.Body.String(), err, test.want)
			}
			if bytes.Contains(response.Body.Bytes(), []byte(test.mutation.Name)) || bytes.Contains(response.Body.Bytes(), []byte(state.Routing.Current().Pools[0].ID)) {
				t.Fatalf("failure leaked policy detail: %q", response.Body.String())
			}
		})
	}
}

// T15's raw policy digest input deliberately differs from the frozen receipt ID.
func TestProxyPolicySessionDigestRawBytesPreserveReceiptValidation(t *testing.T) {
	store, _ := newRoutingPolicyStoreForTest(t)
	server := &Server{RoutingPolicy: store}
	for _, raw := range []string{"identifier", "identifier\n", "identifier\x00", strings.Repeat("Ω", 2048), strings.Repeat("x", 4097), "", string([]byte{0xff})} {
		w := httptest.NewRecorder()
		server.handlePolicySessionDigest(w, httptest.NewRequest(http.MethodPost, RuntimePolicySessionDigestPath, strings.NewReader(raw)))
		valid := len(raw) > 0 && len(raw) <= 4096 && raw != string([]byte{0xff})
		if !valid {
			if w.Code != 400 {
				t.Fatalf("invalid length=%d status=%d", len(raw), w.Code)
			}
			continue
		}
		var result struct {
			Digest string `json:"session_digest"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Digest != store.SessionDigest([]byte(raw)) {
			t.Fatalf("length=%d status=%d err=%v", len(raw), w.Code, err)
		}
		if strings.ContainsAny(raw, "\n\x00") {
			if validCanonicalSessionID([]byte(raw)) {
				t.Fatal("policy input relaxation broadened receipt boundary")
			}
			receipts, err := NewCodexTurnReceiptStore(rand.Reader, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if receipts.register([]byte(raw), []byte("turn"), testCodexTurnReceipt()) != nil {
				t.Fatal("receipt store accepted policy-only raw identity")
			}
			server.CodexTurnReceipts = receipts
			for _, path := range []string{RuntimeCodexTurnReceiptPath, RuntimeCodexTurnReceiptV2Path} {
				body, _ := json.Marshal(map[string]string{"session_id": raw, "turn_id": "turn"})
				response := httptest.NewRecorder()
				server.handleCodexTurnReceipt(response, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
				if response.Code != 400 || response.Body.String() != "invalid Codex turn receipt lookup\n" {
					t.Fatal("receipt control validation changed")
				}
			}
		}
	}
}

type policyReceiptPublisher struct {
	DurableObjectPublisher
	failure error
}

func (p policyReceiptPublisher) PublishImmutable(context.Context, fsutil.SecureDirectory, string, []byte, fs.FileMode) (StableObjectIdentity, error) {
	return StableObjectIdentity{}, p.failure
}
func (p policyReceiptPublisher) ReplaceSelectorExactPrior(context.Context, fsutil.SecureDirectory, string, *StableObjectIdentity, []byte) (StableObjectIdentity, error) {
	return StableObjectIdentity{}, p.failure
}
func TestProxyPolicyControlTypedErrorReceiptsPreserveLegacyResponses(t *testing.T) {
	for _, scenario := range []string{"schema", "references", "generation", "persistence", "cas", "random"} {
		t.Run(scenario, func(t *testing.T) {
			store, _ := newRoutingPolicyStoreForTest(t)
			document := RoutingPolicyDocument{SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1, Pools: []AccountPoolDocument{{Name: "Work", Members: []codex.AccountKey{"a"}}}}
			if err := store.PublishDocument(document); err != nil {
				t.Fatal(err)
			}
			before := store.Current()
			document.AuthorityGeneration++
			document.RoutingGeneration++
			want := "policy_document_invalid"
			switch scenario {
			case "schema":
				document.SchemaVersion = 2
			case "references":
				document.SessionBindings = []SessionBindingDocument{{SessionDigest: strings.Repeat("a", 64), Pool: "missing"}}
			case "generation":
				document.AuthorityGeneration--
				want = "policy_generation_conflict"
			case "persistence":
				store.publisher = policyReceiptPublisher{store.publisher, errors.New("private storage failure")}
				want = "routing_io_failed"
			case "cas":
				store.publisher = policyReceiptPublisher{store.publisher, ErrAuthorityPriorMismatch}
				want = "routing_conflict"
			case "random":
				store.random = strings.NewReader("")
				document.Pools = append(document.Pools, AccountPoolDocument{Name: "New", Members: []codex.AccountKey{"b"}})
				want = "routing_io_failed"
			}
			server := &Server{RoutingPolicy: store, SessionPolicy: store.Resolver()}
			body, _ := json.Marshal(document)
			w := httptest.NewRecorder()
			server.handlePolicyControl(w, httptest.NewRequest(http.MethodPut, RuntimePolicyPath, bytes.NewReader(body)))
			if w.Code != 409 || w.Body.String() != "routing policy rejected\n" || w.Header().Get("X-CQ-Policy-Error") != want {
				t.Fatalf("status=%d body=%q receipt=%q want=%s", w.Code, w.Body.String(), w.Header().Get("X-CQ-Policy-Error"), want)
			}
			if store.Current().RoutingGeneration != before.RoutingGeneration {
				t.Fatal("failed publication changed authority")
			}
		})
	}
	err := invalidRoutingPolicy("invalid schema")
	var typed *RoutingPolicyValidationError
	if !errors.As(err, &typed) || err.Error() != "invalid schema" || errors.Unwrap(err) == nil {
		t.Fatal("validation provenance lost")
	}
	if policyControlErrorCode(errors.Join(errors.New("context"), ErrAuthorityPriorMismatch)) != "routing_conflict" {
		t.Fatal("CAS sentinel lost")
	}
	if policyControlErrorCode(io.ErrUnexpectedEOF) != "routing_io_failed" {
		t.Fatal("entropy classified as document")
	}
}
