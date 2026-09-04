package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type codexRuntimeRoutingHandlerFunc func(http.ResponseWriter, *http.Request, bool) (bool, string)

func (handler codexRuntimeRoutingHandlerFunc) TryServe(writer http.ResponseWriter, request *http.Request, compact bool) (bool, string) {
	return handler(writer, request, compact)
}

func TestCodexRuntimeObservabilityRecordsFixedDecisions(t *testing.T) {
	before := codexProcessRuntimeObservability.snapshot()
	tests := []struct {
		decision codexRuntimeDecision
		want     string
	}{
		{decision: codexRuntimeDecisionAffinityReuse, want: "affinity_reuse"},
		{decision: codexRuntimeDecisionFairnessSelect, want: "fairness_select"},
		{decision: codexRuntimeDecisionTerminalDefault, want: "terminal_default"},
	}
	for _, test := range tests {
		ctx, diagnostics := withRouteDiagnostics(context.Background())
		codexProcessRuntimeObservability.recordDecision(ctx, test.decision)
		var event RouteEvent
		event.applyRouteDiagnostics(diagnostics)
		if event.Decision != test.want {
			t.Fatalf("diagnostic decision = %q, want %q", event.Decision, test.want)
		}
	}
	after := codexProcessRuntimeObservability.snapshot()
	if got := after.AffinityReuse - before.AffinityReuse; got != 1 {
		t.Fatalf("affinity decision delta = %d, want 1", got)
	}
	if got := after.FairnessSelect - before.FairnessSelect; got != 1 {
		t.Fatalf("fairness decision delta = %d, want 1", got)
	}
	if got := after.TerminalDefault - before.TerminalDefault; got != 1 {
		t.Fatalf("terminal-default decision delta = %d, want 1", got)
	}
}

