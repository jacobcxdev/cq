package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	codexStage11ModeEpoch                 = uint64(4)
	codexStage11RouteAccount              = codex.AccountKey("stage11-route-a")
	codexStage11RouteAccountB             = codex.AccountKey("stage11-route-b")
	codexStage11Candidate                 = codex.CandidateID("stage11-candidate-a")
	codexStage11CandidateB                = codex.CandidateID("stage11-candidate-b")
	codexStage11Revision                  = codex.Revision("stage11-revision-a")
	codexStage11RevisionB                 = codex.Revision("stage11-revision-b")
	codexStage11UpstreamID                = "stage11-private-upstream-account-a"
	codexStage11UpstreamIDB               = "stage11-private-upstream-account-b"
	codexStage11UpstreamUser              = "stage11-private-upstream-user-a"
	codexStage11UpstreamUserB             = "stage11-private-upstream-user-b"
	codexStage11UpstreamToken             = "stage11-private-upstream-token-a"
	codexStage11UpstreamTokenB            = "stage11-private-upstream-token-b"
	codexStage11CallerToken               = "stage11-private-caller-token"
	codexStage11CallerAPIKey              = "stage11-private-caller-api-key"
	codexStage11PrivateMarker             = "stage11-private-"
	codexStage11ResponseID                = "resp-stage11-corpus"
	codexStage11ContinuationResponseID    = "resp-stage11-continuation"
	codexStage11CrossoverModelA           = "gpt-5.4-stage11-crossover-a"
	codexStage11CrossoverModelB           = "gpt-5.4-stage11-crossover-b"
	codexStage11MismatchModel             = "gpt-5.4-stage11-mismatch"
	codexStage11UnsupportedAuthorityModel = "gpt-5.4-stage11-unsupported-authority"
	codexStage11FixtureSHA256             = "f457c633d18fb199a3fd6fa25209b3e7cebebcacaaa3569f8ef34501492dbf75"
	codexStage11SmokeSHA256               = "d75adc9740ff14bc46949a129b002b4e7b02cc5162aa3c1fb4f349b96fbdde51"
	codexStage11CategorySchemaSHA256      = "2575392d98f1a492201ed1f5cbd7f07f7d63ec7d76d4457c0193ea39bb7f08f7"
)

var codexStage11LifecycleCategories = []string{
	"simple",
	"tool_loop",
	"succession",
	"parallel",
	"subagents",
	"prewarm",
	"compaction",
	"reconnect",
	"cross_protocol_observe_consistent",
	"delayed_stale",
	"malformed_metadata",
}

func TestCodexStage11LifecycleCorpusUsesProductionHandlers(t *testing.T) {
	if got := codexStage11CategorySchemaSHA256Sum(); got != codexStage11CategorySchemaSHA256 {
		t.Fatalf("category schema SHA-256 = %s, want %s", got, codexStage11CategorySchemaSHA256)
	}
	schema := string(codexStage11CategorySchemaBytes())
	for _, category := range codexStage11LifecycleCategories {
		if !strings.Contains(schema, "\n"+category+"|") {
			t.Fatalf("category schema omits %q", category)
		}
	}
	if !strings.Contains(schema, "capability|websocket_observe_only|ws_routing_enforcement_unavailable") {
		t.Fatal("category schema omits the WebSocket enforcement boundary")
	}
	result := runCodexStage11LifecycleCorpus(t, 1000)
	if result.Cases != 1000 {
		t.Fatalf("cases = %d, want 1000", result.Cases)
	}
	for index, category := range codexStage11LifecycleCategories {
		want := 91
		if index == len(codexStage11LifecycleCategories)-1 {
			want = 90
		}
		if got := result.CategoryCounts[category]; got != want {
			t.Fatalf("category %s count = %d, want %d", category, got, want)
		}
	}
	if result.FixtureSHA256 != codexStage11FixtureSHA256 {
		t.Fatalf("fixture corpus SHA-256 = %s, want %s", result.FixtureSHA256, codexStage11FixtureSHA256)
	}
}

func codexStage11CategorySchemaBytes() []byte {
	return []byte(`stage11-category-schema-v2
simple|http_enforce|durable_v2
tool_loop|ws_observe|live_shadow|zero_ws_journal
succession|http_enforce|durable_v2
parallel|http_enforce|durable_v2
subagents|http_enforce|durable_v2
prewarm|ws_observe_plus_http_enforce|live_prewarm_zero_ws_journal_plus_durable_v2
compaction|http_enforce|durable_v2
reconnect|ws_observe|live_shadow|zero_ws_journal
cross_protocol_observe_consistent|legacy_http_observe_plus_ws_observe|same_actual_account|zero_continuity_errors|zero_ws_journal
delayed_stale|http_enforce|durable_v2
malformed_metadata|http_enforce|durable_v2
capability|websocket_observe_only|ws_routing_enforcement_unavailable
`)
}

func codexStage11CategorySchemaSHA256Sum() string {
	sum := sha256.Sum256(codexStage11CategorySchemaBytes())
	return hex.EncodeToString(sum[:])
}

func TestCodexStage11LifecycleCorpusSmoke(t *testing.T) {
	result := runCodexStage11LifecycleCorpus(t, len(codexStage11LifecycleCategories))
	if result.Cases != len(codexStage11LifecycleCategories) {
		t.Fatalf("cases = %d, want %d", result.Cases, len(codexStage11LifecycleCategories))
	}
	for _, category := range codexStage11LifecycleCategories {
		if result.CategoryCounts[category] != 1 {
			t.Fatalf("category %s count = %d, want 1", category, result.CategoryCounts[category])
		}
	}
	if result.FixtureSHA256 != codexStage11SmokeSHA256 {
		t.Fatalf("smoke fixture SHA-256 = %s, want %s", result.FixtureSHA256, codexStage11SmokeSHA256)
	}
}

func TestCodexStage11CorpusHashSealsFixtureBytes(t *testing.T) {
	inputHeader := http.Header{
		"Authorization": []string{"Bearer " + codexStage11CallerToken},
		"Content-Type":  []string{"application/json"},
		"OpenAI-Beta":   []string{"responses=2026-08-10"},
	}
	routeHeader := http.Header{
		"Authorization":      []string{"Bearer " + codexStage11UpstreamToken},
		"ChatGPT-Account-ID": []string{codexStage11UpstreamID},
		"Content-Type":       []string{"application/json"},
	}
	transcript := codexStage11CorpusTranscript{
		Index:    1,
		Category: "simple",
		Inputs: []codexStage11CorpusInput{{
			ID: "simple", Transport: "http", Path: "/responses", Status: http.StatusOK, Payload: []byte(`{"model":"gpt-5.4"}`),
			Headers: codexStage11CorpusHeaderFacts(inputHeader),
		}},
		Routes: []codexStage11CorpusRoute{{
			Transport: "http", Path: "/responses", AccountKey: string(codexStage11RouteAccount), Payload: []byte(`{"model":"gpt-5.4"}`),
			Headers: codexStage11CorpusHeaderFacts(routeHeader),
		}},
	}
	mutated := transcript
	mutated.Inputs = append([]codexStage11CorpusInput(nil), transcript.Inputs...)
	mutated.Inputs[0].Payload = []byte(`{"model":"gpt-5.5"}`)
	baselineHash, err := hashCodexStage11CorpusTranscripts([]codexStage11CorpusTranscript{transcript})
	if err != nil {
		t.Fatal(err)
	}
	mutatedHash, err := hashCodexStage11CorpusTranscripts([]codexStage11CorpusTranscript{mutated})
	if err != nil {
		t.Fatal(err)
	}
	if baselineHash == mutatedHash {
		t.Fatal("fixture payload mutation retained corpus hash")
	}
	headerMutated := transcript
	headerMutated.Inputs = cloneCodexStage11CorpusInputs(transcript.Inputs)
	for index := range headerMutated.Inputs[0].Headers {
		if headerMutated.Inputs[0].Headers[index].Name == "OpenAI-Beta" {
			headerMutated.Inputs[0].Headers[index].Value = "responses=changed"
		}
	}
	headerHash, err := hashCodexStage11CorpusTranscripts([]codexStage11CorpusTranscript{headerMutated})
	if err != nil {
		t.Fatal(err)
	}
	if baselineHash == headerHash {
		t.Fatal("semantic header mutation retained corpus hash")
	}
	evidenceMutated := transcript
	evidenceMutated.Expected.JournalAdmitted = 1
	evidenceHash, err := hashCodexStage11CorpusTranscripts([]codexStage11CorpusTranscript{evidenceMutated})
	if err != nil {
		t.Fatal(err)
	}
	if baselineHash == evidenceHash {
		t.Fatal("durable lifecycle evidence mutation retained corpus hash")
	}
	encoded, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{codexStage11CallerToken, codexStage11UpstreamToken, codexStage11UpstreamID} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatalf("corpus header facts disclosed %q", private)
		}
	}
	duplicate := transcript
	duplicate.Inputs = append(duplicate.Inputs, duplicate.Inputs[0])
	if _, err := hashCodexStage11CorpusTranscripts([]codexStage11CorpusTranscript{duplicate}); err == nil {
		t.Fatal("duplicate fixture input ID was accepted")
	}
}

