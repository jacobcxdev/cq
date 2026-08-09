package codex

import (
	"context"
	"errors"
	"net/rpc"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
)

type failingCredentialRPCClient struct {
	err          error
	beforeReturn func()
}

func (client failingCredentialRPCClient) Call(string, any, any) error {
	if client.beforeReturn != nil {
		client.beforeReturn()
	}
	return client.err
}

func (client failingCredentialRPCClient) Go(method string, args any, reply any, done chan *rpc.Call) *rpc.Call {
	call := &rpc.Call{ServiceMethod: method, Args: args, Reply: reply, Error: client.err, Done: done}
	done <- call
	return call
}

func (failingCredentialRPCClient) Close() error { return nil }

func TestCredentialControlAssertOwnerRejectsDelegate(t *testing.T) {
	control := &CredentialControl{}

	err := control.AssertOwner()
	if !errors.Is(err, ErrCredentialControlNotOwner) {
		t.Fatalf("AssertOwner error = %T %v, want ErrCredentialControlNotOwner", err, err)
	}
}

func TestCredentialControlAssertOwnerRejectsNilControl(t *testing.T) {
	var control *CredentialControl

	err := control.AssertOwner()
	if !errors.Is(err, ErrCredentialControlNotOwner) {
		t.Fatalf("AssertOwner error = %T %v, want ErrCredentialControlNotOwner", err, err)
	}
}

func TestInitialiseCredentialOwnerRecoversFullCredentialProjection(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	record, err := coordinator.Store.SaveNew(projectionCredential(
		"startup-recovery@example.test", "startup-account", "startup-user", "plus", time.Unix(123, 0),
	))
	if err != nil {
		t.Fatal(err)
	}
	installManagedDirectoryEntry(fs, record.Path)
	catalogue := &projectionCatalogueStub{}
	coordinator.Registry = catalogue
	control := &CredentialControl{owner: true, coordinator: coordinator}

	if err := initialiseCredentialOwner(context.Background(), coordinator, control); err != nil {
		t.Fatal(err)
	}
	if len(catalogue.upserts) != 1 || catalogue.upserts[0].AccountID != "startup-account" {
		t.Fatalf("startup projection upserts = %+v, want recovered managed account", catalogue.upserts)
	}
}

func TestInitialiseCredentialOwnerRejectsRevokedCapabilityBeforeRecovery(t *testing.T) {
	control := &CredentialControl{owner: true}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}

	err := initialiseCredentialOwner(context.Background(), nil, control)
	if !errors.Is(err, ErrCredentialOwnerRevoked) {
		t.Fatalf("initializer error = %T %v, want ErrCredentialOwnerRevoked", err, err)
	}
}

func TestCredentialControlCloseRevokesOwnerBeforeTeardown(t *testing.T) {
	teardownStarted := make(chan struct{})
	releaseTeardown := make(chan struct{})
	control := &CredentialControl{
		owner: true,
		close: func() error {
			close(teardownStarted)
			<-releaseTeardown
			return nil
		},
	}
	if err := control.AssertOwner(); err != nil {
		t.Fatalf("live owner AssertOwner error = %v", err)
	}

	closeResult := make(chan error, 1)
	go func() {
		closeResult <- control.Close()
	}()
	select {
	case <-teardownStarted:
	case <-time.After(time.Second):
		t.Fatal("Close did not start endpoint teardown")
	}
	if err := control.AssertOwner(); !errors.Is(err, ErrCredentialOwnerRevoked) {
		close(releaseTeardown)
		<-closeResult
		t.Fatalf("AssertOwner during teardown error = %T %v, want ErrCredentialOwnerRevoked", err, err)
	}
	if control.Owner() {
		close(releaseTeardown)
		<-closeResult
		t.Fatal("closed control still reports live ownership")
	}
	close(releaseTeardown)
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
	if err := control.AssertOwner(); !errors.Is(err, ErrCredentialOwnerRevoked) {
		t.Fatalf("AssertOwner after teardown error = %T %v, want ErrCredentialOwnerRevoked", err, err)
	}
}

func TestCredentialControlFailedTeardownLeavesOwnerRevoked(t *testing.T) {
	teardownErr := errors.New("synthetic endpoint teardown failure")
	control := &CredentialControl{
		owner: true,
		close: func() error {
			return teardownErr
		},
	}

	if err := control.Close(); !errors.Is(err, teardownErr) {
		t.Fatalf("Close error = %v, want teardown failure", err)
	}
	if err := control.AssertOwner(); !errors.Is(err, ErrCredentialOwnerRevoked) {
		t.Fatalf("AssertOwner error = %T %v, want ErrCredentialOwnerRevoked", err, err)
	}
}