func TestCodexFrozenDispatchPlanDerivesRuntimeDecisionRoles(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	terminalCapacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	frozenDispatchObserveCapacity(t, terminalCapacity, "account-a", CapacityBucketBase, 50, now)
	frozenDispatchObserveCapacity(t, terminalCapacity, "account-z", CapacityBucketBase, 0, now)
	accountA := frozenDispatchTestLogicalAccount("account-a",
		frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	accountZ := frozenDispatchTestLogicalAccount("account-z",
		frozenDispatchCandidate("account-z", "candidate-z", "revision-z", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{accountZ, accountA}}

	tests := []struct {
		name  string
		input CodexFrozenDispatchInput
		want  []codexRuntimeDecision
	}{
		{
			name: "affinity and ordinary configured default",
			input: CodexFrozenDispatchInput{
				Inventory: inventory, Requirements: CodexRouteRequirements{RequestedModel: "gpt-5"},
				AffinityAccountKey: "account-a", DefaultAccountKey: "account-z", Now: now,
			},
			want: []codexRuntimeDecision{codexRuntimeDecisionAffinityReuse, codexRuntimeDecisionFairnessSelect},
		},
		{
			name: "configured default selected by affinity",
			input: CodexFrozenDispatchInput{
				Inventory: inventory, Requirements: CodexRouteRequirements{RequestedModel: "gpt-5"},
				AffinityAccountKey: "account-z", DefaultAccountKey: "account-z", Now: now,
			},
			want: []codexRuntimeDecision{codexRuntimeDecisionAffinityReuse, codexRuntimeDecisionFairnessSelect},
		},
		{
			name: "fairness includes ordinary configured default",
			input: CodexFrozenDispatchInput{
				Inventory: inventory, Requirements: CodexRouteRequirements{RequestedModel: "gpt-5"},
				DefaultAccountKey: "account-z", Now: now,
			},
			want: []codexRuntimeDecision{codexRuntimeDecisionFairnessSelect, codexRuntimeDecisionFairnessSelect},
		},
		{
			name: "known-zero default is terminal",
			input: CodexFrozenDispatchInput{
				Inventory: inventory, Capacity: terminalCapacity,
				Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
				DefaultAccountKey: "account-z", Now: now,
			},
			want: []codexRuntimeDecision{codexRuntimeDecisionFairnessSelect, codexRuntimeDecisionTerminalDefault},
		},
		{
			name: "bound route is not a new selection decision",
			input: CodexFrozenDispatchInput{
				Inventory: inventory, Requirements: CodexRouteRequirements{RequestedModel: "gpt-5"},
				BoundAccountKey: "account-z", DefaultAccountKey: "account-z", Now: now,
			},
			want: []codexRuntimeDecision{codexRuntimeDecisionNone},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := BuildCodexFrozenDispatchPlan(context.Background(), test.input)
			if err != nil {
				t.Fatalf("BuildCodexFrozenDispatchPlan: %v", err)
			}
			accounts := plan.Accounts()
			if len(accounts) != len(test.want) {
				t.Fatalf("accounts = %d, want %d", len(accounts), len(test.want))
			}
			for index := range accounts {
				if accounts[index].decision != test.want[index] {
					t.Fatalf("decision[%d] = %q, want %q", index, accounts[index].decision.name(), test.want[index].name())
				}
			}
		})
	}
}

func TestCodexHTTPRequestSessionCountsActualAccountDispatchOnceAcrossCandidateRetries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	accountA := frozenDispatchTestLogicalAccount("account-a",
		frozenDispatchCandidate("account-a", "candidate-a1", "revision-a1", codex.SourceSystem, false, now.Add(2*time.Hour)),
		frozenDispatchCandidate("account-a", "candidate-a2", "revision-a2", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	accountZ := frozenDispatchTestLogicalAccount("account-z",
		frozenDispatchCandidate("account-z", "candidate-z", "revision-z", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory:          codex.Inventory{Accounts: []codex.LogicalAccount{accountZ, accountA}},
		Requirements:       CodexRouteRequirements{RequestedModel: "gpt-5"},
		AffinityAccountKey: "account-a", DefaultAccountKey: "account-z", Now: now,
	})
	if err != nil {
		t.Fatalf("BuildCodexFrozenDispatchPlan: %v", err)
	}
	accounts := plan.Accounts()
	if len(accounts) != 2 || len(accounts[0].Attempts()) != 2 {
		t.Fatalf("dispatch plan account/attempt shape = %d/%d", len(accounts), len(accounts[0].Attempts()))
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, accounts[0].Choice())
	events := make([]string, 0, 8)
	dispatcher := &codexHTTPSessionDispatcher{
		t: t, events: &events, wantBody: encoded,
		outcomes: []codexHTTPSessionOutcome{
			{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("rejected"))}},
			{response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("accepted"))}},
		},
	}
	slotAccounts := make(map[uint32]codex.AccountKey)
	for _, slot := range CodexHTTPAttemptSlots(plan) {
		slotAccounts[slot.Index] = slot.AccountKey
	}
	lifecycle := &codexHTTPSessionLifecycle{account: "account-a", slotAccounts: slotAccounts, events: &events}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, diagnostics := withRouteDiagnostics(context.Background())
	before := codexProcessRuntimeObservability.snapshot()
	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(ctx, template, plan, frozen, lifecycle)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer result.Response.Body.Close()
	after := codexProcessRuntimeObservability.snapshot()
	if got := after.AffinityReuse - before.AffinityReuse; got != 1 {
		t.Fatalf("affinity decision delta = %d, want 1 across two candidate dispatches", got)
	}
	if got := after.FairnessSelect - before.FairnessSelect; got != 0 {
		t.Fatalf("fairness decision delta = %d, want 0", got)
	}
	if got := after.TerminalDefault - before.TerminalDefault; got != 0 {
		t.Fatalf("terminal-default decision delta = %d, want 0", got)
	}
	var event RouteEvent
	event.applyRouteDiagnostics(diagnostics)
	if event.Decision != "affinity_reuse" {
		t.Fatalf("diagnostic decision = %q, want affinity_reuse", event.Decision)
	}
}

