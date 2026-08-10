package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexStage12AProductionVertical(t *testing.T) {
	const (
		accountA       codex.AccountKey = "account-a"
		accountB       codex.AccountKey = "account-b"
		defaultAccount codex.AccountKey = "account-default"
	)

	coordinator, journalFS, now := openCodexLeaseRuntimeTestCoordinator(t)
	runtime := newCodexLeaseRuntimeTest(t, coordinator)
	expires := now.Add(time.Hour)
	accounts := []codex.LogicalAccount{
		frozenDispatchTestLogicalAccount(defaultAccount, frozenDispatchCandidate(defaultAccount, "candidate-default", "revision-default", codex.SourceExternal, false, expires)),
		frozenDispatchTestLogicalAccount(accountB, frozenDispatchCandidate(accountB, "candidate-b", "revision-b", codex.SourceExternal, false, expires)),
		frozenDispatchTestLogicalAccount(accountA, frozenDispatchCandidate(accountA, "candidate-a", "revision-a", codex.SourceExternal, false, expires)),
	}
	accounts[2].Active = true
	inventory := &stage12AInventory{inventory: codex.Inventory{Accounts: accounts}}

	ledger := NewCodexCapacityLedger(func() time.Time { return *now }, time.Hour)
	frozenDispatchObserveCapacity(t, ledger, accountA, CapacityBucketBase, 90, *now)
	frozenDispatchObserveCapacity(t, ledger, accountB, CapacityBucketBase, 80, *now)
	frozenDispatchObserveCapacity(t, ledger, defaultAccount, CapacityBucketBase, 0, *now)

	var preparationMu sync.Mutex
	var preparationEvents []string
	var inspections []*CodexFrozenRequestInspection
	var frozenRequests []*CodexFrozenRequest
	var transformedBodies [][]byte
	headroomCalls := 0
	encodeCalls := 0
	freezeCalls := 0
	headroom := CodexRequestHeadroomFunc(func(_ context.Context, body []byte, mode HeadroomMode) ([]byte, int, error) {
		preparationMu.Lock()
		defer preparationMu.Unlock()
		if mode != HeadroomModeCache {
			t.Fatalf("Headroom mode = %q, want cache", mode)
		}
		headroomCalls++
		preparationEvents = append(preparationEvents, "headroom")
		transformed := append(bytes.Clone(body), '\n')
		transformedBodies = append(transformedBodies, bytes.Clone(transformed))
		return transformed, 1, nil
	})
	factory := &CodexHTTPRequestPlanFactory{
		Inventory:         inventory,
		Capacity:          ledger,
		Routes:            coordinator,
		Runtime:           runtime,
		DefaultAccountKey: defaultAccount,
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 9, Authoritative: true},
		Headroom:          headroom,
		HeadroomMode:      HeadroomModeCache,
		Now:               func() time.Time { return *now },
	}
	factory.operations.inspect = func(ctx context.Context, encoded []byte, headers http.Header) (*CodexFrozenRequestInspection, error) {
		inspection, err := InspectCodexNativeRequest(ctx, encoded, headers)
		if inspection != nil {
			inspection.encodeRequest = func(body []byte, contentEncoding string, limits CodexZstdLimits) ([]byte, error) {
				preparationMu.Lock()
				encodeCalls++
				preparationEvents = append(preparationEvents, "encode")
				preparationMu.Unlock()
				return EncodeCodexRequest(body, contentEncoding, limits)
			}
			preparationMu.Lock()
			inspections = append(inspections, inspection)
			preparationMu.Unlock()
		}
		return inspection, err
	}
	factory.operations.freeze = func(ctx context.Context, inspection *CodexFrozenRequestInspection, choice RouteChoice, headroom CodexRequestHeadroom, mode HeadroomMode) (*CodexFrozenRequest, error) {
		preparationMu.Lock()
		freezeCalls++
		preparationEvents = append(preparationEvents, "freeze:start")
		preparationMu.Unlock()
		frozen, err := inspection.Freeze(ctx, choice, headroom, mode)
		preparationMu.Lock()
		preparationEvents = append(preparationEvents, "freeze:done")
		if frozen != nil {
			frozenRequests = append(frozenRequests, frozen)
		}
		preparationMu.Unlock()
		return frozen, err
	}

	transport := &stage12ATransport{
		accountsByAuthorization: map[string]codex.AccountKey{
			"Bearer stage12a-private-token-a":       accountA,
			"Bearer stage12a-private-token-b":       accountB,
			"Bearer stage12a-private-token-default": defaultAccount,
		},
		outcomes: map[codex.AccountKey][]stage12AOutcome{
			accountA: {
				stage12AAcceptedOutcome("stage12a-private-response-a", "stage12a-private-turn-state-a"),
				stage12AHard429Outcome(),
			},
			accountB:       {stage12AHard429Outcome()},
			defaultAccount: {stage12AAcceptedOutcome("stage12a-private-response-default", "stage12a-private-turn-state-default")},
		},
	}
	resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
		"revision-a":       testExactCredentialMaterial(accounts[2].Identity, "stage12a-private-token-a"),
		"revision-b":       testExactCredentialMaterial(accounts[1].Identity, "stage12a-private-token-b"),
		"revision-default": testExactCredentialMaterial(accounts[0].Identity, "stage12a-private-token-default"),
	}}
	executor := &CodexAttemptExecutor{
		Inventory: inventory,
		Secrets:   resolver,
		Transport: &CodexTokenTransport{Inner: transport},
	}
	handler, err := NewCodexNativeHTTPHandler(
		factory,
		&CodexHTTPRequestSession{Executor: executor, Capacity: ledger},
		"https://codex.example/private-upstream",
	)
	if err != nil {
		t.Fatal(err)
	}

	tempRoot := t.TempDir()
	processTemp := filepath.Join(tempRoot, "process-temp")
	if err := os.MkdirAll(processTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", processTemp)
	diagnosticsPath := filepath.Join(tempRoot, "diagnostics", "proxy.jsonl")
	if err := os.MkdirAll(filepath.Dir(diagnosticsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := OpenDiagnosticsWriter(diagnosticsPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = diagnostics.Close() })
	config := &Config{
		Port:                          DefaultPort,
		ClaudeUpstream:                "https://claude.example",
		CodexUpstream:                 "https://codex.example/private-upstream",
		LocalToken:                    "opaque-local-proxy-token",
		CodexTurnRouting:              CodexRoutingEnforce,
		CodexWSTurnRouting:            CodexRoutingOff,
		CodexRoutingDefaultAccountKey: defaultAccount,
		CodexLeaseRetentionDays:       7,
	}
	configPath := filepath.Join(tempRoot, "config", "proxy.json")
	if err := saveConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Config:          config,
		CodexNativeHTTP: handler,
		Diag:            diagnostics,
		CodexHealth: func() CodexHealth {
			return CodexHealth{
				AccountCount:      3,
				AccountCountKnown: true,
				HealthCode:        "ok",
				RoutingDefault: CodexRoutingDefaultHealth{
					Configured: true,
					Resolved:   true,
					Routable:   true,
					Status:     CodexRoutingDefaultStatusResolved,
				},
			}
		},
		CodexRouting: &CodexRoutingRuntime{
			HTTP: CodexModeStatus{
				Configured:         CodexRoutingEnforce,
				Effective:          CodexRoutingEnforce,
				ModeEpoch:          9,
				AuthoritativeEpoch: 9,
			},
			WebSocket: CodexModeStatus{Configured: CodexRoutingOff, Effective: CodexRoutingOff},
		},
	}

	firstBody := stage12ARequestBody(t, "stage12a-private-session-one", "stage12a-private-thread-one", "stage12a-private-turn-one", "stage12a-private-prompt-one")
	secondBody := stage12ARequestBody(t, "stage12a-private-session-two", "stage12a-private-thread-two", "stage12a-private-turn-two", "stage12a-private-prompt-two")
	firstInput := encodeCodexZstd(t, firstBody)
	secondInput := encodeCodexZstd(t, secondBody)
	requestBodies := []*stage12AOwnedBody{
		newStage12AOwnedBody(firstInput),
		newStage12AOwnedBody(secondInput),
	}

	firstResponse := stage12AServe(t, server, requestBodies[0], "stage12a-private-session-header-one")
	if firstResponse.Code != http.StatusOK || !strings.Contains(firstResponse.Body.String(), "stage12a-private-response-a") {
		t.Fatalf("first response = %d/%q", firstResponse.Code, firstResponse.Body.String())
	}

	inventory.setActive(accountB, defaultAccount)
	secondResponse := stage12AServe(t, server, requestBodies[1], "stage12a-private-session-header-two")
	if secondResponse.Code != http.StatusOK || !strings.Contains(secondResponse.Body.String(), "stage12a-private-response-default") {
		t.Fatalf("second response = %d/%q", secondResponse.Code, secondResponse.Body.String())
	}

	if got, want := inventory.activeSnapshots(), [][]codex.AccountKey{{accountA}, {defaultAccount, accountB}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory Active snapshots = %v, want %v", got, want)
	}
	attempts, responseBodies := transport.snapshot()
	if got, want := stage12AAttemptAccounts(attempts), []codex.AccountKey{accountA, accountA, accountB, defaultAccount}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attempt accounts = %v, want %v", got, want)
	}
	if got := ledger.Capacity(accountA, CapacityBucketBase); got.State != CapacityZero || got.Source != CapacitySourceHardLimit {
		t.Fatalf("account A hard-429 capacity = %#v", got)
	}
	if got := ledger.Capacity(accountB, CapacityBucketBase); got.State != CapacityZero || got.Source != CapacitySourceHardLimit {
		t.Fatalf("account B hard-429 capacity = %#v", got)
	}
	if got := ledger.Capacity(defaultAccount, CapacityBucketBase); got.State != CapacityZero || got.Source != CapacitySourceLiveRateLimits {
		t.Fatalf("terminal default capacity = %#v", got)
	}

	preparationMu.Lock()
	gotHeadroomCalls := headroomCalls
	gotEncodeCalls := encodeCalls
	gotFreezeCalls := freezeCalls
	gotPreparationEvents := append([]string(nil), preparationEvents...)
	gotTransformedBodies := cloneStage12ABodies(transformedBodies)
	gotInspections := append([]*CodexFrozenRequestInspection(nil), inspections...)
	gotFrozenRequests := append([]*CodexFrozenRequest(nil), frozenRequests...)
	preparationMu.Unlock()
	if gotHeadroomCalls != 2 || gotEncodeCalls != 2 || gotFreezeCalls != 2 {
		t.Fatalf("Headroom/encode/freeze calls = %d/%d/%d, want 2/2/2", gotHeadroomCalls, gotEncodeCalls, gotFreezeCalls)
	}
	if want := []string{"freeze:start", "headroom", "encode", "freeze:done", "freeze:start", "headroom", "encode", "freeze:done"}; !reflect.DeepEqual(gotPreparationEvents, want) {
		t.Fatalf("preparation events = %v, want %v", gotPreparationEvents, want)
	}
	if len(gotTransformedBodies) != 2 {
		t.Fatalf("transformed bodies = %d, want 2", len(gotTransformedBodies))
	}
	wantSecondEncoded, err := EncodeCodexRequest(gotTransformedBodies[1], "zstd", codexTransportRewriteLimits())
	if err != nil {
		t.Fatal(err)
	}
	for index := range attempts {
		if !attempts[index].bodyClosed {
			t.Fatalf("attempt %d request body was not closed", index)
		}
	}
	for index := 1; index < len(attempts); index++ {
		if !bytes.Equal(attempts[index].body, wantSecondEncoded) {
			t.Fatalf("second request attempt %d body changed", index)
		}
	}
	if bytes.Equal(secondInput, wantSecondEncoded) {
		t.Fatal("Headroom transform did not force the single pre-freeze zstd encode")
	}
	wantSemanticHeaders := http.Header{
		"Accept":           {"text/event-stream"},
		"Content-Encoding": {"zstd"},
		"Content-Type":     {"application/json"},
		"Openai-Beta":      {"stage12a-private-semantic-header"},
		"User-Agent":       {"stage12a-client"},
	}
	for index := 1; index < len(attempts); index++ {
		if got := codexReplayHeaders(attempts[index].header); !reflect.DeepEqual(got, wantSemanticHeaders) {
			t.Fatalf("second request attempt %d semantic headers = %#v, want %#v", index, got, wantSemanticHeaders)
		}
		if strings.Contains(attempts[index].header.Get("Authorization"), "stage12a-private-downstream-token") || attempts[index].header.Get("Cookie") != "" || attempts[index].header.Get("Session_id") != "" {
			t.Fatalf("second request attempt %d retained downstream-only headers", index)
		}
	}
	if attempts[1].header.Get("Authorization") == attempts[2].header.Get("Authorization") || attempts[2].header.Get("Authorization") == attempts[3].header.Get("Authorization") {
		t.Fatal("exact attempt credentials were not independently injected")
	}

	for index, body := range requestBodies {
		if got := body.closeCount(); got != 1 {
			t.Fatalf("downstream request body %d closes = %d, want 1", index, got)
		}
	}
	for index, body := range responseBodies {
		if got := body.closeCount(); got != 1 {
			t.Fatalf("upstream response body %d closes = %d, want 1", index, got)
		}
	}
	for index, inspection := range gotInspections {
		if _, err := inspection.Protocol(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
			t.Fatalf("inspection %d retained request authority: %v", index, err)
		}
	}
	for index, frozen := range gotFrozenRequests {
		if _, err := frozen.Replay(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) {
			t.Fatalf("frozen request %d retained replay ownership: %v", index, err)
		}
		if _, err := frozen.Protocol(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
			t.Fatalf("frozen request %d retained protocol authority: %v", index, err)
		}
	}
	journalEnvelope := readCodexLeaseV2CASTestEnvelope(t, journalFS)
	if len(journalEnvelope.Records) != 2 {
		t.Fatalf("journal records = %d, want 2", len(journalEnvelope.Records))
	}
	for index, record := range journalEnvelope.Records {
		if record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct || record.State != LeaseBoundQuiescent {
			t.Fatalf("journal record %d retained ownership: state=%s refs=%d/%d/%d extinct=%t", index, record.State, record.RoutingRefs, record.AttemptRefs, record.ResponseObserverRefs, record.SocketLineageExtinct)
		}
	}

	healthResponse := httptest.NewRecorder()
	server.handleHealth(healthResponse, httptest.NewRequest(http.MethodGet, "http://localhost/health", nil))
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("health response = %d/%q", healthResponse.Code, healthResponse.Body.String())
	}
	malformedBody := newStage12AOwnedBody([]byte(`{"stage12a-private-malformed-body"`))
	requestBodies = append(requestBodies, malformedBody)
	malformedRequest := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", nil)
	malformedRequest.Body = malformedBody
	malformedRequest.Header.Set("Content-Type", "application/json")
	malformedResponse := httptest.NewRecorder()
	server.handleNativeCodex(malformedResponse, malformedRequest)
	if malformedResponse.Code != http.StatusBadRequest || strings.Contains(malformedResponse.Body.String(), "stage12a-private-malformed-body") {
		t.Fatalf("malformed response = %d/%q", malformedResponse.Code, malformedResponse.Body.String())
	}
	if got := malformedBody.closeCount(); got != 1 {
		t.Fatalf("malformed request body closes = %d, want 1", got)
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}

	privacyOutputs := map[string][]byte{
		"journal":     fsysFileBytes(t, journalFS, "/state/leases.json"),
		"config":      stage12AReadFile(t, configPath),
		"diagnostics": stage12AReadFile(t, diagnosticsPath),
		"health":      bytes.Clone(healthResponse.Body.Bytes()),
		"error":       bytes.Clone(malformedResponse.Body.Bytes()),
		"temp":        stage12ATempFiles(t, tempRoot),
	}
	rawFixtures := []string{
		"stage12a-private-session-one", "stage12a-private-session-two",
		"stage12a-private-thread-one", "stage12a-private-thread-two",
		"stage12a-private-turn-one", "stage12a-private-turn-two",
		"stage12a-private-prompt-one", "stage12a-private-prompt-two",
		"stage12a-private-session-header-one", "stage12a-private-session-header-two",
		"stage12a-private-downstream-token", "stage12a-private-cookie",
		"stage12a-private-semantic-header", "stage12a-private-malformed-body",
		"stage12a-private-token-a", "stage12a-private-token-b", "stage12a-private-token-default",
		"stage12a-private-response-a", "stage12a-private-response-default",
		"stage12a-private-turn-state-a", "stage12a-private-turn-state-default",
		"private-account-account-a", "private-account-account-b", "private-account-account-default",
		"private-user-account-a", "private-user-account-b", "private-user-account-default",
		"private-email-account-a@test.invalid", "private-email-account-b@test.invalid", "private-email-account-default@test.invalid",
		"candidate-a", "candidate-b", "candidate-default", "revision-a", "revision-b", "revision-default",
		string(accountA), string(accountB),
	}
	for outputName, output := range privacyOutputs {
		for _, fixture := range rawFixtures {
			if bytes.Contains(output, []byte(fixture)) {
				t.Fatalf("%s output exposed raw fixture %q", outputName, fixture)
			}
		}
	}

	preparationMu.Lock()
	defer preparationMu.Unlock()
	if headroomCalls != 2 || encodeCalls != 2 || freezeCalls != 2 || len(frozenRequests) != 2 {
		t.Fatalf("malformed request crossed preparation boundary: Headroom/encode/freeze/frozen = %d/%d/%d/%d", headroomCalls, encodeCalls, freezeCalls, len(frozenRequests))
	}
}

