package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestCallerAuthorityNativeCodexRejectsLocalTokenBeforeWorker(t *testing.T) {
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
	if response.Code != http.StatusForbidden || body.reads != 0 {
		t.Fatalf("status/body reads = %d/%d, want 403/0", response.Code, body.reads)
	}
	for _, event := range events {
		if event == "execute:worker" {
			t.Fatal("local token reached native Codex worker route")
		}
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

func (worker *callerAuthorityRefreshingWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder, CallerAuthorityKey: worker.bootKey, CallerIndex: worker.bootIndex}, nil
}

func (worker *callerAuthorityRefreshingWorker) CallerIndex(context.Context) (NormalCallerIndexV1, error) {
	return worker.current, nil
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