func TestCodexHTTPRequestSessionCountsFairnessAndTerminalDefaultActualRoutes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	capacity := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	frozenDispatchObserveCapacity(t, capacity, "account-a", CapacityBucketBase, 50, now)
	frozenDispatchObserveCapacity(t, capacity, "account-z", CapacityBucketBase, 0, now)
	accountA := frozenDispatchTestLogicalAccount("account-a",
		frozenDispatchCandidate("account-a", "candidate-a", "revision-a", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	accountZ := frozenDispatchTestLogicalAccount("account-z",
		frozenDispatchCandidate("account-z", "candidate-z", "revision-z", codex.SourceSystem, false, now.Add(time.Hour)),
	)
	plan, err := BuildCodexFrozenDispatchPlan(context.Background(), CodexFrozenDispatchInput{
		Inventory:         codex.Inventory{Accounts: []codex.LogicalAccount{accountZ, accountA}},
		Capacity:          capacity,
		Requirements:      CodexRouteRequirements{RequestedModel: "gpt-5"},
		DefaultAccountKey: "account-z", Now: now,
	})
	if err != nil {
		t.Fatalf("BuildCodexFrozenDispatchPlan: %v", err)
	}
	accounts := plan.Accounts()
	if len(accounts) != 2 || accounts[0].decision != codexRuntimeDecisionFairnessSelect || accounts[1].decision != codexRuntimeDecisionTerminalDefault {
		t.Fatalf("dispatch decisions = %+v", accounts)
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, accounts[0].Choice())
	events := make([]string, 0, 8)
	dispatcher := &codexHTTPSessionDispatcher{
		t: t, events: &events, wantBody: encoded,
		outcomes: []codexHTTPSessionOutcome{
			{response: &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached"}}`)),
			}},
			{response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("accepted"))}},
		},
	}
	slotAccounts := make(map[uint32]codex.AccountKey)
	for _, slot := range CodexHTTPAttemptSlots(plan) {
		slotAccounts[slot.Index] = slot.AccountKey
	}
	lifecycle := &codexHTTPSessionLifecycle{account: "account-a", slotAccounts: slotAccounts, events: &events}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, diagnostics := withRouteDiagnostics(context.Background())
	before := codexProcessRuntimeObservability.snapshot()
	result, err := (&CodexHTTPRequestSession{Executor: dispatcher, Capacity: NewCodexCapacityLedger(nil, 0)}).Do(ctx, template, plan, frozen, lifecycle)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer result.Response.Body.Close()
	if result.Choice.AccountKey != "account-z" {
		t.Fatalf("selected account = %q, want terminal default", result.Choice.AccountKey)
	}
	after := codexProcessRuntimeObservability.snapshot()
	if got := after.FairnessSelect - before.FairnessSelect; got != 1 {
		t.Fatalf("fairness decision delta = %d, want 1", got)
	}
	if got := after.TerminalDefault - before.TerminalDefault; got != 1 {
		t.Fatalf("terminal-default decision delta = %d, want 1", got)
	}
	if got := after.AffinityReuse - before.AffinityReuse; got != 0 {
		t.Fatalf("affinity decision delta = %d, want 0", got)
	}
	var event RouteEvent
	event.applyRouteDiagnostics(diagnostics)
	if event.Decision != "terminal_default" {
		t.Fatalf("diagnostic decision = %q, want terminal_default", event.Decision)
	}
}

