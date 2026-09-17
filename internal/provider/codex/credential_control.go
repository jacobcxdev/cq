package codex

import (
	"context"
	"crypto/rand"
	"errors"
	"net/rpc"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

var (
	ErrCredentialOwnerStale                 = errors.New("credential coordinator endpoint exists but is unreachable")
	ErrCredentialControlDisabled            = errors.New("credential coordinator control unavailable on this platform")
	ErrCredentialControlNotOwner            = errors.New("credential control does not own coordinator authority")
	ErrCredentialOwnerRevoked               = errors.New("credential coordinator owner authority revoked")
	ErrCredentialAuthorityUnavailable       = errors.New("credential authority unavailable")
	ErrCredentialInventoryDegraded          = errors.New("credential inventory degraded")
	ErrCredentialEndpointRecoveryUnrecorded = errors.New("credential endpoint recovery observation failed")
)

type CredentialControl struct {
	owner        bool
	ownerMu      sync.Mutex
	ownerCond    *sync.Cond
	ownerOps     int
	ownerClosing bool
	ownerRevoked bool
	coordinator  *CredentialCoordinator
	client       credentialRPCClient
	close        func() error
}

// CredentialOwnerCapability exposes live owner authority without exposing the
// control's coordinator or endpoint internals.
type CredentialOwnerCapability interface {
	AssertOwner() error
}

// CredentialOwnerInitializer prepares durable credential state before a new
// owner starts serving coordinator RPCs.
type CredentialOwnerInitializer func(context.Context, *CredentialCoordinator, CredentialOwnerCapability) error

// CredentialEndpointRecoveryRecorder records that supervised startup is about
// to mutate an exactly proved crash-recovery endpoint. The callback receives no
// credential, account, endpoint, or filesystem identifiers.
type CredentialEndpointRecoveryRecorder interface {
	RecordCredentialEndpointRecovery() error
}

// CredentialEndpointRecoveryRecorderFunc adapts a privacy-safe callback to a
// CredentialEndpointRecoveryRecorder.
type CredentialEndpointRecoveryRecorderFunc func() error

func (record CredentialEndpointRecoveryRecorderFunc) RecordCredentialEndpointRecovery() error {
	if record == nil {
		return ErrCredentialEndpointRecoveryUnrecorded
	}
	return record()
}

// CredentialOwnerOperation keeps owner authority live until Release.
type CredentialOwnerOperation struct {
	state *credentialOwnerOperationState
}

type credentialOwnerOperationState struct {
	releaseOnce sync.Once
	release     func()
}

func newCredentialOwnerOperation(release func()) *CredentialOwnerOperation {
	return &CredentialOwnerOperation{state: &credentialOwnerOperationState{release: release}}
}

// Release ends an owner-authorised operation. Repeated calls are safe.
func (operation *CredentialOwnerOperation) Release() {
	if operation == nil || operation.state == nil {
		return
	}
	operation.state.releaseOnce.Do(func() {
		if operation.state.release != nil {
			operation.state.release()
		}
	})
}

type credentialRPCClient interface {
	Call(serviceMethod string, args any, reply any) error
	Go(serviceMethod string, args any, reply any, done chan *rpc.Call) *rpc.Call
	Close() error
}

func DefaultCredentialControlPath(stateDir string) string {
	return filepath.Join(stateDir, "credential.sock")
}

// OpenDefaultCredentialControl connects to the per-user credential owner or
// becomes the ephemeral owner when no endpoint exists.
func OpenDefaultCredentialControl(ctx context.Context, fs fsutil.DurableFileSystem, exchanges ...RefreshExchange) (*CredentialControl, error) {
	coordinator, path, err := newDefaultCredentialCoordinator(fs, exchanges...)
	if err != nil {
		return nil, err
	}
	return OpenCredentialControlPrepared(ctx, path, coordinator, initialiseCredentialOwner)
}

// OpenDefaultRecoveringCredentialControl is reserved for supervised owner
// startup that may replace an exactly proved stale endpoint.
func OpenDefaultRecoveringCredentialControl(ctx context.Context, fs fsutil.DurableFileSystem, exchanges ...RefreshExchange) (*CredentialControl, error) {
	coordinator, path, err := newDefaultCredentialCoordinator(fs, exchanges...)
	if err != nil {
		return nil, err
	}
	return OpenRecoveringCredentialControlPrepared(ctx, path, coordinator, initialiseCredentialOwner)
}

// OpenDefaultRecoveringCredentialControlWithLegacyMaintenanceVerifier is the
// supervised default-endpoint opener with the explicit owner-only legacy
// maintenance finalise verifier. Ordinary credential operations never invoke
// the verifier.
func OpenDefaultRecoveringCredentialControlWithLegacyMaintenanceVerifier(ctx context.Context, fs fsutil.DurableFileSystem, verifier LegacyMaintenanceFinaliseVerifier, exchanges ...RefreshExchange) (*CredentialControl, error) {
	coordinator, path, err := newDefaultCredentialCoordinator(fs, exchanges...)
	if err != nil {
		return nil, err
	}
	return OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx, path, coordinator, initialiseCredentialOwner, verifier)
}

