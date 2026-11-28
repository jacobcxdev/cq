package main

import (
	"context"
	"errors"
	"runtime"
	"time"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

const proxyCredentialAuthorityRetryInterval = 100 * time.Millisecond

type proxyCredentialAuthorityOperations struct {
	open  func(context.Context) (*codexprov.CredentialControl, error)
	owner func(*codexprov.CredentialControl) bool
	close func(*codexprov.CredentialControl) error
	wait  func(context.Context, time.Duration) error
}

func openProxyCredentialAuthority(worker bool, configuredPort, portOverride int, open func(context.Context) (*codexprov.CredentialControl, error)) (*codexprov.CredentialControl, error) {
	if !worker && (runtime.GOOS != "windows" || (portOverride != 0 && portOverride != configuredPort)) {
		return open(context.Background())
	}
	// At Windows logon Refresh can acquire the credential pipe first. The
	// registered proxy uses the configured port and needs its own authority.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return acquireProxyWorkerCredentialAuthority(ctx, proxyCredentialAuthorityOperations{
		open:  open,
		owner: (*codexprov.CredentialControl).Owner,
		close: (*codexprov.CredentialControl).Close,
		wait:  waitProxyCredentialAuthority,
	})
}

func acquireProxyWorkerCredentialAuthority(ctx context.Context, operations proxyCredentialAuthorityOperations) (*codexprov.CredentialControl, error) {
	if ctx == nil || operations.open == nil || operations.owner == nil || operations.close == nil || operations.wait == nil {
		return nil, codexprov.ErrCredentialAuthorityUnavailable
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err)
		}
		control, err := operations.open(ctx)
		switch {
		case err != nil:
			if runtime.GOOS != "windows" || !errors.Is(err, codexprov.ErrCredentialOwnerStale) {
				return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err)
			}
		case control == nil:
			return nil, codexprov.ErrCredentialAuthorityUnavailable
		default:
			if err := ctx.Err(); err != nil {
				return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err, operations.close(control))
			}
			if operations.owner(control) {
				return control, nil
			}
			if err := operations.close(control); err != nil {
				return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err)
			}
		}
		if waitErr := operations.wait(ctx, proxyCredentialAuthorityRetryInterval); waitErr != nil {
			return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err, waitErr)
		}
	}
}

func waitProxyCredentialAuthority(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
