package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type callerAuthorityTestConsumer struct {
	consumed map[string]ProviderBranchAdmissionConsumptionV1
	err      error
}

func (consumer *callerAuthorityTestConsumer) Consume(_ context.Context, consumption ProviderBranchAdmissionConsumptionV1) error {
	if consumer.err != nil {
		return consumer.err
	}
	if _, exists := consumer.consumed[consumption.AdmissionID]; exists {
		return ErrNormalCallerAdmissionReplayed
	}
	consumer.consumed[consumption.AdmissionID] = consumption
	return nil
}

type callerAuthorityCountingBody struct {
	reader io.Reader
	reads  int
}

func (body *callerAuthorityCountingBody) Read(buffer []byte) (int, error) {
	body.reads++
	return body.reader.Read(buffer)
}

func (*callerAuthorityCountingBody) Close() error { return nil }

func testNormalCallerAuthority(t *testing.T, credentials []NormalCallerCredentialV1, consumer NormalCallerAdmissionConsumer) *NormalCallerAuthority {
	t.Helper()
	authority, err := NewNormalCallerAuthority(
		bytes.Repeat([]byte{0x42}, 32),
		1,
		credentials,
		consumer,
		func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) },
		bytes.NewReader(bytes.Repeat([]byte{0x31}, 256)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func TestCallerAuthorityAcceptsOnlyAuthenticatedMonotonicIndexUpdates(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	authority := testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "old-bearer", SubjectID: "codex"}}, consumer)
	updated, err := BuildNormalCallerIndexV1(key, 2, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "new-bearer", SubjectID: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.UpdateFromIndex(updated); err != nil {
		t.Fatal(err)
	}
	newRequest := httptest.NewRequest(http.MethodPost, "/responses", nil)
	newRequest.Header.Set("Authorization", "Bearer new-bearer")
	if _, err := authority.authenticate(newRequest, normalCallerRouteCodex); err != nil {
		t.Fatalf("updated bearer rejected: %v", err)
	}
	oldRequest := httptest.NewRequest(http.MethodPost, "/responses", nil)
	oldRequest.Header.Set("Authorization", "Bearer old-bearer")
	if _, err := authority.authenticate(oldRequest, normalCallerRouteCodex); !errors.Is(err, ErrNormalCallerAuthRequired) {
		t.Fatalf("old bearer error = %v", err)
	}
	stale, _ := BuildNormalCallerIndexV1(key, 1, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "old-bearer", SubjectID: "codex"}})
	if err := authority.UpdateFromIndex(stale); !errors.Is(err, ErrNormalCallerAuthUnavailable) {
		t.Fatalf("stale update error = %v", err)
	}
	updated.MAC = strings.Repeat("0", 64)
	if err := authority.UpdateFromIndex(updated); !errors.Is(err, ErrNormalCallerAuthUnavailable) {
		t.Fatalf("unauthenticated update error = %v", err)
	}
}

func TestCallerAuthorityRetainsSupersededBearerUntilExpiry(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	authority, err := NewNormalCallerAuthority(key, 1, []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "old-bearer", SubjectID: "codex-old", ValidUntil: now.Add(time.Hour),
	}}, consumer, func() time.Time { return now }, bytes.NewReader(bytes.Repeat([]byte{0x31}, 256)))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := BuildNormalCallerIndexV1(key, 2, []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "new-bearer", SubjectID: "codex-new", ValidUntil: now.Add(2 * time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.UpdateFromIndex(updated); err != nil {
		t.Fatal(err)
	}

	oldRequest := httptest.NewRequest(http.MethodPost, "/responses", nil)
	oldRequest.Header.Set("Authorization", "Bearer old-bearer")
	if _, err := authority.authenticate(oldRequest, normalCallerRouteCodex); err != nil {
		t.Fatalf("unexpired superseded bearer rejected: %v", err)
	}

	now = now.Add(2 * time.Hour)
	if _, err := authority.authenticate(oldRequest, normalCallerRouteCodex); !errors.Is(err, ErrNormalCallerAuthRequired) {
		t.Fatalf("expired superseded bearer error = %v", err)
	}
}

func TestCallerAuthorityRejectsAmbiguousBearerBeforeBodyOrWorker(t *testing.T) {
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{
		{Domain: NormalCallerClaude, Bearer: "same-bearer", SubjectID: "claude-a"},
		{Domain: NormalCallerCodex, Bearer: "same-bearer", SubjectID: "codex-a"},
	}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}))
	body := &callerAuthorityCountingBody{reader: bytes.NewBufferString(`{"model":"claude-sonnet-4"}`)}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	request.Header.Set("Authorization", "Bearer same-bearer")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || body.reads != 0 {
		t.Fatalf("status/body reads = %d/%d, want 403/0", response.Code, body.reads)
	}
	for _, event := range events {
		if event == "execute:worker" {
			t.Fatal("ambiguous caller reached worker")
		}
	}
}

