package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRuntimeSupervisorRescueControlTransitionsAndRoutes(t *testing.T) {
	events := []string{}
	first := &runtimeTestWorker{holder: runtimeHolder("worker-1"), events: &events}
	second := &runtimeTestWorker{holder: runtimeHolder("worker-2"), events: &events}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{first, second}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	manifest := WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}
	if _, err := supervisor.Boot(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	authority := testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner"}}, consumer)
	random := make([]byte, 256)
	for index := range random {
		random[index] = byte(index)
	}
	authority.random = bytes.NewReader(random)
	if err := supervisor.SetCallerAuthority(authority); err != nil {
		t.Fatal(err)
	}
	rescueCalls := 0
	if err := supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		rescueCalls++
		writer.WriteHeader(http.StatusAccepted)
	}), &runtimeEvidenceTestStore{}); err != nil {
		t.Fatal(err)
	}

	enter := httptest.NewRequest(http.MethodPost, RuntimeRescueEnterPath, nil)
	enter.Header.Set("Authorization", "Bearer local-token")
	enterResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(enterResponse, enter)
	if enterResponse.Code != http.StatusOK {
		t.Fatalf("enter = %d mode=%q body=%q", enterResponse.Code, supervisor.TrafficMode(), enterResponse.Body.String())
	}
	var enterStatus struct {
		Mode TrafficMode `json:"mode"`
	}
	if err := json.Unmarshal(enterResponse.Body.Bytes(), &enterStatus); err != nil {
		t.Fatal(err)
	}
	if enterStatus.Mode != TrafficModeRescue {
		t.Fatalf("reported enter mode = %q, want rescue", enterStatus.Mode)
	}
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)
	rescue := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}"))
	rescueResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(rescueResponse, rescue)
	if rescueResponse.Code != http.StatusAccepted || rescueCalls != 1 {
		t.Fatalf("rescue = %d calls=%d", rescueResponse.Code, rescueCalls)
	}
	exit := httptest.NewRequest(http.MethodPost, RuntimeRescueExitPath, nil)
	exit.Header.Set("Authorization", "Bearer local-token")
	exitResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(exitResponse, exit)
	if exitResponse.Code != http.StatusOK {
		t.Fatalf("exit = %d mode=%q ready=%v body=%q", exitResponse.Code, supervisor.TrafficMode(), supervisor.AdmissionReady(), exitResponse.Body.String())
	}
	waitForRuntimeMode(t, supervisor, TrafficModeNormal)
	if !supervisor.AdmissionReady() {
		t.Fatal("normal admission unavailable after rescue exit")
	}
	if len(consumer.consumed) != 2 {
		t.Fatalf("control consumptions = %d", len(consumer.consumed))
	}
}

func TestRuntimeSupervisorRescueEnterCancelsExitDrain(t *testing.T) {
	key := bytes.Repeat([]byte{0x54}, 32)
	index, err := BuildNormalCallerIndexV1(key, 8, []NormalCallerCredentialV1{{
		Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner",
	}})
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	worker := &runtimeTestWorker{
		holder: runtimeHolder("worker"), events: &events,
		response: RuntimeHTTPResponseV1{StatusCode: http.StatusAccepted, Body: []byte("normal")},
		bootAck:  RuntimeBootAckV1{CallerAuthorityKey: key, CallerIndex: index},
	}
	store := &runtimeEvidenceTestStore{records: []RuntimeModeEvidenceV1{{
		SchemaVersion: 1, Generation: 7, DesiredMode: TrafficModeRescue,
		EffectiveMode: TrafficModeRescue, Phase: RuntimeModePhaseEffective,
	}}}
	supervisor, err := NewRuntimeSupervisor(
		&runtimeTestListener{}, runtimeHolder("supervisor"),
		&runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}},
		&runtimeTestCheckpointStore{events: &events},
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	if err := supervisor.SetCallerAdmissionConsumer(consumer); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{
		Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner",
	}}, consumer)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerClassifier(NewNormalCallerBranchClassifier(nil)); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	rescueCalls := 0
	if err := supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		rescueCalls++
		if request.URL.EscapedPath() == "/responses" {
			first.Do(func() { close(admitted) })
			<-release
		}
		writer.WriteHeader(http.StatusAccepted)
	}), store); err != nil {
		t.Fatal(err)
	}
	supervisor.mu.Lock()
	supervisor.workerManifest = WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}
	supervisor.mu.Unlock()

	rescueDone := make(chan struct{})
	go func() {
		defer close(rescueDone)
		request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString("{}"))
		request.Header.Set("X-Codex-Window-Id", "existing-window")
		supervisor.ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-admitted
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-rescueDone
	})

	exit := httptest.NewRequest(http.MethodPost, RuntimeRescueExitPath, nil)
	exit.Header.Set("Authorization", "Bearer local-token")
	exitResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(exitResponse, exit)
	if exitResponse.Code != http.StatusOK || supervisor.TrafficMode() != TrafficModeRescueExitDraining {
		t.Fatalf("exit = %d mode=%q body=%q", exitResponse.Code, supervisor.TrafficMode(), exitResponse.Body.String())
	}

	enter := httptest.NewRequest(http.MethodPost, RuntimeRescueEnterPath, nil)
	enter.Header.Set("Authorization", "Bearer local-token")
	enterResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(enterResponse, enter)
	if enterResponse.Code != http.StatusOK {
		t.Fatalf("cancel enter = %d mode=%q body=%q", enterResponse.Code, supervisor.TrafficMode(), enterResponse.Body.String())
	}
	var entered struct {
		Mode TrafficMode `json:"mode"`
	}
	if err := json.Unmarshal(enterResponse.Body.Bytes(), &entered); err != nil {
		t.Fatal(err)
	}
	if entered.Mode != TrafficModeRescue {
		t.Fatalf("cancel enter reported mode = %q, want rescue", entered.Mode)
	}

	newRequest := httptest.NewRequest(http.MethodPost, "/new-rescue", bytes.NewBufferString("{}"))
	newResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(newResponse, newRequest)
	if newResponse.Code != http.StatusAccepted || rescueCalls != 2 {
		t.Fatalf("new ingress = %d rescue calls=%d, want rescue", newResponse.Code, rescueCalls)
	}
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	close(release)
	<-rescueDone
}

