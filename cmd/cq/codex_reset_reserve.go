package main

import (
	"errors"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func readCodexResetReserve(fs fsutil.FileSystem, path string, snapshot codex.ResetAccountSnapshot, usage []quota.Result, now time.Time) (quota.ReservePolicy, error) {
	var system codex.ResetAccount
	for _, account := range snapshot.Accounts {
		if !account.Active {
			continue
		}
		if system.AccountKey != "" && system.AccountKey != account.AccountKey {
			return quota.ReservePolicy{}, errors.New("ambiguous reserve system account")
		}
		system = account
	}
	var windows map[quota.WindowName]quota.Window
	for _, result := range usage {
		matches := result.AccountKey != "" && result.AccountKey == string(system.AccountKey)
		if result.AccountKey == "" {
			matches = system.AccountID != "" && result.AccountID == system.AccountID
			if system.AccountID == "" {
				matches = system.Email != "" && result.Email == system.Email
			}
		}
		if matches {
			if windows != nil {
				return quota.ReservePolicy{}, errors.New("ambiguous reserve usage")
			}
			windows = result.Windows
		}
	}
	return proxy.ReadCodexReservePolicy(fs, path, system.AccountKey, now, windows)
}

// Schedule rows alone cannot distinguish a fresh refill from a POST whose
// durable attempt must be replayed. Read the same journal as the executor.
func readCodexReserveSchedule(load func() (codex.ResetAutomationState, error), pending func(codex.AccountKey) ([]codex.ResetAttempt, error)) (codex.ResetAutomationState, error) {
	state, err := load()
	if err != nil || !state.Enabled {
		return state, err
	}
	if pending == nil {
		return state, errors.New("reset attempt storage unavailable")
	}
	seen := make(map[codex.AccountKey]bool)
	for _, entry := range state.Entries {
		if entry.AccountKey == "" || seen[entry.AccountKey] {
			continue
		}
		seen[entry.AccountKey] = true
		attempts, err := pending(entry.AccountKey)
		if err != nil {
			return state, err
		}
		if len(attempts) != 0 {
			return state, errors.New("unresolved reset attempt")
		}
	}
	return state, nil
}
