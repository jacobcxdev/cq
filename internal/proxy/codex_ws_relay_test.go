package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type blockingRelayConn struct {
	closed chan struct{}
	once   sync.Once
	panic  bool
}

func newBlockingRelayConn() *blockingRelayConn {
	return &blockingRelayConn{closed: make(chan struct{})}
}

func (c *blockingRelayConn) ReadMessage() (int, []byte, error) {
	if c.panic {
		c.panic = false
		panic("relay test")
	}
	<-c.closed
	return 0, nil, errors.New("closed")
}

func (*blockingRelayConn) WriteMessage(int, []byte) error   { return nil }
func (*blockingRelayConn) SetReadDeadline(time.Time) error  { return nil }
func (*blockingRelayConn) SetWriteDeadline(time.Time) error { return nil }
func (c *blockingRelayConn) Close() error                   { c.once.Do(func() { close(c.closed) }); return nil }

func TestRelayWebSocketPairJoinsPumpsAcrossRepeatedCancellation(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for iteration := 0; iteration < 1000; iteration++ {
		ctx, cancel := context.WithCancel(context.Background())
		left := newBlockingRelayConn()
		right := newBlockingRelayConn()
		cancel()
		if err := relayWebSocketPair(ctx, left, right); !errors.Is(err, context.Canceled) {
			t.Fatalf("relay error = %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+8 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline+8 {
		t.Fatalf("goroutines = %d, baseline %d", got, baseline)
	}
}

func TestRelayWebSocketPairRecoversPumpPanicAndJoinsPeer(t *testing.T) {
	left := newBlockingRelayConn()
	left.panic = true
	right := newBlockingRelayConn()
	if err := relayWebSocketPair(context.Background(), left, right); err == nil || !strings.Contains(err.Error(), "relay panic") {
		t.Fatalf("relay error = %v", err)
	}
}

func TestCodexWebSocketAttemptExecutorInjectsOnlySelectedCredential(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: true, Subprotocols: []string{"responses"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer selected" || r.Header.Get("ChatGPT-Account-ID") != "account" || r.Header.Get("x-api-key") != "" {
			t.Errorf("headers auth=%q account=%q api-key=%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"), r.Header.Get("x-api-key"))
		}
		if r.Header.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" || !strings.Contains(r.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
			t.Errorf("semantic headers beta=%q extensions=%q", r.Header.Get("OpenAI-Beta"), r.Header.Get("Sec-WebSocket-Extensions"))
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
		"revision": testExactCredentialMaterial(identity, "selected"),
	}}
	executor := NewCodexWebSocketAttemptExecutor(nil, resolver)
	choice := RouteChoice{AccountKey: "identity"}
	attempt := CandidateAttempt{
		AccountKey: "identity", Candidate: codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"},
		Revision: "revision", Source: codex.SourceSystem, Identity: identity,
	}
	headers := http.Header{"Authorization": []string{"Bearer caller"}, "x-api-key": []string{"caller-key"}, "OpenAI-Beta": []string{"responses_websockets=2026-02-06"}, "Sec-WebSocket-Protocol": []string{"responses"}}
	conn, response, _, err := executor.Dial(context.Background(), choice, attempt, "ws"+strings.TrimPrefix(server.URL, "http"), headers)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	if conn.Subprotocol() != "responses" {
		t.Fatalf("subprotocol = %q", conn.Subprotocol())
	}
	_ = conn.Close()
}

