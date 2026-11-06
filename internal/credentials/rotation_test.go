package credentials

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRotationPersistsAttemptBeforeExchangeAndResultBeforeCommit(t *testing.T) {
	var order []string
	state := Rotation[string]{}
	hooks := Hooks[string]{
		Prepare:        func() error { order = append(order, "prepare"); return nil },
		PersistAttempt: func() error { order = append(order, "attempt"); return nil },
		Exchange: func(context.Context) (string, int64, error) {
			order = append(order, "exchange")
			return "rotated", 3600, nil
		},
		PersistResult: func(Rotation[string]) error { order = append(order, "result"); return nil },
		Commit:        func(Rotation[string]) error { order = append(order, "commit"); return nil },
		Complete:      func() error { order = append(order, "complete"); return nil },
	}
	if err := Rotate(context.Background(), &state, hooks); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"prepare", "attempt", "exchange", "result", "commit", "complete"}) {
		t.Fatal(order)
	}
	if !state.MaterialReady || !state.MaterialCommitted || state.Material != "rotated" {
		t.Fatal(state)
	}
}

func TestRotationCommitRetryDoesNotRepeatExchange(t *testing.T) {
	calls := 0
	state := Rotation[string]{}
	fail := true
	hooks := Hooks[string]{Prepare: func() error { return nil }, PersistAttempt: func() error { return nil }, Exchange: func(context.Context) (string, int64, error) { calls++; return "new", 1, nil }, PersistResult: func(Rotation[string]) error { return nil }, Commit: func(Rotation[string]) error {
		if fail {
			return errors.New("disk")
		}
		return nil
	}, Complete: func() error { return nil }}
	if err := Rotate(context.Background(), &state, hooks); err == nil {
		t.Fatal("expected commit error")
	}
	fail = false
	if err := Rotate(context.Background(), &state, hooks); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("exchanged %d times", calls)
	}
}

func TestRotationIndeterminateNeverExchanges(t *testing.T) {
	state := Rotation[string]{AttemptIndeterminate: true}
	calls := 0
	err := Rotate(context.Background(), &state, Hooks[string]{Prepare: func() error { return nil }, Exchange: func(context.Context) (string, int64, error) { calls++; return "", 0, nil }})
	if !errors.Is(err, ErrOutcomeIndeterminate) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestRotationResultPersistenceRetryDoesNotRepeatExchange(t *testing.T) {
	state := Rotation[string]{}
	calls := 0
	fail := true
	hooks := Hooks[string]{Prepare: func() error { return nil }, PersistAttempt: func() error { return nil }, Exchange: func(context.Context) (string, int64, error) { calls++; return "new", 1, nil }, PersistResult: func(Rotation[string]) error {
		if fail {
			return errors.New("disk")
		}
		return nil
	}, Commit: func(Rotation[string]) error { return nil }, Complete: func() error { return nil }}
	if err := Rotate(context.Background(), &state, hooks); err == nil {
		t.Fatal("expected persistence error")
	}
	fail = false
	if err := Rotate(context.Background(), &state, hooks); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