func TestCodexStage11ObserveMismatchIsPostDispatch(t *testing.T) {
	harness := newCodexStage11LifecycleHarness(t, true)
	defer harness.close()
	turn := codexStage11TurnIdentity{
		Session: codexStage11PrivateMarker + "negative-observe-session",
		Thread:  codexStage11PrivateMarker + "negative-observe-thread",
		Turn:    codexStage11PrivateMarker + "negative-observe-turn",
	}
	body := codexStage11TurnBodyWithModel(turn, CodexRequestTurn, "", "", false, codexStage11MismatchModel)
	harness.mustHTTPAccount(t, "negative-observe-http-a", "/responses", body, http.StatusOK, turn.Turn, codexStage11RouteAccount)
	harness.mustWebSocket(t, "negative-observe-ws-b", body, codexStage11RouteAccountB)

	expected := codexStage11ExpectedEvidence{
		httpRequests: 1, webSocketConnections: 1, observeRequests: 2, observeAttempts: 2,
		observeContinuityErrors: 1, upstreamRoutes: 2, journalRecords: 1, journalLanes: 1,
		journalShadowRecords: 1,
		shadowLeases:         1, shadowAccountKey: codexStage11RouteAccount, shadowState: LeaseBoundQuiescent,
		shadowHasResponseAnchor: true,
	}
	harness.assertEvidenceAndShutdown(t, expected)
}

func TestCodexStage11NativeHTTPWebSocketAuthorityUnavailable(t *testing.T) {
	harness := newCodexStage11LifecycleHarness(t, false)
	defer harness.close()
	turn := codexStage11TurnIdentity{
		Session: codexStage11PrivateMarker + "negative-authority-session",
		Thread:  codexStage11PrivateMarker + "negative-authority-thread",
		Turn:    codexStage11PrivateMarker + "negative-authority-turn",
	}
	body := codexStage11TurnBodyWithModel(turn, CodexRequestTurn, "", "", false, codexStage11UnsupportedAuthorityModel)
	harness.mustHTTP(t, "negative-authority-http-a", "/responses", body, http.StatusOK, turn.Turn)
	harness.waitForDurableJournalSettlement(t, 1)
	harness.mustWebSocket(t, "negative-authority-ws-b", body, codexStage11RouteAccountB)

	expected := codexStage11ExpectedEvidence{
		httpRequests: 1, webSocketConnections: 1, observeRequests: 1, observeAttempts: 1,
		upstreamRoutes: 2, journalRecords: 1, journalLanes: 1,
		shadowLeases: 1, shadowAccountKey: codexStage11RouteAccountB, shadowState: LeaseBoundQuiescent,
		shadowHasResponseAnchor: true,
	}
	harness.assertEvidenceAndShutdown(t, expected)
}

func TestCodexStage11WebSocketShadowDoesNotAuthoriseNativeHTTP(t *testing.T) {
	harness := newCodexStage11LifecycleHarness(t, false)
	defer harness.close()
	turn := codexStage11TurnIdentity{
		Session: codexStage11PrivateMarker + "negative-shadow-session",
		Thread:  codexStage11PrivateMarker + "negative-shadow-thread",
		Turn:    codexStage11PrivateMarker + "negative-shadow-turn",
	}
	body := codexStage11TurnBodyWithModel(turn, CodexRequestTurn, "", "", false, codexStage11UnsupportedAuthorityModel)
	harness.mustWebSocket(t, "negative-shadow-ws-b", body, codexStage11RouteAccountB)
	harness.mustHTTP(t, "negative-shadow-http-a", "/responses", body, http.StatusOK, turn.Turn)

	expected := codexStage11ExpectedEvidence{
		httpRequests: 1, webSocketConnections: 1, observeRequests: 1, observeAttempts: 1,
		upstreamRoutes: 2, journalRecords: 1, journalLanes: 1,
		shadowLeases: 1, shadowAccountKey: codexStage11RouteAccountB, shadowState: LeaseBoundQuiescent,
		shadowHasResponseAnchor: true,
	}
	harness.assertEvidenceAndShutdown(t, expected)
}

type codexStage11CorpusCase struct {
	Index    int
	Category string
}

type codexStage11CorpusResult struct {
	Cases          int
	CategoryCounts map[string]int
	FixtureSHA256  string
}

type codexStage11CorpusInput struct {
	ID         string
	Connection string
	Step       int
	Transport  string
	Path       string
	Status     int
	Payload    []byte
	Responses  [][]byte
	Headers    []codexStage11CorpusHeaderFact
}

type codexStage11CorpusRoute struct {
	Connection string
	Step       int
	Transport  string
	Path       string
	AccountKey string
	Payload    []byte
	Headers    []codexStage11CorpusHeaderFact
}

type codexStage11CorpusHeaderFact struct {
	Name    string
	Present bool
	Value   string
}

type codexStage11CorpusTranscript struct {
	Index    int
	Category string
	Inputs   []codexStage11CorpusInput
	Routes   []codexStage11CorpusRoute
	Expected codexStage11CorpusExpected
}

type codexStage11CorpusExpected struct {
	HTTPRequests            int
	WebSocketConnections    int
	ObserveRequests         int
	ObserveAttempts         int
	ObserveContinuityErrors int
	UpstreamRoutes          int
	JournalRecords          int
	JournalLanes            int
	JournalAuthoritative    int
	JournalShadow           int
	JournalAdmitted         int
	JournalQuiescent        int
	JournalSuperseded       int
	JournalPredecessors     int
	ShadowLeases            int
	ShadowAccountKey        string
	ShadowState             string
	ShadowHasEncryptedState bool
	ShadowHasResponseAnchor bool
	CrossProtocolOutcome    string
}

func hashCodexStage11CorpusTranscripts(source []codexStage11CorpusTranscript) (string, error) {
	transcripts := append([]codexStage11CorpusTranscript(nil), source...)
	sort.Slice(transcripts, func(left, right int) bool {
		if transcripts[left].Index != transcripts[right].Index {
			return transcripts[left].Index < transcripts[right].Index
		}
		return transcripts[left].Category < transcripts[right].Category
	})
	hash := sha256.New()
	_, _ = io.WriteString(hash, "stage11-corpus-transcript-v2\n")
	seenCases := make(map[int]struct{}, len(transcripts))
	for _, transcript := range transcripts {
		if _, exists := seenCases[transcript.Index]; exists {
			return "", fmt.Errorf("duplicate Stage 11 corpus case index %d", transcript.Index)
		}
		seenCases[transcript.Index] = struct{}{}
		transcript.Inputs = cloneCodexStage11CorpusInputs(transcript.Inputs)
		transcript.Routes = cloneCodexStage11CorpusRoutes(transcript.Routes)
		sort.Slice(transcript.Inputs, func(left, right int) bool {
			return transcript.Inputs[left].ID < transcript.Inputs[right].ID
		})
		for index, input := range transcript.Inputs {
			if input.ID == "" || (index > 0 && input.ID == transcript.Inputs[index-1].ID) {
				return "", fmt.Errorf("duplicate or empty Stage 11 corpus input ID %q", input.ID)
			}
		}
		sort.Slice(transcript.Routes, func(left, right int) bool {
			leftRoute, _ := json.Marshal(transcript.Routes[left])
			rightRoute, _ := json.Marshal(transcript.Routes[right])
			return bytes.Compare(leftRoute, rightRoute) < 0
		})
		encoded, err := json.Marshal(transcript)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write(encoded)
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func cloneCodexStage11CorpusInputs(source []codexStage11CorpusInput) []codexStage11CorpusInput {
	result := make([]codexStage11CorpusInput, len(source))
	for index, input := range source {
		result[index] = input
		result[index].Payload = bytes.Clone(input.Payload)
		result[index].Responses = make([][]byte, len(input.Responses))
		for responseIndex, response := range input.Responses {
			result[index].Responses[responseIndex] = bytes.Clone(response)
		}
		result[index].Headers = append([]codexStage11CorpusHeaderFact(nil), input.Headers...)
	}
	return result
}

func cloneCodexStage11CorpusRoutes(source []codexStage11CorpusRoute) []codexStage11CorpusRoute {
	result := make([]codexStage11CorpusRoute, len(source))
	for index, route := range source {
		result[index] = route
		result[index].Payload = bytes.Clone(route.Payload)
		result[index].Headers = append([]codexStage11CorpusHeaderFact(nil), route.Headers...)
	}
	return result
}

var codexStage11CorpusHeaderNames = []string{
	"Accept",
	"Authorization",
	"ChatGPT-Account-ID",
	"Content-Encoding",
	"Content-Type",
	"OpenAI-Beta",
	"Sec-WebSocket-Protocol",
	"X-Api-Key",
	"X-Codex-Turn-Metadata",
	"X-Codex-Turn-State",
	"X-Codex-Window-Id",
}

func codexStage11CorpusHeaderFacts(header http.Header) []codexStage11CorpusHeaderFact {
	facts := make([]codexStage11CorpusHeaderFact, 0, len(codexStage11CorpusHeaderNames))
	for _, name := range codexStage11CorpusHeaderNames {
		values, present := codexStage11HeaderValues(header, name)
		value := strings.Join(values, "\x00")
		if present && codexStage11PrivateHeader(name) {
			value = codexStage11HeaderDigest(name, value)
		}
		facts = append(facts, codexStage11CorpusHeaderFact{Name: name, Present: present, Value: value})
	}
	return facts
}

func codexStage11HeaderValues(header http.Header, name string) ([]string, bool) {
	type entry struct {
		name   string
		values []string
	}
	var entries []entry
	for actual, values := range header {
		if strings.EqualFold(actual, name) {
			entries = append(entries, entry{name: actual, values: values})
		}
	}
	if len(entries) == 0 {
		return nil, false
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].name < entries[right].name
	})
	var result []string
	for _, entry := range entries {
		result = append(result, entry.values...)
	}
	return result, true
}