func TestCallerAuthorityRejectsCrossAccountCodexBearer(t *testing.T) {
	authority := testNormalCallerAuthority(t, []NormalCallerCredentialV1{
		{Domain: NormalCallerCodex, Bearer: "same-bearer", SubjectID: "account-a"},
		{Domain: NormalCallerCodex, Bearer: "same-bearer", SubjectID: "account-b"},
	}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)})
	request := httptest.NewRequest(http.MethodPost, "/responses", nil)
	request.Header.Set("Authorization", "Bearer same-bearer")
	if _, err := authority.authenticate(request, normalCallerRouteCodex); !errors.Is(err, ErrNormalCallerAuthScope) {
		t.Fatalf("cross-account bearer error = %v, want %v", err, ErrNormalCallerAuthScope)
	}
}

func TestCallerAuthorityConsumesOneUseAdmissionBeforeWorkerDispatch(t *testing.T) {
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	worker := &callerAuthorityObservingWorker{holder: runtimeHolder("worker"), consumer: consumer}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &callerAuthorityLauncher{worker: worker}, &RuntimeHashCheckpointStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "codex-bearer", SubjectID: "codex-a"}}, consumer))
	request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	request.Header.Set("Authorization", "Bearer codex-bearer")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if worker.calls != 1 || worker.last.Caller.Domain != NormalCallerCodex || worker.last.Header.Get("Authorization") != "" {
		t.Fatalf("worker request = calls %d, caller %#v, authorization %q", worker.calls, worker.last.Caller, worker.last.Header.Get("Authorization"))
	}
}

func TestCallerAuthorityNativeCodexAcceptsLocalToken(t *testing.T) {
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner"}}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}))
	body := &callerAuthorityCountingBody{reader: bytes.NewBufferString(`{"model":"gpt-5.4"}`)}
	request := httptest.NewRequest(http.MethodPost, "/responses", body)
	request.Header.Set("Authorization", "Bearer local-token")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	dispatched := false
	for _, event := range events {
		if event == "execute:worker" {
			dispatched = true
		}
	}
	if !dispatched {
		t.Fatal("local token did not reach native Codex worker route")
	}
}

func TestCallerAuthorityRejectsCrossProviderDispatchBeforeWorker(t *testing.T) {
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "codex-bearer", SubjectID: "codex-a"}}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)})); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerClassifier(NewNormalCallerBranchClassifier(nil)); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/custom-dispatch", bytes.NewBufferString(`{"model":"claude-sonnet-4"}`))
	request.Header.Set("Authorization", "Bearer codex-bearer")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	for _, event := range events {
		if event == "execute:worker" {
			t.Fatal("cross-provider caller reached worker")
		}
	}
}

