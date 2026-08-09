package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexObserveCorpusDoesNotChangeRouteChoice(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(11, false, nil)
	observer := newCodexTurnObserverWithKey(manager, nil, []byte("01234567890123456789012345678901"))
	var corpus strings.Builder
	for index := range 1000 {
		thread := fmt.Sprintf("thread-%03d", index%25)
		turn := fmt.Sprintf("turn-%03d", index/25)
		kind := CodexRequestTurn
		phase := ""
		if index%17 == 0 {
			kind = CodexRequestCompaction
			phase = `,"compaction":"pre_turn"`
		}
		body := fmt.Sprintf(`{"type":"response.create","model":"gpt-5","client_metadata":{"x-codex-turn-metadata":{"session_id":"session-corpus","thread_id":"%s","turn_id":"%s","request_kind":"%s"%s}}}`, thread, turn, kind, phase)
		corpus.WriteString(body)
		ctx, diag := withRouteDiagnostics(context.Background())
		handle := observer.BeginHTTP(ctx, []byte(body), "identity", "", false)
		choice := RouteChoice{AccountKey: codex.AccountKey(fmt.Sprintf("account-%d", index%3)), RequestedModel: "gpt-5", EffectiveModel: "gpt-5"}
		handle.Selected(choice, false)
		handle.ResponseHeaders(http.StatusOK, nil)
		handle.ObserveBytes([]byte("data: {\"type\":\"response.created\",\"response\":{}}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
		handle.Finish(nil)
		accountHint, failover := diag.fields()
		if accountHint != redactedAccountHint("codex", string(choice.AccountKey)) || failover {
			t.Fatalf("route diagnostics changed at %d: %q/%v", index, accountHint, failover)
		}
	}
	sum := sha256.Sum256([]byte(corpus.String()))
	if got := hex.EncodeToString(sum[:]); got != "45c39a8a16bd4ed6173bcb1e7ae456aa48b530436bd9db39ece8f70bec014e73" {
		t.Fatalf("fixture corpus hash = %s", got)
	}
	health := observer.Health()
	if health.Requests != 1000 || health.StrongKeys != 1000 || health.Unknown != 0 || health.ContinuityErrors != 0 {
		t.Fatalf("health = %#v", health)
	}
}

func TestCodexObserveDiagnosticsNeverLeakRawIdentity(t *testing.T) {
	t.Parallel()
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	ctx, diag := withRouteDiagnostics(context.Background())
	body := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"raw-session-secret","thread_id":"raw-thread-secret","turn_id":"raw-turn-secret","request_kind":"turn"}}}`)
	handle := observer.BeginHTTP(ctx, body, "identity", "", false)
	handle.Selected(RouteChoice{AccountKey: "raw-account-secret"}, false)
	handle.ResponseHeaders(http.StatusOK, nil)
	handle.ObserveBytes([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	event := RouteEvent{}
	event.applyRouteDiagnostics(diag)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"raw-session-secret", "raw-thread-secret", "raw-turn-secret", "raw-account-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, encoded)
		}
	}
	if event.TurnHint == "" || event.RequestKind != "turn" || event.Decision == "" || event.AccountHint == "" {
		t.Fatalf("diagnostics = %#v", event)
	}
}

func TestCodexObserveMalformedAndRateLimitCounters(t *testing.T) {
	t.Parallel()
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	observer.BeginHTTP(context.Background(), []byte("{"), "identity", "", false).Finish(nil)
	body := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}}}`)
	handle := observer.BeginHTTP(context.Background(), body, "identity", "", false)
	handle.Selected(RouteChoice{AccountKey: "account"}, false)
	handle.ResponseHeaders(http.StatusOK, nil)
	handle.ObserveBytes([]byte("data: {\"type\":\"codex.rate_limits\"}\n\ndata: {\n\n"))
	handle.Finish(nil)
	health := observer.Health()
	if health.Unknown < 2 || health.QuotaEvents != 1 {
		t.Fatalf("health = %#v", health)
	}
}

