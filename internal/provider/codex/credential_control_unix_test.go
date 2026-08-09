//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
)

func TestCredentialControlPreparedInitializesBeforeAccept(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	var initializerDone atomic.Bool
	var acceptReached atomic.Bool
	owner, err := openCredentialControlPrepared(context.Background(), path, coordinator, false, nil, func(_ context.Context, _ *CredentialCoordinator, capability CredentialOwnerCapability) error {
		if acceptReached.Load() {
			t.Fatal("accept hook ran before owner initializer")
		}
		if err := capability.AssertOwner(); err != nil {
			return err
		}
		initializerDone.Store(true)
		return nil
	}, func() {
		if !initializerDone.Load() {
			t.Fatal("accept hook ran before owner initializer completed")
		}
		acceptReached.Store(true)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if !acceptReached.Load() {
		t.Fatal("owner did not reach accept hook")
	}
	delegate, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer delegate.Close()
	if !owner.Owner() || delegate.Owner() {
		t.Fatalf("owner/delegate authority = %t/%t", owner.Owner(), delegate.Owner())
	}
}

func TestCredentialControlPreparedInitializerFailureRevokesAndCleansEndpoint(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	wantErr := errors.New("initialization failed")
	var captured CredentialOwnerCapability
	control, err := OpenCredentialControlPrepared(context.Background(), path, coordinator, func(_ context.Context, _ *CredentialCoordinator, capability CredentialOwnerCapability) error {
		captured = capability
		return wantErr
	})
	if control != nil || !errors.Is(err, wantErr) {
		t.Fatalf("prepared control = %v, %v, want initializer error", control, err)
	}
	if captured == nil || !errors.Is(captured.AssertOwner(), ErrCredentialOwnerRevoked) {
		t.Fatalf("initializer capability after failure = %v, want revoked", captured)
	}
	for _, artifact := range []string{path, credentialEndpointSidecarPath(path)} {
		if _, statErr := os.Lstat(artifact); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("initializer failure retained %s: %v", filepath.Base(artifact), statErr)
		}
	}
	assertSecureRegularFile(t, credentialEndpointLockPath(path), 0o600)
}

func TestCredentialControlPreparedInitializerPanicRevokesAndCleansEndpoint(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	var captured CredentialOwnerCapability
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = OpenCredentialControlPrepared(context.Background(), path, coordinator, func(_ context.Context, _ *CredentialCoordinator, capability CredentialOwnerCapability) error {
			captured = capability
			panic("initializer panic")
		})
	}()
	if recovered != "initializer panic" {
		t.Fatalf("recovered panic = %v", recovered)
	}
	if captured == nil || !errors.Is(captured.AssertOwner(), ErrCredentialOwnerRevoked) {
		t.Fatalf("initializer capability after panic = %v, want revoked", captured)
	}
	for _, artifact := range []string{path, credentialEndpointSidecarPath(path)} {
		if _, statErr := os.Lstat(artifact); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("initializer panic retained %s: %v", filepath.Base(artifact), statErr)
		}
	}
	assertSecureRegularFile(t, credentialEndpointLockPath(path), 0o600)
}