func TestCallerAuthorityBootsFromWorkerPublishedSafeIndex(t *testing.T) {
	key := bytes.Repeat([]byte{0x52}, 32)
	index, err := BuildNormalCallerIndexV1(key, 7, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "worker-only-bearer", SubjectID: "codex-worker"}})
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events, bootAck: RuntimeBootAckV1{CallerAuthorityKey: key, CallerIndex: index}}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	if err := supervisor.SetCallerAdmissionConsumer(consumer); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	request.Header.Set("Authorization", "Bearer hostile")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	for _, event := range events {
		if event == "execute:worker" {
			t.Fatal("hostile caller reached worker")
		}
	}
}

func TestRuntimeSupervisorRefreshesWorkerCallerIndexBeforeAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{0x53}, 32)
	oldIndex, err := BuildNormalCallerIndexV1(key, 1, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "old-bearer", SubjectID: "codex-worker"}})
	if err != nil {
		t.Fatal(err)
	}
	newIndex, err := BuildNormalCallerIndexV1(key, 2, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "new-bearer", SubjectID: "codex-worker"}})
	if err != nil {
		t.Fatal(err)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	worker := &callerAuthorityRefreshingWorker{
		callerAuthorityObservingWorker: callerAuthorityObservingWorker{holder: runtimeHolder("worker"), consumer: consumer},
		bootKey:                        append([]byte(nil), key...), bootIndex: oldIndex, current: newIndex,
	}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &callerAuthorityLauncher{worker: worker}, &RuntimeHashCheckpointStore{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAdmissionConsumer(consumer); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	request.Header.Set("Authorization", "Bearer new-bearer")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || worker.calls != 1 {
		t.Fatalf("updated request = status %d calls %d", response.Code, worker.calls)
	}
	request = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	request.Header.Set("Authorization", "Bearer old-bearer")
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || worker.calls != 1 {
		t.Fatalf("stale request = status %d calls %d", response.Code, worker.calls)
	}
}

func TestRuntimeSupervisorAllowsOpaqueCallerDuringCredentialRollover(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, sha256.Size)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	current := []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "old-bearer", SubjectID: "account\x00candidate\x00revision-a",
	}}
	state, err := newRuntimeCallerCredentialState(key, func(context.Context) ([]NormalCallerCredentialV1, error) {
		return append([]NormalCallerCredentialV1(nil), current...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state.now = func() time.Time { return now }
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	worker := &callerAuthorityCredentialStateWorker{
		callerAuthorityObservingWorker: callerAuthorityObservingWorker{holder: runtimeHolder("worker"), consumer: consumer},
		key:                            key,
		state:                          state,
	}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &callerAuthorityLauncher{worker: worker}, &RuntimeHashCheckpointStore{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAdmissionConsumer(consumer); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	supervisor.mu.RLock()
	authority := supervisor.callerAuthority
	supervisor.mu.RUnlock()
	authority.mu.Lock()
	authority.now = func() time.Time { return now }
	authority.mu.Unlock()
	current = []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: "new-bearer", SubjectID: "account\x00candidate\x00revision-b",
	}}

	request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	request.Header.Set("Authorization", "Bearer old-bearer")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || worker.calls != 1 {
		t.Fatalf("overlapping caller = status %d calls %d, want %d/1", response.Code, worker.calls, http.StatusNoContent)
	}

	now = now.Add(normalCallerAdmissionLifetime)
	request = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	request.Header.Set("Authorization", "Bearer old-bearer")
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || worker.calls != 1 {
		t.Fatalf("expired caller = status %d calls %d, want %d/1", response.Code, worker.calls, http.StatusUnauthorized)
	}
}

func TestRuntimeSupervisorSerializesCallerIndexRefreshAndApply(t *testing.T) {
	key := bytes.Repeat([]byte{0x54}, 32)
	validUntil := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	bootIndex, err := BuildNormalCallerIndexV1(key, 1, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "boot-bearer", SubjectID: "codex-worker", ValidUntil: validUntil}})
	if err != nil {
		t.Fatal(err)
	}
	olderIndex, err := BuildNormalCallerIndexV1(key, 2, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "older-bearer", SubjectID: "codex-worker", ValidUntil: validUntil}})
	if err != nil {
		t.Fatal(err)
	}
	newerIndex, err := BuildNormalCallerIndexV1(key, 3, []NormalCallerCredentialV1{{Domain: NormalCallerCodex, Bearer: "newer-bearer", SubjectID: "codex-worker", ValidUntil: validUntil}})
	if err != nil {
		t.Fatal(err)
	}
	worker := &callerAuthorityConcurrentRefreshWorker{
		callerAuthorityObservingWorker: callerAuthorityObservingWorker{holder: runtimeHolder("worker")},
		bootKey:                        append([]byte(nil), key...), bootIndex: bootIndex, olderIndex: olderIndex, newerIndex: newerIndex,
		firstRefreshStarted: make(chan struct{}), newerRequestExecuted: make(chan struct{}),
	}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &callerAuthorityLauncher{worker: worker}, &RuntimeHashCheckpointStore{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAdmissionConsumer(callerAuthorityConcurrentConsumer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		bearer string
		code   int
	}
	results := make(chan result, 2)
	request := func(bearer string) {
		httpRequest := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
		httpRequest.Header.Set("Authorization", "Bearer "+bearer)
		response := httptest.NewRecorder()
		supervisor.ServeHTTP(response, httpRequest)
		results <- result{bearer: bearer, code: response.Code}
	}
	go request("older-bearer")
	select {
	case <-worker.firstRefreshStarted:
	case <-time.After(time.Second):
		t.Fatal("first caller-index refresh did not start")
	}
	go request("newer-bearer")

	for range 2 {
		select {
		case got := <-results:
			if got.code != http.StatusNoContent {
				t.Fatalf("%s request status = %d, want %d", got.bearer, got.code, http.StatusNoContent)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent caller-index requests did not complete")
		}
	}
	refreshCalls, executeCalls := worker.counts()
	if refreshCalls != 2 || executeCalls != 2 {
		t.Fatalf("refresh/execute calls = %d/%d, want 2/2", refreshCalls, executeCalls)
	}
	supervisor.mu.RLock()
	authority := supervisor.callerAuthority
	supervisor.mu.RUnlock()
	newerRequest := httptest.NewRequest(http.MethodPost, "/responses", nil)
	newerRequest.Header.Set("Authorization", "Bearer newer-bearer")
	if _, err := authority.authenticate(newerRequest, normalCallerRouteCodex); err != nil {
		t.Fatalf("newest caller index rolled back: %v", err)
	}
	authority.mu.RLock()
	finalEpoch := authority.epoch
	authority.mu.RUnlock()
	if finalEpoch != newerIndex.IndexEpoch {
		t.Fatalf("final caller-index epoch = %d, want %d", finalEpoch, newerIndex.IndexEpoch)
	}
}

func TestCallerAuthorityKeepsUnavailableHealthPublic(t *testing.T) {
	events := []string{}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "{\"status\":\"degraded\",\"supervisor_alive\":true,\"data_plane_ready\":false}\n" {
		t.Fatalf("health response = %d %q", response.Code, response.Body.String())
	}
	if len(events) != 0 {
		t.Fatalf("health reached worker: %#v", events)
	}
}

type callerAuthorityLauncher struct{ worker RuntimeWorkerProcess }

func (launcher *callerAuthorityLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	return launcher.worker, nil
}

type callerAuthorityObservingWorker struct {
	holder   LifecycleHolderProof
	consumer *callerAuthorityTestConsumer
	calls    int
	last     RuntimeHTTPRequestV1
}

type callerAuthorityRefreshingWorker struct {
	callerAuthorityObservingWorker
	bootKey   []byte
	bootIndex NormalCallerIndexV1
	current   NormalCallerIndexV1
}

type callerAuthorityCredentialStateWorker struct {
	callerAuthorityObservingWorker
	key   []byte
	state *runtimeCallerCredentialState
}

type callerAuthorityConcurrentConsumer struct{}

func (callerAuthorityConcurrentConsumer) Consume(context.Context, ProviderBranchAdmissionConsumptionV1) error {
	return nil
}

type callerAuthorityConcurrentRefreshWorker struct {
	callerAuthorityObservingWorker

	bootKey              []byte
	bootIndex            NormalCallerIndexV1
	olderIndex           NormalCallerIndexV1
	newerIndex           NormalCallerIndexV1
	firstRefreshStarted  chan struct{}
	newerRequestExecuted chan struct{}
	newerExecutedOnce    sync.Once

	mu           sync.Mutex
	refreshCalls int
	executeCalls int
}

func (worker *callerAuthorityConcurrentRefreshWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder, CallerAuthorityKey: worker.bootKey, CallerIndex: worker.bootIndex}, nil
}

