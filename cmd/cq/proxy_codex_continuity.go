package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type proxyCodexContinuityAuthority interface {
	proxy.CodexLeaseWriterAuthority
	List(context.Context) (codexprov.Inventory, error)
}

type proxyCodexContinuityDependencies struct {
	FS        fsutil.DurableFileSystem
	StateDir  string
	Routing   *proxy.CodexRoutingRuntime
	Retention time.Duration
	Now       func() time.Time
	Authority proxyCodexContinuityAuthority

	operations proxyCodexContinuityOperations
}

type proxyCodexContinuityOperations struct {
	initialise func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) error
	open       func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) (*proxy.CodexContinuityCoordinator, error)
	newRuntime func(*proxy.CodexContinuityCoordinator, proxy.CodexLeaseAccountRevalidator) (*proxy.CodexLeaseRuntime, error)
}

type proxyCodexContinuity struct {
	Coordinator *proxy.CodexContinuityCoordinator
	Runtime     *proxy.CodexLeaseRuntime
}

type proxyCodexRetainedHTTPAdapterUnavailableError struct{}

func (*proxyCodexRetainedHTTPAdapterUnavailableError) Error() string {
	return "Codex retained HTTP continuity adapter unavailable"
}

type proxyCodexNativeHTTPDependencies struct {
	Status            proxy.CodexModeStatus
	Inventory         codexprov.CredentialInventory
	Capacity          *proxy.CodexCapacityLedger
	Routes            proxy.CodexHTTPRequestRouteSnapshotter
	Runtime           proxy.CodexHTTPRequestPlanRuntime
	DefaultAccountKey codexprov.AccountKey
	Executor          proxy.CodexHTTPAttemptDispatcher
	Refresher         codexprov.CredentialReferenceRefresher
	Headroom          proxy.CodexRequestHeadroom
	HeadroomMode      proxy.HeadroomMode
	Upstream          string
	Now               func() time.Time

	newHandler func(proxy.CodexNativeHTTPRequestPlanner, proxy.CodexNativeHTTPRequestSession, string) (proxy.CodexNativeHTTPRoutingHandler, error)
}

func (continuity *proxyCodexContinuity) Close() error {
	if continuity == nil || continuity.Coordinator == nil {
		return nil
	}
	return continuity.Coordinator.Close()
}