// OpenDefaultRecoveringCredentialControlWithLegacyMaintenanceVerifierAndRecoveryRecorder
// is the supervised default-endpoint opener whose exact crash-recovery path is
// gated by a privacy-safe durable observation.
func OpenDefaultRecoveringCredentialControlWithLegacyMaintenanceVerifierAndRecoveryRecorder(ctx context.Context, fs fsutil.DurableFileSystem, verifier LegacyMaintenanceFinaliseVerifier, recorder CredentialEndpointRecoveryRecorder, exchanges ...RefreshExchange) (*CredentialControl, error) {
	coordinator, path, err := newDefaultCredentialCoordinator(fs, exchanges...)
	if err != nil {
		return nil, err
	}
	return OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(ctx, path, coordinator, initialiseCredentialOwner, verifier, recorder)
}

func newDefaultCredentialCoordinator(fs fsutil.DurableFileSystem, exchanges ...RefreshExchange) (*CredentialCoordinator, string, error) {
	roots, err := userdirs.Default(userdirs.StateRoot)
	if err != nil {
		return nil, "", err
	}
	store, err := NewManagedStore(fs)
	if err != nil {
		return nil, "", err
	}
	coordinator, err := NewCredentialCoordinator(store, roots.State)
	if err != nil {
		return nil, "", err
	}
	if len(exchanges) > 0 {
		coordinator.RefreshExchange = exchanges[0]
	}
	return coordinator, DefaultCredentialControlPath(roots.State), nil
}

func initialiseCredentialOwner(ctx context.Context, coordinator *CredentialCoordinator, capability CredentialOwnerCapability) error {
	if capability == nil {
		return ErrCredentialAuthorityUnavailable
	}
	if err := capability.AssertOwner(); err != nil {
		return err
	}
	return coordinator.RecoverCredentialState(ctx)
}

type RefreshRPCArgs struct {
	Ref      CandidateRef
	Revision Revision
}

type RefreshRPCReply struct{ Result RefreshResult }

func (c *CredentialControl) Refresh(ctx context.Context, ref CandidateRef, revision Revision) (RefreshResult, error) {
	if err := ctx.Err(); err != nil {
		return RefreshResult{}, err
	}
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return RefreshResult{}, err
		}
		defer operation.Release()
		return c.coordinator.Refresh(ctx, ref, revision)
	}
	var reply RefreshRPCReply
	err := c.client.Call("CredentialRPC.Refresh", RefreshRPCArgs{Ref: ref, Revision: revision}, &reply)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return RefreshResult{}, ctxErr
	}
	return reply.Result, credentialRPCError(err)
}

// RefreshReference discards refreshed secret material inside credential control.
func (c *CredentialControl) RefreshReference(ctx context.Context, ref CandidateRef, revision Revision) (CandidateRef, Revision, error) {
	result, err := c.Refresh(ctx, ref, revision)
	if err != nil {
		return CandidateRef{}, "", err
	}
	return result.Ref, result.Revision, nil
}

// AssertOwner verifies that this control still owns live coordinator authority.
func (c *CredentialControl) AssertOwner() error {
	if c == nil || !c.owner {
		return ErrCredentialControlNotOwner
	}
	c.ownerMu.Lock()
	unavailable := c.ownerClosing || c.ownerRevoked
	c.ownerMu.Unlock()
	if unavailable {
		return ErrCredentialOwnerRevoked
	}
	return nil
}