func codexStage11PrivateHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Chatgpt-Account-Id", "X-Api-Key", "X-Codex-Turn-Metadata", "X-Codex-Turn-State", "X-Codex-Window-Id":
		return true
	default:
		return false
	}
}

func codexStage11HeaderDigest(name, value string) string {
	mac := hmac.New(sha256.New, []byte("stage11-corpus-synthetic-header-v1"))
	_, _ = io.WriteString(mac, strings.ToLower(name))
	_, _ = mac.Write([]byte{0})
	_, _ = io.WriteString(mac, value)
	return hex.EncodeToString(mac.Sum(nil))
}

type codexStage11ExpectedEvidence struct {
	httpRequests              int
	webSocketConnections      int
	observeRequests           int
	observeAttempts           int
	observeContinuityErrors   int
	upstreamRoutes            int
	journalRecords            int
	journalLanes              int
	journalShadowRecords      int
	journalSupersededRecords  int
	journalPredecessorRecords int
	shadowLeases              int
	shadowAccountKey          codex.AccountKey
	shadowState               LeaseState
	shadowHasEncryptedState   bool
	shadowHasResponseAnchor   bool
	crossProtocolOutcome      string
}

func runCodexStage11LifecycleCorpus(t *testing.T, caseCount int) codexStage11CorpusResult {
	t.Helper()
	cases, counts := buildCodexStage11LifecycleCorpus(caseCount)
	transcripts := make([]codexStage11CorpusTranscript, 0, len(cases))
	for _, corpusCase := range cases {
		transcripts = append(transcripts, runCodexStage11LifecycleCorpusCase(t, corpusCase))
	}
	fixtureSHA256, err := hashCodexStage11CorpusTranscripts(transcripts)
	if err != nil {
		t.Fatal(err)
	}
	return codexStage11CorpusResult{
		Cases:          len(cases),
		CategoryCounts: counts,
		FixtureSHA256:  fixtureSHA256,
	}
}

func runCodexStage11LifecycleCorpusCase(t *testing.T, corpusCase codexStage11CorpusCase) codexStage11CorpusTranscript {
	t.Helper()
	harness := newCodexStage11LifecycleHarness(t, corpusCase.Category == "cross_protocol_observe_consistent")
	defer harness.close()
	expected := harness.runCase(t, corpusCase)
	harness.assertEvidenceAndShutdown(t, expected)
	return harness.corpusTranscript(corpusCase, expected)
}

func buildCodexStage11LifecycleCorpus(caseCount int) ([]codexStage11CorpusCase, map[string]int) {
	cases := make([]codexStage11CorpusCase, 0, caseCount)
	counts := make(map[string]int, len(codexStage11LifecycleCategories))
	for index := range caseCount {
		category := codexStage11LifecycleCategories[index%len(codexStage11LifecycleCategories)]
		corpusCase := codexStage11CorpusCase{Index: index, Category: category}
		cases = append(cases, corpusCase)
		counts[category]++
	}
	return cases, counts
}

type codexStage11LifecycleHarness struct {
	fsys            *fsutil.MemFS
	sentinels       map[string][]byte
	diagnostics     *DiagnosticsWriter
	diagnosticsPath string
	coordinator     *CodexContinuityCoordinator
	observer        *CodexTurnObserver
	upstream        *httptest.Server
	proxy           *httptest.Server
	proxyHandlers   *codexStage11HandlerTracker
	recorder        *codexStage11UpstreamRecorder
	client          *http.Client
	inputMu         sync.Mutex
	inputs          []codexStage11CorpusInput
	webSocketMu     sync.Mutex
	webSockets      int
	closeOnce       sync.Once
	closeErr        error
}

type codexStage11HandlerTracker struct {
	handler http.Handler
	mu      sync.Mutex
	active  int
	idle    chan struct{}
}

func newCodexStage11HandlerTracker(handler http.Handler) *codexStage11HandlerTracker {
	idle := make(chan struct{})
	close(idle)
	return &codexStage11HandlerTracker{handler: handler, idle: idle}
}

func (tracker *codexStage11HandlerTracker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracker.mu.Lock()
	if tracker.active == 0 {
		tracker.idle = make(chan struct{})
	}
	tracker.active++
	tracker.mu.Unlock()
	defer func() {
		tracker.mu.Lock()
		tracker.active--
		if tracker.active == 0 {
			close(tracker.idle)
		}
		tracker.mu.Unlock()
	}()
	tracker.handler.ServeHTTP(writer, request)
}

func (tracker *codexStage11HandlerTracker) waitForIdle(t *testing.T) {
	t.Helper()
	tracker.mu.Lock()
	idle := tracker.idle
	tracker.mu.Unlock()
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("Stage 11 proxy handler did not shut down")
	}
}

func newCodexStage11LifecycleHarness(t *testing.T, observeHTTP bool) *codexStage11LifecycleHarness {
	t.Helper()
	fixedNow := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	sentinels := map[string][]byte{
		"/state/auth.json":     []byte(`{"sentinel":"stage11-system-auth"}`),
		"/state/registry.json": []byte(`{"sentinel":"stage11-global-registry"}`),
	}
	for path, value := range sentinels {
		if err := fsys.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	options := CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return fixedNow },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{codexStage11ModeEpoch}},
	}
	owner := testCodexLeaseOwner{}
	if err := InitialiseCodexContinuityAuthority(options, owner); err != nil {
		t.Fatalf("initialise Stage 11 continuity: %v", err)
	}
	coordinator, err := OpenCodexContinuityCoordinator(options, owner)
	if err != nil {
		t.Fatalf("open Stage 11 continuity: %v", err)
	}
	runtime, err := NewCodexLeaseRuntime(coordinator, func(_ context.Context, account codex.AccountKey) error {
		if account != codexStage11RouteAccount && account != codexStage11RouteAccountB {
			return errors.New("Stage 11 route account unavailable")
		}
		return nil
	})
	if err != nil {
		_ = coordinator.Close()
		t.Fatalf("create Stage 11 lease runtime: %v", err)
	}

	identity := codex.AccountIdentity{AccountID: codexStage11UpstreamID, UserID: codexStage11UpstreamUser}
	identityB := codex.AccountIdentity{AccountID: codexStage11UpstreamIDB, UserID: codexStage11UpstreamUserB}
	inventory := staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{
		{
			Key:      codexStage11RouteAccount,
			Identity: identity,
			Routable: true,
			Candidates: []codex.CredentialCandidate{{
				Ref:      codex.CandidateRef{AccountKey: codexStage11RouteAccount, CandidateID: codexStage11Candidate},
				Revision: codexStage11Revision,
				Source:   codex.SourceExternal,
				Routable: true,
			}},
		},
		{
			Key:      codexStage11RouteAccountB,
			Identity: identityB,
			Routable: true,
			Candidates: []codex.CredentialCandidate{{
				Ref:      codex.CandidateRef{AccountKey: codexStage11RouteAccountB, CandidateID: codexStage11CandidateB},
				Revision: codexStage11RevisionB,
				Source:   codex.SourceExternal,
				Routable: true,
			}},
		},
	}}}
	resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
		codexStage11Revision:  testExactCredentialMaterial(identity, codexStage11UpstreamToken),
		codexStage11RevisionB: testExactCredentialMaterial(identityB, codexStage11UpstreamTokenB),
	}}

	recorder := &codexStage11UpstreamRecorder{}
	upstream := httptest.NewServer(recorder)
	transport := &CodexTokenTransport{Inner: upstream.Client().Transport}
	attemptExecutor := &CodexAttemptExecutor{Inventory: inventory, Secrets: resolver, Transport: transport}
	capacity := NewCodexCapacityLedger(func() time.Time { return fixedNow }, time.Hour)
	planner := &CodexHTTPRequestPlanFactory{
		Inventory:         inventory,
		Capacity:          capacity,
		Routes:            coordinator,
		Runtime:           runtime,
		DefaultAccountKey: codexStage11RouteAccount,
		Authority: CodexLeaseAuthorityPolicy{
			ModeEpoch:     codexStage11ModeEpoch,
			Authoritative: true,
		},
		Now: func() time.Time { return fixedNow },
	}
	nativeHTTP, err := NewCodexNativeHTTPHandler(
		planner,
		&CodexHTTPRequestSession{Executor: attemptExecutor, Refresher: codexStage11Refresher{}, Capacity: capacity},
		upstream.URL,
	)
	if err != nil {
		upstream.Close()
		_ = coordinator.Close()
		t.Fatal(err)
	}
	router := &CodexRequestRouter{
		Scope: &CodexRequestScope{
			Chooser:   &codexStage11RouteChooser{},
			Inventory: inventory,
			Now:       func() time.Time { return fixedNow },
		},
		Executor: attemptExecutor,
		Capacity: capacity,
	}
	observer, err := NewCodexV2TurnObserver(runtime, CodexLeaseAuthorityPolicy{ModeEpoch: codexStage11ModeEpoch})
	if err != nil {
		upstream.Close()
		_ = coordinator.Close()
		t.Fatal(err)
	}
	observer.BindCapacity(capacity)
	if observer.Leases.mu != coordinator.leases.mu {
		upstream.Close()
		_ = coordinator.Close()
		t.Fatal("Stage 11 WebSocket observer did not share the continuity coordinator core")
	}

	diagnosticsPath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, err := OpenDiagnosticsWriter(diagnosticsPath)
	if err != nil {
		upstream.Close()
		_ = coordinator.Close()
		t.Fatal(err)
	}
	var nativeRouting CodexNativeHTTPRoutingHandler = nativeHTTP
	var httpObserver *CodexTurnObserver
	httpRoutingMode := CodexRoutingEnforce
	if observeHTTP {
		nativeRouting = nil
		httpObserver = observer
		httpRoutingMode = CodexRoutingObserve
	}
	server := &Server{
		Config: &Config{
			ClaudeUpstream:     upstream.URL,
			CodexUpstream:      upstream.URL,
			LocalToken:         "stage11-local-token",
			CodexTurnRouting:   httpRoutingMode,
			CodexWSTurnRouting: CodexRoutingObserve,
		},
		CodexRequests:                    router,
		CodexWebSocketExecutor:           NewCodexWebSocketAttemptExecutor(inventory, resolver),
		CodexNativeHTTP:                  nativeRouting,
		CodexObserver:                    httpObserver,
		CodexWebSocketObserver:           observer,
		CodexWebSocketObserverConfigured: true,
		Diag:                             diagnostics,
	}
	handler, err := server.handler()
	if err != nil {
		_ = diagnostics.Close()
		upstream.Close()
		_ = coordinator.Close()
		t.Fatal(err)
	}
	proxyHandlers := newCodexStage11HandlerTracker(handler)
	proxyServer := httptest.NewServer(proxyHandlers)
	harness := &codexStage11LifecycleHarness{
		fsys:            fsys,
		sentinels:       sentinels,
		diagnostics:     diagnostics,
		diagnosticsPath: diagnosticsPath,
		coordinator:     coordinator,
		observer:        observer,
		upstream:        upstream,
		proxy:           proxyServer,
		proxyHandlers:   proxyHandlers,
		recorder:        recorder,
		client:          &http.Client{Timeout: 5 * time.Second},
	}
	return harness
}

