package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestServerCanaryStopClaimsDrainsAndFinalisesUnderServingProof(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Now().UTC()
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	native := &testCodexCanaryStopNativeHandler{}
	stop, err := NewCodexCanaryStopFunc(recorder, &CodexLeaseRuntime{}, native)
	if err != nil {
		t.Fatal(err)
	}
	listener := listenServingAttestorTestTCP4(t)
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: NewServingAttestor(),
		CodexNativeHTTP: native,
		CodexCanary:     recorder,
		CodexCanaryStop: stop,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.serve(ctx, listener); err != nil {
		t.Fatal(err)
	}
	state := recorder.State()
	if state.Active || state.Finalisation == nil || state.Finalisation.ActiveSessions != 0 || native.drains != 1 {
		t.Fatalf("final state = %+v, drains %d", state, native.drains)
	}
}

type testCodexCanaryStopNativeHandler struct {
	drains   int
	drainErr error
}

func (*testCodexCanaryStopNativeHandler) TryServe(http.ResponseWriter, *http.Request, bool) (bool, string) {
	return true, ""
}

func (handler *testCodexCanaryStopNativeHandler) CloseAndDrain(context.Context) error {
	handler.drains++
	return handler.drainErr
}

func TestServerCanaryStopDrainFailureKeepsActive(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Now().UTC()
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	native := &testCodexCanaryStopNativeHandler{drainErr: errors.New("synthetic drain failure")}
	stop, err := NewCodexCanaryStopFunc(recorder, &CodexLeaseRuntime{}, native)
	if err != nil {
		t.Fatal(err)
	}
	listener := listenServingAttestorTestTCP4(t)
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: NewServingAttestor(),
		CodexNativeHTTP: native,
		CodexCanary:     recorder,
		CodexCanaryStop: stop,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.serve(ctx, listener); err == nil || !strings.Contains(err.Error(), "Codex canary stop failed") {
		t.Fatalf("serve error = %v", err)
	}
	if state := recorder.State(); !state.Active || state.Finalisation != nil || native.drains != 1 {
		t.Fatalf("failed-drain state = %+v, drains %d", state, native.drains)
	}
}

func TestServerCanaryStopPromotionBlockKeepsActive(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Now().UTC()
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	native := &testCodexCanaryStopNativeHandler{}
	runtime := &CodexLeaseRuntime{nativeAdmission: &codexNativeHTTPAdmissionOwner{}}
	runtime.nativeAdmission.blocked.Store(true)
	stop, err := NewCodexCanaryStopFunc(recorder, runtime, native)
	if err != nil {
		t.Fatal(err)
	}
	listener := listenServingAttestorTestTCP4(t)
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: NewServingAttestor(),
		CodexNativeHTTP: native,
		CodexCanary:     recorder,
		CodexCanaryStop: stop,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.serve(ctx, listener); err == nil || !strings.Contains(err.Error(), "Codex canary stop failed") {
		t.Fatalf("serve error = %v", err)
	}
	if state := recorder.State(); !state.Active || state.Finalisation != nil || native.drains != 1 {
		t.Fatalf("promotion-blocked state = %+v, drains %d", state, native.drains)
	}
}

func TestRequestCodexCanaryStopLeavesActiveStateUntouched(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !recorder.State().Active {
		t.Fatal("stop request mutated the active canary state")
	}
	requestData, err := fsys.ReadFile(codexCanaryStopRequestPath(statePath))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestData), statePath) || strings.Contains(string(requestData), "credential") {
		t.Fatalf("stop request contains private fixture data: %s", requestData)
	}
	var request codexCanaryStopRequest
	if err := json.Unmarshal(requestData, &request); err != nil {
		t.Fatal(err)
	}
	if request.RunID != recorder.State().RunID || request.ObservedGeneration != recorder.generation || request.MAC == "" {
		t.Fatalf("stop request = %+v", request)
	}
}