func TestCredentialControlOwnerOperationFencesCloseUntilRelease(t *testing.T) {
	teardownStarted := make(chan struct{})
	control := &CredentialControl{
		owner: true,
		close: func() error {
			close(teardownStarted)
			return nil
		},
	}
	operation, err := control.BeginOwnerOperation()
	if err != nil {
		t.Fatalf("BeginOwnerOperation error = %v", err)
	}

	closeResult := make(chan error, 1)
	go func() {
		closeResult <- control.Close()
	}()
	waitForCredentialControlCloseStart(control)
	pendingOperation, err := control.BeginOwnerOperation()
	if pendingOperation != nil || !errors.Is(err, ErrCredentialOwnerRevoked) {
		t.Fatalf("closing BeginOwnerOperation = %v, %T %v, want nil and ErrCredentialOwnerRevoked", pendingOperation, err, err)
	}
	if err := control.AssertOwner(); !errors.Is(err, ErrCredentialOwnerRevoked) {
		t.Fatalf("closing AssertOwner error = %T %v, want ErrCredentialOwnerRevoked", err, err)
	}
	select {
	case <-teardownStarted:
		t.Fatal("endpoint teardown started while owner operation was active")
	default:
	}

	operation.Release()
	operation.Release()
	select {
	case <-teardownStarted:
	case <-time.After(time.Second):
		t.Fatal("endpoint teardown did not start after owner operation release")
	}
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
	if err := control.AssertOwner(); !errors.Is(err, ErrCredentialOwnerRevoked) {
		t.Fatalf("AssertOwner error = %T %v, want ErrCredentialOwnerRevoked", err, err)
	}
}

func TestCredentialControlBeginOwnerOperationRejectsNonOwners(t *testing.T) {
	tests := []struct {
		name    string
		control *CredentialControl
		wantErr error
	}{
		{name: "nil", wantErr: ErrCredentialControlNotOwner},
		{name: "delegate", control: &CredentialControl{}, wantErr: ErrCredentialControlNotOwner},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation, err := test.control.BeginOwnerOperation()
			if operation != nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("BeginOwnerOperation = %v, %T %v, want nil and %v", operation, err, err, test.wantErr)
			}
		})
	}

	closed := &CredentialControl{owner: true}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	operation, err := closed.BeginOwnerOperation()
	if operation != nil || !errors.Is(err, ErrCredentialOwnerRevoked) {
		t.Fatalf("closed BeginOwnerOperation = %v, %T %v, want nil and ErrCredentialOwnerRevoked", operation, err, err)
	}
}

func TestCredentialOwnerOperationReleaseIsNilSafe(t *testing.T) {
	var operation *CredentialOwnerOperation
	operation.Release()

	operation = &CredentialOwnerOperation{}
	operation.Release()
	operation.Release()
}

func TestCredentialOwnerOperationCopiesShareRelease(t *testing.T) {
	releaseCalls := 0
	operation := newCredentialOwnerOperation(func() { releaseCalls++ })
	copied := *operation

	operation.Release()
	copied.Release()

	if releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", releaseCalls)
	}
}

func credentialControlRPCMethodCalls() []struct {
	name string
	call func(*CredentialControl) error
} {
	return []struct {
		name string
		call func(*CredentialControl) error
	}{
		{name: "list", call: func(control *CredentialControl) error {
			_, err := control.List(context.Background())
			return err
		}},
		{name: "resolve", call: func(control *CredentialControl) error {
			_, err := control.Resolve(context.Background(), CandidateRef{})
			return err
		}},
		{name: "resolve exact", call: func(control *CredentialControl) error {
			_, err := control.ResolveExact(context.Background(), PlannedCandidate{})
			return err
		}},
		{name: "refresh reference", call: func(control *CredentialControl) error {
			_, _, err := control.RefreshReference(context.Background(), CandidateRef{}, "revision")
			return err
		}},
		{name: "save login", call: func(control *CredentialControl) error {
			_, _, err := control.SaveLogin(context.Background(), LoginCredential{})
			return err
		}},
		{name: "adopt", call: func(control *CredentialControl) error {
			_, _, err := control.Adopt(context.Background(), SystemSnapshot{})
			return err
		}},
		{name: "activate", call: func(control *CredentialControl) error {
			_, err := control.Activate(context.Background(), CandidateRef{}, "revision")
			return err
		}},
		{name: "remove", call: func(control *CredentialControl) error {
			_, err := control.RemoveManaged(context.Background(), "account", nil, false)
			return err
		}},
	}
}