type codexStage11RouteChooser struct {
	mu            sync.Mutex
	mismatchCalls int
}

func (chooser *codexStage11RouteChooser) Choose(_ context.Context, requirements CodexRouteRequirements, excluded ...codex.SelectionExclusion) (RouteChoice, error) {
	account := codexStage11RouteAccount
	switch requirements.RequestedModel {
	case codexStage11CrossoverModelB, codexStage11UnsupportedAuthorityModel:
		account = codexStage11RouteAccountB
	case codexStage11MismatchModel:
		chooser.mu.Lock()
		chooser.mismatchCalls++
		if chooser.mismatchCalls > 1 {
			account = codexStage11RouteAccountB
		}
		chooser.mu.Unlock()
	}
	for _, exclusion := range excluded {
		if exclusion.AccountKey == account {
			return RouteChoice{}, errors.New("Stage 11 route account excluded")
		}
	}
	return RouteChoice{
		AccountKey:      account,
		RequestedModel:  requirements.RequestedModel,
		EffectiveModel:  requirements.RequestedModel,
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
	}, nil
}

type codexStage11Refresher struct{}

func (codexStage11Refresher) RefreshReference(_ context.Context, ref codex.CandidateRef, revision codex.Revision) (codex.CandidateRef, codex.Revision, error) {
	return ref, revision, codex.ErrRefreshIneligible
}

func (h *codexStage11LifecycleHarness) close() error {
	h.closeOnce.Do(func() {
		h.proxy.Close()
		h.upstream.Close()
		h.closeErr = errors.Join(h.diagnostics.Close(), h.coordinator.Close())
	})
	return h.closeErr
}

func (h *codexStage11LifecycleHarness) recordCorpusInput(input codexStage11CorpusInput) {
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	h.inputs = append(h.inputs, cloneCodexStage11CorpusInputs([]codexStage11CorpusInput{input})[0])
}

func (h *codexStage11LifecycleHarness) corpusTranscript(corpusCase codexStage11CorpusCase, expected codexStage11ExpectedEvidence) codexStage11CorpusTranscript {
	h.inputMu.Lock()
	inputs := cloneCodexStage11CorpusInputs(h.inputs)
	h.inputMu.Unlock()
	events, _ := h.recorder.snapshot()
	routes := make([]codexStage11CorpusRoute, len(events))
	for index, event := range events {
		routes[index] = codexStage11CorpusRoute{
			Connection: event.Connection,
			Step:       event.Step,
			Transport:  event.Transport,
			Path:       event.Path,
			AccountKey: string(codexStage11ActualAccountKey(event)),
			Payload:    bytes.Clone(event.Payload),
			Headers:    append([]codexStage11CorpusHeaderFact(nil), event.Headers...),
		}
	}
	shadowState := ""
	if expected.shadowLeases != 0 {
		shadowState = expected.shadowState.String()
	}
	return codexStage11CorpusTranscript{
		Index:    corpusCase.Index,
		Category: corpusCase.Category,
		Inputs:   inputs,
		Routes:   routes,
		Expected: codexStage11CorpusExpected{
			HTTPRequests:            expected.httpRequests,
			WebSocketConnections:    expected.webSocketConnections,
			ObserveRequests:         expected.observeRequests,
			ObserveAttempts:         expected.observeAttempts,
			ObserveContinuityErrors: expected.observeContinuityErrors,
			UpstreamRoutes:          expected.upstreamRoutes,
			JournalRecords:          expected.journalRecords,
			JournalLanes:            expected.journalLanes,
			JournalAuthoritative:    expected.journalRecords - expected.journalShadowRecords,
			JournalShadow:           expected.journalShadowRecords,
			JournalAdmitted:         expected.journalRecords,
			JournalQuiescent:        expected.journalRecords - expected.journalSupersededRecords,
			JournalSuperseded:       expected.journalSupersededRecords,
			JournalPredecessors:     expected.journalPredecessorRecords,
			ShadowLeases:            expected.shadowLeases,
			ShadowAccountKey:        string(expected.shadowAccountKey),
			ShadowState:             shadowState,
			ShadowHasEncryptedState: expected.shadowHasEncryptedState,
			ShadowHasResponseAnchor: expected.shadowHasResponseAnchor,
			CrossProtocolOutcome:    expected.crossProtocolOutcome,
		},
	}
}