func (worker *callerAuthorityConcurrentRefreshWorker) CallerIndex(context.Context) (NormalCallerIndexV1, error) {
	worker.mu.Lock()
	worker.refreshCalls++
	call := worker.refreshCalls
	worker.mu.Unlock()
	if call == 1 {
		close(worker.firstRefreshStarted)
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-worker.newerRequestExecuted:
		case <-timer.C:
		}
		return worker.olderIndex, nil
	}
	return worker.newerIndex, nil
}

func (worker *callerAuthorityConcurrentRefreshWorker) ExecuteHTTP(_ context.Context, request RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error) {
	worker.mu.Lock()
	worker.executeCalls++
	worker.mu.Unlock()
	if request.Caller.IndexEpoch == worker.newerIndex.IndexEpoch {
		worker.newerExecutedOnce.Do(func() { close(worker.newerRequestExecuted) })
	}
	return RuntimeHTTPResponseV1{StatusCode: http.StatusNoContent}, nil
}

func (worker *callerAuthorityConcurrentRefreshWorker) counts() (int, int) {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return worker.refreshCalls, worker.executeCalls
}

func (worker *callerAuthorityRefreshingWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder, CallerAuthorityKey: worker.bootKey, CallerIndex: worker.bootIndex}, nil
}

func (worker *callerAuthorityRefreshingWorker) CallerIndex(context.Context) (NormalCallerIndexV1, error) {
	return worker.current, nil
}

