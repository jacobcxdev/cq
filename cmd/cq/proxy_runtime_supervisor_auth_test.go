package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type runtimeSupervisorStartupListener struct{}

func (runtimeSupervisorStartupListener) Accept() (net.Conn, error) { return nil, errors.New("closed") }
func (runtimeSupervisorStartupListener) Close() error              { return nil }
func (runtimeSupervisorStartupListener) Addr() net.Addr {
	return runtimeSupervisorStartupAddr("runtime")
}

type runtimeSupervisorStartupAddr string

func (address runtimeSupervisorStartupAddr) Network() string { return "test" }
func (address runtimeSupervisorStartupAddr) String() string  { return string(address) }

type runtimeSupervisorStartupWorker struct {
	index proxy.NormalCallerIndexV1
	key   []byte
	calls int
}

func (worker *runtimeSupervisorStartupWorker) Boot(context.Context, proxy.WorkerManifestV1) (proxy.RuntimeBootAckV1, error) {
	return proxy.RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: runtimeSupervisorStartupHolder("worker"), CallerIndex: worker.index, CallerAuthorityKey: append([]byte(nil), worker.key...)}, nil
}
func (*runtimeSupervisorStartupWorker) BeginDrain(context.Context, proxy.TrafficMode, uint64) error {
	return nil
}
func (*runtimeSupervisorStartupWorker) AwaitQuiescence(context.Context, uint64) (proxy.RuntimeQuiescenceAckV1, error) {
	return proxy.RuntimeQuiescenceAckV1{SchemaVersion: 1, Quiescent: true}, nil
}
func (*runtimeSupervisorStartupWorker) StopAndReap(context.Context) (proxy.RuntimeWorkerReleaseV1, error) {
	return proxy.RuntimeWorkerReleaseV1{ProcessIdentityDigest: "process", ProcessTreeAbsenceProofDigest: "absence", HolderReleaseProofDigest: "release"}, nil
}
func (worker *runtimeSupervisorStartupWorker) ExecuteHTTP(context.Context, proxy.RuntimeHTTPRequestV1) (proxy.RuntimeHTTPResponseV1, error) {
	worker.calls++
	return proxy.RuntimeHTTPResponseV1{StatusCode: http.StatusNoContent}, nil
}
func (*runtimeSupervisorStartupWorker) HolderProof() proxy.LifecycleHolderProof {
	return runtimeSupervisorStartupHolder("worker")
}

type runtimeSupervisorStartupLauncher struct{ worker proxy.RuntimeWorkerProcess }

func (launcher runtimeSupervisorStartupLauncher) Launch(context.Context, proxy.WorkerManifestV1) (proxy.RuntimeWorkerProcess, error) {
	return launcher.worker, nil
}

type runtimeSupervisorStartupCheckpoint struct{}

func (runtimeSupervisorStartupCheckpoint) Select(context.Context, proxy.RuntimeHolderCheckpointV1) (string, error) {
	return "checkpoint", nil
}

type runtimeSupervisorStartupConsumer struct{}

func (runtimeSupervisorStartupConsumer) Consume(context.Context, proxy.ProviderBranchAdmissionConsumptionV1) error {
	return nil
}

func runtimeSupervisorStartupHolder(description string) proxy.LifecycleHolderProof {
	identity := fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 1}
	return proxy.LifecycleHolderProof{LockIdentity: identity, DescriptionID: description, Mode: proxy.LifecycleShared}
}

func TestRuntimeSupervisorStartupSkipsCredentialDiscovery(t *testing.T) {
	oldAdopt := adoptProxyListenerFn
	oldRun := runProxyAdoptedRuntimeFn
	oldLoad := loadProxyStartConfigFn
	oldDiscoverClaude := discoverClaudeAccountsFn
	oldHTTPClient := newHTTPClientFn
	t.Cleanup(func() {
		adoptProxyListenerFn = oldAdopt
		runProxyAdoptedRuntimeFn = oldRun
		loadProxyStartConfigFn = oldLoad
		discoverClaudeAccountsFn = oldDiscoverClaude
		newHTTPClientFn = oldHTTPClient
	})

	want := errors.New("supervisor served")
	adoptProxyListenerFn = func() (net.Listener, error) { return runtimeSupervisorStartupListener{}, nil }
	runProxyAdoptedRuntimeFn = func(_ context.Context, listener net.Listener, _ func(context.Context, net.Listener, http.Handler) error) error {
		key := bytes.Repeat([]byte{0x65}, 32)
		index, err := proxy.BuildNormalCallerIndexV1(key, 1, []proxy.NormalCallerCredentialV1{{Domain: proxy.NormalCallerCodex, Bearer: "worker-bearer", SubjectID: "worker-subject"}})
		if err != nil {
			t.Fatal(err)
		}
		worker := &runtimeSupervisorStartupWorker{index: index, key: key}
		supervisor, err := proxy.NewRuntimeSupervisor(listener, runtimeSupervisorStartupHolder("supervisor"), runtimeSupervisorStartupLauncher{worker: worker}, runtimeSupervisorStartupCheckpoint{})
		if err != nil {
			t.Fatal(err)
		}
		if err := supervisor.SetCallerAdmissionConsumer(runtimeSupervisorStartupConsumer{}); err != nil {
			t.Fatal(err)
		}
		if _, err := supervisor.Boot(context.Background(), proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
		request.Header.Set("Authorization", "Bearer hostile")
		response := httptest.NewRecorder()
		supervisor.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || worker.calls != 0 {
			t.Fatalf("hostile response/worker calls = %d/%d", response.Code, worker.calls)
		}
		return want
	}
	loadProxyStartConfigFn = func() (*proxy.Config, error) {
		panic("supervisor loaded proxy config")
	}
	discoverClaudeAccountsFn = func() []keyring.ClaudeOAuth {
		panic("supervisor discovered Claude credentials")
	}
	newHTTPClientFn = func(time.Duration, string) httputil.Doer {
		panic("supervisor constructed provider client")
	}

	if err := runProxyStart(proxyCommandOptions{}); !errors.Is(err, want) {
		t.Fatalf("runProxyStart error = %v, want %v", err, want)
	}
}
