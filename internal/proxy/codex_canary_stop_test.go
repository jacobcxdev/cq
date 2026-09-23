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

// Test hooks wrap retained directory capabilities; all state stays in MemFS.
type canaryStopOnceFS struct {
	*fsutil.MemFS
	onRead                                         func(string, string)
	onLock                                         func()
	lockError, lockCloseError, directoryCloseError bool
	beforeRemove                                   func(string, string)
	removeError, syncError, publishError           bool
}

func (fsys *canaryStopOnceFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &canaryStopOnceDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), fs: fsys, path: path}, nil
}

type canaryStopOnceDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	fs   *canaryStopOnceFS
	path string
}

func (directory *canaryStopOnceDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if directory.fs.onRead != nil {
		directory.fs.onRead(directory.path, name)
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}
func (directory *canaryStopOnceDirectory) OpenExclusiveLock(name string, mode os.FileMode) (fsutil.ExclusiveLock, error) {
	if name != ".codex-canary-stop-request.lock" {
		return directory.SecureDirectory.OpenExclusiveLock(name, mode)
	}
	if directory.fs.lockError {
		return nil, errors.New("lock unavailable")
	}
	lock, err := directory.SecureDirectory.OpenExclusiveLock(name, mode)
	if err != nil {
		return nil, err
	}
	if directory.fs.onLock != nil {
		directory.fs.onLock()
	}
	return canaryStopOnceLock{ExclusiveLock: lock, fail: directory.fs.lockCloseError}, nil
}
func (directory *canaryStopOnceDirectory) RemoveChecked(name string, identity fsutil.SecureFileIdentity) error {
	if directory.fs.beforeRemove != nil {
		directory.fs.beforeRemove(directory.path, name)
	}
	if directory.fs.removeError {
		return errors.New("remove failed")
	}
	return directory.IdentityBoundRemover.RemoveChecked(name, identity)
}
func (directory *canaryStopOnceDirectory) Sync() error {
	if directory.fs.syncError && directory.path == "/state/codex-canary-stop" {
		return errors.New("sync failed")
	}
	return directory.SecureDirectory.Sync()
}
func (directory *canaryStopOnceDirectory) RenameChecked(oldName, newName string, identity fsutil.SecureFileIdentity) error {
	if directory.fs.publishError && directory.path == "/state" && newName == "canary.json" {
		return errors.New("publish failed")
	}
	return directory.IdentityBoundRenamer.RenameChecked(oldName, newName, identity)
}
func (directory *canaryStopOnceDirectory) Close() error {
	err := directory.SecureDirectory.Close()
	if directory.fs.directoryCloseError {
		return errors.New("directory close failed")
	}
	return err
}

type canaryStopOnceLock struct {
	fsutil.ExclusiveLock
	fail bool
}

func (lock canaryStopOnceLock) Close() error {
	err := lock.ExclusiveLock.Close()
	if lock.fail {
		return errors.New("lock close failed")
	}
	return err
}

func TestRequestCodexCanaryStopOnceSerialisesPublishers(t *testing.T) {
	fsys := &canaryStopOnceFS{MemFS: fsutil.NewMemFS()}
	now := time.Now().UTC()
	path := "/state/canary.json"
	recorder, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	acquired := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	fsys.onLock = func() { once.Do(func() { close(acquired); <-release }) }
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("publisher panicked")
			}
		}()
		done <- RequestCodexCanaryStopOnce(fsys, path, nil, now)
	}()
	<-acquired
	other := RequestCodexCanaryStopOnce(fsys, path, nil, now)
	close(release)
	first := <-done
	if first != nil || !errors.Is(other, ErrCodexCanaryStopUnavailable) {
		t.Fatalf("publishers: %v / %v", first, other)
	}
	before, err := fsys.ReadFile("/state/codex-canary-stop/request.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = RequestCodexCanaryStopOnce(fsys, path, nil, now); err != nil {
		t.Fatal(err)
	}
	after, _ := fsys.ReadFile("/state/codex-canary-stop/request.json")
	if string(before) != string(after) {
		t.Fatal("repeat rewrote intent")
	}
}
func TestRequestCodexCanaryStopOnceSeesConcurrentClaim(t *testing.T) {
	fsys := &canaryStopOnceFS{MemFS: fsutil.NewMemFS()}
	now := time.Now().UTC()
	path := "/state/canary.json"
	// Serving recorder uses the underlying filesystem, independently of CLI reads.
	recorder, err := StartCodexCanary(fsys.MemFS, path, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	if err = RequestCodexCanaryStop(fsys.MemFS, path, nil, now); err != nil {
		t.Fatal(err)
	}
	before, _ := fsys.ReadFile("/state/codex-canary-stop/request.json")
	claimed := false
	fsys.onRead = func(directory, name string) {
		if !claimed && directory == "/state/codex-canary-stop" && name == "request.json" {
			claimed = true
			if _, err := claimCodexCanaryStopRequest(recorder, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = RequestCodexCanaryStopOnce(fsys, path, nil, now); err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("claim interleaving was not exercised")
	}
	if _, err = fsys.Stat("/state/codex-canary-stop/request.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("duplicate request published after claim")
	}
	after, _ := fsys.ReadFile("/state/codex-canary-stop/inflight.json")
	if string(before) != string(after) {
		t.Fatal("claim changed intent")
	}
	// A claimed intent remains valid after its original expiry.
	if err = RequestCodexCanaryStopOnce(fsys, path, nil, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}
func TestRequestCodexCanaryStopOnceFailsClosed(t *testing.T) {
	for _, kind := range []string{"lock", "lock-close", "directory-close", "invalid", "unsafe", "mismatch", "stale"} {
		t.Run(kind, func(t *testing.T) {
			fsys := &canaryStopOnceFS{MemFS: fsutil.NewMemFS()}
			now := time.Now().UTC()
			path := "/state/canary.json"
			recorder, err := StartCodexCanary(fsys.MemFS, path, nil, canaryTestTuple(), now)
			if err != nil {
				t.Fatal(err)
			}
			defer recorder.Close()
			if err = RequestCodexCanaryStop(fsys.MemFS, path, nil, now); err != nil {
				t.Fatal(err)
			}
			requestPath := "/state/codex-canary-stop/request.json"
			switch kind {
			case "lock":
				fsys.lockError = true
			case "lock-close":
				fsys.lockCloseError = true
			case "directory-close":
				fsys.directoryCloseError = true
			case "invalid":
				if err = fsys.WriteFile(requestPath, []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unsafe":
				if err = fsys.Chmod(requestPath, 0o644); err != nil {
					t.Fatal(err)
				}
			case "stale":
				now = now.Add(time.Hour)
			case "mismatch":
				body, _ := fsys.ReadFile(requestPath)
				var request codexCanaryStopRequest
				if err = json.Unmarshal(body, &request); err != nil {
					t.Fatal(err)
				}
				request.RunID, err = newCodexCanaryRandomID()
				if err != nil {
					t.Fatal(err)
				}
				request.MAC, err = codexCanaryStopRequestMAC(recorder.key, request)
				if err != nil {
					t.Fatal(err)
				}
				body, _ = json.MarshalIndent(request, "", "  ")
				if err = fsys.WriteFile(requestPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := fsys.ReadFile(requestPath)
			stateBefore, _ := fsys.ReadFile(path)
			if err = RequestCodexCanaryStopOnce(fsys, path, nil, now); !errors.Is(err, ErrCodexCanaryStopUnavailable) {
				t.Fatalf("error: %v", err)
			}
			after, _ := fsys.ReadFile(requestPath)
			stateAfter, _ := fsys.ReadFile(path)
			if string(before) != string(after) || string(stateBefore) != string(stateAfter) {
				t.Fatal("failed stop changed intent or state")
			}
		})
	}
}
func TestRequestCodexCanaryStopOnceFencesFinalisationRace(t *testing.T) {
	fsys := &canaryStopOnceFS{MemFS: fsutil.NewMemFS()}
	now := time.Now().UTC()
	path := "/state/canary.json"
	recorder, err := StartCodexCanary(fsys.MemFS, path, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	reads := 0
	finalised := false
	fsys.onRead = func(directory, name string) {
		if directory != "/state" || name != "canary.json" {
			return
		}
		reads++
		if reads != 3 {
			return
		}
		if err := RequestCodexCanaryStop(fsys.MemFS, path, nil, now); err != nil {
			t.Fatal(err)
		}
		claimed, err := claimCodexCanaryStopRequest(recorder, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = recorder.finaliseCodexCanaryStop(now.Add(time.Second), claimed, [32]byte{1}, 0); err != nil {
			t.Fatal(err)
		}
		finalised = true
	}
	if err = RequestCodexCanaryStopOnce(fsys, path, nil, now); !errors.Is(err, ErrCodexCanaryStopUnavailable) {
		t.Fatalf("race error: %v", err)
	}
	if !finalised {
		t.Fatalf("finalisation interleaving missed (reads=%d)", reads)
	}
	if _, err = fsys.Stat("/state/codex-canary-stop/request.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published after finalisation")
	}
}

func TestCodexCanaryCompletedRunCanRestartAndStop(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Now().UTC()
	path := "/state/canary.json"
	first, err := StartCodexCanaryOnce(fsys, path, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	finaliseCodexCanaryForTest(t, first, now.Add(time.Minute))
	firstID := first.State().RunID
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := StartCodexCanaryOnce(fsys, path, nil, canaryTestTuple(), now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.State().RunID == firstID {
		t.Fatal("run identity reused")
	}
	if err = RequestCodexCanaryStopOnce(fsys, path, nil, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("second run cannot stop: %v", err)
	}
}

func TestCodexCanaryRestartRetirementFailsClosed(t *testing.T) {
	for _, kind := range []string{"orphan", "mixed", "replacement", "remove", "sync", "publish", "close", "owner", "publisher"} {
		t.Run(kind, func(t *testing.T) {
			fsys := &canaryStopOnceFS{MemFS: fsutil.NewMemFS()}
			now := time.Now().UTC()
			path := "/state/canary.json"
			inflight := "/state/codex-canary-stop/inflight.json"
			first, err := StartCodexCanary(fsys.MemFS, path, nil, canaryTestTuple(), now)
			if err != nil {
				t.Fatal(err)
			}
			finaliseCodexCanaryForTest(t, first, now.Add(time.Minute))
			if kind != "owner" {
				if err = first.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				defer first.Close()
			}
			oldState, _ := fsys.ReadFile(path)
			oldIntent, _ := fsys.ReadFile(inflight)
			switch kind {
			case "orphan":
				if err = fsys.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "mixed":
				if err = fsys.WriteFile("/state/codex-canary-stop/request.json", oldIntent, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = fsys.WriteFile(inflight, []byte("unrelated-invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				fsys.beforeRemove = func(directory, name string) {
					if directory != "/state/codex-canary-stop" || name != "inflight.json" {
						return
					}
					if err := fsys.Remove(inflight); err != nil {
						t.Fatal(err)
					}
					if err := fsys.WriteFile(inflight, oldIntent, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "remove":
				fsys.removeError = true
			case "sync":
				fsys.syncError = true
			case "publish":
				fsys.publishError = true
			case "close":
				fsys.directoryCloseError = true
			case "publisher":
				lock, err := fsys.MemFS.OpenExclusiveLock("/state/.codex-canary-stop-request.lock", 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			recorder, err := StartCodexCanaryOnce(fsys, path, nil, canaryTestTuple(), now.Add(2*time.Minute))
			if err == nil || recorder != nil {
				if recorder != nil {
					_ = recorder.Close()
				}
				t.Fatal("unsafe restart succeeded")
			}
			if kind == "orphan" {
				if _, err = fsys.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("orphan intent authorised new run")
				}
			} else {
				after, _ := fsys.ReadFile(path)
				if string(oldState) != string(after) {
					t.Fatal("failed retirement replaced signed completed record")
				}
			}
			if kind == "mixed" {
				request, _ := fsys.ReadFile("/state/codex-canary-stop/request.json")
				if string(request) != string(oldIntent) {
					t.Fatal("valid intent deleted before all artifacts were validated")
				}
			}
			if kind == "replacement" {
				replacement, _ := fsys.ReadFile(inflight)
				if string(replacement) != string(oldIntent) {
					t.Fatal("replacement identity deleted")
				}
			}
			if kind == "sync" || kind == "publish" {
				// Retirement may have committed before a later failure. The old finalisation
				// remains sufficient proof and the next attempt must not recreate old intent.
				fsys.syncError = false
				fsys.publishError = false
				retried, err := StartCodexCanaryOnce(fsys, path, nil, canaryTestTuple(), now.Add(3*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				defer retried.Close()
				if retried.State().RunID == first.State().RunID {
					t.Fatal("retry did not create new run")
				}
			}
		})
	}
}
func TestCodexCanaryRestartRetirementHoldsServingOwner(t *testing.T) {
	fsys := &canaryStopOnceFS{MemFS: fsutil.NewMemFS()}
	now := time.Now().UTC()
	path := "/state/canary.json"
	first, err := StartCodexCanary(fsys.MemFS, path, nil, canaryTestTuple(), now)
	if err != nil {
		t.Fatal(err)
	}
	finaliseCodexCanaryForTest(t, first, now.Add(time.Minute))
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	attempted := false
	fsys.beforeRemove = func(directory, name string) {
		if directory != "/state/codex-canary-stop" || name != "inflight.json" {
			return
		}
		attempted = true
		other, err := StartCodexCanary(fsys.MemFS, path, nil, canaryTestTuple(), now.Add(2*time.Minute))
		if other != nil {
			_ = other.Close()
		}
		if !errors.Is(err, ErrCodexCanaryActive) {
			t.Fatalf("concurrent owner acquired during retirement: %v", err)
		}
	}
	second, err := StartCodexCanaryOnce(fsys, path, nil, canaryTestTuple(), now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !attempted {
		t.Fatal("owner interleaving not exercised")
	}
}
