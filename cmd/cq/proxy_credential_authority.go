package main

import (
	"context"
	"errors"
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

func acquireProxyWorkerCredentialAuthority(ctx context.Context, operations proxyCredentialAuthorityOperations) (*codexprov.CredentialControl, error) {
	if ctx == nil || operations.open == nil || operations.owner == nil || operations.close == nil || operations.wait == nil {
		return nil, codexprov.ErrCredentialAuthorityUnavailable
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err)
		}
		control, err := operations.open(ctx)
		if err != nil {
			return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err)
		}
		if control == nil {
			return nil, codexprov.ErrCredentialAuthorityUnavailable
		}
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err, operations.close(control))
		}
		if operations.owner(control) {
			return control, nil
		}
		if err := operations.close(control); err != nil {
			return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err)
		}
		if err := operations.wait(ctx, proxyCredentialAuthorityRetryInterval); err != nil {
			return nil, errors.Join(codexprov.ErrCredentialAuthorityUnavailable, err)
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
