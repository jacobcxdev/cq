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

func (continuity *proxyCodexContinuity) Close() error {
	if continuity == nil || continuity.Coordinator == nil {
		return nil
	}
	return continuity.Coordinator.Close()
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
