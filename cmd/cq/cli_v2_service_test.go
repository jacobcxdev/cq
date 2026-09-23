package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type selectedServiceStore struct {
	record               installstate.Record
	exists               bool
	failSave, failRemove int
	loadErr              error
}

func (s *selectedServiceStore) Load() (installstate.Record, error) {
	if s.loadErr != nil {
		return installstate.Record{}, s.loadErr
	}
	if !s.exists {
		return installstate.Record{}, installstate.ErrNotInstalled
	}
	r := s.record
	r.Services = append([]string(nil), r.Services...)
	return r, nil
}
func (s *selectedServiceStore) Save(r installstate.Record) error {
	s.record = r
	s.exists = true
	if s.failSave > 0 {
		s.failSave--
		return errors.New("save failed after write")
	}
	return r.Validate()
}
func (s *selectedServiceStore) Remove() error {
	s.exists = false
	if s.failRemove > 0 {
		s.failRemove--
		return errors.New("remove failed after write")
	}
	return nil
}
func (s *selectedServiceStore) CheckClaim(installstate.Owner, string) error { return nil }

type selectedServiceLock struct {
	held, busy bool
	checks     int
}

func (l *selectedServiceLock) Acquire() (installer.InstallLock, error) {
	l.checks++
	if l.held || l.busy {
		return nil, installer.ErrInstallationInProgress
	}
	l.held = true
	return l, nil
}
func (l *selectedServiceLock) Close() error { l.held = false; return nil }

type selectedServicePlatform struct {
	fakeServicePlatform                                    // Whole-component methods stay unavailable to canonical paths.
	statuses                                               map[serviceSelection]componentStatus
	definitions                                            map[serviceSelection]string
	calls                                                  []string
	lock                                                   *selectedServiceLock
	fail                                                   string
	restoreFails, restoreLies, staleCompletion, losePolicy bool
	inspectErr                                             error
	afterMutation                                          func()
	afterRestore                                           func()
	inspectHook                                            func(context.Context, serviceSelection) error
}

