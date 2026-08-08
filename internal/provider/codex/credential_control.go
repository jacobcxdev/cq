package codex

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

var (
	ErrCredentialOwnerStale      = errors.New("credential coordinator endpoint exists but is unreachable")
	ErrCredentialControlDisabled = errors.New("credential coordinator control unavailable on this platform")
)

type CredentialControl struct {
	owner       bool
	coordinator *CredentialCoordinator
	client      credentialRPCClient
	close       func() error
}

type credentialRPCClient interface {
	Call(serviceMethod string, args any, reply any) error
	Close() error
}

func DefaultCredentialControlPath(home string) string {
	return filepath.Join(home, ".config", "cq", "state", "credential.sock")
}

// OpenDefaultCredentialControl connects to the per-user credential owner or
// becomes the ephemeral owner when no endpoint exists.
func OpenDefaultCredentialControl(ctx context.Context, fs fsutil.DurableFileSystem, exchanges ...RefreshExchange) (*CredentialControl, error) {
	store, err := NewManagedStore(fs)
	if err != nil {
		return nil, err
	}
	coordinator, err := NewCredentialCoordinator(store)
	if err != nil {
		return nil, err
	}
	if len(exchanges) > 0 {
		coordinator.RefreshExchange = exchanges[0]
	}
	control, err := OpenCredentialControl(DefaultCredentialControlPath(store.Home), coordinator)
	if err != nil {
		return nil, err
	}
	if control.Owner() {
		if _, err := coordinator.RecoverRemoval(ctx); err != nil {
			_ = control.Close()
			return nil, err
		}
	}
	return control, nil
}

type RefreshRPCArgs struct {
	Ref      CandidateRef
	Revision Revision
}

type RefreshRPCReply struct{ Result RefreshResult }

func (c *CredentialControl) Refresh(ctx context.Context, ref CandidateRef, revision Revision) (RefreshResult, error) {
	if c.owner {
		return c.coordinator.Refresh(ctx, ref, revision)
	}
	var reply RefreshRPCReply
	err := c.client.Call("CredentialRPC.Refresh", RefreshRPCArgs{Ref: ref, Revision: revision}, &reply)
	return reply.Result, err
}

func (c *CredentialControl) Owner() bool { return c != nil && c.owner }

func (c *CredentialControl) Close() error {
	if c == nil {
		return nil
	}
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
		return c.coordinator.SaveLogin(ctx, credential)
	}
	var reply SaveLoginRPCReply
	err := c.client.Call("CredentialRPC.SaveLogin", SaveLoginRPCArgs{Credential: credential}, &reply)
	return reply.Ref, reply.Revision, err
}

func (c *CredentialControl) List(ctx context.Context) (Inventory, error) {
	if c.owner {
		return c.coordinator.List(ctx)
	}
	var reply Inventory
	err := c.client.Call("CredentialRPC.List", struct{}{}, &reply)
	return reply, err
}

func (c *CredentialControl) Resolve(ctx context.Context, ref CandidateRef) (CredentialMaterial, error) {
	if c.owner {
		return c.coordinator.Resolve(ctx, ref)
	}
	var reply CredentialMaterial
	err := c.client.Call("CredentialRPC.Resolve", ref, &reply)
	return reply, err
}

type AdoptRPCArgs struct{ Snapshot SystemSnapshot }
type AdoptRPCReply struct {
	Ref      CandidateRef
	Revision Revision
}

func (c *CredentialControl) Adopt(ctx context.Context, snapshot SystemSnapshot) (CandidateRef, Revision, error) {
	if c.owner {
		return c.coordinator.Adopt(ctx, snapshot)
	}
	var reply AdoptRPCReply
	err := c.client.Call("CredentialRPC.Adopt", AdoptRPCArgs{Snapshot: snapshot}, &reply)
	return reply.Ref, reply.Revision, err
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
		return c.coordinator.Activate(ctx, ref, revision)
	}
	var reply ActivateRPCReply
	err := c.client.Call("CredentialRPC.Activate", ActivateRPCArgs{Ref: ref, Revision: revision}, &reply)
	result := ActivationResult{SystemCommitted: reply.SystemCommitted}
	if reply.ProjectionError != "" {
		result.ProjectionError = errors.New(reply.ProjectionError)
	}
	return result, err
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
		return c.coordinator.RemoveManaged(ctx, key, revisions, force)
	}
	var reply RemoveRPCReply
	err := c.client.Call("CredentialRPC.RemoveManaged", RemoveRPCArgs{AccountKey: key, Revisions: revisions, Force: force}, &reply)
	result := RemovalResult{ManagedDeleted: reply.ManagedDeleted, SystemDeactivated: reply.SystemDeactivated, PendingRecovery: reply.PendingRecovery}
	if reply.ProjectionError != "" {
		result.ProjectionError = errors.New(reply.ProjectionError)
	}
	return result, err
}

type credentialRPC struct{ Coordinator *CredentialCoordinator }

func (r *credentialRPC) Ping(_ struct{}, _ *struct{}) error { return nil }
func (r *credentialRPC) List(_ struct{}, reply *Inventory) error {
	result, err := r.Coordinator.List(context.Background())
	*reply = result
	return err
}
func (r *credentialRPC) Resolve(ref CandidateRef, reply *CredentialMaterial) error {
	result, err := r.Coordinator.Resolve(context.Background(), ref)
	*reply = result
	return err
}
func (r *credentialRPC) Refresh(args RefreshRPCArgs, reply *RefreshRPCReply) error {
	result, err := r.Coordinator.Refresh(context.Background(), args.Ref, args.Revision)
	reply.Result = result
	return err
}
func (r *credentialRPC) SaveLogin(args SaveLoginRPCArgs, reply *SaveLoginRPCReply) error {
	ref, revision, err := r.Coordinator.SaveLogin(context.Background(), args.Credential)
	reply.Ref, reply.Revision = ref, revision
	return err
}
func (r *credentialRPC) Adopt(args AdoptRPCArgs, reply *AdoptRPCReply) error {
	ref, revision, err := r.Coordinator.Adopt(context.Background(), args.Snapshot)
	reply.Ref, reply.Revision = ref, revision
	return err
}
func (r *credentialRPC) Activate(args ActivateRPCArgs, reply *ActivateRPCReply) error {
	result, err := r.Coordinator.Activate(context.Background(), args.Ref, args.Revision)
	reply.SystemCommitted = result.SystemCommitted
	if result.ProjectionError != nil {
		reply.ProjectionError = result.ProjectionError.Error()
	}
	return err
}
func (r *credentialRPC) RemoveManaged(args RemoveRPCArgs, reply *RemoveRPCReply) error {
	result, err := r.Coordinator.RemoveManaged(context.Background(), args.AccountKey, args.Revisions, args.Force)
	reply.ManagedDeleted = result.ManagedDeleted
	reply.SystemDeactivated = result.SystemDeactivated
	reply.PendingRecovery = result.PendingRecovery
	if result.ProjectionError != nil {
		reply.ProjectionError = result.ProjectionError.Error()
	}
	return err
}