// BeginOwnerOperation holds live owner authority until its guard is released.
func (c *CredentialControl) BeginOwnerOperation() (*CredentialOwnerOperation, error) {
	if c == nil || !c.owner {
		return nil, ErrCredentialControlNotOwner
	}
	c.ownerMu.Lock()
	if c.ownerClosing || c.ownerRevoked {
		c.ownerMu.Unlock()
		return nil, ErrCredentialOwnerRevoked
	}
	c.ownerOps++
	c.ownerMu.Unlock()
	return newCredentialOwnerOperation(c.finishOwnerOperation), nil
}

func (c *CredentialControl) beginCredentialOwnerOperation() (*CredentialOwnerOperation, error) {
	operation, err := c.BeginOwnerOperation()
	if err != nil {
		return nil, ErrCredentialAuthorityUnavailable
	}
	return operation, nil
}

func credentialRPCError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var serverError rpc.ServerError
	if errors.As(err, &serverError) {
		switch serverError.Error() {
		case ErrCredentialAuthorityUnavailable.Error():
			return ErrCredentialAuthorityUnavailable
		case context.Canceled.Error():
			return context.Canceled
		case context.DeadlineExceeded.Error():
			return context.DeadlineExceeded
		}
		return err
	}
	return ErrCredentialAuthorityUnavailable
}

func (c *CredentialControl) finishOwnerOperation() {
	c.ownerMu.Lock()
	c.ownerOps--
	if c.ownerOps == 0 && c.ownerCond != nil {
		c.ownerCond.Broadcast()
	}
	c.ownerMu.Unlock()
}

func (c *CredentialControl) ownerConditionLocked() *sync.Cond {
	if c.ownerCond == nil {
		c.ownerCond = sync.NewCond(&c.ownerMu)
	}
	return c.ownerCond
}

func (c *CredentialControl) Owner() bool { return c.AssertOwner() == nil }

func (c *CredentialControl) Close() error {
	if c == nil {
		return nil
	}
	c.ownerMu.Lock()
	if c.owner {
		c.ownerClosing = true
		condition := c.ownerConditionLocked()
		condition.Broadcast()
		for c.ownerOps > 0 {
			condition.Wait()
		}
		c.ownerRevoked = true
	}
	c.ownerMu.Unlock()
	if c.close != nil {
		return c.close()
	}
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

type SaveLoginRPCArgs struct{ Credential LoginCredential }
type SaveLoginRPCReply struct {
	Ref      CandidateRef
	Revision Revision
}

func (c *CredentialControl) SaveLogin(ctx context.Context, credential LoginCredential) (CandidateRef, Revision, error) {
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return CandidateRef{}, "", err
		}
		defer operation.Release()
		return c.coordinator.SaveLogin(ctx, credential)
	}
	var reply SaveLoginRPCReply
	err := c.client.Call("CredentialRPC.SaveLogin", SaveLoginRPCArgs{Credential: credential}, &reply)
	return reply.Ref, reply.Revision, credentialRPCError(err)
}

type CredentialRPCRequestID [16]byte

type ListRPCArgs struct{ RequestID CredentialRPCRequestID }
type CancelCredentialRPCArgs struct{ RequestID CredentialRPCRequestID }

func newCredentialRPCRequestID() (CredentialRPCRequestID, error) {
	var requestID CredentialRPCRequestID
	if _, err := rand.Read(requestID[:]); err != nil {
		return CredentialRPCRequestID{}, errors.New("create credential RPC request ID")
	}
	return requestID, nil
}

func (c *CredentialControl) cancelRPCRequest(requestID CredentialRPCRequestID) {
	c.client.Go(
		"CredentialRPC.CancelRequest",
		CancelCredentialRPCArgs{RequestID: requestID},
		&struct{}{},
		make(chan *rpc.Call, 1),
	)
}

func (c *CredentialControl) List(ctx context.Context) (Inventory, error) {
	if err := ctx.Err(); err != nil {
		return Inventory{}, err
	}
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return Inventory{}, err
		}
		defer operation.Release()
		return c.coordinator.List(ctx)
	}
	requestID, err := newCredentialRPCRequestID()
	if err != nil {
		return Inventory{}, err
	}
	var reply Inventory
	call := c.client.Go("CredentialRPC.List", ListRPCArgs{RequestID: requestID}, &reply, make(chan *rpc.Call, 1))
	select {
	case <-ctx.Done():
		c.cancelRPCRequest(requestID)
		return Inventory{}, ctx.Err()
	case completed := <-call.Done:
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.cancelRPCRequest(requestID)
			return Inventory{}, ctxErr
		}
		return reply, credentialRPCError(completed.Error)
	}
}