func TestCodexRequestEnvelopeAccountsForExactOwnedCloneBytes(t *testing.T) {
	before := codexProcessRuntimeObservability.snapshot()
	encoded := []byte("encoded-private-fixture")
	decoded := []byte(`{"model":"gpt-5.4","input":"decoded-private-fixture"}`)

	envelope, err := NewCodexRequestEnvelope(encoded, decoded, http.Header{
		"Content-Type": {"application/json"},
	}, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	wantOwned := uint64(len(encoded) + len(decoded))
	afterEnvelope := codexProcessRuntimeObservability.snapshot()
	if got := afterEnvelope.CurrentReplayBytes - before.CurrentReplayBytes; got != wantOwned {
		t.Fatalf("envelope owned-byte delta = %d, want %d", got, wantOwned)
	}

	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	afterReplay := codexProcessRuntimeObservability.snapshot()
	if got := afterReplay.CurrentReplayBytes - before.CurrentReplayBytes; got != wantOwned {
		t.Fatalf("shared envelope and replay owned-byte delta = %d, want %d", got, wantOwned)
	}

	replay.Release()
	replay.Release()
	envelope.Release()
	envelope.Release()
	afterRelease := codexProcessRuntimeObservability.snapshot()
	if got := afterRelease.CurrentReplayBytes - before.CurrentReplayBytes; got != 0 {
		t.Fatalf("released owned-byte delta = %d, want 0", got)
	}
}

func TestCodexRuntimeHealthTracksCurrentJournalFailure(t *testing.T) {
	var observability codexRuntimeObservability
	observability.recordJournalCommit(time.Millisecond, 10, 2, 3, true)
	failed := observability.snapshot()
	if !failed.StateFailed || failed.StateFailures != 1 {
		t.Fatalf("failed journal health = %#v", failed)
	}
	observability.recordJournalCommit(time.Millisecond, 10, 2, 3, false)
	recovered := observability.snapshot()
	if recovered.StateFailed || recovered.StateFailures != 1 {
		t.Fatalf("recovered journal health = %#v", recovered)
	}
}

func TestCodexFrozenRequestAccountsForTransferredReplayBytes(t *testing.T) {
	choice := codexHTTPSessionChoice("account-frozen")
	body := frozenRequestBody(choice.RequestedModel, CodexRequestTurn, "private frozen prompt")
	before := codexProcessRuntimeObservability.snapshot()
	inspection, err := InspectCodexNativeRequest(context.Background(), body, http.Header{
		"Content-Type": {"application/json"},
	})
	if err != nil {
		t.Fatalf("InspectCodexNativeRequest: %v", err)
	}
	frozen, err := inspection.Freeze(context.Background(), choice, nil, HeadroomModeCache)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	afterFreeze := codexProcessRuntimeObservability.snapshot()
	wantOwned := uint64(len(body))
	if got := afterFreeze.CurrentReplayBytes - before.CurrentReplayBytes; got != wantOwned {
		t.Fatalf("frozen owned-byte delta = %d, want encoded+decoded %d", got, wantOwned)
	}
	frozen.Release()
	if got := codexProcessRuntimeObservability.snapshot().CurrentReplayBytes - before.CurrentReplayBytes; got != 0 {
		t.Fatalf("released frozen owned-byte delta = %d, want 0", got)
	}
}

func TestServerHealthExposesOnlyAggregateCodexRuntimeObservability(t *testing.T) {
	encoded := []byte("private-request-body-fixture")
	decoded := []byte(`{"model":"private-model-fixture","input":"private-decoded-fixture"}`)
	envelope, err := NewCodexRequestEnvelope(encoded, decoded, http.Header{
		"X-Codex-Turn-Metadata": {"private-semantic-header-fixture"},
		"Authorization":         {"Bearer private-credential-fixture"},
	}, "private-model-fixture")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()
	want := codexProcessRuntimeObservability.snapshot()

	response := httptest.NewRecorder()
	(&Server{}).handleHealth(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health response = %d/%q", response.Code, response.Body.String())
	}
	var health struct {
		Runtime *codexRuntimeObservabilitySnapshot `json:"codex_runtime_observability"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.Runtime == nil {
		t.Fatal("health omitted codex_runtime_observability")
	}
	want.HeapAllocBytes = health.Runtime.HeapAllocBytes
	want.HeapSysBytes = health.Runtime.HeapSysBytes
	want.Goroutines = health.Runtime.Goroutines
	if *health.Runtime != want {
		t.Fatalf("health runtime observability = %+v, want %+v", *health.Runtime, want)
	}
	for _, private := range []string{
		string(encoded), string(decoded), "private-semantic-header-fixture",
		"private-credential-fixture", "private-account-fixture", "private-candidate-fixture",
		"private-path-fixture", "private-model-fixture",
	} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("health exposed private fixture %q", private)
		}
	}
}

func TestCodexRuntimeHealthDegradesForEachPressureSignal(t *testing.T) {
	tests := []struct {
		name      string
		runtime   codexRuntimeObservabilitySnapshot
		ephemeral proxyEphemeralStateSnapshot
	}{
		{name: "journal failure", runtime: codexRuntimeObservabilitySnapshot{StateFailed: true}},
		{name: "persistence backlog", runtime: codexRuntimeObservabilitySnapshot{PersistenceWaiters: 101}},
		{name: "persistence wait", runtime: codexRuntimeObservabilitySnapshot{LastWaitMicros: 1_000_001}},
		{name: "journal commit", runtime: codexRuntimeObservabilitySnapshot{LastCommitMicros: 1_000_001}},
		{name: "journal size", runtime: codexRuntimeObservabilitySnapshot{JournalBytes: 12<<20 + 1}},
		{name: "replay memory", runtime: codexRuntimeObservabilitySnapshot{CurrentReplayBytes: 256<<20 + 1}},
		{name: "heap memory", runtime: codexRuntimeObservabilitySnapshot{HeapAllocBytes: 512<<20 + 1}},
		{name: "admission backlog", ephemeral: proxyEphemeralStateSnapshot{AdmissionReceipts: 10_001}},
		{name: "dispatch backlog", ephemeral: proxyEphemeralStateSnapshot{DispatchReceipts: 10_001}},
		{name: "admission prune", ephemeral: proxyEphemeralStateSnapshot{AdmissionFailed: true}},
		{name: "dispatch prune", ephemeral: proxyEphemeralStateSnapshot{DispatchFailed: true}},
	}
	if codexRuntimeHealthDegraded(codexRuntimeObservabilitySnapshot{}, proxyEphemeralStateSnapshot{}) {
		t.Fatal("zero runtime health degraded")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !codexRuntimeHealthDegraded(test.runtime, test.ephemeral) {
				t.Fatal("pressure signal did not degrade health")
			}
		})
	}
}

func TestServerPropagatesRuntimeDecisionDiagnosticsToNativeAndCompact(t *testing.T) {
	before := codexProcessRuntimeObservability.snapshot()
	tests := []struct {
		name      string
		path      string
		compact   bool
		handle    func(*Server, http.ResponseWriter, *http.Request)
		routeKind string
	}{
		{
			name: "native", path: codexResponsesPath, routeKind: "codex_native",
			handle: func(server *Server, writer http.ResponseWriter, request *http.Request) {
				server.handleNativeCodex(writer, request)
			},
		},
		{
			name: "compact", path: codexCompactResponsesPath, compact: true, routeKind: "codex_compact",
			handle: func(server *Server, writer http.ResponseWriter, request *http.Request) {
				server.handleNativeCodexCompact(writer, request, codexCompactResponsesPath)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir() + "/diagnostics.jsonl"
			diagnostics, err := OpenDiagnosticsWriter(path)
			if err != nil {
				t.Fatalf("OpenDiagnosticsWriter: %v", err)
			}
			server := &Server{
				Diag: diagnostics,
				CodexNativeHTTP: codexRuntimeRoutingHandlerFunc(func(writer http.ResponseWriter, request *http.Request, compact bool) (bool, string) {
					if compact != test.compact {
						t.Fatalf("compact = %t, want %t", compact, test.compact)
					}
					codexProcessRuntimeObservability.recordDecision(request.Context(), codexRuntimeDecisionFairnessSelect)
					_ = request.Body.Close()
					writer.WriteHeader(http.StatusOK)
					return true, "gpt-5.4"
				}),
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"model":"gpt-5.4"}`))
			test.handle(server, response, request)
			if err := diagnostics.Close(); err != nil {
				t.Fatalf("Close diagnostics: %v", err)
			}
			events := readDiagnosticsEvents(t, path)
			if len(events) != 1 || events[0].RouteKind != test.routeKind || events[0].Decision != "fairness_select" {
				t.Fatalf("diagnostics = %+v, want %s/fairness_select", events, test.routeKind)
			}
		})
	}
	after := codexProcessRuntimeObservability.snapshot()
	if got := after.FairnessSelect - before.FairnessSelect; got != uint64(len(tests)) {
		t.Fatalf("fairness decision delta = %d, want %d", got, len(tests))
	}
}