func TestCodexWebSocketRelayPreservesLogicalPayloadAcrossCompressedLegs(t *testing.T) {
	type upstreamObservation struct {
		beta       string
		extensions string
		protocol   string
		payload    []byte
		err        error
	}
	observations := make(chan upstreamObservation, 1)
	upstreamReply := []byte(`{"type":"response.created","response":{"id":"synthetic-response"}}`)
	upgrader := websocket.Upgrader{
		CheckOrigin:       func(*http.Request) bool { return true },
		EnableCompression: true,
		Subprotocols:      []string{"responses"},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		observation := upstreamObservation{
			beta:       request.Header.Get("OpenAI-Beta"),
			extensions: request.Header.Get("Sec-WebSocket-Extensions"),
			protocol:   request.Header.Get("Sec-WebSocket-Protocol"),
		}
		defer func() { observations <- observation }()
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			observation.err = err
			return
		}
		defer connection.Close()
		if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			observation.err = err
			return
		}
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			observation.err = err
			return
		}
		if messageType != websocket.TextMessage {
			observation.err = errors.New("upstream received non-text logical message")
			return
		}
		observation.payload = bytes.Clone(payload)
		observation.err = connection.WriteMessage(websocket.TextMessage, upstreamReply)
	}))
	defer upstream.Close()

	plan := requestPlan("one", "candidate")
	attempt := plan.Attempts[0]
	resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
		attempt.Revision: testExactCredentialMaterial(attempt.Identity, "selected"),
	}}
	server := &Server{
		Config: &Config{
			ClaudeUpstream: "https://claude.test",
			CodexUpstream:  upstream.URL,
			LocalToken:     "local-token",
		},
		CodexRequests:          &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}},
		CodexWebSocketExecutor: NewCodexWebSocketAttemptExecutor(nil, resolver),
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	dialer := websocket.Dialer{
		EnableCompression: true,
		HandshakeTimeout:  2 * time.Second,
		Subprotocols:      []string{"responses"},
	}
	header := http.Header{"OpenAI-Beta": []string{"responses_websockets=2026-02-06"}}
	client, response, err := dialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+legacyCodexResponsesPath, header)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	defer client.Close()
	if response == nil || response.StatusCode != http.StatusSwitchingProtocols || !strings.Contains(response.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
		t.Fatalf("downstream compression response = %#v", response)
	}
	if client.Subprotocol() != "responses" {
		t.Fatalf("downstream subprotocol = %q, want responses", client.Subprotocol())
	}

	clientPayload := []byte(`{"type":"response.create","model":"gpt-5.4","input":"logical payload across compressed legs"}`)
	if err := client.WriteMessage(websocket.TextMessage, clientPayload); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, reply, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage || !bytes.Equal(reply, upstreamReply) {
		t.Fatalf("downstream logical reply = type %d %s, want %s", messageType, reply, upstreamReply)
	}

	select {
	case observation := <-observations:
		if observation.err != nil {
			t.Fatal(observation.err)
		}
		if observation.beta != "responses_websockets=2026-02-06" || observation.protocol != "responses" || !strings.Contains(observation.extensions, "permessage-deflate") {
			t.Fatalf("upstream handshake beta=%q protocol=%q extensions=%q", observation.beta, observation.protocol, observation.extensions)
		}
		if !bytes.Equal(observation.payload, clientPayload) {
			t.Fatalf("upstream logical payload = %s, want %s", observation.payload, clientPayload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream compressed-leg observation timed out")
	}
}

func TestCodexWebSocketAttemptExecutorRelistsSameIdentityRevisionBeforeDial(t *testing.T) {
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	ref := codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"}
	inventory := staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{
		Key: "identity", Identity: identity, Routable: true,
		Candidates: []codex.CredentialCandidate{{
			Ref: ref, Revision: "revision-new", Source: codex.SourceExternal, Routable: true,
		}},
	}}}}
	resolver := &testExactSecretResolver{
		errors: map[codex.Revision]error{"revision-old": codex.ErrStaleRevision},
		materials: map[codex.Revision]codex.CredentialMaterial{
			"revision-new": testExactCredentialMaterial(identity, "rotated-secret"),
		},
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	authorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		authorization <- request.Header.Get("Authorization")
		conn, err := upgrader.Upgrade(w, request, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()

	executor := NewCodexWebSocketAttemptExecutor(inventory, resolver)
	attempt := CandidateAttempt{
		AccountKey: "identity", Candidate: ref, Revision: "revision-old",
		Source: codex.SourceExternal, Identity: identity, Ordinal: 1,
	}
	var dispatched CandidateAttempt
	conn, response, _, actual, err := executor.dialOnDispatch(
		context.Background(), RouteChoice{AccountKey: "identity"}, attempt,
		"ws"+strings.TrimPrefix(server.URL, "http"), nil,
		func(resolved CandidateAttempt) { dispatched = resolved },
	)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := <-authorization; got != "Bearer rotated-secret" {
		t.Fatalf("authorization = %q", got)
	}
	if actual.Revision != "revision-new" || dispatched.Revision != "revision-new" {
		t.Fatalf("actual revision = %q, dispatched revision = %q", actual.Revision, dispatched.Revision)
	}
	if actual.Candidate != ref || actual.Source != codex.SourceExternal || actual.Identity != identity || actual.Ordinal != attempt.Ordinal {
		t.Fatalf("actual attempt = %+v", actual)
	}
}

func TestCodexWebSocketAttemptExecutorRejectsUnsafeRelistBeforeDial(t *testing.T) {
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	ref := codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"}
	attempt := CandidateAttempt{
		AccountKey: "identity", Candidate: ref, Revision: "revision-old",
		Source: codex.SourceSystem, Identity: identity, Ordinal: 1,
	}
	for name, inventory := range testRejectedReplanInventories(ref, identity) {
		t.Run(name, func(t *testing.T) {
			resolver := &testExactSecretResolver{
				errors:    map[codex.Revision]error{"revision-old": codex.ErrStaleRevision},
				materials: make(map[codex.Revision]codex.CredentialMaterial),
			}
			executor := NewCodexWebSocketAttemptExecutor(staticCredentialInventory{inventory: inventory}, resolver)
			dispatched := false
			_, response, _, _, err := executor.dialOnDispatch(
				context.Background(), RouteChoice{AccountKey: "identity"}, attempt,
				"ws://127.0.0.1:1/responses", nil,
				func(CandidateAttempt) { dispatched = true },
			)
			if !errors.Is(err, codex.ErrStaleRevision) {
				t.Fatalf("error = %v, want ErrStaleRevision", err)
			}
			if response != nil || dispatched {
				t.Fatalf("response = %#v, dispatched = %v", response, dispatched)
			}
			resolver.mu.Lock()
			plans := len(resolver.plans)
			resolver.mu.Unlock()
			if plans != 1 {
				t.Fatalf("exact resolutions = %d, want 1", plans)
			}
		})
	}
}