type stage12AInventory struct {
	mu        sync.Mutex
	inventory codex.Inventory
	snapshots [][]codex.AccountKey
}

func (inventory *stage12AInventory) List(ctx context.Context) (codex.Inventory, error) {
	if err := ctx.Err(); err != nil {
		return codex.Inventory{}, err
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	result := inventory.inventory
	result.Accounts = append([]codex.LogicalAccount(nil), inventory.inventory.Accounts...)
	var active []codex.AccountKey
	for index := range result.Accounts {
		result.Accounts[index].Candidates = append([]codex.CredentialCandidate(nil), result.Accounts[index].Candidates...)
		if result.Accounts[index].Active {
			active = append(active, result.Accounts[index].Key)
		}
	}
	inventory.snapshots = append(inventory.snapshots, active)
	return result, nil
}

func (inventory *stage12AInventory) setActive(active ...codex.AccountKey) {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	selected := make(map[codex.AccountKey]bool, len(active))
	for _, account := range active {
		selected[account] = true
	}
	for index := range inventory.inventory.Accounts {
		inventory.inventory.Accounts[index].Active = selected[inventory.inventory.Accounts[index].Key]
	}
}

func (inventory *stage12AInventory) activeSnapshots() [][]codex.AccountKey {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	result := make([][]codex.AccountKey, len(inventory.snapshots))
	for index := range inventory.snapshots {
		result[index] = append([]codex.AccountKey(nil), inventory.snapshots[index]...)
	}
	return result
}

type stage12AOutcome struct {
	status int
	header http.Header
	body   string
}

func stage12AAcceptedOutcome(responseID, turnState string) stage12AOutcome {
	return stage12AOutcome{
		status: http.StatusOK,
		header: http.Header{
			"Content-Type":       {"text/event-stream"},
			"X-Codex-Turn-State": {turnState},
		},
		body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + responseID + "\"}}\n\n",
	}
}

func stage12AHard429Outcome() stage12AOutcome {
	return stage12AOutcome{
		status: http.StatusTooManyRequests,
		header: http.Header{"Content-Type": {"application/json"}},
		body:   codexLiveUsageLimitBody,
	}
}

type stage12AAttempt struct {
	account    codex.AccountKey
	body       []byte
	header     http.Header
	bodyClosed bool
}

type stage12ATransport struct {
	mu                      sync.Mutex
	accountsByAuthorization map[string]codex.AccountKey
	outcomes                map[codex.AccountKey][]stage12AOutcome
	attempts                []stage12AAttempt
	responseBodies          []*stage12AOwnedBody
}

func (transport *stage12ATransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.Body == nil {
		return nil, errors.New("Stage12A transport request unavailable")
	}
	body, readErr := io.ReadAll(request.Body)
	closeErr := request.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	account := transport.accountsByAuthorization[request.Header.Get("Authorization")]
	if account == "" {
		return nil, errors.New("Stage12A transport received unknown credential")
	}
	queue := transport.outcomes[account]
	if len(queue) == 0 {
		return nil, errors.New("Stage12A transport outcome unavailable")
	}
	outcome := queue[0]
	transport.outcomes[account] = queue[1:]
	transport.attempts = append(transport.attempts, stage12AAttempt{
		account:    account,
		body:       bytes.Clone(body),
		header:     request.Header.Clone(),
		bodyClosed: true,
	})
	responseBody := newStage12AOwnedBody([]byte(outcome.body))
	transport.responseBodies = append(transport.responseBodies, responseBody)
	return &http.Response{
		StatusCode: outcome.status,
		Header:     outcome.header.Clone(),
		Body:       responseBody,
		Request:    request,
	}, nil
}

func (transport *stage12ATransport) snapshot() ([]stage12AAttempt, []*stage12AOwnedBody) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	attempts := make([]stage12AAttempt, len(transport.attempts))
	for index, attempt := range transport.attempts {
		attempt.body = bytes.Clone(attempt.body)
		attempt.header = attempt.header.Clone()
		attempts[index] = attempt
	}
	return attempts, append([]*stage12AOwnedBody(nil), transport.responseBodies...)
}