func TestCredentialControlPreparedDelegateSkipsInitializer(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	owner, err := OpenCredentialControlPrepared(context.Background(), path, coordinator, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var calls atomic.Int32
	delegate, err := OpenCredentialControlPrepared(context.Background(), path, coordinator, func(context.Context, *CredentialCoordinator, CredentialOwnerCapability) error {
		calls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer delegate.Close()
	if delegate.Owner() || calls.Load() != 0 {
		t.Fatalf("delegate owner=%t initializer calls=%d", delegate.Owner(), calls.Load())
	}
}

func TestCredentialControlFirstOwnerSecondDelegates(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	path := shortControlPath(t)
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("first control did not own endpoint")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}
	client, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Owner() {
		t.Fatal("second control unexpectedly owns endpoint")
	}
	ref, revision, err := client.SaveLogin(context.Background(), testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	if ref.AccountKey == "" || ref.CandidateID == "" || revision == "" {
		t.Fatalf("delegated reply = %+v %q", ref, revision)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	inventory, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Accounts) != 1 || inventory.Accounts[0].Key != ref.AccountKey {
		t.Fatalf("delegated inventory = %+v", inventory)
	}
	material, err := client.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if material.AccessToken != "access" {
		t.Fatalf("delegated material = %+v", material)
	}
}

func TestCredentialControlDelegatesRefreshToOwner(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		return &auth.CodexTokenResponse{AccessToken: "rpc-refreshed", RefreshToken: "rpc-rotated"}, nil
	}
	path := shortControlPath(t)
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	client, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("rpc@test.invalid", "rpc-account", "rpc-user", "plus")
	credential.Claims = auth.DecodeCodexClaims(credential.Tokens.IDToken)
	ref, revision, err := client.SaveLogin(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	result, err := client.Refresh(context.Background(), ref, revision)
	if err != nil {
		t.Fatal(err)
	}
	if result.Material.AccessToken != "rpc-refreshed" {
		t.Fatalf("result material did not come from owner")
	}
}

func TestCredentialControlResolveExactPreservesTypedStaleRevision(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	systemPath := "/fake/home/.codex/auth.json"
	jwt := fakeCodexJWT("rpc@example.test", "rpc-account", "rpc-user", "plus")
	fs.files[systemPath] = codexAuthJSON("first-secret", "rpc-account", jwt)
	fs.modes[systemPath] = 0o600

	path := shortControlPath(t)
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	client, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	inventory, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	planned := PlanCandidate(inventory.Accounts[0], inventory.Accounts[0].Candidates[0])
	fs.files[systemPath] = codexAuthJSON("second-secret", "rpc-account", jwt)

	material, err := client.ResolveExact(context.Background(), planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolveExact error = %T %v, want typed ErrStaleRevision", err, err)
	}
	if material != (CredentialMaterial{}) {
		t.Fatalf("ResolveExact returned stale replacement material: %+v", material)
	}
}

type blockingExternalCredentialSource struct {
	started    chan struct{}
	finished   chan struct{}
	release    chan struct{}
	startOnce  sync.Once
	finishOnce sync.Once
	candidate  ExternalCandidate
}

func (s *blockingExternalCredentialSource) Name() string { return "blocking-external" }
func (s *blockingExternalCredentialSource) List(context.Context) ([]ExternalCandidate, error) {
	return []ExternalCandidate{s.candidate}, nil
}
func (s *blockingExternalCredentialSource) Resolve(ctx context.Context, _ ExternalCandidateRef) (CredentialMaterial, error) {
	s.startOnce.Do(func() { close(s.started) })
	defer s.finishOnce.Do(func() { close(s.finished) })
	select {
	case <-ctx.Done():
		return CredentialMaterial{}, ctx.Err()
	case <-s.release:
		return testCredentialMaterial(s.candidate.Identity, "released"), nil
	}
}

func TestCredentialControlResolveExactHonoursClientCancellation(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	source := &blockingExternalCredentialSource{
		started: make(chan struct{}), finished: make(chan struct{}), release: make(chan struct{}),
		candidate: ExternalCandidate{
			Ref: ExternalCandidateRef{Source: "blocking-external", RecordID: "record-1", Revision: "revision-1"},
			Identity: AccountIdentity{
				AccountID: "rpc-account", UserID: "rpc-user", Email: "rpc@example.test",
			},
			Routable: true,
		},
	}
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	serverConn, clientConn := net.Pipe()
	server := rpc.NewServer()
	ownerControl := &CredentialControl{owner: true, coordinator: coordinator}
	if err := server.RegisterName("CredentialRPC", &credentialRPC{Coordinator: coordinator, Control: ownerControl}); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)
	client := &CredentialControl{client: rpc.NewClient(clientConn)}
	defer client.Close()
	defer serverConn.Close()
	defer close(source.release)

	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	planned := PlanCandidate(inventory.Accounts[0], inventory.Accounts[0].Candidates[0])
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, resolveErr := client.ResolveExact(ctx, planned)
		result <- resolveErr
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("delegated exact resolution did not reach owner")
	}
	cancel()
	select {
	case resolveErr := <-result:
		if !errors.Is(resolveErr, context.Canceled) {
			t.Fatalf("ResolveExact error = %v, want context.Canceled", resolveErr)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("delegated exact resolution ignored client cancellation")
	}
	select {
	case <-source.finished:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("delegated exact resolution did not cancel owner-side source resolution")
	}
}

type blockingListExternalCredentialSource struct {
	mu        sync.Mutex
	calls     int
	started   chan struct{}
	finished  chan struct{}
	release   chan struct{}
	candidate ExternalCandidate
}

func (s *blockingListExternalCredentialSource) Name() string { return "blocking-list" }
func (s *blockingListExternalCredentialSource) List(ctx context.Context) ([]ExternalCandidate, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return []ExternalCandidate{s.candidate}, nil
	}
	close(s.started)
	defer close(s.finished)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return []ExternalCandidate{s.candidate}, nil
	}
}
func (s *blockingListExternalCredentialSource) Resolve(context.Context, ExternalCandidateRef) (CredentialMaterial, error) {
	return CredentialMaterial{}, errors.New("unexpected external resolution")
}