func (h *codexStage11LifecycleHarness) runCase(t *testing.T, corpusCase codexStage11CorpusCase) codexStage11ExpectedEvidence {
	t.Helper()
	identity := func(lane, turn string) codexStage11TurnIdentity {
		return codexStage11TurnIdentity{
			Session: fmt.Sprintf("%ssession-%04d", codexStage11PrivateMarker, corpusCase.Index),
			Thread:  fmt.Sprintf("%sthread-%s-%s-%04d", codexStage11PrivateMarker, corpusCase.Category, lane, corpusCase.Index),
			Turn:    fmt.Sprintf("%sturn-%s-%s-%04d", codexStage11PrivateMarker, corpusCase.Category, turn, corpusCase.Index),
		}
	}
	requestID := func(step string) string {
		return fmt.Sprintf("case-%04d-%s-%s", corpusCase.Index, corpusCase.Category, step)
	}

	switch corpusCase.Category {
	case "simple":
		turn := identity("main", "main")
		h.mustHTTP(t, requestID("http"), "/responses", codexStage11TurnBody(turn, CodexRequestTurn, "", "", false), http.StatusOK, turn.Turn)
		return codexStage11ExpectedEvidence{httpRequests: 1, upstreamRoutes: 1, journalRecords: 1, journalLanes: 1}

	case "tool_loop":
		turn := identity("main", "tool")
		h.mustWebSocketToolLoop(t, requestID("tool"), turn)
		return codexStage11ExpectedEvidence{
			webSocketConnections: 1, observeRequests: 2, observeAttempts: 1, upstreamRoutes: 2,
			shadowLeases: 1, shadowAccountKey: codexStage11RouteAccount, shadowState: LeaseBoundQuiescent,
			shadowHasEncryptedState: true, shadowHasResponseAnchor: true,
		}

	case "succession":
		first := identity("main", "first")
		second := identity("main", "second")
		h.mustHTTP(t, requestID("first"), "/v1/responses", codexStage11TurnBody(first, CodexRequestTurn, "", "", false), http.StatusOK, first.Turn)
		h.mustHTTP(t, requestID("second"), "/responses", codexStage11TurnBody(second, CodexRequestTurn, "", "", false), http.StatusOK, second.Turn)
		return codexStage11ExpectedEvidence{
			httpRequests: 2, upstreamRoutes: 2, journalRecords: 2, journalLanes: 1,
			journalSupersededRecords: 1, journalPredecessorRecords: 1,
		}

	case "parallel":
		left := identity("left", "left")
		right := identity("right", "right")
		h.mustParallelHTTP(t, []codexStage11HTTPRequest{
			{ID: requestID("left"), Path: "/responses", Body: codexStage11TurnBody(left, CodexRequestTurn, "", "", false), Needle: left.Turn},
			{ID: requestID("right"), Path: "/responses", Body: codexStage11TurnBody(right, CodexRequestTurn, "", "", false), Needle: right.Turn},
		})
		return codexStage11ExpectedEvidence{httpRequests: 2, upstreamRoutes: 2, journalRecords: 2, journalLanes: 2}

	case "subagents":
		root := identity("root", "root")
		child := identity("child", "child")
		h.mustParallelHTTP(t, []codexStage11HTTPRequest{
			{ID: requestID("root"), Path: "/responses", Body: codexStage11TurnBody(root, CodexRequestTurn, "", "", false), Needle: root.Turn},
			{ID: requestID("child"), Path: "/responses", Body: codexStage11TurnBody(child, CodexRequestTurn, "", "", false), Needle: child.Turn},
		})
		return codexStage11ExpectedEvidence{httpRequests: 2, upstreamRoutes: 2, journalRecords: 2, journalLanes: 2}

	case "prewarm":
		prewarm := identity("main", "unused")
		prewarm.Turn = ""
		h.mustWebSocket(t, requestID("prewarm"), codexStage11TurnBody(prewarm, CodexRequestPrewarm, "", "", true), codexStage11RouteAccount)
		turn := identity("main", "turn")
		h.mustHTTP(t, requestID("turn"), "/responses", codexStage11TurnBody(turn, CodexRequestTurn, "", "", false), http.StatusOK, turn.Turn)
		return codexStage11ExpectedEvidence{httpRequests: 1, webSocketConnections: 1, observeRequests: 1, observeAttempts: 1, upstreamRoutes: 2, journalRecords: 1, journalLanes: 1}

	case "compaction":
		turn := identity("main", "compact")
		path := "/responses/compact"
		if corpusCase.Index%2 == 0 {
			path = "/v1/responses/compact"
		}
		h.mustHTTP(t, requestID("compact"), path, codexStage11TurnBody(turn, CodexRequestCompaction, string(CodexCompactionStandalone), "", false), http.StatusOK, turn.Turn)
		return codexStage11ExpectedEvidence{httpRequests: 1, upstreamRoutes: 1, journalRecords: 1, journalLanes: 1}

	case "reconnect":
		turn := identity("main", "ws")
		key := testCodexLeaseKeyFor(turn.Session, turn.Thread, turn.Turn)
		h.mustWebSocketSequence(t, []codexStage11WebSocketStep{{
			ID:       requestID("ws-1"),
			Frame:    codexStage11TurnBody(turn, CodexRequestTurn, "", "", false),
			Evidence: []string{codexStage11ResponseID, "encrypted_content", `"end_turn":false`},
		}}, codexStage11RouteAccount)
		h.assertShadowLease(t, key, codexStage11RouteAccount, LeaseContinuationPending, true, codexStage11ResponseID)
		h.mustWebSocketSequence(t, []codexStage11WebSocketStep{{
			ID:       requestID("ws-2"),
			Frame:    codexStage11ContinuationTurnBody(turn),
			Evidence: []string{codexStage11ContinuationResponseID, `"end_turn":true`},
		}}, codexStage11RouteAccount)
		h.assertShadowLease(t, key, codexStage11RouteAccount, LeaseBoundQuiescent, true, codexStage11ContinuationResponseID)
		return codexStage11ExpectedEvidence{
			webSocketConnections: 2, observeRequests: 2, observeAttempts: 2, upstreamRoutes: 2,
			shadowLeases: 1, shadowAccountKey: codexStage11RouteAccount, shadowState: LeaseBoundQuiescent,
			shadowHasEncryptedState: true, shadowHasResponseAnchor: true,
		}

	case "cross_protocol_observe_consistent":
		turn := identity("main", "cross")
		model := codexStage11CrossoverModelA
		account := codexStage11RouteAccount
		outcome := "legacy_http_and_ws_observe_same_a"
		if corpusCase.Index%2 != 0 {
			model = codexStage11CrossoverModelB
			account = codexStage11RouteAccountB
			outcome = "legacy_http_and_ws_observe_same_b"
		}
		body := codexStage11TurnBodyWithModel(turn, CodexRequestTurn, "", "", false, model)
		h.mustHTTPAccount(t, requestID("legacy-http-observe"), "/responses", body, http.StatusOK, turn.Turn, account)
		h.mustWebSocket(t, requestID("ws-observe"), body, account)
		return codexStage11ExpectedEvidence{
			httpRequests: 1, webSocketConnections: 1, observeRequests: 2, observeAttempts: 2,
			upstreamRoutes: 2, journalRecords: 1, journalLanes: 1,
			journalShadowRecords: 1,
			shadowLeases:         1, shadowAccountKey: account, shadowState: LeaseBoundQuiescent,
			shadowHasResponseAnchor: true, crossProtocolOutcome: outcome,
		}

	case "delayed_stale":
		first := identity("main", "first")
		second := identity("main", "second")
		h.mustHTTP(t, requestID("first"), "/responses", codexStage11TurnBody(first, CodexRequestTurn, "", "", false), http.StatusOK, first.Turn)
		h.mustHTTP(t, requestID("second"), "/responses", codexStage11TurnBody(second, CodexRequestTurn, "", "", false), http.StatusOK, second.Turn)
		lateID := requestID("late")
		upstreamStart := h.upstreamEventCount()
		status, response, err := h.doHTTP(lateID, "/responses", codexStage11TurnBody(first, CodexRequestTurn, "", "", false))
		if err != nil {
			t.Fatal(err)
		}
		var failure struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response, &failure); err != nil {
			t.Fatalf("decode delayed-stale response: %v; body=%s", err, response)
		}
		if status != http.StatusServiceUnavailable || failure.Type != "error" || failure.Error.Type != "api_error" || failure.Error.Message != "Codex native HTTP routing unavailable" {
			t.Fatalf("delayed-stale response = status %d payload %#v, want exact fail-closed routing error", status, failure)
		}
		h.assertNoUpstream(t, upstreamStart, lateID)
		return codexStage11ExpectedEvidence{
			httpRequests: 3, upstreamRoutes: 2, journalRecords: 2, journalLanes: 1,
			journalSupersededRecords: 1, journalPredecessorRecords: 1,
		}

	case "malformed_metadata":
		malformedID := requestID("malformed")
		malformed := []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":"{"},"input":[{"role":"user","content":"%sprompt-%04d"}]}`, codexStage11PrivateMarker, corpusCase.Index))
		upstreamStart := h.upstreamEventCount()
		status, response, err := h.doHTTP(malformedID, "/responses", malformed)
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusBadRequest || strings.Contains(string(response), codexStage11PrivateMarker) {
			t.Fatalf("malformed response status/body = %d/%s", status, response)
		}
		h.assertNoUpstream(t, upstreamStart, malformedID)
		turn := identity("recovery", "valid")
		h.mustHTTP(t, requestID("valid"), "/responses", codexStage11TurnBody(turn, CodexRequestTurn, "", "", false), http.StatusOK, turn.Turn)
		return codexStage11ExpectedEvidence{httpRequests: 2, upstreamRoutes: 1, journalRecords: 1, journalLanes: 1}
	default:
		t.Fatalf("unknown Stage 11 lifecycle category %q", corpusCase.Category)
		return codexStage11ExpectedEvidence{}
	}
}

type codexStage11TurnIdentity struct {
	Session string
	Thread  string
	Turn    string
}

func codexStage11TurnBody(identity codexStage11TurnIdentity, kind CodexRequestKind, compaction, previous string, prewarm bool) []byte {
	return codexStage11TurnBodyWithModel(identity, kind, compaction, previous, prewarm, "gpt-5.4")
}

func codexStage11TurnBodyWithModel(identity codexStage11TurnIdentity, kind CodexRequestKind, compaction, previous string, prewarm bool, model string) []byte {
	metadata := map[string]any{
		"session_id":   identity.Session,
		"thread_id":    identity.Thread,
		"turn_id":      identity.Turn,
		"request_kind": kind,
	}
	if compaction != "" {
		metadata["compaction"] = compaction
	}
	payload := map[string]any{
		"type":  "response.create",
		"model": model,
		"client_metadata": map[string]any{
			"x-codex-turn-metadata": metadata,
		},
		"input": []map[string]any{{"role": "user", "content": codexStage11PrivateMarker + "prompt"}},
	}
	if previous != "" {
		payload["previous_response_id"] = previous
	}
	if prewarm {
		payload["generate"] = false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return encoded
}

func codexStage11ContinuationTurnBody(identity codexStage11TurnIdentity) []byte {
	body := codexStage11TurnBody(identity, CodexRequestTurn, "", codexStage11ResponseID, false)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		panic(err)
	}
	payload["input"] = []map[string]any{
		{
			"type":    "function_call_output",
			"call_id": "stage11-call",
			"output":  "stage11-tool-result",
		},
		{
			"type":              "reasoning",
			"summary":           []any{},
			"encrypted_content": "opaque-stage11-state",
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return encoded
}

type codexStage11HTTPRequest struct {
	ID     string
	Path   string
	Body   []byte
	Needle string
}

func (h *codexStage11LifecycleHarness) mustParallelHTTP(t *testing.T, requests []codexStage11HTTPRequest) {
	t.Helper()
	upstreamStart := h.upstreamEventCount()
	type result struct {
		request codexStage11HTTPRequest
		status  int
		body    []byte
		err     error
	}
	results := make(chan result, len(requests))
	launch := func(request codexStage11HTTPRequest) {
		go func() {
			status, body, err := h.doHTTP(request.ID, request.Path, request.Body)
			results <- result{request: request, status: status, body: body, err: err}
		}()
	}
	check := func(got result) {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("parallel HTTP %s status = %d, body=%s", got.request.ID, got.status, got.body)
		}
	}
	if len(requests) < 2 {
		for _, request := range requests {
			launch(request)
		}
		for range requests {
			check(<-results)
		}
	} else {
		blocked, release := h.recorder.blockNextHTTPContaining([]byte(requests[0].Needle))
		defer release()
		launch(requests[0])
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("first parallel HTTP dispatch did not reach upstream")
		}
		for _, request := range requests[1:] {
			launch(request)
		}
		for range requests[1:] {
			check(<-results)
		}
		release()
		check(<-results)
	}
	events := h.newUpstreamEvents(t, upstreamStart, len(requests))
	for index, request := range requests {
		account := codexStage11RouteAccount
		if index > 0 {
			account = codexStage11RouteAccountB
		}
		h.assertUpstreamEvent(t, events, request.ID, "http", "/responses", request.Needle, account)
	}
}