type stage12AOwnedBody struct {
	mu     sync.Mutex
	reader *bytes.Reader
	closes int
}

func newStage12AOwnedBody(body []byte) *stage12AOwnedBody {
	return &stage12AOwnedBody{reader: bytes.NewReader(bytes.Clone(body))}
}

func (body *stage12AOwnedBody) Read(destination []byte) (int, error) {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.reader.Read(destination)
}

func (body *stage12AOwnedBody) Close() error {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.closes++
	return nil
}

func (body *stage12AOwnedBody) closeCount() int {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closes
}

func stage12ARequestBody(t *testing.T, sessionID, threadID, turnID, prompt string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type":  "response.create",
		"model": "gpt-5",
		"client_metadata": map[string]any{
			"x-codex-turn-metadata": map[string]any{
				"session_id":   sessionID,
				"thread_id":    threadID,
				"turn_id":      turnID,
				"request_kind": "turn",
			},
		},
		"input": []any{map[string]any{"role": "user", "content": prompt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func stage12AServe(t *testing.T, server *Server, body *stage12AOwnedBody, sessionHeader string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses?include=usage", nil)
	request.Body = body
	request.Header = http.Header{
		"Accept":           {"text/event-stream"},
		"Authorization":    {"Bearer stage12a-private-downstream-token"},
		"Content-Encoding": {"zstd"},
		"Content-Type":     {"application/json"},
		"Cookie":           {"stage12a-private-cookie"},
		"Openai-Beta":      {"stage12a-private-semantic-header"},
		"Session_id":       {sessionHeader},
		"User-Agent":       {"stage12a-client"},
	}
	response := httptest.NewRecorder()
	server.handleNativeCodex(response, request)
	return response
}

func stage12AAttemptAccounts(attempts []stage12AAttempt) []codex.AccountKey {
	accounts := make([]codex.AccountKey, len(attempts))
	for index := range attempts {
		accounts[index] = attempts[index].account
	}
	return accounts
}

func cloneStage12ABodies(bodies [][]byte) [][]byte {
	cloned := make([][]byte, len(bodies))
	for index := range bodies {
		cloned[index] = bytes.Clone(bodies[index])
	}
	return cloned
}

func stage12AReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func stage12ATempFiles(t *testing.T, root string) []byte {
	t.Helper()
	var contents []byte
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			contents = append(contents, relative...)
			contents = append(contents, '\n')
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			contents = append(contents, data...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
