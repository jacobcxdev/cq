package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

var (
	ErrAuthorityLockOrder      = errors.New("authority lock order inversion")
	ErrLifecycleHolderConflict = errors.New("lifecycle holder conflict")
	ErrLifecycleLockMode       = errors.New("invalid lifecycle lock mode")
	ErrLifecycleLockHeld       = errors.New("lifecycle lock held")
)

type LifecycleLockMode string

const (
	LifecycleExclusive LifecycleLockMode = "exclusive"
	LifecycleShared    LifecycleLockMode = "shared"
)

type AuthorityLockRank uint8

const (
	AuthorityLockLifecycle AuthorityLockRank = iota + 1
	AuthorityLockMutation
	AuthorityLockAuthority
	AuthorityLockCompatibility
)

type LifecycleLockDescription interface {
	Identity() fsutil.SecureFileIdentity
	DescriptionID() string
	Mode() LifecycleLockMode
	DowngradeToShared() error
	Close() error
}

type LifecycleLockBackend interface {
	CreateLifecycleDescription(context.Context, fsutil.SecureDirectory, string) (LifecycleLockDescription, error)
	AcquireLifecycleDescription(context.Context, fsutil.SecureDirectory, string, LifecycleLockMode) (LifecycleLockDescription, error)
}

type LifecycleHolderProof struct {
	LockIdentity  fsutil.SecureFileIdentity
	DescriptionID string
	Mode          LifecycleLockMode
}

type AuthorityLockOrder struct {
	mu   sync.Mutex
	held []AuthorityLockRank
}

func NewAuthorityLockOrder() *AuthorityLockOrder { return &AuthorityLockOrder{} }

func (order *AuthorityLockOrder) Acquire(rank AuthorityLockRank) (func() error, error) {
	if order == nil || rank < AuthorityLockLifecycle || rank > AuthorityLockCompatibility {
		return nil, ErrAuthorityLockOrder
	}
	order.mu.Lock()
	defer order.mu.Unlock()
	if len(order.held) != 0 && rank <= order.held[len(order.held)-1] {
		return nil, ErrAuthorityLockOrder
	}
	order.held = append(order.held, rank)
	released := false
	return func() error {
		order.mu.Lock()
		defer order.mu.Unlock()
		if released {
			return nil
		}
		if len(order.held) == 0 || order.held[len(order.held)-1] != rank {
			return ErrAuthorityLockOrder
		}
		released = true
		order.held = order.held[:len(order.held)-1]
		return nil
	}, nil
}

type LifecycleLockHandle struct {
	mu           sync.Mutex
	description  LifecycleLockDescription
	releaseOrder func() error
	released     bool
}

func AcquireLifecycle(ctx context.Context, backend LifecycleLockBackend, directory fsutil.SecureDirectory, basename string, mode LifecycleLockMode, order *AuthorityLockOrder) (*LifecycleLockHandle, error) {
	return acquireLifecycle(ctx, backend, directory, basename, mode, order, false)
}

func InitialiseLifecycle(ctx context.Context, backend LifecycleLockBackend, directory fsutil.SecureDirectory, basename string, order *AuthorityLockOrder) (*LifecycleLockHandle, error) {
	return acquireLifecycle(ctx, backend, directory, basename, LifecycleExclusive, order, true)
}

func acquireLifecycle(ctx context.Context, backend LifecycleLockBackend, directory fsutil.SecureDirectory, basename string, mode LifecycleLockMode, order *AuthorityLockOrder, initialise bool) (*LifecycleLockHandle, error) {
	if backend == nil || order == nil {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	if mode != LifecycleExclusive && mode != LifecycleShared {
		return nil, ErrLifecycleLockMode
	}
	name, err := lifecycleLockName(basename)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	releaseOrder, err := order.Acquire(AuthorityLockLifecycle)
	if err != nil {
		return nil, err
	}
	var description LifecycleLockDescription
	if initialise {
		description, err = backend.CreateLifecycleDescription(ctx, directory, name)
	} else {
		description, err = backend.AcquireLifecycleDescription(ctx, directory, name, mode)
	}
	if err != nil {
		_ = releaseOrder()
		return nil, err
	}
	if description == nil || description.DescriptionID() == "" || description.Identity().Links != 1 || description.Mode() != mode {
		if description != nil {
			_ = description.Close()
		}
		_ = releaseOrder()
		return nil, ErrLifecycleHolderConflict
	}
	return &LifecycleLockHandle{description: description, releaseOrder: releaseOrder}, nil
}

func (handle *LifecycleLockHandle) HolderProof() LifecycleHolderProof {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.released || handle.description == nil {
		return LifecycleHolderProof{}
	}
	return LifecycleHolderProof{LockIdentity: handle.description.Identity(), DescriptionID: handle.description.DescriptionID(), Mode: handle.description.Mode()}
}

func (handle *LifecycleLockHandle) DowngradeToShared() error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.released || handle.description == nil || handle.description.Mode() != LifecycleExclusive {
		return ErrLifecycleLockMode
	}
	identity := handle.description.Identity()
	descriptionID := handle.description.DescriptionID()
	if err := handle.description.DowngradeToShared(); err != nil {
		return err
	}
	if handle.description.Mode() != LifecycleShared || handle.description.Identity() != identity || handle.description.DescriptionID() != descriptionID {
		return fmt.Errorf("%w: downgrade replaced description", ErrLifecycleHolderConflict)
	}
	return nil
}