func TestRuntimeSupervisorRescueExitHandsOffNewIngressWhileSessionDrains(t *testing.T) {
	key := bytes.Repeat([]byte{0x53}, 32)
	index, err := BuildNormalCallerIndexV1(key, 5, []NormalCallerCredentialV1{{
		Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner",
	}})
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	worker := &runtimeTestWorker{
		holder: runtimeHolder("worker"), events: &events,
		response: RuntimeHTTPResponseV1{StatusCode: http.StatusAccepted, Body: []byte("normal")},
		bootAck:  RuntimeBootAckV1{CallerAuthorityKey: key, CallerIndex: index},
	}
	store := &runtimeEvidenceTestStore{records: []RuntimeModeEvidenceV1{{
		SchemaVersion: 1, Generation: 4, DesiredMode: TrafficModeRescue,
		EffectiveMode: TrafficModeRescue, Phase: RuntimeModePhaseEffective,
	}}}
	supervisor, err := NewRuntimeSupervisor(
		&runtimeTestListener{}, runtimeHolder("supervisor"),
		&runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}},
		&runtimeTestCheckpointStore{events: &events},
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	if err := supervisor.SetCallerAdmissionConsumer(consumer); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{
		Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner",
	}}, consumer)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerClassifier(NewNormalCallerBranchClassifier(nil)); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan struct{})
	release := make(chan struct{})
	if err := supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(admitted)
		<-release
		writer.WriteHeader(http.StatusAccepted)
	}), store); err != nil {
		t.Fatal(err)
	}
	supervisor.mu.Lock()
	supervisor.workerManifest = WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}
	supervisor.mu.Unlock()

	rescueDone := make(chan struct{})
	go func() {
		defer close(rescueDone)
		request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString("{}"))
		request.Header.Set("X-Codex-Window-Id", "existing-window")
		supervisor.ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-admitted
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-rescueDone
	})

	exitContext, cancelExit := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelExit()
	exit := httptest.NewRequest(http.MethodPost, RuntimeRescueExitPath, nil).WithContext(exitContext)
	exit.Header.Set("Authorization", "Bearer local-token")
	exitResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(exitResponse, exit)
	if exitResponse.Code != http.StatusOK {
		t.Fatalf("exit = %d mode=%q body=%q", exitResponse.Code, supervisor.TrafficMode(), exitResponse.Body.String())
	}
	var exitStatus struct {
		Mode                 TrafficMode `json:"mode"`
		Generation           uint64      `json:"generation"`
		ActiveRescueRequests int         `json:"active_rescue_requests"`
		DrainingSessions     []string    `json:"draining_sessions"`
	}
	if err := json.Unmarshal(exitResponse.Body.Bytes(), &exitStatus); err != nil {
		t.Fatal(err)
	}
	wantSession := hashPrefix("codex-window", "existing-window")
	if exitStatus.Mode != TrafficModeRescueExitDraining || exitStatus.Generation != 5 || exitStatus.ActiveRescueRequests != 1 || len(exitStatus.DrainingSessions) != 1 || exitStatus.DrainingSessions[0] != wantSession {
		t.Fatalf("exit status = %#v", exitStatus)
	}

	repeatedExit := httptest.NewRequest(http.MethodPost, RuntimeRescueExitPath, nil)
	repeatedExit.Header.Set("Authorization", "Bearer local-token")
	repeatedExitResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(repeatedExitResponse, repeatedExit)
	if repeatedExitResponse.Code != http.StatusOK {
		t.Fatalf("repeated exit = %d mode=%q body=%q", repeatedExitResponse.Code, supervisor.TrafficMode(), repeatedExitResponse.Body.String())
	}

	normal := httptest.NewRequest(http.MethodPost, "/normal?x=1", bytes.NewBufferString("body"))
	normal.Header.Set("Authorization", "Bearer local-token")
	normalResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(normalResponse, normal)
	if normalResponse.Code != http.StatusAccepted || normalResponse.Body.String() != "normal" {
		t.Fatalf("new ingress = %d %q", normalResponse.Code, normalResponse.Body.String())
	}

	healthResponse := httptest.NewRecorder()
	supervisor.ServeHTTP(healthResponse, httptest.NewRequest(http.MethodGet, "/health", nil))
	var health struct {
		Status               string      `json:"status"`
		DataPlaneReady       bool        `json:"data_plane_ready"`
		Mode                 TrafficMode `json:"mode"`
		ActiveRescueRequests int         `json:"active_rescue_requests"`
		DrainingSessions     []string    `json:"draining_sessions"`
	}
	if err := json.Unmarshal(healthResponse.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if healthResponse.Code != http.StatusOK || health.Status != "ok" || !health.DataPlaneReady || health.Mode != TrafficModeRescueExitDraining || health.ActiveRescueRequests != 1 || len(health.DrainingSessions) != 1 || health.DrainingSessions[0] != wantSession {
		t.Fatalf("draining health = %d %#v", healthResponse.Code, health)
	}

	close(release)
	<-rescueDone
	waitForRuntimeMode(t, supervisor, TrafficModeNormal)
	if !supervisor.AdmissionReady() || len(store.records) != 3 || store.records[2].EffectiveMode != TrafficModeNormal {
		t.Fatalf("completed exit = mode %q ready %v records %#v", supervisor.TrafficMode(), supervisor.AdmissionReady(), store.records)
	}
}

