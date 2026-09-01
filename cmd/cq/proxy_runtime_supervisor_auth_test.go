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
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
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

type runtimeSupervisorCallerResolver func(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error)

func (resolve runtimeSupervisorCallerResolver) ResolveExact(ctx context.Context, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
	return resolve(ctx, planned)
}

type runtimeSupervisorCallerInventory func(context.Context) (codexprov.Inventory, error)

func (inventory runtimeSupervisorCallerInventory) List(ctx context.Context) (codexprov.Inventory, error) {
	return inventory(ctx)
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

func TestServeRuntimeSupervisorWaitsForActiveRequestDrain(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- serveRuntimeSupervisor(ctx, listener, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			writer.WriteHeader(http.StatusNoContent)
		}))
	}()
	clientResult := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if response != nil {
			_ = response.Body.Close()
		}
		clientResult <- requestErr
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		t.Fatalf("serve returned before active request drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-clientResult; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after active request drained")
	}
}

func TestOwnedRuntimeSupervisorStartupSkipsCredentialDiscovery(t *testing.T) {
	oldAdopt := adoptProxyListenerFn
	oldRun := runProxyOwnedRuntimeFn
	oldLoad := loadProxyStartConfigFn
	oldDiscoverClaude := discoverClaudeAccountsFn
	oldHTTPClient := newHTTPClientFn
	t.Cleanup(func() {
		adoptProxyListenerFn = oldAdopt
		runProxyOwnedRuntimeFn = oldRun
		loadProxyStartConfigFn = oldLoad
		discoverClaudeAccountsFn = oldDiscoverClaude
		newHTTPClientFn = oldHTTPClient
	})

	want := errors.New("supervisor served")
	adoptProxyListenerFn = func() (net.Listener, error) { return nil, nil }
	loadProxyStartConfigFn = func() (*proxy.Config, error) { return &proxy.Config{Port: 29280}, nil }
	runProxyOwnedRuntimeFn = func(_ context.Context, port int, _ func(context.Context, net.Listener, http.Handler) error) (bool, error) {
		if port != 29280 {
			t.Fatalf("supervisor port = %d, want 29280", port)
		}
		return true, want
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

func TestNormalCallerCredentialsResolveExternalCodexBearer(t *testing.T) {
	account := codexprov.LogicalAccount{
		Key: "account-key",
		Identity: codexprov.AccountIdentity{
			AccountID: "account-id", UserID: "user-id",
		},
		Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: "account-key", CandidateID: "external-candidate"},
			Revision: "revision", Source: codexprov.SourceExternal, Routable: true,
		}},
	}
	resolverCalls := 0
	credentials, err := normalCallerCredentials(context.Background(), &proxy.Config{LocalToken: "local-token"}, nil, codexprov.Inventory{Accounts: []codexprov.LogicalAccount{account}}, runtimeSupervisorCallerResolver(func(_ context.Context, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
		resolverCalls++
		if planned.Ref != account.Candidates[0].Ref || planned.Revision != "revision" || planned.Source != codexprov.SourceExternal {
			t.Fatalf("resolved plan = %#v", planned)
		}
		return codexprov.CredentialMaterial{AccessToken: "external-token"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 1 || len(credentials) != 2 || credentials[1].Domain != proxy.NormalCallerCodex || credentials[1].Bearer != "external-token" {
		t.Fatalf("caller credentials = %#v, resolver calls = %d", credentials, resolverCalls)
	}
}

func TestNormalCallerCredentialsRetryStaleExternalCodexRevision(t *testing.T) {
	identity := codexprov.AccountIdentity{AccountID: "account-id", UserID: "user-id"}
	account := codexprov.LogicalAccount{
		Key:      "account-key",
		Identity: identity,
		Routable: true,
		Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: "account-key", CandidateID: "external-candidate"},
			Revision: "revision-a", Source: codexprov.SourceExternal, Routable: true,
		}},
	}
	refreshed := account
	refreshed.Candidates = append([]codexprov.CredentialCandidate(nil), account.Candidates...)
	refreshed.Candidates[0].Revision = "revision-b"
	listCalls := 0
	resolverCalls := 0
	credentials, err := normalCallerCredentialsFromInventory(
		context.Background(),
		&proxy.Config{LocalToken: "local-token"},
		nil,
		codexprov.Inventory{Accounts: []codexprov.LogicalAccount{account}},
		runtimeSupervisorCallerInventory(func(context.Context) (codexprov.Inventory, error) {
			listCalls++
			return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{refreshed}}, nil
		}),
		runtimeSupervisorCallerResolver(func(_ context.Context, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			resolverCalls++
			switch planned.Revision {
			case "revision-a":
				return codexprov.CredentialMaterial{}, codexprov.ErrStaleRevision
			case "revision-b":
				return codexprov.CredentialMaterial{
					AccessToken: "external-token",
					IDToken:     registryCredentialJWT(identity),
					AccountID:   identity.AccountID,
				}, nil
			default:
				t.Fatalf("resolved revision = %q", planned.Revision)
				return codexprov.CredentialMaterial{}, nil
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 1 || resolverCalls != 2 {
		t.Fatalf("stale retry calls = list %d resolve %d, want 1/2", listCalls, resolverCalls)
	}
	if len(credentials) != 2 || credentials[1].Bearer != "external-token" || credentials[1].SubjectID != "account-key\x00external-candidate\x00revision-b" {
		t.Fatalf("refreshed caller credentials = %#v", credentials)
	}
}

func TestNormalCallerCredentialsDeduplicateSharedAccountBearer(t *testing.T) {
	account := codexprov.LogicalAccount{
		Key:      "account-key",
		Identity: codexprov.AccountIdentity{AccountID: "account-id", UserID: "user-id"},
		Candidates: []codexprov.CredentialCandidate{
			{Ref: codexprov.CandidateRef{AccountKey: "account-key", CandidateID: "system-candidate"}, Revision: "system-revision", Source: codexprov.SourceSystem, Routable: true},
			{Ref: codexprov.CandidateRef{AccountKey: "account-key", CandidateID: "external-candidate"}, Revision: "external-revision", Source: codexprov.SourceExternal, Routable: true},
		},
	}
	credentials, err := normalCallerCredentials(context.Background(), &proxy.Config{LocalToken: "local-token"}, nil, codexprov.Inventory{Accounts: []codexprov.LogicalAccount{account}}, runtimeSupervisorCallerResolver(func(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
		return codexprov.CredentialMaterial{AccessToken: "shared-account-token"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 2 || credentials[1].Domain != proxy.NormalCallerCodex || credentials[1].Bearer != "shared-account-token" || credentials[1].SubjectID != "account-key\x00system-candidate\x00system-revision" {
		t.Fatalf("shared-account caller credentials = %#v", credentials)
	}
}

func TestNormalCallerCredentialsPreserveCrossAccountBearerAmbiguity(t *testing.T) {
	accounts := []codexprov.LogicalAccount{
		{
			Key: "account-a",
			Candidates: []codexprov.CredentialCandidate{{
				Ref: codexprov.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"}, Revision: "revision-a", Source: codexprov.SourceSystem, Routable: true,
			}},
		},
		{
			Key: "account-b",
			Candidates: []codexprov.CredentialCandidate{{
				Ref: codexprov.CandidateRef{AccountKey: "account-b", CandidateID: "candidate-b"}, Revision: "revision-b", Source: codexprov.SourceExternal, Routable: true,
			}},
		},
	}
	credentials, err := normalCallerCredentials(context.Background(), &proxy.Config{LocalToken: "local-token"}, nil, codexprov.Inventory{Accounts: accounts}, runtimeSupervisorCallerResolver(func(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
		return codexprov.CredentialMaterial{AccessToken: "cross-account-token"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 3 || credentials[1].Bearer != "cross-account-token" || credentials[2].Bearer != "cross-account-token" || credentials[1].SubjectID == credentials[2].SubjectID {
		t.Fatalf("cross-account caller credentials = %#v", credentials)
	}
}

func TestNormalCallerCredentialsUseCodexBearerExpiry(t *testing.T) {
	accessExpiry := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	identityExpiry := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	accessToken := registryAccessJWT(accessExpiry)
	account := codexprov.LogicalAccount{
		Key:      "account-key",
		Identity: codexprov.AccountIdentity{AccountID: "account-id", UserID: "user-id"},
		Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: "account-key", CandidateID: "system-candidate"},
			Revision: "revision", Source: codexprov.SourceSystem, Routable: true,
			AccessExpiresAt: identityExpiry,
		}},
	}
	credentials, err := normalCallerCredentials(context.Background(), &proxy.Config{LocalToken: "local-token"}, nil, codexprov.Inventory{Accounts: []codexprov.LogicalAccount{account}}, runtimeSupervisorCallerResolver(func(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
		return codexprov.CredentialMaterial{AccessToken: accessToken}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 2 || !credentials[1].ValidUntil.Equal(accessExpiry) {
		t.Fatalf("Codex caller expiry = %v, want bearer expiry %v", credentials[1].ValidUntil, accessExpiry)
	}
}

func TestNormalCallerCredentialsDoNotUseIdentityExpiryForOpaqueCodexBearer(t *testing.T) {
	identityExpiry := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	account := codexprov.LogicalAccount{
		Key:      "account-key",
		Identity: codexprov.AccountIdentity{AccountID: "account-id", UserID: "user-id"},
		Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: "account-key", CandidateID: "external-candidate"},
			Revision: "revision", Source: codexprov.SourceExternal, Routable: true,
			AccessExpiresAt: identityExpiry,
		}},
	}
	credentials, err := normalCallerCredentials(context.Background(), &proxy.Config{LocalToken: "local-token"}, nil, codexprov.Inventory{Accounts: []codexprov.LogicalAccount{account}}, runtimeSupervisorCallerResolver(func(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
		return codexprov.CredentialMaterial{AccessToken: "opaque-access-token"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 2 || !credentials[1].ValidUntil.IsZero() {
		t.Fatalf("opaque Codex caller expiry = %v, want unknown", credentials[1].ValidUntil)
	}
}

func TestNormalCallerCredentialsFailWhenExternalCodexBearerCannotResolve(t *testing.T) {
	want := errors.New("resolve failed")
	account := codexprov.LogicalAccount{
		Key:      "account-key",
		Identity: codexprov.AccountIdentity{AccountID: "account-id", UserID: "user-id"},
		Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: "account-key", CandidateID: "external-candidate"},
			Revision: "revision", Source: codexprov.SourceExternal, Routable: true,
		}},
	}
	_, err := normalCallerCredentials(context.Background(), &proxy.Config{LocalToken: "local-token"}, nil, codexprov.Inventory{Accounts: []codexprov.LogicalAccount{account}}, runtimeSupervisorCallerResolver(func(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
		return codexprov.CredentialMaterial{}, want
	}))
	if !errors.Is(err, want) {
		t.Fatalf("normalCallerCredentials error = %v, want %v", err, want)
	}
}
