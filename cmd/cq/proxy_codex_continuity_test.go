package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestOpenProxyCodexContinuityOrdersFreshAuthorityBeforeRuntime(t *testing.T) {
	t.Parallel()

	fsys := fsutil.NewMemFS()
	authority := &proxyCodexContinuityTestAuthority{}
	events := make([]string, 0, 3)
	var gotInitialiseOptions, gotOpenOptions proxy.CodexContinuityOpenOptions
	var gotInitialiseOwner, gotOpenOwner proxy.CodexLeaseWriterAuthority
	result, err := openProxyCodexContinuity(proxyCodexContinuityDependencies{
		FS:        fsys,
		StateDir:  "/state",
		Routing:   &proxy.CodexRoutingRuntime{HTTP: proxy.CodexModeStatus{Effective: proxy.CodexRoutingObserve, ModeEpoch: 4}},
		Retention: 7 * 24 * time.Hour,
		Now:       time.Now,
		Authority: authority,
		operations: proxyCodexContinuityOperations{
			initialise: func(options proxy.CodexContinuityOpenOptions, owner proxy.CodexLeaseWriterAuthority) error {
				events = append(events, "initialise")
				gotInitialiseOptions, gotInitialiseOwner = options, owner
				return nil
			},
			open: func(options proxy.CodexContinuityOpenOptions, owner proxy.CodexLeaseWriterAuthority) (*proxy.CodexContinuityCoordinator, error) {
				events = append(events, "open")
				gotOpenOptions, gotOpenOwner = options, owner
				return &proxy.CodexContinuityCoordinator{}, nil
			},
			newRuntime: func(*proxy.CodexContinuityCoordinator, proxy.CodexLeaseAccountRevalidator) (*proxy.CodexLeaseRuntime, error) {
				events = append(events, "runtime")
				return &proxy.CodexLeaseRuntime{}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Coordinator == nil || result.Runtime == nil {
		t.Fatalf("continuity result = %#v", result)
	}
	if !reflect.DeepEqual(events, []string{"initialise", "open", "runtime"}) {
		t.Fatalf("events = %v", events)
	}
	if !sameProxyCodexContinuityOptions(gotInitialiseOptions, gotOpenOptions) || gotInitialiseOwner != authority || gotOpenOwner != authority {
		t.Fatal("initialise and open did not receive identical options and owner")
	}
}

func sameProxyCodexContinuityOptions(left, right proxy.CodexContinuityOpenOptions) bool {
	return left.FS == right.FS &&
		left.KeyPath == right.KeyPath &&
		left.JournalPath == right.JournalPath &&
		left.Policy.Retention == right.Policy.Retention &&
		reflect.ValueOf(left.Policy.Now).Pointer() == reflect.ValueOf(right.Policy.Now).Pointer() &&
		reflect.DeepEqual(left.Modes, right.Modes)
}

func TestOpenProxyCodexContinuityUsesExistingPairInEveryMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []proxy.CodexRoutingMode{proxy.CodexRoutingOff, proxy.CodexRoutingObserve, proxy.CodexRoutingEnforce} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			fsys := fsutil.NewMemFS()
			if err := fsys.MkdirAll("/state", 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/state/codex-turn-leases.key", "/state/codex-turn-leases.json"} {
				if err := fsys.WriteFile(path, []byte("existing"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var events []string
			result, err := openProxyCodexContinuity(proxyCodexContinuityDependencies{
				FS: fsys, StateDir: "/state", Routing: &proxy.CodexRoutingRuntime{HTTP: proxy.CodexModeStatus{Effective: mode, ModeEpoch: 5}},
				Retention: time.Hour, Now: time.Now, Authority: &proxyCodexContinuityTestAuthority{},
				operations: proxyCodexContinuityOperations{
					initialise: func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) error {
						events = append(events, "initialise")
						return nil
					},
					open: func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) (*proxy.CodexContinuityCoordinator, error) {
						events = append(events, "open")
						return &proxy.CodexContinuityCoordinator{}, nil
					},
					newRuntime: func(*proxy.CodexContinuityCoordinator, proxy.CodexLeaseAccountRevalidator) (*proxy.CodexLeaseRuntime, error) {
						events = append(events, "runtime")
						return &proxy.CodexLeaseRuntime{}, nil
					},
				},
			})
			if err != nil || result == nil {
				t.Fatalf("open = %#v, %v", result, err)
			}
			if !reflect.DeepEqual(events, []string{"open", "runtime"}) {
				t.Fatalf("events = %v, want direct open then runtime", events)
			}
		})
	}
}

func TestOpenProxyCodexContinuityLeavesFreshOffStateUntouched(t *testing.T) {
	t.Parallel()

	fsys := fsutil.NewMemFS()
	called := false
	result, err := openProxyCodexContinuity(proxyCodexContinuityDependencies{
		FS: fsys, StateDir: "/state", Routing: &proxy.CodexRoutingRuntime{
			HTTP:      proxy.CodexModeStatus{Effective: proxy.CodexRoutingOff},
			WebSocket: proxy.CodexModeStatus{Effective: proxy.CodexRoutingOff},
		},
		Retention: time.Hour, Now: time.Now, Authority: &proxyCodexContinuityTestAuthority{},
		operations: proxyCodexContinuityOperations{
			initialise: func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) error {
				called = true
				return nil
			},
			open: func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) (*proxy.CodexContinuityCoordinator, error) {
				called = true
				return nil, nil
			},
			newRuntime: func(*proxy.CodexContinuityCoordinator, proxy.CodexLeaseAccountRevalidator) (*proxy.CodexLeaseRuntime, error) {
				called = true
				return nil, nil
			},
		},
	})
	if err != nil || result != nil || called {
		t.Fatalf("fresh off state = result %#v err %v called=%t", result, err, called)
	}
	for _, path := range []string{"/state/codex-turn-leases.key", "/state/codex-turn-leases.json"} {
		if _, err := fsys.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fresh off state created %q: %v", path, err)
		}
	}
}

