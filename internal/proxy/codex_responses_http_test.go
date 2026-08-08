package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type sequenceRouteChooser struct {
	mu               sync.Mutex
	choices          []RouteChoice
	calls            int
	lastRequirements CodexRouteRequirements
}

func (chooser *sequenceRouteChooser) Choose(_ context.Context, requirements CodexRouteRequirements, excluded ...codex.SelectionExclusion) (RouteChoice, error) {
	chooser.mu.Lock()
	defer chooser.mu.Unlock()
	chooser.calls++
	chooser.lastRequirements = requirements
	blocked := make(map[codex.AccountKey]bool, len(excluded))
	for _, exclusion := range excluded {
		blocked[exclusion.AccountKey] = true
	}
	for _, choice := range chooser.choices {
		if !blocked[choice.AccountKey] {
			chooser.choices = append(chooser.choices[1:], choice)
			return cloneRouteChoice(choice), nil
		}
	}
	return RouteChoice{}, errors.New("no route")
}

type enforcementExecutor struct {
	mu       sync.Mutex
	results  map[codex.AccountKey][]attemptResult
	accounts []codex.AccountKey
	bodies   [][]byte
	headers  []http.Header
}

func (executor *enforcementExecutor) Do(_ context.Context, choice RouteChoice, _ CandidateAttempt, request *http.Request) (*http.Response, error) {
	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, err
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.accounts = append(executor.accounts, choice.AccountKey)
	executor.bodies = append(executor.bodies, data)
	executor.headers = append(executor.headers, request.Header.Clone())
	queue := executor.results[choice.AccountKey]
	if len(queue) == 0 {
		return nil, errors.New("unexpected enforcement attempt")
	}
	result := queue[0]
	executor.results[choice.AccountKey] = queue[1:]
	if result.err != nil {
		return nil, result.err
	}
	header := result.header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: result.status, Header: header, Body: io.NopCloser(bytes.NewBufferString(result.body))}, nil
}

func TestCodexHTTPEnforcerPinsRepeatedSamplingAndSupersedesAtNextTurn(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}, {AccountKey: "two"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"one": {{status: http.StatusOK, body: completedSSE("response-one")}, {status: http.StatusOK, body: completedSSE("response-two")}},
		"two": {{status: http.StatusOK, body: completedSSE("response-three")}},
	}}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsutil.NewMemFS())

	first := strongHTTPProtocolRequest(t, "thread", "turn-one", CodexRequestTurn, "")
	for range 2 {
		response, choice, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, first, protocolHTTPRequest(first))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if choice.AccountKey != "one" {
			t.Fatalf("repeated choice = %q", choice.AccountKey)
		}
	}
	second := strongHTTPProtocolRequest(t, "thread", "turn-two", CodexRequestTurn, "")
	response, choice, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, second, protocolHTTPRequest(second))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if choice.AccountKey != "two" || chooser.calls != 2 {
		t.Fatalf("next choice=%q chooser calls=%d", choice.AccountKey, chooser.calls)
	}
	if _, err := enforcer.Leases.Acquire(context.Background(), NewCodexLeaseKey(first.Metadata.Metadata), func(context.Context) (codex.AccountKey, error) { return "one", nil }); !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("stale turn error = %v", err)
	}
}

func TestCodexHTTPEnforcerFailoverEndsAtAdmission(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}, {AccountKey: "two"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"one": {{status: http.StatusUnauthorized}},
		"two": {{status: http.StatusOK, body: completedSSE("response")}, {status: http.StatusTooManyRequests, body: `{"error":{"type":"insufficient_quota"}}`}},
	}}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsutil.NewMemFS())
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	response, choice, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, request, protocolHTTPRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if choice.AccountKey != "two" {
		t.Fatalf("admitted choice = %q", choice.AccountKey)
	}
	response, choice, _, err = enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, request, protocolHTTPRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || choice.AccountKey != "two" {
		t.Fatalf("post-admission status=%d choice=%q", response.StatusCode, choice.AccountKey)
	}
	if got := executor.accounts; len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "two" {
		t.Fatalf("attempt accounts = %v", got)
	}
}

func TestCodexHTTPEnforcerKeepsParallelLanesIndependent(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}, {AccountKey: "two"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"one": {{status: http.StatusOK, body: completedSSE("one")}},
		"two": {{status: http.StatusOK, body: completedSSE("two")}},
	}}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsutil.NewMemFS())
	for index, thread := range []string{"thread-one", "thread-two"} {
		request := strongHTTPProtocolRequest(t, thread, "turn", CodexRequestTurn, "")
		response, choice, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, request, protocolHTTPRequest(request))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		want := codex.AccountKey([]string{"one", "two"}[index])
		if choice.AccountKey != want {
			t.Fatalf("thread %d choice=%q want=%q", index, choice.AccountKey, want)
		}
	}
}