func (p *selectedServicePlatform) PreflightSelected(ctx context.Context, _ string, s serviceSelection) error {
	p.calls = append(p.calls, "preflight:"+string(s))
	return ctx.Err()
}
func (p *selectedServicePlatform) InspectSelected(ctx context.Context, s serviceSelection) (serviceStatus, error) {
	if err := ctx.Err(); err != nil {
		return serviceStatus{}, err
	}
	if p.inspectHook != nil {
		if err := p.inspectHook(ctx, s); err != nil {
			return serviceStatus{}, err
		}
	}
	if p.inspectErr != nil {
		return serviceStatus{}, p.inspectErr
	}
	status := serviceStatus{}
	for _, id := range s.components() {
		p.calls = append(p.calls, "inspect:"+string(id))
		status.setComponent(id, p.statuses[id])
	}
	return status, nil
}
func (p *selectedServicePlatform) SnapshotSelected(ctx context.Context, s serviceSelection) (servicePlatformSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return servicePlatformSnapshot{}, err
	}
	snapshot := servicePlatformSnapshot{Manager: "fake-selected", Components: []serviceComponentSnapshot{}}
	for _, id := range s.components() {
		p.calls = append(p.calls, "snapshot:"+string(id))
		c := p.statuses[id]
		enabled := false
		if c.Observed.Enabled != nil {
			enabled = *c.Observed.Enabled
		}
		snapshot.Components = append(snapshot.Components, serviceComponentSnapshot{ID: c.ID, Exists: c.Registered, Enabled: enabled, Running: c.Running, Definition: []byte(p.definitions[id])})
	}
	return snapshot, nil
}
func (p *selectedServicePlatform) RestoreSelected(ctx context.Context, s serviceSelection, snapshot servicePlatformSnapshot) error {
	if !p.lock.held {
		panic("restore without lifecycle lock")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.calls = append(p.calls, "restore:"+string(s))
	if p.restoreFails {
		return errors.New("restore failed")
	}
	if p.restoreLies {
		return nil
	}
	for _, id := range s.components() {
		for _, v := range snapshot.Components {
			c := p.statuses[id]
			if v.ID != c.ID {
				continue
			}
			c.Registered = v.Exists
			c.Running = v.Running
			obs := *c.Observed
			obs.Enabled = serviceBool(v.Enabled)
			obs.Healthy = serviceBool(v.Running)
			c.Observed = &obs
			p.statuses[id] = c
			p.definitions[id] = string(v.Definition)
		}
	}
	if p.afterRestore != nil {
		p.afterRestore()
	}
	return nil
}
func (p *selectedServicePlatform) mutate(action string, id serviceSelection) error {
	if !p.lock.held {
		panic("mutation without lifecycle lock")
	}
	call := string(id) + "-" + action
	p.calls = append(p.calls, call)
	c := p.statuses[id]
	obs := *c.Observed
	c.Observed = &obs
	switch action {
	case "install":
		c.Registered = true
		obs.Owner = "cq"
		p.definitions[id] = "new definition"
		fallthrough
	case "start":
		obs.Enabled = serviceBool(true)
		c.Running = id == serviceProxy
		obs.Healthy = serviceBool(true)
	case "stop":
		obs.Enabled = serviceBool(false)
		c.Running = false
		obs.Healthy = serviceBool(false)
	case "restart":
		c.Running = id == serviceProxy
		obs.Healthy = serviceBool(true)
		if p.losePolicy {
			obs.Enabled = serviceBool(true)
		}
	case "uninstall":
		c.Registered = false
		c.Running = false
		obs.Enabled = serviceBool(false)
		p.definitions[id] = ""
	}
	if id == serviceRefresh && (action == "start" || action == "install" || action == "restart") && !p.staleCompletion {
		now := time.Now()
		obs.LastRunAt = &now
		zero := 0
		obs.LastExitCode = &zero
	}
	p.statuses[id] = c
	if p.afterMutation != nil {
		p.afterMutation()
	}
	if p.fail == call {
		return errors.New("manager failed after mutation")
	}
	return nil
}
func (p *selectedServicePlatform) InstallProxy(context.Context, string) error {
	return p.mutate("install", serviceProxy)
}
func (p *selectedServicePlatform) InstallRefresh(context.Context, string) error {
	return p.mutate("install", serviceRefresh)
}
func (p *selectedServicePlatform) StartProxy(context.Context) error {
	return p.mutate("start", serviceProxy)
}
func (p *selectedServicePlatform) StartRefresh(context.Context) error {
	return p.mutate("start", serviceRefresh)
}
func (p *selectedServicePlatform) StopProxy(context.Context) error {
	return p.mutate("stop", serviceProxy)
}
func (p *selectedServicePlatform) StopRefresh(context.Context) error {
	return p.mutate("stop", serviceRefresh)
}
func (p *selectedServicePlatform) RestartProxy(context.Context) error {
	return p.mutate("restart", serviceProxy)
}
func (p *selectedServicePlatform) RestartRefresh(context.Context) error {
	return p.mutate("restart", serviceRefresh)
}
func (p *selectedServicePlatform) RemoveProxy(context.Context) error {
	return p.mutate("uninstall", serviceProxy)
}
func (p *selectedServicePlatform) RemoveRefresh(context.Context) error {
	return p.mutate("uninstall", serviceRefresh)
}

func newSelectedServiceHarness(t *testing.T) (*serviceLifecycle, *selectedServicePlatform, *selectedServiceStore) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "cq")
	roots := userdirs.Roots{Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "state"), Cache: filepath.Join(dir, "cache"), Runtime: filepath.Join(dir, "runtime"), Logs: filepath.Join(dir, "logs")}
	lock := &selectedServiceLock{}
	p := &selectedServicePlatform{statuses: map[serviceSelection]componentStatus{}, definitions: map[serviceSelection]string{}, lock: lock}
	for _, id := range serviceAll.components() {
		then := time.Now().Add(-time.Minute)
		zero := 0
		p.statuses[id] = componentStatus{ID: "native." + string(id), Manager: "launchd", Registered: true, Running: id == serviceProxy, ConfiguredExecutable: exe, LiveExecutable: exe, Observed: &serviceObservation{Enabled: serviceBool(true), Healthy: serviceBool(true), Owner: "cq", Roots: &roots, LastRunAt: &then, LastExitCode: &zero}}
		p.definitions[id] = "original " + string(id)
	}
	s := &selectedServiceStore{exists: true, record: installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerManual, Version: "fixture-version", Executable: exe, BinaryDigest: strings.Repeat("a", 64), Services: []string{"native.proxy", "native.token-refresh"}}}
	l := &serviceLifecycle{Platform: p, Store: s, Executable: exe, Version: "next-version", StatusAttempts: 1, MutationLocker: lock, DigestExecutable: func(string) (string, error) { return strings.Repeat("a", 64), nil }}
	return l, p, s
}
func serviceFixture(t *testing.T, scenario string) *v2Fixture {
	f := &v2Fixture{}
	l, p, _ := newSelectedServiceHarness(t)
	if scenario == "service-disabled-refresh" {
		c := p.statuses[serviceRefresh]
		c.Observed.Enabled = serviceBool(false)
		p.statuses[serviceRefresh] = c
	}
	if scenario == "service-rollback" {
		p.fail = "token-refresh-stop"
		p.restoreFails = true
	}
	f.Lookup = func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Service(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			out := handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return l, nil })
			for _, call := range p.calls {
				f.Call(call)
				if strings.Contains(call, "token-refresh") {
					f.Call("refresh-manager")
				}
			}
			return out
		}, ok
	}
	return f
}
func init() {
	for _, scenario := range []string{"service-selective-stop", "service-disabled-refresh", "service-rollback"} {
		scenario := scenario
		registerV2Fixture(scenario, func(t *testing.T) *v2Fixture { return serviceFixture(t, scenario) })
	}
}
func TestCLIV2ServiceContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "stop affects selected component only", Scenario: "service-selective-stop", Args: []string{"service", "stop", "--component", "proxy", "--json"}, Exit: 0, Command: "service stop", WantJSON: `{"component":"proxy","action":"stop"}`, Forbid: []string{"refresh-manager", "credential-activate", "consume"}, Calls: map[string]int{"proxy-stop": 1}})
	runV2Case(t, v2Case{Name: "disabled refresh newly completes", Scenario: "service-disabled-refresh", Args: []string{"service", "restart", "--component", "token-refresh", "--json"}, Exit: 0, Command: "service restart", WantJSON: `{"component":"token-refresh","action":"restart","components":[{"id":"token-refresh","enabled":false,"healthy":false}]}`, Calls: map[string]int{"token-refresh-restart": 1}})
}
func runSelectedService(t *testing.T, ctx context.Context, l *serviceLifecycle, args ...string) (int, v2ServiceData, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := cli.Run(ctx, args, &cli.Session{Out: &stdout, Err: &stderr}, func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Service(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return l, nil })
		}, ok
	})
	var envelope struct {
		Data   v2ServiceData
		Errors []cli.Diagnostic
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	code := ""
	if len(envelope.Errors) > 0 {
		code = envelope.Errors[0].Code
	}
	return exit, envelope.Data, code
}
func TestCLIV2ServiceActionSelectionMatrix(t *testing.T) {
	for _, action := range []string{"install", "start", "stop", "restart", "status", "uninstall"} {
		for _, selection := range []serviceSelection{serviceAll, serviceProxy, serviceRefresh} {
			t.Run(action+"/"+string(selection), func(t *testing.T) {
				l, p, s := newSelectedServiceHarness(t)
				previousRecord := s.record
				if action == "start" {
					for _, id := range serviceAll.components() {
						c := p.statuses[id]
						c.Running = false
						c.Observed.Enabled = serviceBool(false)
						p.statuses[id] = c
					}
				}
				before := map[serviceSelection]componentStatus{}
				for id, c := range p.statuses {
					before[id] = c
				}
				exit, data, code := runSelectedService(t, context.Background(), l, "service", action, "--component", string(selection), "--json")
				if exit != 0 {
					t.Fatalf("exit %d %s data=%+v", exit, code, data)
				}
				if data.Rollback != "not_needed" || len(data.Components) != len(selection.components()) {
					t.Fatalf("data=%+v", data)
				}
				for i, id := range selection.components() {
					if data.Components[i].ID != id {
						t.Fatalf("order=%v", data.Components)
					}
				}
				for _, id := range serviceAll.components() {
					if selection != serviceAll && id != selection {
						if !reflect.DeepEqual(before[id], p.statuses[id]) {
							t.Fatalf("unselected %s changed", id)
						}
						for _, call := range p.calls {
							if strings.Contains(call, string(id)) {
								t.Fatalf("unselected manager access %s", call)
							}
						}
						if !s.record.HasService(before[id].ID) {
							t.Fatal("lost unselected ownership")
						}
					}
				}
				if action == "install" && selection == serviceAll {
					previousRecord.Version = l.Version
				}
				if action != "uninstall" && !sameServiceOwnership(previousRecord, s.record) {
					t.Fatal("existing ownership metadata changed")
				}
				if action == "uninstall" {
					for _, id := range selection.components() {
						if s.exists && s.record.HasService(before[id].ID) {
							t.Fatal("selected ownership retained")
						}
					}
				}
				var mutations []string
				for _, call := range p.calls {
					if strings.HasSuffix(call, "-"+action) {
						mutations = append(mutations, call)
					}
				}
				if selection == serviceAll && action != "status" {
					want := []string{"proxy-" + action, "token-refresh-" + action}
					if action == "stop" || action == "uninstall" {
						want[0], want[1] = want[1], want[0]
					}
					if !reflect.DeepEqual(mutations, want) {
						t.Fatalf("order=%v want=%v", mutations, want)
					}
				}
				if p.lock.held {
					t.Fatal("lock leaked")
				}
			})
		}
	}
}
func TestCLIV2ServiceIdempotenceAndMissing(t *testing.T) {
	for _, action := range []string{"start", "stop", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			if action != "start" {
				for _, id := range serviceAll.components() {
					c := p.statuses[id]
					c.Running = false
					c.Observed.Enabled = serviceBool(false)
					if action == "uninstall" {
						c.Registered = false
					}
					p.statuses[id] = c
				}
			}
			exit, _, _ := runSelectedService(t, context.Background(), l, "service", action, "--json")
			if exit != 0 {
				t.Fatal(exit)
			}
			for _, call := range p.calls {
				if action != "stop" && strings.HasSuffix(call, "-"+action) {
					t.Fatalf("idempotent mutation %s", call)
				}
			}
		})
	}
	for _, action := range []string{"start", "restart"} {
		t.Run("missing/"+action, func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			c := p.statuses[serviceRefresh]
			c.Registered = false
			p.statuses[serviceRefresh] = c
			exit, _, code := runSelectedService(t, context.Background(), l, "service", action, "--json")
			if exit != 3 || code != "service_not_installed" {
				t.Fatalf("%d %s", exit, code)
			}
			for _, call := range p.calls {
				if strings.Contains(call, "-"+action) {
					t.Fatal(call)
				}
			}
		})
	}
}
func TestCLIV2ServiceRollback(t *testing.T) {
	for _, mode := range []string{"restored", "failed", "lied", "save", "remove"} {
		t.Run(mode, func(t *testing.T) {
			l, p, s := newSelectedServiceHarness(t)
			previous := s.record
			snapshot, _ := p.SnapshotSelected(context.Background(), serviceAll)
			p.calls = nil
			action := "install"
			p.fail = "token-refresh-install"
			switch mode {
			case "failed":
				p.restoreFails = true
			case "lied":
				p.restoreLies = true
			case "save":
				p.fail = ""
				s.exists = false
				s.record = installstate.Record{}
				previous = s.record
				s.failSave = 1
				for _, id := range serviceAll.components() {
					c := p.statuses[id]
					c.Registered = false
					c.Running = false
					c.Observed.Enabled = serviceBool(false)
					p.statuses[id] = c
				}
				snapshot, _ = p.SnapshotSelected(context.Background(), serviceAll)
			case "remove":
				p.fail = ""
				s.failRemove = 1
				action = "uninstall"
			}
			exit, data, code := runSelectedService(t, context.Background(), l, "service", action, "--json")
			wantExit := 1
			wantRollback := "restored"
			if mode == "failed" || mode == "lied" {
				wantExit = 8
				wantRollback = "failed"
			}
			if exit != wantExit || data.Rollback != wantRollback {
				t.Fatalf("exit=%d code=%s rollback=%s", exit, code, data.Rollback)
			}
			if wantRollback == "restored" {
				after, _ := p.SnapshotSelected(context.Background(), serviceAll)
				if !sameServicePlatformSnapshot(snapshot, after) {
					t.Fatal("native restoration differs")
				}
				if mode == "save" {
					if s.exists {
						t.Fatal("new ownership remains")
					}
				} else if !sameServiceOwnership(previous, s.record) || !s.exists {
					t.Fatal("ownership restoration differs")
				}
			}
		})
	}
}
func TestCLIV2ServiceDisabledRestartRequiresNewCompletion(t *testing.T) {
	for _, mode := range []string{"fresh", "stale", "policy"} {
		t.Run(mode, func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			c := p.statuses[serviceRefresh]
			c.Observed.Enabled = serviceBool(false)
			p.statuses[serviceRefresh] = c
			p.staleCompletion = mode == "stale"
			p.losePolicy = mode == "policy"
			exit, data, _ := runSelectedService(t, context.Background(), l, "service", "restart", "--component", "token-refresh", "--json")
			if mode == "fresh" {
				if exit != 0 || *data.Components[0].Enabled || *data.Components[0].Healthy {
					t.Fatalf("exit=%d data=%+v", exit, data)
				}
			} else if exit != 1 || data.Rollback != "restored" {
				t.Fatalf("exit=%d rollback=%s", exit, data.Rollback)
			}
		})
	}
}
func TestCLIV2ServiceOwnership(t *testing.T) {
	for _, owner := range []string{"package", "foreign", "unknown", "missing", "other-executable"} {
		for _, action := range []string{"install", "start", "stop", "restart", "uninstall"} {
			t.Run(owner+"/"+action, func(t *testing.T) {
				l, p, s := newSelectedServiceHarness(t)
				switch owner {
				case "package":
					s.record.Owner = installstate.OwnerHomebrew
				case "foreign":
					p.statuses[serviceProxy].Observed.Owner = "foreign"
				case "unknown":
					s.loadErr = installstate.ErrUnknownSchema
				case "missing":
					s.exists = false
				case "other-executable":
					s.record.Executable = filepath.Join(t.TempDir(), "other")
				}
				exit, _, code := runSelectedService(t, context.Background(), l, "service", action, "--component", "proxy", "--json")
				allowed := owner == "package" && (action == "start" || action == "stop" || action == "restart")
				if allowed {
					if exit != 0 {
						t.Fatalf("%d %s", exit, code)
					}
				} else {
					if exit != 6 || code != "service_owner_conflict" {
						t.Fatalf("%d %s", exit, code)
					}
					for _, call := range p.calls {
						if strings.HasSuffix(call, "-"+action) {
							t.Fatal(call)
						}
					}
				}
			})
		}
	}
}
func TestCLIV2ServiceTimeoutAndInterrupt(t *testing.T) {
	for _, mode := range []string{"lock", "prepare", "mutation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "lock" {
				p.lock.busy = true
			}
			if mode == "mutation" {
				p.afterMutation = func() { time.Sleep(3 * time.Millisecond) }
			}
			if mode == "cancel" {
				p.afterMutation = cancel
			}
			if mode == "prepare" {
				inv := cli.Invocation{Path: "service stop", Options: map[string][]string{"timeout": {"1ms"}, "component": {"proxy"}}}
				out := handleV2ServiceWithPreparation(ctx, inv, nil, func(ctx context.Context) (*serviceLifecycle, error) {
					<-ctx.Done()
					return nil, errors.New("prepare error")
				})
				if out.ExitCode != 7 {
					t.Fatalf("preparation timeout=%+v", out)
				}
				return
			}
			exit, data, code := runSelectedService(t, ctx, l, "service", "stop", "--component", "proxy", "--timeout", "1ms", "--json")
			want := 7
			if mode == "cancel" {
				want = 130
			}
			if exit != want {
				t.Fatalf("%d %s", exit, code)
			}
			if mode == "lock" {
				if len(p.calls) != 0 || data.Rollback != "not_needed" {
					t.Fatalf("lock access %v data=%+v", p.calls, data)
				}
			} else if data.Rollback != "failed" {
				t.Fatalf("rollback=%s", data.Rollback)
			}
			if mode != "lock" && (data.Components[0].State != "indeterminate" || data.Components[0].Enabled != nil || data.Components[0].Healthy != nil) {
				t.Fatalf("pre-mutation state shown as current: %+v", data.Components[0])
			}
			if p.lock.held {
				t.Fatal("lock leaked")
			}
		})
	}
}
func TestCLIV2ServiceStatusObservations(t *testing.T) {
	for _, mode := range []string{"idle", "boundary", "old", "disabled", "failed", "no-run", "no-roots", "no-enabled", "absent", "no-native"} {
		t.Run(mode, func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			c := p.statuses[serviceRefresh]
			switch mode {
			case "boundary":
				at := time.Now().Add(-35 * time.Minute)
				c.Observed.LastRunAt = &at
			case "old":
				at := time.Now().Add(-36 * time.Minute)
				c.Observed.LastRunAt = &at
			case "disabled":
				c.Observed.Enabled = serviceBool(false)
			case "failed":
				code := 2
				c.Observed.LastExitCode = &code
			case "no-run":
				c.Observed.LastRunAt = nil
				c.Observed.LastExitCode = nil
			case "no-roots":
				c.Observed.Roots = nil
			case "no-enabled":
				c.Observed.Enabled = nil
			case "absent":
				c.Registered = false
			case "no-native":
				c.Observed = nil
			}
			p.statuses[serviceRefresh] = c
			exit, data, _ := runSelectedService(t, context.Background(), l, "service", "status", "--component", "token-refresh", "--json")
			if exit != 0 {
				t.Fatal(exit)
			}
			strict, _, code := runSelectedService(t, context.Background(), l, "service", "status", "--component", "token-refresh", "--strict", "--json")
			expected := 1
			if mode == "idle" {
				expected = 0
			}
			if mode == "absent" {
				expected = 3
			}
			if strict != expected {
				t.Fatalf("strict=%d %s data=%+v", strict, code, data)
			}
			if mode == "no-roots" || mode == "no-enabled" || mode == "no-native" {
				if data.Components[0].State != "indeterminate" || data.Components[0].Healthy != nil {
					t.Fatalf("invented observation %+v", data.Components[0])
				}
			}
			if p.lock.checks != 0 {
				t.Fatal("status acquired mutation lock")
			}
		})
	}
}
func TestCLIV2ServiceRefreshFreshnessBoundary(t *testing.T) {
	_, p, _ := newSelectedServiceHarness(t)
	c := p.statuses[serviceRefresh]
	now := time.Now()
	at := now.Add(-35 * time.Minute)
	c.Observed.LastRunAt = &at
	if got := projectV2ServiceComponent(serviceRefresh, c, now); got.Healthy == nil || !*got.Healthy {
		t.Fatal("35m boundary unhealthy")
	}
	if got := projectV2ServiceComponent(serviceRefresh, c, now.Add(time.Nanosecond)); got.Healthy == nil || *got.Healthy {
		t.Fatal("older than35m healthy")
	}
}
func TestCLIV2ServiceSyntaxAndHelp(t *testing.T) {
	for _, action := range []string{"install", "start", "stop", "restart", "status", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			runV2Case(t, v2Case{Name: "dry run rejected", Scenario: "no-access", Args: []string{"service", action, "--dry-run", "--json"}, Exit: 2, Code: "unknown_option", Command: "service " + action})
			for _, extra := range [][]string{{"--component", "bad"}, {"--timeout", "0s"}, {"--timeout", "11m"}, {"--component", "proxy", "--component", "all"}} {
				code := "invalid_argument"
				if len(extra) > 2 {
					code = "duplicate_option"
				}
				runV2Case(t, v2Case{Name: fmt.Sprint(extra), Scenario: "no-access", Args: append([]string{"service", action, "--json"}, extra...), Exit: 2, Code: code, Command: "service " + action})
			}
			var out bytes.Buffer
			lookup := func(string) (cli.Handler, bool) { t.Fatal("help accessed state"); return nil, false }
			if exit := cli.Run(context.Background(), []string{"service", action, "--help", "--json"}, &cli.Session{Out: &out}, lookup); exit != 0 {
				t.Fatal(exit)
			}
			want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "help", "service-"+action+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("help differs from golden: %q", out.String())
			}
		})
	}
}