func (h *codexStage11LifecycleHarness) mustHTTP(t *testing.T, requestID, path string, body []byte, wantStatus int, needle string) {
	t.Helper()
	h.mustHTTPAccount(t, requestID, path, body, wantStatus, needle, codexStage11RouteAccount)
}

func (h *codexStage11LifecycleHarness) mustHTTPAccount(t *testing.T, requestID, path string, body []byte, wantStatus int, needle string, account codex.AccountKey) {
	t.Helper()
	upstreamStart := h.upstreamEventCount()
	status, response, err := h.doHTTP(requestID, path, body)
	if err != nil {
		t.Fatal(err)
	}
	if status != wantStatus {
		t.Fatalf("HTTP %s status = %d, want %d; body=%s", requestID, status, wantStatus, response)
	}
	upstreamPath := "/responses"
	if strings.HasSuffix(path, "/compact") {
		upstreamPath = "/responses/compact"
	}
	events := h.newUpstreamEvents(t, upstreamStart, 1)
	h.assertUpstreamEvent(t, events, requestID, "http", upstreamPath, needle, account)
}

func (h *codexStage11LifecycleHarness) doHTTP(requestID, path string, body []byte) (int, []byte, error) {
	request, err := http.NewRequest(http.MethodPost, h.proxy.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "identity")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("OpenAI-Beta", "responses=2026-08-10")
	request.Header.Set("Authorization", "Bearer "+codexStage11CallerToken)
	request.Header.Set("x-api-key", codexStage11CallerAPIKey)
	if metadata, found := codexStage11StrongMetadataHeader(body); found {
		request.Header.Set("X-Codex-Turn-Metadata", metadata)
	}
	requestHeaders := request.Header.Clone()
	response, err := h.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	h.recordCorpusInput(codexStage11CorpusInput{
		ID: requestID, Transport: "http", Path: path, Status: response.StatusCode,
		Payload: body, Responses: [][]byte{data}, Headers: codexStage11CorpusHeaderFacts(requestHeaders),
	})
	if strings.Contains(string(data), codexStage11PrivateMarker+"prompt") {
		return response.StatusCode, data, errors.New("private request prompt leaked into HTTP response")
	}
	return response.StatusCode, data, nil
}

func codexStage11StrongMetadataHeader(body []byte) (string, bool) {
	var payload struct {
		ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	metadata, found := payload.ClientMetadata["x-codex-turn-metadata"]
	if !found || len(metadata) == 0 || metadata[0] != '{' {
		return "", false
	}
	return string(metadata), true
}

type codexStage11WebSocketStep struct {
	ID       string
	Frame    []byte
	Evidence []string
	After    func(*testing.T)
}

func (h *codexStage11LifecycleHarness) mustWebSocket(t *testing.T, requestID string, frame []byte, account codex.AccountKey) {
	t.Helper()
	h.mustWebSocketSequence(t, []codexStage11WebSocketStep{{ID: requestID, Frame: frame}}, account)
}

func (h *codexStage11LifecycleHarness) mustWebSocketToolLoop(t *testing.T, requestID string, identity codexStage11TurnIdentity) {
	t.Helper()
	key := testCodexLeaseKeyFor(identity.Session, identity.Thread, identity.Turn)
	h.mustWebSocketSequence(t, []codexStage11WebSocketStep{
		{
			ID:       requestID + "-1",
			Frame:    codexStage11TurnBody(identity, CodexRequestTurn, "", "", false),
			Evidence: []string{codexStage11ResponseID, "encrypted_content", `"end_turn":false`},
			After: func(t *testing.T) {
				h.assertShadowLease(t, key, codexStage11RouteAccount, LeaseContinuationPending, true, codexStage11ResponseID)
			},
		},
		{
			ID:       requestID + "-2",
			Frame:    codexStage11ContinuationTurnBody(identity),
			Evidence: []string{codexStage11ContinuationResponseID, `"end_turn":true`},
		},
	}, codexStage11RouteAccount)
	h.assertShadowLease(t, key, codexStage11RouteAccount, LeaseBoundQuiescent, true, codexStage11ContinuationResponseID)
}

func (h *codexStage11LifecycleHarness) mustWebSocketSequence(t *testing.T, steps []codexStage11WebSocketStep, account codex.AccountKey) {
	t.Helper()
	journalBefore, err := h.fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	upstreamStart := h.upstreamEventCount()
	connectionID := h.nextWebSocketConnectionID()
	url := "ws" + strings.TrimPrefix(h.proxy.URL, "http") + "/responses"
	header := http.Header{
		"Authorization":          []string{"Bearer " + codexStage11CallerToken},
		"x-api-key":              []string{codexStage11CallerAPIKey},
		"OpenAI-Beta":            []string{"responses_websockets=2026-02-06"},
		"Sec-WebSocket-Protocol": []string{"responses"},
	}
	connection, response, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for stepIndex, step := range steps {
		if err := connection.WriteMessage(websocket.TextMessage, step.Frame); err != nil {
			t.Fatal(err)
		}
		responses := make([][]byte, 0, 2)
		for _, want := range []string{"response.created", "response.completed"} {
			messageType, message, err := connection.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if messageType != websocket.TextMessage || !bytes.Contains(message, []byte(want)) {
				t.Fatalf("WebSocket %s frame = type %d %s, want %s", step.ID, messageType, message, want)
			}
			responses = append(responses, bytes.Clone(message))
		}
		joined := bytes.Join(responses, []byte{'\n'})
		for _, evidence := range step.Evidence {
			if !bytes.Contains(joined, []byte(evidence)) {
				t.Fatalf("WebSocket %s responses lack lifecycle evidence %q: %s", step.ID, evidence, joined)
			}
		}
		h.recordCorpusInput(codexStage11CorpusInput{
			ID: step.ID, Connection: connectionID, Step: stepIndex + 1,
			Transport: "websocket", Path: "/responses", Status: http.StatusSwitchingProtocols,
			Payload: step.Frame, Responses: responses, Headers: codexStage11CorpusHeaderFacts(header),
		})
		if step.After != nil {
			step.After(t)
		}
	}
	_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	h.proxyHandlers.waitForIdle(t)
	events := h.newUpstreamEvents(t, upstreamStart, len(steps))
	for index, step := range steps {
		h.assertWebSocketUpstreamEvent(t, events[index], step, connectionID, index+1, account)
	}
	journalAfter, err := h.fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journalBefore, journalAfter) {
		var beforeEnvelope, afterEnvelope codexLeaseJournalEnvelopeV2
		_ = json.Unmarshal(journalBefore, &beforeEnvelope)
		_ = json.Unmarshal(journalAfter, &afterEnvelope)
		t.Fatalf("WebSocket observation overlapped a durable continuity journal mutation: generation %d -> %d, records %d -> %d", beforeEnvelope.Generation, afterEnvelope.Generation, len(beforeEnvelope.Records), len(afterEnvelope.Records))
	}
}

func (h *codexStage11LifecycleHarness) nextWebSocketConnectionID() string {
	h.webSocketMu.Lock()
	defer h.webSocketMu.Unlock()
	h.webSockets++
	return fmt.Sprintf("ws-%d", h.webSockets)
}

type codexStage11UpstreamEvent struct {
	Connection    string
	Step          int
	Transport     string
	Path          string
	Authorization string
	AccountID     string
	APIKey        string
	Payload       []byte
	Headers       []codexStage11CorpusHeaderFact
}

type codexStage11UpstreamRecorder struct {
	mu               sync.Mutex
	webSockets       int
	events           []codexStage11UpstreamEvent
	errors           []string
	httpBlockNeedle  []byte
	httpBlockStarted chan struct{}
	httpBlockRelease chan struct{}
	httpBlockUsed    bool
}