func TestCredentialControlRPCTransportLossReturnsTypedAuthorityUnavailable(t *testing.T) {
	transportErr := errors.New("dial /private/sensitive/credential.sock: connection reset")
	for _, test := range credentialControlRPCMethodCalls() {
		t.Run(test.name, func(t *testing.T) {
			control := &CredentialControl{client: failingCredentialRPCClient{err: transportErr}}
			err := test.call(control)
			if !errors.Is(err, ErrCredentialAuthorityUnavailable) {
				t.Fatalf("error = %T %v, want ErrCredentialAuthorityUnavailable", err, err)
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("authority error leaks transport details: %v", err)
			}
		})
	}
}

func TestCredentialControlRPCServerErrorIsNotAuthorityUnavailable(t *testing.T) {
	want := rpc.ServerError("synthetic coordinator domain error")
	for _, test := range credentialControlRPCMethodCalls() {
		t.Run(test.name, func(t *testing.T) {
			control := &CredentialControl{client: failingCredentialRPCClient{err: want}}
			err := test.call(control)
			if err == nil || err.Error() != want.Error() {
				t.Fatalf("error = %T %v, want server domain error", err, err)
			}
			if errors.Is(err, ErrCredentialAuthorityUnavailable) {
				t.Fatalf("server domain error misclassified as authority loss: %v", err)
			}
		})
	}
}

func TestCredentialControlRPCServerAuthorityErrorRestoresTypedSentinel(t *testing.T) {
	control := &CredentialControl{client: failingCredentialRPCClient{
		err: rpc.ServerError(ErrCredentialAuthorityUnavailable.Error()),
	}}

	_, err := control.List(context.Background())
	if !errors.Is(err, ErrCredentialAuthorityUnavailable) {
		t.Fatalf("List error = %T %v, want typed authority unavailable", err, err)
	}
}

func TestCredentialControlClosedOwnerOperationsReturnAuthorityUnavailable(t *testing.T) {
	control := &CredentialControl{owner: true}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "list", call: func() error {
			_, err := control.List(context.Background())
			return err
		}},
		{name: "resolve exact", call: func() error {
			_, err := control.ResolveExact(context.Background(), PlannedCandidate{})
			return err
		}},
		{name: "refresh reference", call: func() error {
			_, _, err := control.RefreshReference(context.Background(), CandidateRef{}, "revision")
			return err
		}},
		{name: "resolve", call: func() error {
			_, err := control.Resolve(context.Background(), CandidateRef{})
			return err
		}},
		{name: "save login", call: func() error {
			_, _, err := control.SaveLogin(context.Background(), LoginCredential{})
			return err
		}},
		{name: "adopt", call: func() error {
			_, _, err := control.Adopt(context.Background(), SystemSnapshot{})
			return err
		}},
		{name: "activate", call: func() error {
			_, err := control.Activate(context.Background(), CandidateRef{}, "revision")
			return err
		}},
		{name: "remove", call: func() error {
			_, err := control.RemoveManaged(context.Background(), "account", nil, false)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, ErrCredentialAuthorityUnavailable) {
				t.Fatalf("error = %T %v, want ErrCredentialAuthorityUnavailable", err, err)
			}
		})
	}
}

func TestCredentialControlRefreshReferencePreservesCancelledContext(t *testing.T) {
	control := &CredentialControl{owner: true}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := control.RefreshReference(ctx, CandidateRef{}, "revision")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshReference error = %T %v, want context.Canceled", err, err)
	}
}

func TestCredentialControlRefreshReferencePreservesRPCContextErrors(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			control := &CredentialControl{client: failingCredentialRPCClient{err: want}}
			_, _, err := control.RefreshReference(context.Background(), CandidateRef{}, "revision")
			if !errors.Is(err, want) {
				t.Fatalf("RefreshReference error = %T %v, want %v", err, err, want)
			}
			if errors.Is(err, ErrCredentialAuthorityUnavailable) {
				t.Fatalf("context error misclassified as authority loss: %v", err)
			}
		})
	}
}

func TestCredentialControlRefreshCancellationWinsOverConcurrentTransportLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	control := &CredentialControl{client: failingCredentialRPCClient{
		err:          errors.New("private transport failure"),
		beforeReturn: cancel,
	}}

	_, _, err := control.RefreshReference(ctx, CandidateRef{}, "revision")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshReference error = %T %v, want context.Canceled", err, err)
	}
}