func TestClaimCodexCanaryStopAcceptsInterveningGeneration(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	requestData, err := fsys.ReadFile(codexCanaryStopRequestPath(statePath))
	if err != nil {
		t.Fatal(err)
	}
	var request codexCanaryStopRequest
	if err := json.Unmarshal(requestData, &request); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordKeyedMismatch(); err != nil {
		t.Fatal(err)
	}
	if recorder.generation <= request.ObservedGeneration {
		t.Fatalf("generation = %d, observed = %d", recorder.generation, request.ObservedGeneration)
	}
	claimed, err := claimCodexCanaryStopRequest(recorder, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.request.Nonce != request.Nonce || claimed.digest == ([32]byte{}) {
		t.Fatalf("claimed stop = %+v", claimed)
	}
	if _, err := fsys.ReadFile(codexCanaryStopRequestPath(statePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request still exists: %v", err)
	}
	if _, err := fsys.ReadFile(codexCanaryStopInflightPath(statePath)); err != nil {
		t.Fatalf("inflight request: %v", err)
	}
}

func TestClaimCodexCanaryStopRejectsInvalidRequest(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*codexCanaryStopRequest)
		remac  bool
		append []byte
	}{
		{name: "wrong MAC", mutate: func(request *codexCanaryStopRequest) { request.MAC = "invalid" }},
		{name: "cross protocol operation", mutate: func(request *codexCanaryStopRequest) { request.Operation = "validate_http" }, remac: true},
		{name: "wrong run", mutate: func(request *codexCanaryStopRequest) { request.RunID = strings.Repeat("a", 43) }, remac: true},
		{name: "wrong readiness", mutate: func(request *codexCanaryStopRequest) { request.ReadinessFingerprint = strings.Repeat("c", 64) }, remac: true},
		{name: "expired", mutate: func(request *codexCanaryStopRequest) {
			request.RequestedAt = now.Add(-10 * time.Minute)
			request.ExpiresAt = request.RequestedAt.Add(codexCanaryStopRequestTTL)
		}, remac: true},
		{name: "noncanonical", append: []byte("\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			statePath := "/state/canary.json"
			recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
			if err != nil {
				t.Fatal(err)
			}
			if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
				t.Fatal(err)
			}
			requestPath := codexCanaryStopRequestPath(statePath)
			data, err := fsys.ReadFile(requestPath)
			if err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				var request codexCanaryStopRequest
				if err := json.Unmarshal(data, &request); err != nil {
					t.Fatal(err)
				}
				test.mutate(&request)
				if test.remac {
					request.MAC, err = codexCanaryStopRequestMAC(recorder.key, request)
					if err != nil {
						t.Fatal(err)
					}
				}
				data, err = json.MarshalIndent(request, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
			}
			data = append(data, test.append...)
			if err := fsys.WriteFile(requestPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := claimCodexCanaryStopRequest(recorder, now.Add(time.Minute)); !errors.Is(err, ErrCodexCanaryStopUnavailable) {
				t.Fatalf("claim error = %v", err)
			}
			if _, err := fsys.ReadFile(codexCanaryStopInflightPath(statePath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid request was claimed: %v", err)
			}
		})
	}
}

func TestClaimCodexCanaryStopResumesInflightAfterExpiry(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Rename(codexCanaryStopRequestPath(statePath), codexCanaryStopInflightPath(statePath)); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimCodexCanaryStopRequest(recorder, now.Add(2*codexCanaryStopRequestTTL))
	if err != nil || claimed.digest == ([32]byte{}) {
		t.Fatalf("resume inflight = %+v, %v", claimed, err)
	}
}

func TestRequestCodexCanaryStopRejectsDuplicateWithoutStateWrite(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	if _, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now); err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	before, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now.Add(time.Second)); !errors.Is(err, ErrCodexCanaryStopAlreadyRequested) {
		t.Fatalf("duplicate request error = %v", err)
	}
	after, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("duplicate stop request changed canary state")
	}
}

func TestFinaliseCodexCanaryStopBindsExactEnvelopeAndConcurrentCounters(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	const counterWrites = 16
	var wait sync.WaitGroup
	wait.Add(counterWrites)
	for range counterWrites {
		go func() {
			defer wait.Done()
			if err := recorder.RecordKeyedMismatch(); err != nil {
				t.Errorf("counter write: %v", err)
			}
		}()
	}
	wait.Wait()
	claimed, err := claimCodexCanaryStopRequest(recorder, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	processDigest := sha256.Sum256([]byte("sealed installed process fixture"))
	final, err := recorder.finaliseCodexCanaryStop(now.Add(2*time.Minute), claimed, processDigest, 0)
	if err != nil {
		t.Fatal(err)
	}
	state := recorder.State()
	if state.Active || state.KeyedMismatches != counterWrites || state.Finalisation == nil || state.Finalisation.ActiveSessions != 0 {
		t.Fatalf("final state = %+v", state)
	}
	persisted, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if final.generation != recorder.generation || final.envelopeDigest != sha256.Sum256(persisted) || final.countersDigest != codexCanaryCountersDigest(state) || final.stopRequestDigest != claimed.digest || final.processBindingDigest != processDigest {
		t.Fatalf("final envelope binding = %+v", final)
	}
	if _, err := OpenCodexCanary(fsys, statePath, nil); err != nil {
		t.Fatalf("open final signed envelope: %v", err)
	}
}

func TestFinaliseCodexCanaryStopRequiresZeroActiveSessions(t *testing.T) {
	fsys := fsutil.NewMemFS()
	statePath := "/state/canary.json"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimCodexCanaryStopRequest(recorder, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.finaliseCodexCanaryStop(now, claimed, [32]byte{1}, 1); !errors.Is(err, ErrCodexCanaryStopUnavailable) {
		t.Fatalf("active-session finalise error = %v", err)
	}
	after, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !recorder.State().Active {
		t.Fatal("failed active-session finalise mutated state")
	}
}

func TestFinaliseCodexCanaryStopWriteFailureKeepsInflightAndActive(t *testing.T) {
	fsys := &failingDurableFS{MemFS: fsutil.NewMemFS()}
	statePath := "/state/canary.json"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, statePath, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestCodexCanaryStop(fsys, statePath, nil, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimCodexCanaryStopRequest(recorder, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	fsys.failWrite = true
	if _, err := recorder.finaliseCodexCanaryStop(now, claimed, [32]byte{1}, 0); err == nil {
		t.Fatal("expected final state write failure")
	}
	after, err := fsys.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !recorder.State().Active {
		t.Fatal("failed final write changed active state")
	}
	if _, err := fsys.ReadFile(codexCanaryStopInflightPath(statePath)); err != nil {
		t.Fatalf("inflight request missing after failure: %v", err)
	}
}

func codexCanaryStopRequestPath(statePath string) string {
	return codexCanaryStopDirectoryPath(statePath) + "/" + codexCanaryStopRequestName
}

func codexCanaryStopInflightPath(statePath string) string {
	return codexCanaryStopDirectoryPath(statePath) + "/" + codexCanaryStopInflightName
}