func TestCodexObserveClassifiesRequestInputs(t *testing.T) {
	t.Parallel()
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	metadata := `{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}`
	body := encodeCodexZstd(t, []byte(`{"type":"response.create","model":"gpt-5"}`))
	observer.BeginHTTP(context.Background(), body, "zstd", metadata, false).Finish(nil)
	observer.BeginHTTP(context.Background(), []byte("not-zstd"), "zstd", metadata, false).Finish(nil)
	observer.BeginHTTP(context.Background(), []byte(`{"type":"response.create"}`), "identity", "", false).Finish(nil)
	observer.BeginHTTP(context.Background(), []byte(`{"type":"response.create"}`), "identity", `{`, false).Finish(nil)

	health := observer.Health()
	if health.StrongKeys != 1 || health.MetadataHeaders != 3 || health.ZstdRequests != 2 ||
		health.RequestDecodeErrors != 1 || health.MetadataParseErrors != 1 || health.MissingTurnIdentity != 1 {
		t.Fatalf("health = %#v", health)
	}
}

func TestCodexObservePrewarmAdoptsMatchingRealTurn(t *testing.T) {
	t.Parallel()
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(3, false, nil), nil, []byte("01234567890123456789012345678901"))
	choice := RouteChoice{AccountKey: "account-a"}
	prewarmBody := []byte(`{"type":"response.create","generate":false,"client_metadata":{"x-codex-turn-metadata":{"session_id":"session-p","thread_id":"thread-p","turn_id":"","request_kind":"prewarm"}}}`)
	prewarm := observer.BeginWebSocket(context.Background(), prewarmBody, nil, 41)
	prewarm.Selected(choice, false)
	prewarm.ObserveWebSocketEvent([]byte(`{"type":"response.created","response":{"id":"resp-prewarm"}}`))
	prewarm.ObserveWebSocketEvent([]byte(`{"type":"response.completed","response":{"id":"resp-prewarm"}}`))
	prewarm.Finish(nil)

	realBody := []byte(`{"type":"response.create","previous_response_id":"resp-prewarm","client_metadata":{"x-codex-turn-metadata":{"session_id":"session-p","thread_id":"thread-p","turn_id":"turn-real","request_kind":"turn"}}}`)
	real := observer.BeginWebSocket(context.Background(), realBody, nil, 41)
	real.Selected(choice, false)
	lease, found := observer.Leases.Get(testCodexLeaseKeyFor("session-p", "thread-p", "turn-real"))
	if !found || lease.ResponseAnchor != "resp-prewarm" || lease.UpstreamSocketGeneration != 41 || lease.AccountKey != "account-a" {
		t.Fatalf("adopted lease = %#v, found = %v", lease, found)
	}
}

func TestCodexObservePersistsResponseContinuityMutations(t *testing.T) {
	store := openTestCodexLeaseStore(t, fsutil.NewMemFS())
	manager := NewCodexTurnLeaseManager(13, true, nil)
	observer := newCodexTurnObserverWithKey(manager, store, []byte("01234567890123456789012345678901"))
	body := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	handle := observer.BeginHTTP(context.Background(), body, "identity", "", false)
	handle.Selected(RouteChoice{AccountKey: "account"}, false)
	handle.ResponseHeaders(http.StatusOK, nil)
	handle.ObserveBytes([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-one\",\"output\":[{\"encrypted_content\":\"opaque\"}]}}\n\n"))
	handle.ObserveBytes([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-two\"}}\n\n"))
	handle.Finish(nil)

	key := testCodexLeaseKeyFor("session", "thread", "turn")
	lease, found := manager.Get(key)
	if !found || lease.ResponseAnchor != "response-two" || !lease.HasEncryptedState || lease.State != LeaseBoundQuiescent {
		t.Fatalf("lease=%#v found=%v", lease, found)
	}
	record, account, found := store.LookupMode(key, []codex.AccountKey{"account"}, 13, true)
	if !found || account != "account" || !record.HasResponseAnchor || !record.HasEncryptedState || record.State != LeaseBoundQuiescent {
		t.Fatalf("record=%#v account=%q found=%v", record, account, found)
	}
}