func TestResolvePlannedCandidateCancelsDelegatedInventoryRelist(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	identity := AccountIdentity{AccountID: "rpc-account", UserID: "rpc-user", Email: "rpc@example.test"}
	source := &blockingListExternalCredentialSource{
		started: make(chan struct{}), finished: make(chan struct{}), release: make(chan struct{}),
		candidate: ExternalCandidate{
			Ref:      ExternalCandidateRef{Source: "blocking-list", RecordID: "record-1", Revision: "revision-2"},
			Identity: identity, Routable: true,
		},
	}
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	serverConn, clientConn := net.Pipe()
	server := rpc.NewServer()
	ownerControl := &CredentialControl{owner: true, coordinator: coordinator}
	if err := server.RegisterName("CredentialRPC", &credentialRPC{Coordinator: coordinator, Control: ownerControl}); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)
	client := &CredentialControl{client: rpc.NewClient(clientConn)}
	defer client.Close()
	defer serverConn.Close()
	defer close(source.release)

	accountKey := generationAccountKey(identity, SourceExternal, source.Name()+":"+source.candidate.Ref.RecordID)
	planned := PlannedCandidate{
		Ref: CandidateRef{
			AccountKey:  accountKey,
			CandidateID: CandidateID(SourceExternal.String() + ":" + shortHash(source.Name()+":"+source.candidate.Ref.RecordID)),
		},
		Revision: "revision-1", Source: SourceExternal, Identity: identity,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, resolveErr := ResolvePlannedCandidate(ctx, client, client, planned)
		result <- resolveErr
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("stale replan did not reach delegated inventory relist")
	}
	cancel()
	select {
	case resolveErr := <-result:
		if !errors.Is(resolveErr, context.Canceled) {
			t.Fatalf("ResolvePlannedCandidate error = %v, want context.Canceled", resolveErr)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("stale replan ignored client cancellation during delegated inventory relist")
	}
	select {
	case <-source.finished:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("delegated inventory relist did not cancel owner-side source listing")
	}
}

func TestCredentialRPCCancellationCanArriveBeforeRequestBegins(t *testing.T) {
	rpcServer := &credentialRPC{}
	requestID := CredentialRPCRequestID{1}
	if err := rpcServer.CancelRequest(CancelCredentialRPCArgs{RequestID: requestID}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	ctx, finish := rpcServer.beginRequest(requestID)
	defer finish()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("request context error = %v, want context.Canceled", ctx.Err())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("request did not consume cancellation that arrived before registration")
	}
	rpcServer.requestMu.Lock()
	_, requestPresent := rpcServer.requests[requestID]
	_, pendingPresent := rpcServer.pending[requestID]
	rpcServer.requestMu.Unlock()
	if requestPresent || pendingPresent {
		t.Fatalf("request registry retained cancelled request: request=%t pending=%t", requestPresent, pendingPresent)
	}
}

func TestCredentialControlSimultaneousStartupHasOneOwner(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	controls := make(chan *CredentialControl, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			control, err := OpenCredentialControl(path, coordinator)
			controls <- control
			errs <- err
		}()
	}
	wg.Wait()
	close(controls)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	owners := 0
	for control := range controls {
		if control.Owner() {
			owners++
		}
		defer control.Close()
	}
	if owners != 1 {
		t.Fatalf("owners = %d, want 1", owners)
	}
}

func TestCredentialControlOwnerCloseDrainsClientsBeforeRebind(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	client, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if !replacement.Owner() {
		t.Fatal("replacement did not own drained endpoint")
	}
	if err := client.client.Call("CredentialRPC.Ping", struct{}{}, &struct{}{}); err == nil {
		t.Fatal("drained client remained usable")
	}
}

func TestCredentialControlStaleEndpointFailsClosed(t *testing.T) {
	path := shortControlPath(t)
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	control, err := OpenCredentialControl(path, nil)
	if control != nil || !errors.Is(err, ErrCredentialOwnerStale) {
		t.Fatalf("OpenCredentialControl = %v, %v, want stale-owner error", control, err)
	}
}

func shortControlPath(t *testing.T) string {
	t.Helper()
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(tempRoot, "cqctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}