func TestCodexHTTPEnforcerReplaysEncodedBytesAndFailsClosedOnJournalError(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"one": {{status: http.StatusOK, body: completedSSE("response")}},
	}}
	fsys := &failingDurableFS{MemFS: fsutil.NewMemFS()}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsys)
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	upstream := protocolHTTPRequest(request)
	original, _ := io.ReadAll(upstream.Body)
	upstream.Body = io.NopCloser(bytes.NewReader(original))
	fsys.failWrite = true
	response, _, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, request, upstream)
	if err == nil || response != nil {
		t.Fatalf("response=%v error=%v", response, err)
	}
	if len(executor.bodies) != 1 || !bytes.Equal(executor.bodies[0], original) {
		t.Fatal("request bytes changed before dispatch")
	}
}

func TestCodexHTTPEnforcerRejectsUnprovenHTTPContinuation(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{}}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsutil.NewMemFS())
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	request.PreviousResponseID = "response-old"
	_, _, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, request, protocolHTTPRequest(request))
	if !errors.Is(err, ErrCodexContinuity) || len(executor.accounts) != 0 {
		t.Fatalf("continuation error=%v attempts=%v", err, executor.accounts)
	}
}

func TestCodexHTTPEnforcerRestartRehydratesAccountBinding(t *testing.T) {
	fsys := fsutil.NewMemFS()
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	firstExecutor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{"one": {{status: http.StatusOK, body: completedSSE("response")}}}}
	first := testHTTPEnforcer(t, &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}}}, firstExecutor, fsys)
	response, _, _, err := first.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, request, protocolHTTPRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	secondExecutor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{"one": {{status: http.StatusOK, body: completedSSE("response-two")}}}}
	restarted := testHTTPEnforcer(t, &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "two"}}}, secondExecutor, fsys)
	response, choice, _, err := restarted.Do(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"}, request, protocolHTTPRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if choice.AccountKey != "one" {
		t.Fatalf("restored choice = %q", choice.AccountKey)
	}
}

func TestCodexHTTPEnforcerCompactUsesResponsesNamespace(t *testing.T) {
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestCompaction, CodexCompactionMidTurn)
	if key := NewCodexLeaseKey(request.Metadata.Metadata); key.Lane.Namespace != CodexResponsesNamespace {
		t.Fatalf("namespace = %q", key.Lane.Namespace)
	}
}

func TestCodexHTTPEnforcerMidTurnCompactionRequiresExistingLease(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{}}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsutil.NewMemFS())
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestCompaction, CodexCompactionMidTurn)
	_, _, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: request.Model}, request, protocolHTTPRequest(request))
	if !errors.Is(err, ErrCodexContinuity) || chooser.calls != 0 || len(executor.accounts) != 0 {
		t.Fatalf("error=%v selector calls=%d attempts=%v", err, chooser.calls, executor.accounts)
	}
}

func TestCodexHTTPEnforcerPreTurnCompactionRequiresBothCapacityBuckets(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{"one": {{status: http.StatusOK, body: `{}`}}}}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsutil.NewMemFS())
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestCompaction, CodexCompactionPreTurn)
	response, _, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: codexSparkModel}, request, protocolHTTPRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	buckets := routeBuckets(codexSparkModel, chooser.lastRequirements.RequiredModels, "pro")
	if len(buckets) != 2 || buckets[0] != CapacityBucketForModel(codexSparkModel) || buckets[1] != CapacityBucketBase {
		t.Fatalf("pre-turn buckets = %v", buckets)
	}
}

func TestCodexHTTPEnforcerParsesCompressedStrongMetadataWithoutChangingReplay(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{"one": {{status: http.StatusOK, body: completedSSE("response")}}}}
	enforcer := testHTTPEnforcer(t, chooser, executor, fsutil.NewMemFS())
	decoded := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	encoded := encodeCodexZstd(t, decoded)
	header := make(http.Header)
	header.Set("Content-Encoding", "zstd")
	request, enforce, err := enforcer.Parse(encoded, header)
	if err != nil || !enforce {
		t.Fatalf("enforce=%v error=%v", enforce, err)
	}
	upstream, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", bytes.NewReader(encoded))
	upstream.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	response, _, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: request.Model}, request, upstream)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if len(executor.bodies) != 1 || !bytes.Equal(executor.bodies[0], encoded) {
		t.Fatal("compressed request replay changed")
	}
}