func TestCodexRequestEnvelopeDefersReplayAccountingUntilActiveBodyCloses(t *testing.T) {
	before := codexProcessRuntimeObservability.snapshot()
	encoded := []byte("encoded-active-body")
	decoded := []byte("decoded-active-body")
	wantOwned := uint64(len(encoded) + len(decoded))
	envelope, err := NewCodexRequestEnvelope(encoded, decoded, nil, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	body, err := replay.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	afterOwn := codexProcessRuntimeObservability.snapshot()
	if got := afterOwn.CurrentReplayBytes - before.CurrentReplayBytes; got != wantOwned {
		t.Fatalf("owned-byte delta = %d, want %d", got, wantOwned)
	}
	if afterOwn.PeakReplayBytes < before.CurrentReplayBytes+wantOwned || afterOwn.PeakReplayBytes < before.PeakReplayBytes {
		t.Fatalf("peak = %d, baseline current/peak = %d/%d", afterOwn.PeakReplayBytes, before.CurrentReplayBytes, before.PeakReplayBytes)
	}

	replay.Release()
	replay.Release()
	envelope.Release()
	envelope.Release()
	afterRelease := codexProcessRuntimeObservability.snapshot()
	if got := afterRelease.CurrentReplayBytes - before.CurrentReplayBytes; got != wantOwned {
		t.Fatalf("active-body retained-byte delta = %d, want %d", got, wantOwned)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("duplicate Close: %v", err)
	}
	afterClose := codexProcessRuntimeObservability.snapshot()
	if got := afterClose.CurrentReplayBytes - before.CurrentReplayBytes; got != 0 {
		t.Fatalf("closed-body owned-byte delta = %d, want 0", got)
	}
	if afterClose.PeakReplayBytes != afterRelease.PeakReplayBytes {
		t.Fatalf("peak decreased after release = %d, want %d", afterClose.PeakReplayBytes, afterRelease.PeakReplayBytes)
	}
}

func TestCodexRequestEnvelopeConcurrentOwnershipReturnsToBaseline(t *testing.T) {
	before := codexProcessRuntimeObservability.snapshot()
	encoded := []byte("encoded-concurrent")
	decoded := []byte("decoded-concurrent")
	wantEach := uint64(len(encoded) + len(decoded))
	const count = 32

	type ownedPair struct {
		envelope *CodexRequestEnvelope
		replay   *CodexRequestReplay
	}
	pairs := make(chan ownedPair, count)
	errorsCh := make(chan error, count)
	var create sync.WaitGroup
	create.Add(count)
	for range count {
		go func() {
			defer create.Done()
			envelope, err := NewCodexRequestEnvelope(encoded, decoded, nil, "gpt-5.4")
			if err != nil {
				errorsCh <- err
				return
			}
			replay, err := envelope.Replay()
			if err != nil {
				envelope.Release()
				errorsCh <- err
				return
			}
			pairs <- ownedPair{envelope: envelope, replay: replay}
		}()
	}
	create.Wait()
	close(pairs)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("create concurrent ownership: %v", err)
	}
	owned := make([]ownedPair, 0, count)
	for pair := range pairs {
		owned = append(owned, pair)
	}
	afterOwn := codexProcessRuntimeObservability.snapshot()
	wantDelta := uint64(count) * wantEach
	if got := afterOwn.CurrentReplayBytes - before.CurrentReplayBytes; got != wantDelta {
		t.Fatalf("concurrent owned-byte delta = %d, want %d", got, wantDelta)
	}
	if afterOwn.PeakReplayBytes < before.CurrentReplayBytes+wantDelta {
		t.Fatalf("concurrent peak = %d, want at least %d", afterOwn.PeakReplayBytes, before.CurrentReplayBytes+wantDelta)
	}

	var release sync.WaitGroup
	release.Add(len(owned))
	for _, pair := range owned {
		pair := pair
		go func() {
			defer release.Done()
			pair.replay.Release()
			pair.replay.Release()
			pair.envelope.Release()
			pair.envelope.Release()
		}()
	}
	release.Wait()
	afterRelease := codexProcessRuntimeObservability.snapshot()
	if got := afterRelease.CurrentReplayBytes - before.CurrentReplayBytes; got != 0 {
		t.Fatalf("released concurrent owned-byte delta = %d, want 0", got)
	}
	if afterRelease.PeakReplayBytes != afterOwn.PeakReplayBytes {
		t.Fatalf("peak changed on release = %d, want %d", afterRelease.PeakReplayBytes, afterOwn.PeakReplayBytes)
	}
}