func TestCLIV2ServiceSelectedOwnershipAndRollback(t *testing.T) {
	for _, selection := range []serviceSelection{serviceProxy, serviceRefresh} {
		for _, mode := range []string{"install", "uninstall", "rollback", "save-failure", "rollback-save-failure"} {
			t.Run(string(selection)+"/"+mode, func(t *testing.T) {
				l, p, s := newSelectedServiceHarness(t)
				other := serviceProxy
				if selection == serviceProxy {
					other = serviceRefresh
				}
				beforeOther := p.statuses[other]
				beforeDefinition := p.definitions[other]
				previous := s.record
				action := "uninstall"
				if mode == "install" {
					action = "install"
					c := p.statuses[selection]
					c.Registered = false
					c.Running = false
					c.Observed.Enabled = serviceBool(false)
					p.statuses[selection] = c
					s.record = s.record.WithoutServices(c.ID)
					previous = s.record
				}
				if mode == "rollback" {
					p.fail = string(selection) + "-uninstall"
				}
				if mode == "save-failure" {
					s.failSave = 1
				}
				if mode == "rollback-save-failure" {
					s.failSave = 2
				}
				exit, data, _ := runSelectedService(t, context.Background(), l, "service", action, "--component", string(selection), "--json")
				want := 0
				if mode == "rollback" || mode == "save-failure" {
					want = 1
				}
				if mode == "rollback-save-failure" {
					want = 8
				}
				if exit != want {
					t.Fatalf("exit=%d rollback=%s", exit, data.Rollback)
				}
				if !reflect.DeepEqual(beforeOther, p.statuses[other]) || beforeDefinition != p.definitions[other] {
					t.Fatal("unselected native state changed")
				}
				if !s.record.HasService(beforeOther.ID) {
					t.Fatal("unselected ownership lost")
				}
				identity := s.record
				identity.Services = previous.Services
				if !sameServiceOwnership(identity, previous) {
					t.Fatal("ownership metadata changed")
				}
				for _, call := range p.calls {
					if strings.Contains(call, string(other)) {
						t.Fatalf("unselected manager touched: %s", call)
					}
				}
				if mode == "rollback" || mode == "save-failure" {
					if !sameServiceOwnership(s.record, previous) || data.Rollback != "restored" {
						t.Fatal("incomplete rollback")
					}
				}
			})
		}
	}
}

