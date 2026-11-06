// Package credentials contains credential lifecycle mechanics shared by providers.
package credentials

import (
	"context"
	"errors"
)

var ErrOutcomeIndeterminate = errors.New("credential refresh outcome indeterminate")

// Rotation is one retained rotating-token exchange. A durable attempt without a
// durable result must be recovered as indeterminate, never dispatched again.
// Material remains private to its provider's credential authority.
type Rotation[T any] struct {
	Material             T
	MaterialReady        bool
	MaterialCommitted    bool
	AttemptPersisted     bool
	AttemptIndeterminate bool
	ResultPersisted      bool
	ExpiresIn            int64
	ResultErr            error
}

// Hooks keep provider ownership, record identity, serialization and journal
// authority outside the common transaction. Callers hold their owner lock.
type Hooks[T any] struct {
	Prepare        func() error
	PersistAttempt func() error
	Exchange       func(context.Context) (T, int64, error)
	PersistResult  func(Rotation[T]) error
	Commit         func(Rotation[T]) error
	Complete       func() error
	Changed        func(Rotation[T])
}

// Rotate completes a selected exchange or resumes its retained result. Failed
// persistence is retryable without repeating an exchange already performed.
func Rotate[T any](ctx context.Context, state *Rotation[T], hooks Hooks[T]) error {
	changed := func() {
		if hooks.Changed != nil {
			hooks.Changed(*state)
		}
	}
	if err := hooks.Prepare(); err != nil {
		return err
	}
	if state.AttemptIndeterminate {
		return ErrOutcomeIndeterminate
	}
	if state.ResultErr == nil && !state.MaterialReady {
		if !state.AttemptPersisted {
			if err := hooks.PersistAttempt(); err != nil {
				return err
			}
			state.AttemptPersisted = true
			changed()
		}
		state.Material, state.ExpiresIn, state.ResultErr = hooks.Exchange(ctx)
		state.MaterialReady = state.ResultErr == nil
		changed()
	}
	if !state.ResultPersisted {
		if err := hooks.PersistResult(*state); err != nil {
			return err
		}
		state.ResultPersisted = true
		changed()
	}
	if !state.MaterialCommitted {
		if err := hooks.Commit(*state); err != nil {
			return err
		}
		state.MaterialCommitted = true
		changed()
	}
	if err := hooks.Complete(); err != nil {
		return err
	}
	return state.ResultErr
}