func (c *CredentialControl) Resolve(ctx context.Context, ref CandidateRef) (CredentialMaterial, error) {
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return CredentialMaterial{}, err
		}
		defer operation.Release()
		return c.coordinator.Resolve(ctx, ref)
	}
	var reply CredentialMaterial
	err := c.client.Call("CredentialRPC.Resolve", ref, &reply)
	return reply, credentialRPCError(err)
}

type resolveErrorCode string

const (
	resolveErrorStaleRevision     resolveErrorCode = "stale_revision"
	resolveErrorInventoryDegraded resolveErrorCode = "inventory_degraded"
)

type ResolveExactRPCArgs struct {
	RequestID CredentialRPCRequestID
	Planned   PlannedCandidate
}
type ResolveExactRPCReply struct {
	Material  CredentialMaterial
	ErrorCode resolveErrorCode
}

func (c *CredentialControl) ResolveExact(ctx context.Context, planned PlannedCandidate) (CredentialMaterial, error) {
	if err := ctx.Err(); err != nil {
		return CredentialMaterial{}, err
	}
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return CredentialMaterial{}, err
		}
		defer operation.Release()
		return c.coordinator.ResolveExact(ctx, planned)
	}
	requestID, err := newCredentialRPCRequestID()
	if err != nil {
		return CredentialMaterial{}, err
	}
	var reply ResolveExactRPCReply
	call := c.client.Go("CredentialRPC.ResolveExact", ResolveExactRPCArgs{RequestID: requestID, Planned: planned}, &reply, make(chan *rpc.Call, 1))
	select {
	case <-ctx.Done():
		c.cancelRPCRequest(requestID)
		return CredentialMaterial{}, ctx.Err()
	case completed := <-call.Done:
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.cancelRPCRequest(requestID)
			return CredentialMaterial{}, ctxErr
		}
		if completed.Error != nil {
			return CredentialMaterial{}, credentialRPCError(completed.Error)
		}
	}
	switch reply.ErrorCode {
	case "":
		return reply.Material, nil
	case resolveErrorStaleRevision:
		return CredentialMaterial{}, ErrStaleRevision
	case resolveErrorInventoryDegraded:
		return CredentialMaterial{}, ErrCredentialInventoryDegraded
	default:
		return CredentialMaterial{}, errors.New("credential resolver returned an unknown typed error")
	}
}

type AdoptRPCArgs struct{ Snapshot SystemSnapshot }
type AdoptRPCReply struct {
	Ref      CandidateRef
	Revision Revision
}

type MigrateLegacyManagedRPCReply struct {
	Result LegacyManagedMigrationResult
}

func (c *CredentialControl) MigrateLegacyManaged(ctx context.Context) (LegacyManagedMigrationResult, error) {
	if c == nil {
		return LegacyManagedMigrationResult{}, ErrCredentialAuthorityUnavailable
	}
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return LegacyManagedMigrationResult{}, err
		}
		defer operation.Release()
		return c.coordinator.MigrateLegacyManaged(ctx)
	}
	var reply MigrateLegacyManagedRPCReply
	err := c.client.Call("CredentialRPC.MigrateLegacyManaged", struct{}{}, &reply)
	return reply.Result, credentialRPCError(err)
}

func (c *CredentialControl) Adopt(ctx context.Context, snapshot SystemSnapshot) (CandidateRef, Revision, error) {
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return CandidateRef{}, "", err
		}
		defer operation.Release()
		return c.coordinator.Adopt(ctx, snapshot)
	}
	var reply AdoptRPCReply
	err := c.client.Call("CredentialRPC.Adopt", AdoptRPCArgs{Snapshot: snapshot}, &reply)
	return reply.Ref, reply.Revision, credentialRPCError(err)
}

type ActivateRPCArgs struct {
	Ref      CandidateRef
	Revision Revision
}

type ActivateRPCReply struct {
	SystemCommitted bool
	ProjectionError string
}

func (c *CredentialControl) Activate(ctx context.Context, ref CandidateRef, revision Revision) (ActivationResult, error) {
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return ActivationResult{}, err
		}
		defer operation.Release()
		return c.coordinator.Activate(ctx, ref, revision)
	}
	var reply ActivateRPCReply
	err := c.client.Call("CredentialRPC.Activate", ActivateRPCArgs{Ref: ref, Revision: revision}, &reply)
	result := ActivationResult{SystemCommitted: reply.SystemCommitted}
	if reply.ProjectionError != "" {
		result.ProjectionError = errors.New(reply.ProjectionError)
	}
	return result, credentialRPCError(err)
}