func (recorder *codexStage11UpstreamRecorder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if isWebSocketUpgrade(request) {
		recorder.serveWebSocket(writer, request)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
	if err != nil {
		recorder.recordError("read HTTP body")
		http.Error(writer, "read body", http.StatusBadRequest)
		return
	}
	recorder.record(codexStage11UpstreamEvent{
		Transport:     "http",
		Path:          request.URL.Path,
		Authorization: request.Header.Get("Authorization"),
		AccountID:     request.Header.Get("ChatGPT-Account-ID"),
		APIKey:        request.Header.Get("x-api-key"),
		Payload:       body,
		Headers:       codexStage11CorpusHeaderFacts(request.Header),
	})
	recorder.maybeBlockHTTP(body)
	if request.URL.Path == "/responses/compact" {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"id":%q,"output":[{"type":"compaction","encrypted_content":"opaque"}]}`, codexStage11ResponseID)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n", codexStage11ResponseID)
	_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":%q}}\n\n", codexStage11ResponseID)
}

func (recorder *codexStage11UpstreamRecorder) serveWebSocket(writer http.ResponseWriter, request *http.Request) {
	connectionID := recorder.nextWebSocketConnectionID()
	upgrader := websocket.Upgrader{
		CheckOrigin:       func(*http.Request) bool { return true },
		EnableCompression: true,
		Subprotocols:      []string{"responses"},
	}
	connection, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		recorder.recordError("upgrade WebSocket")
		return
	}
	defer connection.Close()
	for step := 1; ; step++ {
		messageType, frame, err := connection.ReadMessage()
		if err != nil {
			if step > 1 || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			recorder.recordError("read WebSocket frame")
			return
		}
		if messageType != websocket.TextMessage {
			recorder.recordError("non-text WebSocket frame")
			return
		}
		recorder.record(codexStage11UpstreamEvent{
			Connection:    connectionID,
			Step:          step,
			Transport:     "websocket",
			Path:          request.URL.Path,
			Authorization: request.Header.Get("Authorization"),
			AccountID:     request.Header.Get("ChatGPT-Account-ID"),
			APIKey:        request.Header.Get("x-api-key"),
			Payload:       frame,
			Headers:       codexStage11CorpusHeaderFacts(request.Header),
		})
		created, completed := codexStage11WebSocketResponses(frame)
		for _, responseFrame := range [][]byte{created, completed} {
			if err := connection.WriteMessage(websocket.TextMessage, responseFrame); err != nil {
				recorder.recordError("write WebSocket frame")
				return
			}
		}
	}
}

func codexStage11WebSocketResponses(frame []byte) ([]byte, []byte) {
	if bytes.Contains(frame, []byte(`"previous_response_id":"`+codexStage11ResponseID+`"`)) {
		created := []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"encrypted_content":"opaque-stage11-state"}}`, codexStage11ContinuationResponseID))
		completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"end_turn":true,"encrypted_content":"opaque-stage11-state"}}`, codexStage11ContinuationResponseID))
		return created, completed
	}
	if bytes.Contains(frame, []byte("tool_loop")) || bytes.Contains(frame, []byte("reconnect")) {
		created := []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"encrypted_content":"opaque-stage11-state"}}`, codexStage11ResponseID))
		completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"end_turn":false,"encrypted_content":"opaque-stage11-state"}}`, codexStage11ResponseID))
		return created, completed
	}
	created := []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, codexStage11ResponseID))
	completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q}}`, codexStage11ResponseID))
	return created, completed
}

func (recorder *codexStage11UpstreamRecorder) nextWebSocketConnectionID() string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.webSockets++
	return fmt.Sprintf("ws-%d", recorder.webSockets)
}

func (recorder *codexStage11UpstreamRecorder) blockNextHTTPContaining(needle []byte) (<-chan struct{}, func()) {
	recorder.mu.Lock()
	recorder.httpBlockNeedle = bytes.Clone(needle)
	recorder.httpBlockStarted = make(chan struct{})
	recorder.httpBlockRelease = make(chan struct{})
	recorder.httpBlockUsed = false
	started := recorder.httpBlockStarted
	release := recorder.httpBlockRelease
	recorder.mu.Unlock()
	var once sync.Once
	return started, func() { once.Do(func() { close(release) }) }
}

func (recorder *codexStage11UpstreamRecorder) maybeBlockHTTP(payload []byte) {
	recorder.mu.Lock()
	if recorder.httpBlockUsed || len(recorder.httpBlockNeedle) == 0 || !bytes.Contains(payload, recorder.httpBlockNeedle) {
		recorder.mu.Unlock()
		return
	}
	recorder.httpBlockUsed = true
	started := recorder.httpBlockStarted
	release := recorder.httpBlockRelease
	close(started)
	recorder.mu.Unlock()
	<-release
}

func (recorder *codexStage11UpstreamRecorder) record(event codexStage11UpstreamEvent) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	event.Payload = append([]byte(nil), event.Payload...)
	event.Headers = append([]codexStage11CorpusHeaderFact(nil), event.Headers...)
	recorder.events = append(recorder.events, event)
}

func (recorder *codexStage11UpstreamRecorder) recordError(message string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.errors = append(recorder.errors, message)
}

func (recorder *codexStage11UpstreamRecorder) snapshot() ([]codexStage11UpstreamEvent, []string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	events := append([]codexStage11UpstreamEvent(nil), recorder.events...)
	errors := append([]string(nil), recorder.errors...)
	return events, errors
}

func (h *codexStage11LifecycleHarness) upstreamEventCount() int {
	events, _ := h.recorder.snapshot()
	return len(events)
}

func (h *codexStage11LifecycleHarness) newUpstreamEvents(t *testing.T, start, want int) []codexStage11UpstreamEvent {
	t.Helper()
	events, recorderErrors := h.recorder.snapshot()
	if len(recorderErrors) != 0 {
		t.Fatalf("upstream recorder errors = %v", recorderErrors)
	}
	if got := len(events) - start; got != want {
		t.Fatalf("new upstream events = %d, want %d", got, want)
	}
	return events[start:]
}

