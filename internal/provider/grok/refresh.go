package grok

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jacobcxdev/cq/internal/credentials"
)

type retainedOperation struct {
	original   privateTokens
	generation string
	operation  storedOperation
}

// Refresh renews CQ-owned expiring sessions; native/CodexBar tokens are never
// imported into this refresh authority.
func (a *SessionAuthority) Refresh(ctx context.Context) error {
	lock, err := a.Store.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	records, err := lock.store.read()
	if err != nil {
		return err
	}
	var errs []error
	for i, record := range records.Accounts {
		if record.Operation == nil && record.Tokens.Expiry.After(a.now().Add(time.Minute)) {
			continue
		}
		if err := a.refreshLocked(ctx, lock.store, &records, i); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func (a *SessionAuthority) refreshLocked(ctx context.Context, store Store, records *storeData, index int) error {
	record := &records.Accounts[index]
	a.retainedMu.Lock()
	retained, exists := a.retained[record.Account.ID]
	a.retainedMu.Unlock()
	if exists && retained.original == record.Tokens && retained.generation == record.Generation {
		operation := retained.operation
		record.Operation = &operation
	}
	if a.Exchange == nil || record.Tokens.Refresh == "" {
		return ErrAuthExpired
	}
	if record.Operation == nil {
		record.Operation = &storedOperation{}
	}
	op := record.Operation
	state := credentials.Rotation[privateTokens]{AttemptPersisted: op.Attempted, AttemptIndeterminate: op.Attempted && !op.ResultRecorded && op.Result == nil && !op.Failed, ResultPersisted: op.ResultRecorded, MaterialCommitted: op.Committed}
	if op.Result != nil {
		state.Material = *op.Result
		state.MaterialReady = true
	}
	if op.Failed {
		state.ResultErr = fmt.Errorf("%w: renewal rejected", ErrAuthExpired)
	}
	original := record.Tokens
	update := func(state credentials.Rotation[privateTokens]) {
		op.Attempted = state.AttemptPersisted
		op.ResultRecorded = state.ResultPersisted
		op.Committed = state.MaterialCommitted
		op.Failed = state.ResultErr != nil
		if state.MaterialReady {
			tokens := state.Material
			op.Result = &tokens
		}
		a.retainedMu.Lock()
		if a.retained == nil {
			a.retained = make(map[AccountID]retainedOperation)
		}
		a.retained[record.Account.ID] = retainedOperation{original: original, generation: record.Generation, operation: *op}
		a.retainedMu.Unlock()
	}
	save := func(state credentials.Rotation[privateTokens]) error { update(state); return store.write(*records) }
	err := credentials.Rotate(ctx, &state, credentials.Hooks[privateTokens]{
		Prepare:        func() error { return nil },
		Changed:        update,
		PersistAttempt: func() error { copy := state; copy.AttemptPersisted = true; return save(copy) },
		Exchange: func(ctx context.Context) (privateTokens, int64, error) {
			tokens, err := a.Exchange(ctx, record.Tokens.Refresh)
			if err != nil {
				return privateTokens{}, 0, fmt.Errorf("%w: renewal outcome unavailable", ErrAuthExpired)
			}
			if !validToken(tokens.AccessToken) || tokens.ExpiresAt.IsZero() || !tokens.ExpiresAt.After(a.now()) || tokens.RefreshToken != "" && !validToken(tokens.RefreshToken) || tokens.IDToken != "" && !validToken(tokens.IDToken) {
				return privateTokens{}, 0, fmt.Errorf("%w: invalid renewal", ErrAuthExpired)
			}
			account := record.Account
			if tokens.UserID != "" && tokens.UserID != account.UserID || tokens.PrincipalType != "" && tokens.PrincipalType != account.PrincipalType || tokens.PrincipalID != "" && tokens.PrincipalID != account.PrincipalID || tokens.TeamID != "" && tokens.TeamID != account.TeamID {
				return privateTokens{}, 0, fmt.Errorf("%w: renewal identity changed", ErrAuthExpired)
			}
			material := record.Tokens
			material.Access = tokens.AccessToken
			material.Expiry = tokens.ExpiresAt
			if tokens.RefreshToken != "" {
				material.Refresh = tokens.RefreshToken
			}
			if tokens.IDToken != "" {
				material.ID = tokens.IDToken
			}
			return material, 0, nil
		},
		PersistResult: func(value credentials.Rotation[privateTokens]) error {
			value.ResultPersisted = true
			if err := save(value); err != nil {
				value.ResultPersisted = false
				update(value)
				return err
			}
			return nil
		},
		Commit: func(value credentials.Rotation[privateTokens]) error {
			if value.ResultErr != nil {
				return value.ResultErr
			}
			record.Tokens = value.Material
			value.MaterialCommitted = true
			if err := save(value); err != nil {
				value.MaterialCommitted = false
				update(value)
				return err
			}
			return nil
		},
		Complete: func() error {
			record.Operation = nil
			if err := store.write(*records); err != nil {
				record.Operation = op
				return err
			}
			a.retainedMu.Lock()
			delete(a.retained, record.Account.ID)
			a.retainedMu.Unlock()
			return nil
		},
	})
	if errors.Is(err, credentials.ErrOutcomeIndeterminate) {
		return fmt.Errorf("%w: renewal outcome indeterminate", ErrAuthExpired)
	}
	return err
}