type RemoveRPCArgs struct {
	AccountKey AccountKey
	Revisions  RevisionSet
	Force      bool
}

type RemoveRPCReply struct {
	ManagedDeleted    int
	SystemDeactivated bool
	ProjectionError   string
	PendingRecovery   bool
}

func (c *CredentialControl) RemoveManaged(ctx context.Context, key AccountKey, revisions RevisionSet, force bool) (RemovalResult, error) {
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return RemovalResult{}, err
		}
		defer operation.Release()
		return c.coordinator.RemoveManaged(ctx, key, revisions, force)
	}
	var reply RemoveRPCReply
	err := c.client.Call("CredentialRPC.RemoveManaged", RemoveRPCArgs{AccountKey: key, Revisions: revisions, Force: force}, &reply)
	result := RemovalResult{ManagedDeleted: reply.ManagedDeleted, SystemDeactivated: reply.SystemDeactivated, PendingRecovery: reply.PendingRecovery}
	if reply.ProjectionError != "" {
		result.ProjectionError = errors.New(reply.ProjectionError)
	}
	return result, credentialRPCError(err)
}

type credentialRPC struct {
	Coordinator *CredentialCoordinator
	Control     *CredentialControl
	requestMu   sync.Mutex
	requests    map[CredentialRPCRequestID]context.CancelFunc
	pending     map[CredentialRPCRequestID]struct{}
}

func (r *credentialRPC) Ping(_ struct{}, _ *struct{}) error { return nil }

func (r *credentialRPC) beginCoordinatorOperation() (*CredentialOwnerOperation, error) {
	if r.Control == nil {
		return nil, ErrCredentialAuthorityUnavailable
	}
	operation, err := r.Control.BeginOwnerOperation()
	if err != nil {
		return nil, ErrCredentialAuthorityUnavailable
	}
	return operation, nil
}

func (r *credentialRPC) List(args ListRPCArgs, reply *Inventory) error {
	ctx, finish := r.beginRequest(args.RequestID)
	defer finish()
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	result, err := r.Coordinator.List(ctx)
	*reply = result
	return err
}
func (r *credentialRPC) Resolve(ref CandidateRef, reply *CredentialMaterial) error {
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	result, err := r.Coordinator.Resolve(context.Background(), ref)
	*reply = result
	return err
}
func (r *credentialRPC) ResolveExact(args ResolveExactRPCArgs, reply *ResolveExactRPCReply) error {
	ctx, finish := r.beginRequest(args.RequestID)
	defer finish()
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	result, err := r.Coordinator.ResolveExact(ctx, args.Planned)
	if errors.Is(err, ErrStaleRevision) {
		reply.ErrorCode = resolveErrorStaleRevision
		return nil
	}
	if errors.Is(err, ErrCredentialInventoryDegraded) {
		reply.ErrorCode = resolveErrorInventoryDegraded
		return nil
	}
	reply.Material = result
	return err
}
func (r *credentialRPC) CancelRequest(args CancelCredentialRPCArgs, _ *struct{}) error {
	var cancel context.CancelFunc
	r.requestMu.Lock()
	cancel = r.requests[args.RequestID]
	if cancel == nil {
		if r.pending == nil {
			r.pending = make(map[CredentialRPCRequestID]struct{})
		}
		r.pending[args.RequestID] = struct{}{}
	}
	r.requestMu.Unlock()
	if cancel != nil {
		cancel()
		return nil
	}
	time.AfterFunc(time.Minute, func() {
		r.requestMu.Lock()
		delete(r.pending, args.RequestID)
		r.requestMu.Unlock()
	})
	return nil
}