func newProxyCodexNativeHTTP(deps proxyCodexNativeHTTPDependencies) (proxy.CodexNativeHTTPRoutingHandler, error) {
	if deps.Status.Effective != proxy.CodexRoutingEnforce {
		if err := proxyCodexRetainedHTTPAdapterBlocker(deps.Status); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if deps.Status.ModeEpoch == 0 || deps.Status.AuthoritativeEpoch != deps.Status.ModeEpoch {
		return nil, errors.New("Codex native HTTP authority epoch unavailable")
	}
	if deps.Inventory == nil || deps.Capacity == nil || deps.Routes == nil || deps.Runtime == nil || deps.Executor == nil || deps.Refresher == nil || deps.Now == nil {
		return nil, errors.New("Codex native HTTP dependencies unavailable")
	}

	planner := &proxy.CodexHTTPRequestPlanFactory{
		Inventory:         deps.Inventory,
		Capacity:          deps.Capacity,
		Routes:            deps.Routes,
		Runtime:           deps.Runtime,
		DefaultAccountKey: deps.DefaultAccountKey,
		Authority: proxy.CodexLeaseAuthorityPolicy{
			ModeEpoch:                   deps.Status.ModeEpoch,
			Authoritative:               true,
			RetainedAuthoritativeEpochs: append([]uint64(nil), deps.Status.RetainedAuthoritativeEpochs...),
		},
		Headroom:     deps.Headroom,
		HeadroomMode: deps.HeadroomMode,
		Now:          deps.Now,
	}
	session := &proxy.CodexHTTPRequestSession{
		Executor:  deps.Executor,
		Refresher: deps.Refresher,
		Capacity:  deps.Capacity,
	}
	newHandler := deps.newHandler
	if newHandler == nil {
		newHandler = func(planner proxy.CodexNativeHTTPRequestPlanner, session proxy.CodexNativeHTTPRequestSession, upstream string) (proxy.CodexNativeHTTPRoutingHandler, error) {
			return proxy.NewCodexNativeHTTPHandler(planner, session, upstream)
		}
	}
	handler, err := newHandler(planner, session, deps.Upstream)
	if err != nil {
		return nil, fmt.Errorf("construct Codex native HTTP handler: %w", err)
	}
	if handler == nil {
		return nil, errors.New("Codex native HTTP handler unavailable")
	}
	return handler, nil
}

func proxyCodexRetainedHTTPAdapterBlocker(status proxy.CodexModeStatus) error {
	if status.Effective != proxy.CodexRoutingEnforce && len(status.RetainedAuthoritativeEpochs) != 0 {
		return &proxyCodexRetainedHTTPAdapterUnavailableError{}
	}
	return nil
}

func newProxyCodexMemoryObserver(runtime *proxy.CodexRoutingRuntime, capacity *proxy.CodexCapacityLedger) (*proxy.CodexTurnObserver, error) {
	if runtime == nil || (runtime.HTTP.Effective != proxy.CodexRoutingObserve && runtime.WebSocket.Effective != proxy.CodexRoutingObserve) {
		return nil, nil
	}
	if capacity == nil {
		return nil, errors.New("Codex observe capacity unavailable")
	}
	epoch := max(runtime.HTTP.ModeEpoch, runtime.WebSocket.ModeEpoch)
	if epoch == 0 {
		return nil, errors.New("Codex observe mode epoch unavailable")
	}
	observer, err := proxy.NewCodexTurnObserver(proxy.NewCodexTurnLeaseManager(epoch, false, time.Now), nil)
	if err != nil {
		return nil, fmt.Errorf("construct Codex memory observer: %w", err)
	}
	observer.BindCapacity(capacity)
	return observer, nil
}

func openProxyCodexContinuity(deps proxyCodexContinuityDependencies) (*proxyCodexContinuity, error) {
	options, err := proxy.NewCodexContinuityOpenOptions(deps.FS, deps.StateDir, deps.Routing, deps.Retention, deps.Now)
	if err != nil {
		return nil, err
	}
	if deps.Authority == nil {
		return nil, codexprov.ErrCredentialAuthorityUnavailable
	}

	keyExists, err := proxyCodexContinuityPathExists(deps.FS, options.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex continuity key: %w", err)
	}
	journalExists, err := proxyCodexContinuityPathExists(deps.FS, options.JournalPath)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex continuity journal: %w", err)
	}
	authorityNeeded := proxyCodexContinuityAuthorityNeeded(deps.Routing)
	if !keyExists && !journalExists && !authorityNeeded {
		return nil, nil
	}

	operations := deps.operations
	if operations.initialise == nil {
		operations.initialise = proxy.InitialiseCodexContinuityAuthority
	}
	if operations.open == nil {
		operations.open = proxy.OpenCodexContinuityCoordinator
	}
	if operations.newRuntime == nil {
		operations.newRuntime = proxy.NewCodexLeaseRuntime
	}
	if (!keyExists || !journalExists) && authorityNeeded {
		if err := operations.initialise(options, deps.Authority); err != nil {
			return nil, fmt.Errorf("initialise Codex continuity authority: %w", err)
		}
	}

	coordinator, err := operations.open(options, deps.Authority)
	if err != nil {
		return nil, fmt.Errorf("open Codex continuity authority: %w", err)
	}
	runtime, err := operations.newRuntime(coordinator, newProxyCodexContinuityAccountRevalidator(deps.Authority))
	if err != nil {
		closeErr := coordinator.Close()
		return nil, errors.Join(
			fmt.Errorf("construct Codex continuity runtime: %w", err),
			proxyCodexContinuityCloseError(closeErr),
		)
	}
	return &proxyCodexContinuity{Coordinator: coordinator, Runtime: runtime}, nil
}

func proxyCodexContinuityCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close Codex continuity authority: %w", err)
}

func proxyCodexContinuityPathExists(fsys fsutil.FileSystem, path string) (bool, error) {
	_, err := fsys.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func proxyCodexContinuityAuthorityNeeded(runtime *proxy.CodexRoutingRuntime) bool {
	if runtime == nil {
		return false
	}
	for _, status := range []proxy.CodexModeStatus{runtime.HTTP, runtime.WebSocket} {
		if status.Effective != proxy.CodexRoutingOff || status.AuthoritativeEpoch != 0 || len(status.RetainedAuthoritativeEpochs) != 0 {
			return true
		}
	}
	return false
}

func newProxyCodexContinuityAccountRevalidator(authority interface {
	List(context.Context) (codexprov.Inventory, error)
}) proxy.CodexLeaseAccountRevalidator {
	return func(ctx context.Context, account codexprov.AccountKey) error {
		if ctx == nil || account == "" || authority == nil {
			return fmt.Errorf("%w: account authority unavailable", proxy.ErrCodexContinuity)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		inventory, err := authority.List(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if errors.Is(err, codexprov.ErrCredentialInventoryDegraded) {
				return codexprov.ErrCredentialInventoryDegraded
			}
			return codexprov.ErrCredentialAuthorityUnavailable
		}

		matches := 0
		routable := false
		for _, logical := range inventory.Accounts {
			if logical.Key != account {
				continue
			}
			matches++
			if logical.Unstable || !logical.Routable {
				continue
			}
			for _, candidate := range logical.Candidates {
				if candidate.Ref.AccountKey == account && candidate.Ref.CandidateID != "" && candidate.Revision != "" && candidate.Routable && !candidate.DispatchBlocked {
					routable = true
					break
				}
			}
		}
		if matches != 1 || !routable {
			return fmt.Errorf("%w: account is not stably routable", proxy.ErrCodexContinuity)
		}
		return nil
	}
}