func TestRuntimeSupervisorRescueControlRejectsBeforeBodyAndDeniesRegisteredBearer(t *testing.T) {
	events := []string{}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	authority := testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: NormalCallerLocal, Bearer: "local-token", SubjectID: "local-owner"}}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)})
	if err := supervisor.SetCallerAuthority(authority); err != nil {
		t.Fatal(err)
	}
	body := &callerAuthorityCountingBody{reader: bytes.NewBufferString("forbidden")}
	request := httptest.NewRequest(http.MethodPost, RuntimeRescueEnterPath, body)
	request.Header.Set("Authorization", "Bearer hostile")
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || body.reads != 0 {
		t.Fatalf("response/body reads = %d/%d", response.Code, body.reads)
	}
	if !authority.DeniesBearer([]byte("local-token")) || authority.DeniesBearer([]byte("candidate-token")) {
		t.Fatal("registered bearer deny set is not exact")
	}
}

func TestRunAdoptedRuntimeSupervisorRestartsDirectlyInRescue(t *testing.T) {
	store := &runtimeEvidenceTestStore{records: []RuntimeModeEvidenceV1{{SchemaVersion: 1, Generation: 7, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescue, Phase: RuntimeModePhaseEffective}}}
	events := []string{}
	served := false
	err := RunAdoptedRuntimeSupervisorConfigured(
		context.Background(),
		&runtimeTestListener{},
		runtimeHolder("supervisor"),
		&runtimeTestLauncher{events: &events},
		&runtimeTestCheckpointStore{events: &events},
		nil,
		WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"},
		func(supervisor *RuntimeSupervisor) error {
			return supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), store)
		},
		func(_ context.Context, _ net.Listener, handler http.Handler) error {
			served = true
			supervisor := handler.(*RuntimeSupervisor)
			if supervisor.TrafficMode() != TrafficModeRescue || supervisor.AdmissionReady() {
				t.Fatalf("mode=%q ready=%v", supervisor.TrafficMode(), supervisor.AdmissionReady())
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !served || len(events) != 0 {
		t.Fatalf("served=%v events=%#v", served, events)
	}
}

type runtimeRescueFailingLauncher struct{ err error }

func (launcher runtimeRescueFailingLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	return nil, launcher.err
}

func TestRunAdoptedRuntimeSupervisorServesRescueControlWhenNormalWorkerBootFails(t *testing.T) {
	bootErr := errors.New("normal worker boot failed")
	served := false
	err := RunAdoptedRuntimeSupervisorConfigured(
		context.Background(),
		&runtimeTestListener{},
		runtimeHolder("supervisor"),
		runtimeRescueFailingLauncher{err: bootErr},
		&runtimeTestCheckpointStore{events: &[]string{}},
		nil,
		WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"},
		func(supervisor *RuntimeSupervisor) error {
			return supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &runtimeEvidenceTestStore{})
		},
		func(_ context.Context, _ net.Listener, handler http.Handler) error {
			served = true
			supervisor := handler.(*RuntimeSupervisor)
			if supervisor.AdmissionReady() {
				t.Fatal("normal admission enabled after failed worker boot")
			}
			return nil
		},
	)
	if !errors.Is(err, bootErr) || !served {
		t.Fatalf("error=%v served=%v", err, served)
	}
}
