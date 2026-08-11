package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
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

func TestOpenProxyCodexContinuityInjectsCanaryAdmissionAuthority(t *testing.T) {
	fsys := fsutil.NewMemFS()
	canary, err := proxy.StartCodexCanary(fsys, "/state/canary.json", nil, proxy.CodexCanaryTuple{
		CQBuild: "build", ClientBuild: "client", ParserSchema: 1, LeaseSchema: 3,
		SemanticsRevision: "semantics", RetryBudget: 1, FixtureHash: "fixture", ReadinessFingerprint: strings.Repeat("a", 64),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var gotCanary *proxy.CodexCanaryRecorder
	result, err := openProxyCodexContinuity(proxyCodexContinuityDependencies{
		FS: fsys, StateDir: "/state", Routing: &proxy.CodexRoutingRuntime{HTTP: proxy.CodexModeStatus{
			Effective: proxy.CodexRoutingEnforce, ModeEpoch: 4, AuthoritativeEpoch: 4,
		}}, Retention: time.Hour, Now: time.Now, Authority: &proxyCodexContinuityTestAuthority{}, Canary: canary,
		operations: proxyCodexContinuityOperations{
			initialise: func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) error { return nil },
			open: func(proxy.CodexContinuityOpenOptions, proxy.CodexLeaseWriterAuthority) (*proxy.CodexContinuityCoordinator, error) {
				return &proxy.CodexContinuityCoordinator{}, nil
			},
			newCanaryRuntime: func(_ *proxy.CodexContinuityCoordinator, _ proxy.CodexLeaseAccountRevalidator, recorder *proxy.CodexCanaryRecorder) (*proxy.CodexLeaseRuntime, error) {
				gotCanary = recorder
				return &proxy.CodexLeaseRuntime{}, nil
			},
		},
	})
	if err != nil || result == nil || gotCanary != canary {
		t.Fatalf("canary continuity = %#v, canary %p, error %v", result, gotCanary, err)
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
	authority.inventory = proxyCodexContinuityInventory(account, true, true, false)
	if err := newProxyCodexContinuityAccountRevalidator(authority)(context.Background(), account); err != nil {
		t.Fatalf("uniquely matched generation-local account rejected: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newProxyCodexContinuityAccountRevalidator(authority)(canceled, account); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestNewProxyCodexNativeHTTPInstallsForEnforcementAndRetainedAuthority(t *testing.T) {
	t.Parallel()

	for _, status := range []proxy.CodexModeStatus{
		{Configured: proxy.CodexRoutingOff, Effective: proxy.CodexRoutingOff},
		{Configured: proxy.CodexRoutingObserve, Effective: proxy.CodexRoutingObserve, ModeEpoch: 8},
		{Configured: proxy.CodexRoutingEnforce, Effective: proxy.CodexRoutingOff, ModeEpoch: 9},
	} {
		called := false
		handler, err := newProxyCodexNativeHTTP(proxyCodexNativeHTTPDependencies{
			Status: status,
			newHandler: func(proxy.CodexNativeHTTPRequestPlanner, proxy.CodexNativeHTTPRequestSession, string) (proxy.CodexNativeHTTPRoutingHandler, error) {
				called = true
				return &proxyCodexNativeHTTPTestHandler{}, nil
			},
		})
		if err != nil || handler != nil || called {
			t.Fatalf("status %#v installed native handler: handler=%T err=%v called=%t", status, handler, err, called)
		}
	}
	retainedDependency := &proxyCodexNativeHTTPTestDependency{}
	retainedCapacity := proxy.NewCodexCapacityLedger(time.Now, time.Hour)
	var retainedPlanner *proxy.CodexHTTPRequestPlanFactory
	wantRetainedHandler := &proxyCodexNativeHTTPTestHandler{}
	retainedHandler, retainedErr := newProxyCodexNativeHTTP(proxyCodexNativeHTTPDependencies{
		Status: proxy.CodexModeStatus{
			Configured:                  proxy.CodexRoutingObserve,
			Effective:                   proxy.CodexRoutingObserve,
			ModeEpoch:                   10,
			RetainedAuthoritativeEpochs: []uint64{9},
		},
		Inventory: retainedDependency, Capacity: retainedCapacity, Routes: retainedDependency, Runtime: retainedDependency,
		Executor: retainedDependency, Refresher: retainedDependency, Upstream: "https://codex.example", Now: time.Now,
		newRetainedHandler: func(planner proxy.CodexRetainedNativeHTTPRequestPlanner, _ proxy.CodexNativeHTTPRequestSession, _ string) (proxy.CodexNativeHTTPRoutingHandler, error) {
			retainedPlanner, _ = planner.(*proxy.CodexHTTPRequestPlanFactory)
			return wantRetainedHandler, nil
		},
	})
	if retainedErr != nil || retainedHandler != wantRetainedHandler || retainedPlanner == nil || retainedPlanner.Authority.Authoritative || retainedPlanner.Authority.ModeEpoch != 10 || !reflect.DeepEqual(retainedPlanner.Authority.RetainedAuthoritativeEpochs, []uint64{9}) {
		t.Fatalf("retained observe = handler %T error %T %v", retainedHandler, retainedErr, retainedErr)
	}
	for _, status := range []proxy.CodexModeStatus{
		{Effective: proxy.CodexRoutingEnforce, ModeEpoch: 9},
		{Effective: proxy.CodexRoutingEnforce, ModeEpoch: 9, AuthoritativeEpoch: 8},
	} {
		called := false
		handler, err := newProxyCodexNativeHTTP(proxyCodexNativeHTTPDependencies{
			Status: status,
			newHandler: func(proxy.CodexNativeHTTPRequestPlanner, proxy.CodexNativeHTTPRequestSession, string) (proxy.CodexNativeHTTPRoutingHandler, error) {
				called = true
				return &proxyCodexNativeHTTPTestHandler{}, nil
			},
		})
		if err == nil || handler != nil || called {
			t.Fatalf("invalid authority status %#v = handler %T error %v called=%t", status, handler, err, called)
		}
	}

	dependency := &proxyCodexNativeHTTPTestDependency{}
	capacity := proxy.NewCodexCapacityLedger(time.Now, time.Hour)
	retained := []uint64{4, 7}
	var planner *proxy.CodexHTTPRequestPlanFactory
	var session *proxy.CodexHTTPRequestSession
	var upstream string
	wantHandler := &proxyCodexNativeHTTPTestHandler{}
	handler, err := newProxyCodexNativeHTTP(proxyCodexNativeHTTPDependencies{
		Status: proxy.CodexModeStatus{
			Configured:                  proxy.CodexRoutingEnforce,
			Effective:                   proxy.CodexRoutingEnforce,
			ModeEpoch:                   9,
			AuthoritativeEpoch:          9,
			RetainedAuthoritativeEpochs: retained,
		},
		Inventory:         dependency,
		Capacity:          capacity,
		Routes:            dependency,
		Runtime:           dependency,
		DefaultAccountKey: "default-account",
		Executor:          dependency,
		Refresher:         dependency,
		Upstream:          "https://codex.example/backend-api",
		Now:               time.Now,
		newHandler: func(gotPlanner proxy.CodexNativeHTTPRequestPlanner, gotSession proxy.CodexNativeHTTPRequestSession, gotUpstream string) (proxy.CodexNativeHTTPRoutingHandler, error) {
			planner, _ = gotPlanner.(*proxy.CodexHTTPRequestPlanFactory)
			session, _ = gotSession.(*proxy.CodexHTTPRequestSession)
			upstream = gotUpstream
			return wantHandler, nil
		},
	})
	if err != nil || handler != wantHandler {
		t.Fatalf("native handler = %T, %v", handler, err)
	}
	if planner == nil || planner.Inventory != dependency || planner.Capacity != capacity || planner.Routes != dependency || planner.Runtime != dependency || planner.DefaultAccountKey != "default-account" || planner.Authority.ModeEpoch != 9 || !planner.Authority.Authoritative || !reflect.DeepEqual(planner.Authority.RetainedAuthoritativeEpochs, retained) {
		t.Fatalf("planner dependencies = %#v", planner)
	}
	if session == nil || session.Executor != dependency || session.Refresher != dependency || session.Capacity != capacity || upstream != "https://codex.example/backend-api" {
		t.Fatalf("session/upstream = %#v %q", session, upstream)
	}
	retained[0] = 99
	if !reflect.DeepEqual(planner.Authority.RetainedAuthoritativeEpochs, []uint64{4, 7}) {
		t.Fatalf("planner retained epochs aliased routing state: %v", planner.Authority.RetainedAuthoritativeEpochs)
	}
}

func TestNewProxyCodexV2ObserversAreObserveOnlyAndNonPersistent(t *testing.T) {
	t.Parallel()

	capacity := proxy.NewCodexCapacityLedger(time.Now, time.Hour)
	continuity := &proxyCodexContinuity{Runtime: &proxy.CodexLeaseRuntime{}}
	shared := proxy.NewCodexTurnLeaseManager(1, false, time.Now)
	var policies []proxy.CodexLeaseAuthorityPolicy
	newObserver := func(_ *proxy.CodexLeaseRuntime, policy proxy.CodexLeaseAuthorityPolicy) (*proxy.CodexTurnObserver, error) {
		policies = append(policies, policy)
		return proxy.NewCodexTurnObserver(shared.ForMode(policy.ModeEpoch, false), nil)
	}
	httpObserver, wsObserver, err := newProxyCodexV2Observers(proxyCodexV2ObserverDependencies{Routing: &proxy.CodexRoutingRuntime{
		HTTP:      proxy.CodexModeStatus{Effective: proxy.CodexRoutingObserve, ModeEpoch: 7},
		WebSocket: proxy.CodexModeStatus{Effective: proxy.CodexRoutingObserve, ModeEpoch: 8},
	}, Continuity: continuity, Capacity: capacity, newObserver: newObserver})
	if err != nil || httpObserver == nil || wsObserver == nil || httpObserver == wsObserver || httpObserver.Store != nil || wsObserver.Store != nil || httpObserver.Leases == nil || wsObserver.Leases == nil {
		t.Fatalf("observe v2 observers = HTTP %#v WS %#v, %v", httpObserver, wsObserver, err)
	}
	if !reflect.DeepEqual(policies, []proxy.CodexLeaseAuthorityPolicy{{ModeEpoch: 7}, {ModeEpoch: 8}}) {
		t.Fatalf("observer policies = %#v", policies)
	}
	for _, modes := range []*proxy.CodexRoutingRuntime{
		nil,
		{HTTP: proxy.CodexModeStatus{Effective: proxy.CodexRoutingOff}},
		{HTTP: proxy.CodexModeStatus{Effective: proxy.CodexRoutingEnforce, ModeEpoch: 9}},
	} {
		httpObserver, wsObserver, err := newProxyCodexV2Observers(proxyCodexV2ObserverDependencies{Routing: modes, Continuity: continuity, Capacity: capacity, newObserver: newObserver})
		if err != nil || httpObserver != nil || wsObserver != nil {
			t.Fatalf("non-observe modes %#v created HTTP/WS observers %#v/%#v, %v", modes, httpObserver, wsObserver, err)
		}
	}
}

type proxyCodexNativeHTTPTestHandler struct{}

func (*proxyCodexNativeHTTPTestHandler) TryServe(http.ResponseWriter, *http.Request, bool) (bool, string) {
	return true, ""
}

type proxyCodexNativeHTTPTestDependency struct{}

func (*proxyCodexNativeHTTPTestDependency) List(context.Context) (codexprov.Inventory, error) {
	return codexprov.Inventory{}, nil
}

func (*proxyCodexNativeHTTPTestDependency) LoadRouteSnapshot(context.Context, proxy.LeaseKey, []codexprov.AccountKey, proxy.CodexLeaseAuthorityPolicy) (proxy.CodexLeaseRouteSnapshot, error) {
	return proxy.CodexLeaseRouteSnapshot{}, nil
}

func (*proxyCodexNativeHTTPTestDependency) BeginRequestContext(context.Context, proxy.CodexLeaseRequestPlan) (*proxy.CodexLeaseRequestHandle, error) {
	return nil, nil
}

func (*proxyCodexNativeHTTPTestDependency) DispatchFrozen(context.Context, proxy.RouteChoice, proxy.CandidateAttempt, *http.Request, func(proxy.CandidateAttempt) error) (*http.Response, proxy.CandidateAttempt, bool, error) {
	return nil, proxy.CandidateAttempt{}, false, nil
}

func (*proxyCodexNativeHTTPTestDependency) RefreshReference(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
	return codexprov.CandidateRef{}, "", nil
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