func (h *codexStage11LifecycleHarness) assertUpstreamEvent(t *testing.T, events []codexStage11UpstreamEvent, requestID, transport, path, needle string, account codex.AccountKey) {
	t.Helper()
	var matches []codexStage11UpstreamEvent
	for _, event := range events {
		if bytes.Contains(event.Payload, []byte(needle)) {
			matches = append(matches, event)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("upstream events containing lifecycle identity for %s = %d, want 1", requestID, len(matches))
	}
	event := matches[0]
	actualAccountKey := codexStage11ActualAccountKey(event)
	if actualAccountKey != account {
		t.Fatalf("upstream AccountKey for %s = %q, want %q from auth/account headers", requestID, actualAccountKey, account)
	}
	if event.Transport != transport || event.Path != path || event.APIKey != "" {
		t.Fatalf("upstream route for %s = transport %q path %q api-key %q", requestID, event.Transport, event.Path, event.APIKey)
	}
	if event.Authorization == "Bearer "+codexStage11CallerToken || event.AccountID == codexStage11CallerToken || !bytes.Contains(event.Payload, []byte(needle)) {
		t.Fatalf("upstream evidence for %s did not contain only selected route and lifecycle identity", requestID)
	}
}

func (h *codexStage11LifecycleHarness) assertWebSocketUpstreamEvent(t *testing.T, event codexStage11UpstreamEvent, step codexStage11WebSocketStep, connection string, sequence int, account codex.AccountKey) {
	t.Helper()
	actualAccountKey := codexStage11ActualAccountKey(event)
	if actualAccountKey != account {
		t.Fatalf("upstream AccountKey for %s = %q, want %q from auth/account headers", step.ID, actualAccountKey, account)
	}
	if event.Connection != connection || event.Step != sequence || event.Transport != "websocket" || event.Path != "/responses" || event.APIKey != "" {
		t.Fatalf("upstream WebSocket route for %s = connection %q step %d transport %q path %q api-key %q", step.ID, event.Connection, event.Step, event.Transport, event.Path, event.APIKey)
	}
	if !bytes.Equal(event.Payload, step.Frame) {
		t.Fatalf("upstream WebSocket payload for %s changed: got %s want %s", step.ID, event.Payload, step.Frame)
	}
	if event.Authorization == "Bearer "+codexStage11CallerToken || event.AccountID == codexStage11CallerToken {
		t.Fatalf("upstream WebSocket evidence for %s retained caller authority", step.ID)
	}
}

func (h *codexStage11LifecycleHarness) assertShadowLease(t *testing.T, key LeaseKey, account codex.AccountKey, state LeaseState, encrypted bool, responseAnchor string) {
	t.Helper()
	lease, found := h.observer.Leases.Get(key)
	if !found {
		t.Fatalf("shared shadow lease for %#v is absent", key)
	}
	if lease.Authoritative || lease.ModeEpoch != codexStage11ModeEpoch || lease.AccountKey != account || lease.State != state || lease.HasEncryptedState != encrypted || lease.ResponseAnchor != responseAnchor {
		t.Fatalf("shared shadow lease = %#v, want account=%q state=%s encrypted=%t anchor=%q", lease, account, state, encrypted, responseAnchor)
	}
}

func codexStage11ActualAccountKey(event codexStage11UpstreamEvent) codex.AccountKey {
	if event.Authorization == "Bearer "+codexStage11UpstreamToken && event.AccountID == codexStage11UpstreamID {
		return codexStage11RouteAccount
	}
	if event.Authorization == "Bearer "+codexStage11UpstreamTokenB && event.AccountID == codexStage11UpstreamIDB {
		return codexStage11RouteAccountB
	}
	return ""
}

func (h *codexStage11LifecycleHarness) assertNoUpstream(t *testing.T, start int, requestID string) {
	t.Helper()
	if got := h.upstreamEventCount() - start; got != 0 {
		t.Fatalf("rejected request %s added %d upstream events", requestID, got)
	}
}

func (h *codexStage11LifecycleHarness) assertEvidenceAndShutdown(t *testing.T, expected codexStage11ExpectedEvidence) {
	t.Helper()
	waitForDiagnosticsEvents(t, h.diagnosticsPath, expected.httpRequests+expected.webSocketConnections)
	h.waitForDurableJournalSettlement(t, expected.journalRecords)
	h.assertEvidence(t, expected)
	if err := h.close(); err != nil {
		t.Fatalf("close Stage 11 lifecycle harness: %v", err)
	}
	h.assertProtectedAuthoritySentinels(t)
}

func (h *codexStage11LifecycleHarness) waitForDurableJournalSettlement(t *testing.T, wantRecords int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		journal, err := h.fsys.ReadFile("/state/leases.json")
		if err != nil {
			t.Fatal(err)
		}
		var envelope codexLeaseJournalEnvelopeV2
		if err := json.Unmarshal(journal, &envelope); err != nil {
			t.Fatal(err)
		}
		settled := len(envelope.Records) == wantRecords
		for _, record := range envelope.Records {
			terminal := record.State == LeaseBoundQuiescent || record.State == LeaseSuperseded
			settled = settled && terminal && record.RoutingRefs == 0 && record.AttemptRefs == 0 && record.ResponseObserverRefs == 0
		}
		if settled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable journal did not settle: %#v", envelope.Records)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *codexStage11LifecycleHarness) assertEvidence(t *testing.T, expected codexStage11ExpectedEvidence) {
	t.Helper()
	events, recorderErrors := h.recorder.snapshot()
	if len(recorderErrors) != 0 {
		t.Fatalf("upstream recorder errors = %v", recorderErrors)
	}
	if len(events) != expected.upstreamRoutes {
		t.Fatalf("upstream routes = %d, want %d", len(events), expected.upstreamRoutes)
	}
	health := h.observer.Health()
	if health.Requests != uint64(expected.observeRequests) || health.Attempts != uint64(expected.observeAttempts) || health.StrongKeys != uint64(expected.observeRequests) ||
		health.Unknown != 0 || health.ContinuityErrors != uint64(expected.observeContinuityErrors) || health.Failovers != 0 {
		t.Fatalf("observe health = %#v, want requests=%d attempts=%d continuity-errors=%d", health, expected.observeRequests, expected.observeAttempts, expected.observeContinuityErrors)
	}
	var shadowLeases []CodexTurnLease
	for _, lease := range h.observer.Leases.Snapshot() {
		if !lease.Authoritative {
			shadowLeases = append(shadowLeases, lease)
		}
	}
	if len(shadowLeases) != expected.shadowLeases {
		t.Fatalf("shared shadow leases = %d, want %d: %#v", len(shadowLeases), expected.shadowLeases, shadowLeases)
	}
	if expected.shadowLeases == 1 {
		lease := shadowLeases[0]
		if lease.AccountKey != expected.shadowAccountKey || lease.State != expected.shadowState || lease.HasEncryptedState != expected.shadowHasEncryptedState || (lease.ResponseAnchor != "") != expected.shadowHasResponseAnchor {
			t.Fatalf("shared shadow lease = %#v, want account=%q state=%s encrypted=%t anchored=%t", lease, expected.shadowAccountKey, expected.shadowState, expected.shadowHasEncryptedState, expected.shadowHasResponseAnchor)
		}
	}

	diagnostics, err := os.ReadFile(h.diagnosticsPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := h.fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{
		codexStage11PrivateMarker,
		string(codexStage11RouteAccount),
		string(codexStage11RouteAccountB),
		string(codexStage11Candidate),
		string(codexStage11CandidateB),
		string(codexStage11Revision),
		string(codexStage11RevisionB),
		codexStage11ResponseID,
		codexStage11ContinuationResponseID,
		codexStage11CallerAPIKey,
	} {
		if bytes.Contains(diagnostics, []byte(private)) {
			t.Fatalf("diagnostics leaked %q", private)
		}
		if bytes.Contains(journal, []byte(private)) {
			t.Fatalf("lease journal leaked %q", private)
		}
	}
	var envelope codexLeaseJournalEnvelopeV2
	if err := json.Unmarshal(journal, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Records) != expected.journalRecords || len(envelope.Lanes) != expected.journalLanes {
		t.Fatalf("journal records/lanes = %d/%d, want %d/%d", len(envelope.Records), len(envelope.Lanes), expected.journalRecords, expected.journalLanes)
	}
	h.assertDurableJournalSemantics(t, envelope, expected)
	lines := bytes.Split(bytes.TrimSpace(diagnostics), []byte("\n"))
	if len(lines) != expected.httpRequests+expected.webSocketConnections {
		t.Fatalf("diagnostic route events = %d, want %d", len(lines), expected.httpRequests+expected.webSocketConnections)
	}
}

func (h *codexStage11LifecycleHarness) assertDurableJournalSemantics(t *testing.T, envelope codexLeaseJournalEnvelopeV2, expected codexStage11ExpectedEvidence) {
	t.Helper()
	var authoritative, shadow, admitted, quiescent, superseded, predecessors int
	for _, record := range envelope.Records {
		if record.Authoritative {
			authoritative++
		} else {
			shadow++
		}
		switch record.State {
		case LeaseBoundQuiescent:
			quiescent++
		case LeaseSuperseded:
			superseded++
		default:
			t.Fatalf("durable record state = %s, want terminal lifecycle evidence", record.State)
		}
		if record.ModeEpoch != codexStage11ModeEpoch || record.ProtocolSchema != CurrentCodexLeaseSchema {
			t.Fatalf("durable record epoch/schema = %d/%d, want %d/%d", record.ModeEpoch, record.ProtocolSchema, codexStage11ModeEpoch, CurrentCodexLeaseSchema)
		}
		if record.SessionHash == "" || record.ThreadHash == "" || record.NamespaceHash == "" || record.TurnHash == "" || record.AccountHash == "" {
			t.Fatalf("durable record lacks hashed route identity: %#v", record)
		}
		if record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || codexLeaseCurrentAttemptState(record) != CodexAttemptProviderCompleted {
			t.Fatalf("durable record did not settle after lifecycle completion: %#v", record)
		}
		if !record.EverAdmitted || record.AdmissionJournalGeneration == 0 || record.AdmissionJournalGeneration > envelope.Generation || record.AdmissionRequestGeneration == 0 || record.AdmissionRequestGeneration > record.Generation || record.AdmittedAt.IsZero() || record.AdmissionRequestKind != record.RequestKind {
			t.Fatalf("durable record lacks coherent admission evidence: %#v", record)
		}
		admitted++
		if !record.HasResponseAnchor || record.CorrelationHash == "" {
			t.Fatalf("durable record lacks its accepted response anchor: %#v", record)
		}

		hasPredecessor := record.PredecessorTurnHash != "" || record.PredecessorModeEpoch != 0 || record.PredecessorAuthoritative || record.PredecessorGeneration != 0
		if !hasPredecessor {
			continue
		}
		if record.PredecessorTurnHash == "" || record.PredecessorModeEpoch == 0 || record.PredecessorGeneration == 0 {
			t.Fatalf("durable predecessor tuple is incomplete: %#v", record)
		}
		predecessors++
		matched := false
		for _, predecessor := range envelope.Records {
			if predecessor.SessionHash == record.SessionHash && predecessor.ThreadHash == record.ThreadHash && predecessor.NamespaceHash == record.NamespaceHash &&
				predecessor.TurnHash == record.PredecessorTurnHash && predecessor.ModeEpoch == record.PredecessorModeEpoch && predecessor.Authoritative == record.PredecessorAuthoritative &&
				predecessor.RecordGeneration == record.PredecessorGeneration && predecessor.State == LeaseSuperseded {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("durable predecessor tuple has no retained superseded target: %#v", record)
		}
	}

	for _, lane := range envelope.Lanes {
		hasAuthoritativeAdmission := false
		for _, record := range envelope.Records {
			if record.SessionHash == lane.SessionHash && record.ThreadHash == lane.ThreadHash && record.NamespaceHash == lane.NamespaceHash && record.Authoritative && record.EverAdmitted {
				hasAuthoritativeAdmission = true
				break
			}
		}
		if !hasAuthoritativeAdmission {
			if !codexLaneAffinityIsZero(lane) {
				t.Fatalf("shadow-only durable lane exposed authoritative admission affinity: %#v", lane)
			}
			continue
		}
		matched := false
		for _, record := range envelope.Records {
			if record.SessionHash == lane.SessionHash && record.ThreadHash == lane.ThreadHash && record.NamespaceHash == lane.NamespaceHash &&
				record.TurnHash == lane.LastAdmittedTurnHash && record.ModeEpoch == lane.LastAdmittedModeEpoch && record.Authoritative == lane.LastAdmittedAuthoritative &&
				record.AccountHash == lane.LastAdmittedAccountHash && record.AdmissionJournalGeneration == lane.LastAdmissionJournalGeneration && record.AdmittedAt.Equal(lane.LastAdmittedAt) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("durable lane lacks coherent latest admission evidence: %#v", lane)
		}
	}

	wantAuthoritative := expected.journalRecords - expected.journalShadowRecords
	wantQuiescent := expected.journalRecords - expected.journalSupersededRecords
	if authoritative != wantAuthoritative || shadow != expected.journalShadowRecords || admitted != expected.journalRecords || quiescent != wantQuiescent || superseded != expected.journalSupersededRecords || predecessors != expected.journalPredecessorRecords {
		t.Fatalf("durable authority/lifecycle counts = authoritative %d shadow %d admitted %d quiescent %d superseded %d predecessors %d; want %d/%d/%d/%d/%d/%d", authoritative, shadow, admitted, quiescent, superseded, predecessors, wantAuthoritative, expected.journalShadowRecords, expected.journalRecords, wantQuiescent, expected.journalSupersededRecords, expected.journalPredecessorRecords)
	}
}

func (h *codexStage11LifecycleHarness) assertProtectedAuthoritySentinels(t *testing.T) {
	t.Helper()
	for path, before := range h.sentinels {
		after, err := h.fsys.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("protected authority sentinel changed: %s", path)
		}
	}
}
