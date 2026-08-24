package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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
	if exitResponse.Code != http.StatusOK || supervisor.TrafficMode() != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatalf("exit = %d mode=%q ready=%v body=%q", exitResponse.Code, supervisor.TrafficMode(), supervisor.AdmissionReady(), exitResponse.Body.String())
	}
	if len(consumer.consumed) != 2 {
		t.Fatalf("control consumptions = %d", len(consumer.consumed))
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