func (r *credentialRPC) beginRequest(requestID CredentialRPCRequestID) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	r.requestMu.Lock()
	_, cancelled := r.pending[requestID]
	delete(r.pending, requestID)
	if !cancelled {
		if r.requests == nil {
			r.requests = make(map[CredentialRPCRequestID]context.CancelFunc)
		}
		r.requests[requestID] = cancel
	}
	r.requestMu.Unlock()
	if cancelled {
		cancel()
	}
	return ctx, func() {
		r.requestMu.Lock()
		delete(r.requests, requestID)
		r.requestMu.Unlock()
		cancel()
	}
}
func (r *credentialRPC) Refresh(args RefreshRPCArgs, reply *RefreshRPCReply) error {
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	result, err := r.Coordinator.Refresh(context.Background(), args.Ref, args.Revision)
	reply.Result = result
	return err
}
func (r *credentialRPC) SaveLogin(args SaveLoginRPCArgs, reply *SaveLoginRPCReply) error {
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	ref, revision, err := r.Coordinator.SaveLogin(context.Background(), args.Credential)
	reply.Ref, reply.Revision = ref, revision
	return err
}
func (r *credentialRPC) MigrateLegacyManaged(_ struct{}, reply *MigrateLegacyManagedRPCReply) error {
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	result, err := r.Coordinator.MigrateLegacyManaged(context.Background())
	reply.Result = result
	return err
}
func (r *credentialRPC) Adopt(args AdoptRPCArgs, reply *AdoptRPCReply) error {
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	ref, revision, err := r.Coordinator.Adopt(context.Background(), args.Snapshot)
	reply.Ref, reply.Revision = ref, revision
	return err
}
func (r *credentialRPC) Activate(args ActivateRPCArgs, reply *ActivateRPCReply) error {
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	result, err := r.Coordinator.Activate(context.Background(), args.Ref, args.Revision)
	reply.SystemCommitted = result.SystemCommitted
	if result.ProjectionError != nil {
		reply.ProjectionError = result.ProjectionError.Error()
	}
	return err
}
func (r *credentialRPC) RemoveManaged(args RemoveRPCArgs, reply *RemoveRPCReply) error {
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	result, err := r.Coordinator.RemoveManaged(context.Background(), args.AccountKey, args.Revisions, args.Force)
	reply.ManagedDeleted = result.ManagedDeleted
	reply.SystemDeactivated = result.SystemDeactivated
	reply.PendingRecovery = result.PendingRecovery
	if result.ProjectionError != nil {
		reply.ProjectionError = result.ProjectionError.Error()
	}
	return err
}

// CanonicalCredentialAdmin uses explicit V2 methods. An older broker rejects
// these names before mutation; callers must not fall back to legacy RPCs.
type CanonicalCredentialAdmin struct{ control *CredentialControl }

func (c *CredentialControl) CanonicalAdmin() *CanonicalCredentialAdmin {
	return &CanonicalCredentialAdmin{control: c}
}

type MutationFailure string

func mutationFailure(err error) MutationFailure {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, ErrCredentialAuthorityUnavailable), errors.Is(err, ErrCredentialControlDisabled), errors.Is(err, ErrCredentialOwnerRevoked):
		return "unavailable"
	case errors.Is(err, ErrStaleRevision):
		return "stale"
	case errors.Is(err, ErrAccountNotActivatable):
		return "not_activatable"
	}
	var reference *AccountReferenceError
	if errors.As(err, &reference) {
		return MutationFailure("reference_" + string(reference.Code))
	}
	return "io"
}
func (f MutationFailure) err() error {
	switch f {
	case "":
		return nil
	case "cancelled":
		return context.Canceled
	case "deadline":
		return context.DeadlineExceeded
	case "unavailable":
		return ErrCredentialAuthorityUnavailable
	case "stale":
		return ErrStaleRevision
	case "not_activatable":
		return ErrAccountNotActivatable
	case "reference_missing":
		return &AccountReferenceError{Code: AccountReferenceMissing}
	case "reference_ambiguous":
		return &AccountReferenceError{Code: AccountReferenceAmbiguous}
	case "reference_unstable":
		return &AccountReferenceError{Code: AccountReferenceUnstable}
	}
	return errors.New("credential mutation failed")
}

type SaveLoginV2Args struct {
	RequestID  CredentialRPCRequestID
	Deadline   time.Time
	Credential LoginCredential
}
type SaveLoginV2Reply struct {
	Ref      CandidateRef
	Revision Revision
	Failure  MutationFailure
}
type ActivateAccountV2Args struct {
	RequestID CredentialRPCRequestID
	Deadline  time.Time
	Selection ActivationSelection
}
type ActivateAccountV2Reply struct {
	SystemCommitted, Changed, ProjectionFailed bool
	Failure                                    MutationFailure
}
type RemoveManagedV2Args struct {
	RequestID   CredentialRPCRequestID
	Deadline    time.Time
	AccountKey  AccountKey
	Revisions   RevisionSet
	Force       bool
	OperationID string
	Native      *SystemSnapshot
}
type RemoveManagedV2Reply struct {
	ManagedDeleted                                       int
	SystemDeactivated, ProjectionFailed, PendingRecovery bool
	Failure                                              MutationFailure
}