func TestCodexHTTPEnforcerRejectsHTTPPrewarmWithoutSocketLineage(t *testing.T) {
	enforcer := testHTTPEnforcer(t, &sequenceRouteChooser{}, &enforcementExecutor{}, fsutil.NewMemFS())
	body := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"","request_kind":"prewarm"}}}`)
	_, enforce, err := enforcer.Parse(body, nil)
	if !errors.Is(err, ErrCodexContinuity) || enforce {
		t.Fatalf("enforce=%v error=%v", enforce, err)
	}
}

func TestCodexHTTPEnforcerParsesRequestTurnState(t *testing.T) {
	enforcer := testHTTPEnforcer(t, &sequenceRouteChooser{}, &enforcementExecutor{}, fsutil.NewMemFS())
	body := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	header := make(http.Header)
	header.Set("x-codex-turn-state", "state-one")
	request, enforce, err := enforcer.Parse(body, header)
	if err != nil || !enforce || !request.HasTurnState || request.TurnState != "state-one" {
		t.Fatalf("request=%#v enforce=%v error=%v", request, enforce, err)
	}
}

func TestCodexHTTPEnforcerPinsAndValidatesTurnState(t *testing.T) {
	stateHeader := make(http.Header)
	stateHeader.Set("x-codex-turn-state", "state-one")
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"one": {
			{status: http.StatusOK, header: stateHeader, body: completedSSE("response-one")},
			{status: http.StatusOK, body: completedSSE("response-two")},
		},
	}}
	enforcer := testHTTPEnforcer(t, &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "one"}}}, executor, fsutil.NewMemFS())
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")

	for range 2 {
		response, _, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: request.Model}, request, protocolHTTPRequest(request))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if got := executor.headers[1].Get("x-codex-turn-state"); got != "state-one" {
		t.Fatalf("forwarded turn state = %q", got)
	}

	request.HasTurnState = true
	request.TurnState = "state-two"
	_, _, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: request.Model}, request, protocolHTTPRequest(request))
	if !errors.Is(err, ErrCodexContinuity) || len(executor.accounts) != 2 {
		t.Fatalf("error=%v attempts=%v", err, executor.accounts)
	}
}

func TestCodexHTTPFenceOnlyRestoresRetainedAuthority(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store := openTestCodexLeaseStore(t, fsys)
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	lease := CodexTurnLease{
		Key:           NewCodexLeaseKey(request.Metadata.Metadata),
		State:         LeaseBoundQuiescent,
		AccountKey:    "one",
		Generation:    4,
		ModeEpoch:     6,
		Authoritative: true,
		LastSeen:      time.Now(),
	}
	if err := store.CommitCurrentLeases([]CodexTurnLease{lease}); err != nil {
		t.Fatal(err)
	}
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "two"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"one": {{status: http.StatusOK, body: completedSSE("response")}},
	}}
	enforcer, err := NewCodexHTTPEnforcerWithRetainedEpochs(testHTTPRouter(chooser, executor), 9, false, []uint64{6}, store)
	if err != nil {
		t.Fatal(err)
	}
	response, choice, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: request.Model}, request, protocolHTTPRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if choice.AccountKey != "one" || chooser.calls != 0 {
		t.Fatalf("choice=%q selector calls=%d", choice.AccountKey, chooser.calls)
	}
}

func TestCodexHTTPRollbackKeepsExactAuthorityFence(t *testing.T) {
	dir := t.TempDir()
	httpRequirements := testCodexRequirements(CodexRoutingHTTP)
	wsRequirements := testCodexRequirements(CodexRoutingWebSocket)
	if err := SaveCodexReadinessMarker(dir, testCodexMarker(httpRequirements)); err != nil {
		t.Fatal(err)
	}
	config := &Config{CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingOff}
	enforced, err := openCodexRoutingRuntimeAt(dir, config, httpRequirements, wsRequirements)
	if err != nil {
		t.Fatal(err)
	}

	store := openTestCodexLeaseStore(t, fsutil.NewMemFS())
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	if err := store.CommitCurrentLeases([]CodexTurnLease{{
		Key:           NewCodexLeaseKey(request.Metadata.Metadata),
		State:         LeaseBoundQuiescent,
		AccountKey:    "one",
		Generation:    4,
		ModeEpoch:     enforced.HTTP.AuthoritativeEpoch,
		Authoritative: true,
		LastSeen:      time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}

	config.CodexTurnRouting = CodexRoutingOff
	rolledBack, err := openCodexRoutingRuntimeAt(dir, config, httpRequirements, wsRequirements)
	if err != nil {
		t.Fatal(err)
	}
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "two"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"one": {{status: http.StatusOK, body: completedSSE("response")}},
	}}
	enforcer, err := NewCodexHTTPEnforcerWithRetainedEpochs(testHTTPRouter(chooser, executor), rolledBack.HTTP.ModeEpoch, false, rolledBack.HTTP.RetainedAuthoritativeEpochs, store)
	if err != nil {
		t.Fatal(err)
	}
	response, choice, _, err := enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: request.Model}, request, protocolHTTPRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if choice.AccountKey != "one" || chooser.calls != 0 {
		t.Fatalf("choice=%q selector calls=%d runtime=%+v", choice.AccountKey, chooser.calls, rolledBack.HTTP)
	}
}

func TestCodexHTTPFenceOnlyLeavesUnseenTurnToLegacyRoute(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "two"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{}}
	store := openTestCodexLeaseStore(t, fsutil.NewMemFS())
	enforcer, err := NewCodexHTTPEnforcerWithRetainedEpochs(testHTTPRouter(chooser, executor), 9, false, []uint64{6}, store)
	if err != nil {
		t.Fatal(err)
	}
	request := strongHTTPProtocolRequest(t, "thread", "unseen", CodexRequestTurn, "")
	_, _, _, err = enforcer.Do(context.Background(), CodexRouteRequirements{RequestedModel: request.Model}, request, protocolHTTPRequest(request))
	if !errors.Is(err, ErrCodexNoAuthorityFence) || chooser.calls != 0 || len(executor.accounts) != 0 {
		t.Fatalf("error=%v selector calls=%d attempts=%v", err, chooser.calls, executor.accounts)
	}
}

func TestCodexHTTPFenceOnlyServerFallbackUsesLegacyRoute(t *testing.T) {
	chooser := &sequenceRouteChooser{choices: []RouteChoice{{AccountKey: "two"}}}
	executor := &enforcementExecutor{results: map[codex.AccountKey][]attemptResult{
		"two": {{status: http.StatusOK, body: completedSSE("response")}},
	}}
	router := testHTTPRouter(chooser, executor)
	enforcer, err := NewCodexHTTPEnforcerWithRetainedEpochs(router, 9, false, []uint64{6}, openTestCodexLeaseStore(t, fsutil.NewMemFS()))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{CodexRequests: router, CodexHTTPEnforcer: enforcer}
	request := strongHTTPProtocolRequest(t, "thread", "unseen", CodexRequestTurn, "")
	response, choice, observation, err := server.doCodexHTTPRoute(context.Background(), request.Model, request, protocolHTTPRequest(request), nil, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if choice.AccountKey != "two" || observation != nil || chooser.calls != 1 || len(executor.accounts) != 1 {
		t.Fatalf("choice=%q observation=%v selector calls=%d attempts=%v", choice.AccountKey, observation, chooser.calls, executor.accounts)
	}
}

func testHTTPEnforcer(t *testing.T, chooser CodexRouteChooser, executor ExplicitAccountExecutor, fsys fsutil.DurableFileSystem) *CodexHTTPEnforcer {
	t.Helper()
	store := openTestCodexLeaseStore(t, fsys)
	enforcer, err := NewCodexHTTPEnforcer(testHTTPRouter(chooser, executor), 7, store)
	if err != nil {
		t.Fatal(err)
	}
	return enforcer
}

func testHTTPRouter(chooser CodexRouteChooser, executor ExplicitAccountExecutor) *CodexRequestRouter {
	accounts := []codex.LogicalAccount{}
	for _, key := range []codex.AccountKey{"one", "two"} {
		accounts = append(accounts, codex.LogicalAccount{Key: key, Routable: true, Candidates: []codex.CredentialCandidate{{Ref: codex.CandidateRef{AccountKey: key, CandidateID: codex.CandidateID(key + "-candidate")}, Revision: "revision", Source: codex.SourceManaged, CQAuthored: true, AccessExpiresAt: time.Now().Add(time.Hour)}}})
	}
	return &CodexRequestRouter{Scope: &CodexRequestScope{Chooser: chooser, Inventory: staticCredentialInventory{inventory: codex.Inventory{Accounts: accounts}}}, Executor: executor}
}

func strongHTTPProtocolRequest(t *testing.T, thread, turn string, kind CodexRequestKind, phase CodexCompactionPhase) CodexProtocolRequest {
	t.Helper()
	metadata := CodexTurnMetadata{SessionID: "session", ThreadID: thread, TurnID: turn, RequestKind: kind, CompactionPhase: phase}
	return CodexProtocolRequest{Type: "response.create", Model: "gpt-5.4", Metadata: CodexTurnMetadataResult{Metadata: metadata, Source: CodexTurnMetadataNested, Found: true, Strong: true}}
}

func protocolHTTPRequest(request CodexProtocolRequest) *http.Request {
	body := []byte(`{"type":"response.create","model":"gpt-5.4"}`)
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return req
}

func completedSSE(responseID string) string {
	return "data: {\"type\":\"response.created\",\"response\":{\"id\":\"" + responseID + "\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + responseID + "\"}}\n\n"
}
