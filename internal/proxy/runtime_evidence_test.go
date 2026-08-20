package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type runtimeEvidenceTestStore struct {
	mu      sync.Mutex
	records []RuntimeModeEvidenceV1
	failAt  int
}

func (store *runtimeEvidenceTestStore) Load(context.Context) (RuntimeModeEvidenceV1, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) == 0 {
		return RuntimeModeEvidenceV1{}, false, nil
	}
	return store.records[len(store.records)-1], true, nil
}

func (store *runtimeEvidenceTestStore) Commit(_ context.Context, record RuntimeModeEvidenceV1) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failAt > 0 && len(store.records)+1 == store.failAt {
		return errors.New("injected evidence failure")
	}
	store.records = append(store.records, record)
	return nil
}

func TestRescueLifecycleRoutesNewIngressAndReapsWorker(t *testing.T) {
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	store := &runtimeEvidenceTestStore{}
	rescueCalls := 0
	if err := supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		rescueCalls++
		writer.WriteHeader(http.StatusAccepted)
	}), store); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.EnterRescue(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if response.Code != http.StatusAccepted || rescueCalls != 1 {
		t.Fatalf("rescue response = %d calls=%d", response.Code, rescueCalls)
	}
	if mode := supervisor.TrafficMode(); mode != TrafficModeRescue {
		t.Fatalf("mode = %q", mode)
	}
	if len(store.records) != 2 || store.records[0].EffectiveMode != TrafficModeRescueDraining || store.records[1].EffectiveMode != TrafficModeRescue {
		t.Fatalf("records = %#v", store.records)
	}
	want := []string{"boot:worker", "checkpoint:worker", "drain:worker", "quiesce:worker", "reap:worker"}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("events = %#v", events)
		}
	}
}

func TestRescueLifecycleExitBlocksIngressUntilWorkerReady(t *testing.T) {
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events}
	store := &runtimeEvidenceTestStore{records: []RuntimeModeEvidenceV1{{SchemaVersion: 1, Generation: 4, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescue, Phase: RuntimeModePhaseEffective}}}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusAccepted) }), store); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.ExitRescue(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	if mode := supervisor.TrafficMode(); mode != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatalf("mode=%q ready=%v", mode, supervisor.AdmissionReady())
	}
	if len(store.records) != 3 || store.records[1].EffectiveMode != TrafficModeRescueExitDraining || store.records[2].EffectiveMode != TrafficModeNormal {
		t.Fatalf("records = %#v", store.records)
	}
}

func TestRescueLifecycleRecoversCommittedDrainingIntent(t *testing.T) {
	events := []string{}
	worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events}
	store := &runtimeEvidenceTestStore{records: []RuntimeModeEvidenceV1{{SchemaVersion: 1, Generation: 9, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescueDraining, Phase: RuntimeModePhaseIntent}}}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.ConfigureRescue(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), store); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.ReconcileRescue(context.Background(), WorkerManifestV1{}); err != nil {
		t.Fatal(err)
	}
	if supervisor.TrafficMode() != TrafficModeRescue || len(store.records) != 2 || store.records[1].Generation != 9 {
		t.Fatalf("mode=%q records=%#v", supervisor.TrafficMode(), store.records)
	}
}

func TestRuntimeModeEvidenceStoreReopensSelectedTransition(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "runtime-mode.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x71}, 512)), lock)
	key := bytes.Repeat([]byte{0x72}, 32)
	store, err := OpenRuntimeModeEvidenceStore(context.Background(), filesystem, directory, publisher, key)
	if err != nil {
		t.Fatal(err)
	}
	intent := RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: 1, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescueDraining, Phase: RuntimeModePhaseIntent}
	if err := store.Commit(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRuntimeModeEvidenceStore(context.Background(), filesystem, directory, publisher, key)
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := reopened.Load(context.Background()); err != nil || !found || got != intent {
		t.Fatalf("reopened = %#v found=%v err=%v", got, found, err)
	}
	receipt := RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: 1, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescue, Phase: RuntimeModePhaseEffective}
	if err := reopened.Commit(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
}