func TestCodexRequestEnvelopeLargeOwnershipIsReleased(t *testing.T) {
	before := codexProcessRuntimeObservability.snapshot()
	body := codexProtocolRequestBodyAtSize(t, maxRequestBody+1)
	envelope, err := NewCodexRequestEnvelope(body, nil, nil, "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	afterOwn := codexProcessRuntimeObservability.snapshot()
	if got := afterOwn.CurrentReplayBytes - before.CurrentReplayBytes; got != uint64(len(body)) {
		t.Fatalf("owned byte delta = %d, want %d", got, len(body))
	}
	envelope.Release()
	afterRelease := codexProcessRuntimeObservability.snapshot()
	if afterRelease.CurrentReplayBytes != before.CurrentReplayBytes {
		t.Fatalf("released current bytes = %d, want %d", afterRelease.CurrentReplayBytes, before.CurrentReplayBytes)
	}
}

func TestCodexHTTPRequestSessionReleasesReplayOwnershipOnErrorCancelAndPanic(t *testing.T) {
	tests := []struct {
		name              string
		roundTrip         codexTransportRoundTripFunc
		cancelOnRoundTrip bool
		wantPanic         bool
	}{
		{
			name: "error",
			roundTrip: func(_ *http.Request) (*http.Response, error) {
				return nil, errors.New("synthetic dispatch error")
			},
		},
		{
			name: "cancel after dispatch", cancelOnRoundTrip: true,
		},
		{
			name: "panic after dispatch fence",
			roundTrip: func(_ *http.Request) (*http.Response, error) {
				panic("synthetic dispatch panic")
			},
			wantPanic: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := codexProcessRuntimeObservability.snapshot()
			events := []string{}
			identity := codex.AccountIdentity{AccountID: "account-id", UserID: "user-id"}
			choice := codexHTTPSessionChoice("account-a")
			attempt := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
			attempt.Identity = identity
			attempt.Source = codex.SourceSystem
			plan := CodexFrozenDispatchPlan{
				status: CodexRoutePlanReady,
				accounts: []CodexFrozenDispatchAccount{{
					choice: choice, attempts: []CandidateAttempt{attempt}, decision: codexRuntimeDecisionFairnessSelect,
				}},
			}
			frozen, _ := newCodexHTTPSessionFrozenRequest(t, choice)
			lifecycle := &codexHTTPSessionLifecycle{
				account:      "account-a",
				slotAccounts: map[uint32]codex.AccountKey{1: "account-a"},
				events:       &events,
			}
			template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			panicked := false
			func() {
				defer func() {
					panicked = recover() != nil
				}()
				ctx := context.Background()
				roundTrip := test.roundTrip
				if test.cancelOnRoundTrip {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					roundTrip = func(request *http.Request) (*http.Response, error) {
						cancel()
						return nil, request.Context().Err()
					}
				}
				executor := &CodexAttemptExecutor{
					Secrets: &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
						"revision-a": testExactCredentialMaterial(identity, "private-access-token"),
					}},
					Transport: &CodexTokenTransport{Inner: roundTrip},
				}
				_, _ = (&CodexHTTPRequestSession{Executor: executor}).Do(ctx, template, plan, frozen, lifecycle)
			}()
			if panicked != test.wantPanic {
				t.Fatalf("panic = %t, want %t", panicked, test.wantPanic)
			}
			after := codexProcessRuntimeObservability.snapshot()
			if got := after.CurrentReplayBytes - before.CurrentReplayBytes; got != 0 {
				t.Fatalf("owned-byte delta after terminal path = %d, want 0", got)
			}
		})
	}
}