func (worker *callerAuthorityCredentialStateWorker) Boot(ctx context.Context, _ WorkerManifestV1) (RuntimeBootAckV1, error) {
	_, index, err := worker.state.snapshot(ctx)
	if err != nil {
		return RuntimeBootAckV1{}, err
	}
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder, CallerAuthorityKey: append([]byte(nil), worker.key...), CallerIndex: index}, nil
}

func (worker *callerAuthorityCredentialStateWorker) CallerIndex(ctx context.Context) (NormalCallerIndexV1, error) {
	_, index, err := worker.state.snapshot(ctx)
	return index, err
}

func (worker *callerAuthorityObservingWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder}, nil
}
func (*callerAuthorityObservingWorker) BeginDrain(context.Context, TrafficMode, uint64) error {
	return nil
}
func (*callerAuthorityObservingWorker) AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error) {
	return RuntimeQuiescenceAckV1{SchemaVersion: 1, Quiescent: true}, nil
}
func (*callerAuthorityObservingWorker) StopAndReap(context.Context) (RuntimeWorkerReleaseV1, error) {
	return RuntimeWorkerReleaseV1{ProcessIdentityDigest: "p", ProcessTreeAbsenceProofDigest: "a", HolderReleaseProofDigest: "h"}, nil
}
func (worker *callerAuthorityObservingWorker) ExecuteHTTP(_ context.Context, request RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error) {
	worker.calls++
	worker.last = request
	if _, ok := worker.consumer.consumed[request.Caller.AdmissionID]; !ok {
		return RuntimeHTTPResponseV1{}, errors.New("worker dispatch preceded admission consumption")
	}
	return RuntimeHTTPResponseV1{StatusCode: http.StatusNoContent}, nil
}
func (worker *callerAuthorityObservingWorker) HolderProof() LifecycleHolderProof {
	return worker.holder
}