func TestProxyCodexContinuityAccountRevalidatorFailsClosed(t *testing.T) {
	t.Parallel()

	account := codexprov.AccountKey("opaque-account")
	tests := []struct {
		name      string
		inventory codexprov.Inventory
		err       error
		want      error
	}{
		{name: "inventory unavailable", err: errors.New("private inventory failure"), want: codexprov.ErrCredentialAuthorityUnavailable},
		{name: "missing", inventory: codexprov.Inventory{}, want: proxy.ErrCodexContinuity},
		{name: "unstable", inventory: proxyCodexContinuityInventory(account, true, true, false), want: proxy.ErrCodexContinuity},
		{name: "unroutable", inventory: proxyCodexContinuityInventory(account, false, false, false), want: proxy.ErrCodexContinuity},
		{name: "dispatch blocked", inventory: proxyCodexContinuityInventory(account, true, false, true), want: proxy.ErrCodexContinuity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			authority := &proxyCodexContinuityTestAuthority{inventory: test.inventory, err: test.err}
			err := newProxyCodexContinuityAccountRevalidator(authority)(context.Background(), account)
			if !errors.Is(err, test.want) || (test.err != nil && errors.Is(err, test.err)) {
				t.Fatalf("revalidate error = %T %v, want safe %v", err, err, test.want)
			}
		})
	}

	authority := &proxyCodexContinuityTestAuthority{inventory: proxyCodexContinuityInventory(account, true, false, false)}
	if err := newProxyCodexContinuityAccountRevalidator(authority)(context.Background(), account); err != nil {
		t.Fatalf("routable stable account rejected: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newProxyCodexContinuityAccountRevalidator(authority)(canceled, account); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func proxyCodexContinuityInventory(account codexprov.AccountKey, routable, unstable, blocked bool) codexprov.Inventory {
	return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
		Key: account, Routable: routable, Unstable: unstable,
		Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: account, CandidateID: "candidate"},
			Revision: "revision", Routable: routable, DispatchBlocked: blocked,
		}},
	}}}
}

type proxyCodexContinuityTestAuthority struct {
	inventory codexprov.Inventory
	err       error
}

func (authority *proxyCodexContinuityTestAuthority) AssertOwner() error { return nil }

func (authority *proxyCodexContinuityTestAuthority) BeginOwnerOperation() (*codexprov.CredentialOwnerOperation, error) {
	return &codexprov.CredentialOwnerOperation{}, nil
}

func (authority *proxyCodexContinuityTestAuthority) List(context.Context) (codexprov.Inventory, error) {
	return authority.inventory, authority.err
}