func TestCodexObservePersistsCompactEncryptedAffinity(t *testing.T) {
	store := openTestCodexLeaseStore(t, fsutil.NewMemFS())
	manager := NewCodexTurnLeaseManager(14, true, nil)
	observer := newCodexTurnObserverWithKey(manager, store, []byte("01234567890123456789012345678901"))
	body := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"compaction","compaction":"standalone"}}}`)
	handle := observer.BeginHTTP(context.Background(), body, "identity", "", true)
	handle.Selected(RouteChoice{AccountKey: "account"}, false)
	handle.ResponseHeaders(http.StatusOK, nil)
	handle.ObserveBytes([]byte(`{"id":"compact-response","output":[{"encrypted_content":"opaque"}]}`))
	handle.Finish(nil)

	lease, found := manager.Get(testCodexLeaseKeyFor("session", "thread", "turn"))
	if !found || lease.ResponseAnchor != "compact-response" || !lease.HasEncryptedState || lease.State != LeaseBoundQuiescent {
		t.Fatalf("lease=%#v found=%v", lease, found)
	}
}

func TestCodexObserveScenarioCorpusCoversLifecycleClasses(t *testing.T) {
	t.Parallel()
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	categories := []string{"simple", "tool_loop", "succession", "parallel", "subagent", "prewarm", "compaction", "reconnect", "crossover", "delayed_stale"}
	seen := make(map[string]int)
	var corpus strings.Builder
	for index := range 1000 {
		category := categories[index%len(categories)]
		seen[category]++
		kind := CodexRequestTurn
		turn := fmt.Sprintf("turn-%d", index)
		extra := ""
		if category == "prewarm" {
			kind = CodexRequestPrewarm
			turn = ""
		}
		if category == "compaction" {
			kind = CodexRequestCompaction
			extra = `,"compaction":"mid_turn"`
		}
		body := fmt.Sprintf(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"scenario-session","thread_id":"%s-%d","turn_id":"%s","request_kind":"%s"%s}}}`, category, index%7, turn, kind, extra)
		if index%97 == 0 {
			body = `{"client_metadata":{"x-codex-turn-metadata":"{"}}`
		}
		corpus.WriteString(body)
		observer.BeginHTTP(context.Background(), []byte(body), "identity", "", category == "compaction")
	}
	for _, category := range categories {
		if seen[category] != 100 {
			t.Fatalf("category %s count = %d", category, seen[category])
		}
	}
	sum := sha256.Sum256([]byte(corpus.String()))
	if got := hex.EncodeToString(sum[:]); got != "618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1" {
		t.Fatalf("scenario corpus hash = %s", got)
	}
	if health := observer.Health(); health.Requests != 1000 || health.Unknown != 11 {
		t.Fatalf("health = %#v", health)
	}
}

func TestSanitisedCodexFixtureCapture(t *testing.T) {
	t.Parallel()
	body := []byte(`{"type":"response.create","previous_response_id":"raw-response","client_metadata":{"x-codex-turn-metadata":{"session_id":"raw-session","thread_id":"raw-thread","turn_id":"raw-turn","request_kind":"turn"}}}`)
	fixture, err := BuildSanitisedCodexFixture(body, "identity", "", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := WriteSanitisedCodexFixture(path, fixture); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"raw-session", "raw-thread", "raw-turn", "raw-response"} {
		if strings.Contains(string(data), raw) {
			t.Fatalf("fixture leaked %q: %s", raw, data)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !fixture.HasPreviousID {
		t.Fatalf("mode = %o, fixture = %#v", info.Mode().Perm(), fixture)
	}
}

func testCodexLeaseKeyFor(session, thread, turn string) LeaseKey {
	return LeaseKey{Lane: LaneKey{Session: session, Thread: thread, Namespace: CodexResponsesNamespace}, Turn: turn}
}