func TestCLIV2ServiceInstallChangedIdentity(t *testing.T) {
	for _, selection := range []serviceSelection{serviceAll, serviceProxy} {
		t.Run(string(selection), func(t *testing.T) {
			l, p, s := newSelectedServiceHarness(t)
			l.DigestExecutable = func(string) (string, error) { return strings.Repeat("b", 64), nil }
			exit, _, _ := runSelectedService(t, context.Background(), l, "service", "install", "--component", string(selection), "--json")
			if selection == serviceAll {
				if exit != 0 || s.record.BinaryDigest != strings.Repeat("b", 64) || s.record.Version != l.Version {
					t.Fatalf("identity not updated exit=%d record=%+v", exit, s.record)
				}
			} else {
				if exit != 6 {
					t.Fatal(exit)
				}
				for _, call := range p.calls {
					if strings.HasSuffix(call, "-install") {
						t.Fatal("unselected ownership changed")
					}
				}
			}
		})
	}
}

func TestCLIV2ServiceOutputContract(t *testing.T) {
	l, p, s := newSelectedServiceHarness(t)
	s.record.Owner = installstate.OwnerHomebrew
	exit, data, _ := runSelectedService(t, context.Background(), l, "service", "stop", "--component", "proxy", "--json")
	if exit != 0 || data.Components[0].Owner != "package" {
		t.Fatalf("owner lost: exit=%d data=%+v", exit, data)
	}
	raw, _ := json.Marshal(data.Components[0])
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	expected := []string{"id", "manager", "owner", "installed", "enabled", "state", "healthy", "pid", "executable", "last_run_at", "last_exit_code", "error_code", "config_dir", "state_dir", "cache_dir", "runtime_dir", "log_dir"}
	if len(fields) != len(expected) {
		t.Fatalf("fields=%v", fields)
	}
	for _, key := range expected {
		if _, ok := fields[key]; !ok {
			t.Fatal("missing " + key)
		}
	}
	if data.Components[0].ConfigDir == nil || *data.Components[0].ConfigDir != p.statuses[serviceProxy].Observed.Roots.Config {
		t.Fatal("installed roots lost")
	}
	var out bytes.Buffer
	inv := cli.Invocation{Path: "service status", Options: map[string][]string{"component": {"proxy"}, "timeout": {"10s"}}}
	result := handleV2ServiceWithPreparation(context.Background(), inv, &cli.Session{Out: &out}, func(context.Context) (*serviceLifecycle, error) { return l, nil })
	want := "CQ services (proxy): status\nproxy: stopped, enabled=false, healthy=false\n\nRollback: not_needed\n"
	if result.Human != want {
		t.Fatalf("human=%q", result.Human)
	}
}