func (handle *LifecycleLockHandle) Release() error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.released {
		return nil
	}
	if err := handle.releaseOrder(); err != nil {
		return err
	}
	handle.released = true
	err := handle.description.Close()
	return err
}

func lifecycleLockName(basename string) (string, error) {
	if err := validateAuthorityEntryName(basename); err != nil {
		return "", err
	}
	name := ".cq-instance-" + basename + ".lifecycle.lock"
	if err := validateAuthorityEntryName(name); err != nil {
		return "", err
	}
	return name, nil
}

type ProductionLifecycleBackend struct {
	inspector fsutil.SecurePathInspector
	random    io.Reader
}

func NewProductionLifecycleBackend(inspector fsutil.SecurePathInspector, random io.Reader) *ProductionLifecycleBackend {
	return &ProductionLifecycleBackend{inspector: inspector, random: random}
}

func (backend *ProductionLifecycleBackend) AcquireLifecycleDescription(ctx context.Context, directory fsutil.SecureDirectory, name string, mode LifecycleLockMode) (LifecycleLockDescription, error) {
	if err := backend.validateAcquisition(ctx, directory, name, mode); err != nil {
		return nil, err
	}
	opened, err := directory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	file, ok := opened.(lifecycleDescriptionFile)
	if !ok {
		_ = opened.Close()
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return backend.retainLifecycleDescription(directory, name, mode, file, nil)
}

func (backend *ProductionLifecycleBackend) CreateLifecycleDescription(ctx context.Context, directory fsutil.SecureDirectory, name string) (LifecycleLockDescription, error) {
	if err := backend.validateAcquisition(ctx, directory, name, LifecycleExclusive); err != nil {
		return nil, err
	}
	created, err := directory.CreateExclusive(name, 0o600)
	if err != nil {
		return nil, err
	}
	file, ok := created.(lifecycleDescriptionFile)
	if !ok {
		_ = created.Close()
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return backend.retainLifecycleDescription(directory, name, LifecycleExclusive, file, func() error {
		if err := created.Sync(); err != nil {
			return err
		}
		return directory.Sync()
	})
}

func (backend *ProductionLifecycleBackend) validateAcquisition(ctx context.Context, directory fsutil.SecureDirectory, name string, mode LifecycleLockMode) error {
	if backend == nil || backend.inspector == nil || backend.random == nil || directory == nil {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	if !strings.HasPrefix(name, ".cq-instance-") || !strings.HasSuffix(name, ".lifecycle.lock") {
		return ErrAuthorityPathGrammar
	}
	basename := strings.TrimSuffix(strings.TrimPrefix(name, ".cq-instance-"), ".lifecycle.lock")
	derived, err := lifecycleLockName(basename)
	if err != nil || derived != name {
		return ErrAuthorityPathGrammar
	}
	if mode != LifecycleExclusive && mode != LifecycleShared {
		return ErrLifecycleLockMode
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateAuthorityDirectory(backend.inspector, directory); err != nil {
		return err
	}
	return nil
}

type lifecycleDescriptionFile interface {
	Stat() (os.FileInfo, error)
	Fd() uintptr
	Close() error
}

func ValidateDistinctLifecycleHolders(holders ...LifecycleHolderProof) error {
	if len(holders) < 2 {
		return ErrLifecycleHolderConflict
	}
	identity := holders[0].LockIdentity
	descriptions := make(map[string]struct{}, len(holders))
	for _, holder := range holders {
		if holder.Mode != LifecycleShared || holder.LockIdentity != identity || holder.LockIdentity.Links != 1 || holder.DescriptionID == "" {
			return ErrLifecycleHolderConflict
		}
		if _, duplicate := descriptions[holder.DescriptionID]; duplicate {
			return ErrLifecycleHolderConflict
		}
		descriptions[holder.DescriptionID] = struct{}{}
	}
	return nil
}