func (c *CredentialControl) callCanonicalMutation(ctx context.Context, method string, requestID CredentialRPCRequestID, args, reply any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan *rpc.Call, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- &rpc.Call{Error: errors.New("credential RPC failed")}
			}
		}()
		c.client.Go(method, args, reply, done)
	}()
	select {
	case completed := <-done:
		if completed.Error != nil {
			var serverError rpc.ServerError
			if errors.As(completed.Error, &serverError) && strings.HasPrefix(string(serverError), "rpc: can't find method ") {
				return errors.New("canonical credential operation unavailable")
			}
			return &MutationOutcomeUnknown{Err: credentialRPCError(completed.Error)}
		}
		return nil
	case <-ctx.Done():
		go func() { defer func() { _ = recover() }(); c.cancelRPCRequest(requestID) }()
		return &MutationOutcomeUnknown{Err: ctx.Err()}
	}
}
func (a *CanonicalCredentialAdmin) SaveLogin(ctx context.Context, credential LoginCredential) (CandidateRef, Revision, error) {
	c := a.control
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return CandidateRef{}, "", err
		}
		defer operation.Release()
		return c.coordinator.SaveLoginCanonical(ctx, credential)
	}
	id, err := newCredentialRPCRequestID()
	if err != nil {
		return CandidateRef{}, "", err
	}
	deadline, _ := ctx.Deadline()
	reply := new(SaveLoginV2Reply)
	if err := c.callCanonicalMutation(ctx, "CredentialRPC.SaveLoginV2", id, SaveLoginV2Args{id, deadline, credential}, reply); err != nil {
		return CandidateRef{}, "", err
	}
	return reply.Ref, reply.Revision, reply.Failure.err()
}
func (a *CanonicalCredentialAdmin) Adopt(context.Context, SystemSnapshot) (CandidateRef, Revision, error) {
	return CandidateRef{}, "", errors.New("canonical login does not adopt credentials")
}
func (a *CanonicalCredentialAdmin) Activate(ctx context.Context, ref CandidateRef, revision Revision) (ActivationResult, error) {
	result, err := a.ActivateAccount(ctx, ActivationSelection{AccountKey: ref.AccountKey, Ref: ref, Revision: revision})
	return result.ActivationResult, err
}
func (a *CanonicalCredentialAdmin) ActivateAccount(ctx context.Context, selection ActivationSelection) (AccountActivationResult, error) {
	c := a.control
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return AccountActivationResult{}, err
		}
		defer operation.Release()
		return c.coordinator.ActivateAccount(ctx, selection)
	}
	id, err := newCredentialRPCRequestID()
	if err != nil {
		return AccountActivationResult{}, err
	}
	deadline, _ := ctx.Deadline()
	reply := new(ActivateAccountV2Reply)
	if err := c.callCanonicalMutation(ctx, "CredentialRPC.ActivateAccountV2", id, ActivateAccountV2Args{id, deadline, selection}, reply); err != nil {
		return AccountActivationResult{}, err
	}
	result := AccountActivationResult{ActivationResult: ActivationResult{SystemCommitted: reply.SystemCommitted}, Changed: reply.Changed}
	if reply.ProjectionFailed {
		result.ProjectionError = errors.New("account metadata projection failed")
	}
	return result, reply.Failure.err()
}
func (a *CanonicalCredentialAdmin) RemoveManaged(ctx context.Context, key AccountKey, revisions RevisionSet, force bool) (RemovalResult, error) {
	return a.RemoveSelected(ctx, key, revisions, "", nil)
}
func (a *CanonicalCredentialAdmin) RemoveSelected(ctx context.Context, key AccountKey, revisions RevisionSet, operationID string, native *SystemSnapshot) (RemovalResult, error) {
	c := a.control
	if c.owner {
		operation, err := c.beginCredentialOwnerOperation()
		if err != nil {
			return RemovalResult{}, err
		}
		defer operation.Release()
		return c.coordinator.RemoveSelected(ctx, key, revisions, operationID, native)
	}
	id, err := newCredentialRPCRequestID()
	if err != nil {
		return RemovalResult{}, err
	}
	deadline, _ := ctx.Deadline()
	reply := new(RemoveManagedV2Reply)
	if err := c.callCanonicalMutation(ctx, "CredentialRPC.RemoveManagedV2", id, RemoveManagedV2Args{RequestID: id, Deadline: deadline, AccountKey: key, Revisions: revisions, Force: false, OperationID: operationID, Native: native}, reply); err != nil {
		return RemovalResult{}, err
	}
	result := RemovalResult{ManagedDeleted: reply.ManagedDeleted, SystemDeactivated: reply.SystemDeactivated, PendingRecovery: reply.PendingRecovery}
	if reply.ProjectionFailed {
		result.ProjectionError = errors.New("account metadata projection failed")
	}
	return result, reply.Failure.err()
}
func (r *credentialRPC) beginMutationRequest(id CredentialRPCRequestID, deadline time.Time) (context.Context, func()) {
	ctx, done := r.beginRequest(id)
	if deadline.IsZero() {
		return ctx, done
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	return ctx, func() { cancel(); done() }
}
func (r *credentialRPC) SaveLoginV2(args SaveLoginV2Args, reply *SaveLoginV2Reply) error {
	ctx, done := r.beginMutationRequest(args.RequestID, args.Deadline)
	defer done()
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		reply.Failure = mutationFailure(err)
		return nil
	}
	defer operation.Release()
	reply.Ref, reply.Revision, err = r.Coordinator.SaveLoginCanonical(ctx, args.Credential)
	reply.Failure = mutationFailure(err)
	return nil
}
func (r *credentialRPC) ActivateAccountV2(args ActivateAccountV2Args, reply *ActivateAccountV2Reply) error {
	ctx, done := r.beginMutationRequest(args.RequestID, args.Deadline)
	defer done()
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		reply.Failure = mutationFailure(err)
		return nil
	}
	defer operation.Release()
	result, err := r.Coordinator.ActivateAccount(ctx, args.Selection)
	reply.SystemCommitted, reply.Changed, reply.ProjectionFailed, reply.Failure = result.SystemCommitted, result.Changed, result.ProjectionError != nil, mutationFailure(err)
	return nil
}
func (r *credentialRPC) RemoveManagedV2(args RemoveManagedV2Args, reply *RemoveManagedV2Reply) error {
	ctx, done := r.beginMutationRequest(args.RequestID, args.Deadline)
	defer done()
	operation, err := r.beginCoordinatorOperation()
	if err != nil {
		reply.Failure = mutationFailure(err)
		return nil
	}
	defer operation.Release()
	result, err := r.Coordinator.RemoveSelected(ctx, args.AccountKey, args.Revisions, args.OperationID, args.Native)
	reply.ManagedDeleted, reply.SystemDeactivated, reply.ProjectionFailed, reply.PendingRecovery, reply.Failure = result.ManagedDeleted, result.SystemDeactivated, result.ProjectionError != nil, result.PendingRecovery, mutationFailure(err)
	return nil
}