type ownerFenceBlockingSource struct {
	started chan struct{}
	release chan struct{}
}

func (source *ownerFenceBlockingSource) Name() string { return "owner-fence-blocking" }
func (source *ownerFenceBlockingSource) List(context.Context) ([]ExternalCandidate, error) {
	close(source.started)
	<-source.release
	return nil, nil
}
func (source *ownerFenceBlockingSource) Resolve(context.Context, ExternalCandidateRef) (CredentialMaterial, error) {
	return CredentialMaterial{}, errors.New("unexpected resolve")
}

func TestCredentialRPCCloseWaitsForActiveReadAndRejectsNewRead(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	source := &ownerFenceBlockingSource{started: make(chan struct{}), release: make(chan struct{})}
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	control := &CredentialControl{owner: true, coordinator: coordinator}
	rpcServer := &credentialRPC{Coordinator: coordinator, Control: control}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- rpcServer.List(ListRPCArgs{RequestID: CredentialRPCRequestID{1}}, &Inventory{})
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("delegated read did not start")
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- control.Close() }()
	waitForCredentialControlCloseStart(control)
	if err := rpcServer.List(ListRPCArgs{RequestID: CredentialRPCRequestID{2}}, &Inventory{}); !errors.Is(err, ErrCredentialAuthorityUnavailable) {
		t.Fatalf("new delegated read error = %T %v, want authority unavailable", err, err)
	}

	close(source.release)
	if err := <-firstResult; err != nil {
		t.Fatalf("active delegated read error = %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
}

func TestCredentialRPCCloseWaitsForActiveMutationAndRejectsNewMutation(t *testing.T) {
	coordinator, _, ref, revision := testRefreshRecord(t)
	exchangeStarted := make(chan struct{})
	releaseExchange := make(chan struct{})
	coordinator.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		close(exchangeStarted)
		<-releaseExchange
		return successfulRefresh(context.Background(), "")
	}
	control := &CredentialControl{owner: true, coordinator: coordinator}
	rpcServer := &credentialRPC{Coordinator: coordinator, Control: control}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- rpcServer.Refresh(RefreshRPCArgs{Ref: ref, Revision: revision}, &RefreshRPCReply{})
	}()
	select {
	case <-exchangeStarted:
	case <-time.After(time.Second):
		t.Fatal("delegated mutation did not start")
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- control.Close() }()
	waitForCredentialControlCloseStart(control)
	if err := rpcServer.SaveLogin(SaveLoginRPCArgs{}, &SaveLoginRPCReply{}); !errors.Is(err, ErrCredentialAuthorityUnavailable) {
		t.Fatalf("new delegated mutation error = %T %v, want authority unavailable", err, err)
	}

	close(releaseExchange)
	if err := <-firstResult; err != nil {
		t.Fatalf("active delegated mutation error = %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
}

func TestCredentialRPCAllCoordinatorHandlersRejectRevokedOwner(t *testing.T) {
	control := &CredentialControl{owner: true}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	rpcServer := &credentialRPC{Control: control}
	tests := []struct {
		name string
		call func() error
	}{
		{name: "list", call: func() error { return rpcServer.List(ListRPCArgs{}, &Inventory{}) }},
		{name: "resolve", call: func() error { return rpcServer.Resolve(CandidateRef{}, &CredentialMaterial{}) }},
		{name: "resolve exact", call: func() error { return rpcServer.ResolveExact(ResolveExactRPCArgs{}, &ResolveExactRPCReply{}) }},
		{name: "refresh", call: func() error { return rpcServer.Refresh(RefreshRPCArgs{}, &RefreshRPCReply{}) }},
		{name: "save login", call: func() error { return rpcServer.SaveLogin(SaveLoginRPCArgs{}, &SaveLoginRPCReply{}) }},
		{name: "adopt", call: func() error { return rpcServer.Adopt(AdoptRPCArgs{}, &AdoptRPCReply{}) }},
		{name: "activate", call: func() error { return rpcServer.Activate(ActivateRPCArgs{}, &ActivateRPCReply{}) }},
		{name: "remove", call: func() error { return rpcServer.RemoveManaged(RemoveRPCArgs{}, &RemoveRPCReply{}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, ErrCredentialAuthorityUnavailable) {
				t.Fatalf("handler error = %T %v, want authority unavailable", err, err)
			}
		})
	}
}

func waitForCredentialControlCloseStart(control *CredentialControl) {
	control.ownerMu.Lock()
	defer control.ownerMu.Unlock()
	for !control.ownerClosing {
		control.ownerConditionLocked().Wait()
	}
}