func TestCLIV2ServiceUnavailableAndCancelledPreparation(t *testing.T) {
	l, p, _ := newSelectedServiceHarness(t)
	p.inspectErr = errServiceUnavailable
	exit, _, code := runSelectedService(t, context.Background(), l, "service", "status", "--json")
	if exit != 4 || code != "service_unavailable" {
		t.Fatalf("%d %s", exit, code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	inv := cli.Invocation{Path: "service status", Options: map[string][]string{"component": {"all"}, "timeout": {"10s"}}}
	result := handleV2ServiceWithPreparation(ctx, inv, nil, func(context.Context) (*serviceLifecycle, error) { cancel(); return nil, errServiceUnavailable })
	if result.ExitCode != 130 {
		t.Fatalf("out=%+v", result)
	}
}

func TestCLIV2ServiceVerificationRequiresExecutableIdentity(t *testing.T) {
	for _, action := range []serviceAction{serviceInstall, serviceStart, serviceRestart} {
		t.Run(string(action), func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			before := p.statuses[serviceProxy]
			wrong := before
			wrong.ConfiguredExecutable = filepath.Join(t.TempDir(), "other-cq")
			p.statuses[serviceProxy] = wrong
			_, err := l.waitSelected(context.Background(), action, serviceProxy, before, time.Now())
			if !errors.Is(err, ErrServiceUnhealthy) {
				t.Fatalf("accepted different executable with healthy=true: %v", err)
			}
		})
	}
}

func TestCLIV2ServiceVerificationPreservesUnavailable(t *testing.T) {
	l, p, _ := newSelectedServiceHarness(t)
	p.afterMutation = func() { p.inspectErr = errServiceUnavailable }
	p.afterRestore = func() { p.inspectErr = nil }
	exit, data, code := runSelectedService(t, context.Background(), l, "service", "stop", "--component", "proxy", "--json")
	if exit != 4 || code != "service_unavailable" {
		t.Fatalf("verification unavailable became exit=%d code=%s", exit, code)
	}
	if data.Rollback != "restored" || data.Components[0].Enabled == nil || !*data.Components[0].Enabled {
		t.Fatalf("restoration not observed: %+v", data)
	}
}

func TestCLIV2ServiceRollbackInvalidatesObservations(t *testing.T) {
	for _, mode := range []string{"unavailable", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			l, p, _ := newSelectedServiceHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.fail = "proxy-stop" // Refresh was already stopped and successfully observed.
			p.afterRestore = func() {
				if mode == "unavailable" {
					p.inspectErr = errServiceUnavailable
				} else {
					p.inspectHook = func(ctx context.Context, _ serviceSelection) error { cancel(); return ctx.Err() }
				}
			}
			exit, data, code := runSelectedService(t, ctx, l, "service", "stop", "--json")
			expected := 1
			if mode == "cancelled" {
				expected = 130
			}
			if exit != expected {
				t.Fatalf("exit=%d code=%s", exit, code)
			}
			// Snapshot verification established rollback before the final inspection failed.
			if data.Rollback != "restored" {
				t.Fatalf("rollback=%s", data.Rollback)
			}
			for _, component := range data.Components {
				if component.State != "indeterminate" || component.Enabled != nil || component.Healthy != nil {
					t.Fatalf("pre-restore state shown as current: %+v", component)
				}
			}
			for _, id := range serviceAll.components() {
				if !*p.statuses[id].Observed.Enabled {
					t.Fatalf("fixture did not restore %s", id)
				}
			}
			if p.lock.held {
				t.Fatal("lock leaked")
			}
		})
	}
}