func (a *CanonicalCredentialAdmin) List(ctx context.Context) (Inventory, error) {
	if err := ctx.Err(); err != nil {
		return Inventory{}, err
	}
	type observation struct {
		inventory Inventory
		err       error
	}
	done := make(chan observation, 1)
	go func() {
		result := observation{}
		defer func() {
			if recover() != nil {
				result.err = ErrCredentialAuthorityUnavailable
			}
			done <- result
		}()
		result.inventory, result.err = a.control.List(ctx)
	}()
	select {
	case result := <-done:
		return result.inventory, result.err
	case <-ctx.Done():
		return Inventory{}, ctx.Err()
	}
}

// MutationOutcomeUnknown means a dispatched mutation has no terminal reply.
// Retrying or treating missing write evidence as rollback would be unsafe.
type MutationOutcomeUnknown struct{ Err error }

func (e *MutationOutcomeUnknown) Error() string { return "credential mutation outcome is unknown" }
func (e *MutationOutcomeUnknown) Unwrap() error { return e.Err }
func OpenDefaultCanonicalCredentialControl(ctx context.Context, fs fsutil.DurableFileSystem) (*CredentialControl, error) {
	coordinator, path, err := newDefaultCredentialCoordinator(fs)
	if err != nil {
		return nil, err
	}
	return OpenCredentialControlPrepared(ctx, path, coordinator, func(ctx context.Context, _ *CredentialCoordinator, capability CredentialOwnerCapability) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return capability.AssertOwner()
	})
}