func TestCLIV2ServiceVerificationPollsObservedHealth(t *testing.T) {
	l, p, _ := newSelectedServiceHarness(t)
	l.StatusAttempts = 2
	l.Wait = func(context.Context, time.Duration) error { return nil }
	calls := 0
	p.inspectHook = func(context.Context, serviceSelection) error {
		calls++
		p.statuses[serviceProxy].Observed.Healthy = serviceBool(calls == 2)
		return nil
	}
	before := p.statuses[serviceProxy]
	_, err := l.waitSelected(context.Background(), serviceRestart, serviceProxy, before, time.Now())
	if err != nil || calls != 2 {
		t.Fatalf("observed health polling err=%v calls=%d", err, calls)
	}
}

func TestCLIV2ServiceFactoryReceivesBudgetSelection(t *testing.T) {
	old := selectedServiceLifecycleFactory
	t.Cleanup(func() { selectedServiceLifecycleFactory = old })
	calls := 0
	selectedServiceLifecycleFactory = func(ctx context.Context, action serviceAction, selection serviceSelection) (*serviceLifecycle, error) {
		calls++
		if action != serviceInspect || selection != serviceRefresh {
			t.Fatalf("preparation inputs: %s %s", action, selection)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("preparation missing total budget")
		}
		<-ctx.Done()
		return nil, errors.New("late discovery error")
	}
	inv := cli.Invocation{Path: "service status", Options: map[string][]string{"component": {"token-refresh"}, "timeout": {"1ms"}}}
	outcome := handleV2Service(context.Background(), inv, nil)
	if outcome.ExitCode != 7 || calls != 1 {
		t.Fatalf("factory budget exit=%d calls=%d", outcome.ExitCode, calls)
	}
}
