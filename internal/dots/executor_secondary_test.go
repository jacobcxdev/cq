package dots

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type executorTestChild struct {
	running                    bool
	code                       int
	closed, terminated, killed int
}

func (p *executorTestChild) poll() (int, bool)       { return p.code, p.running }
func (p *executorTestChild) closeStdin() error       { p.closed++; p.running = false; return nil }
func (p *executorTestChild) terminate() error        { p.terminated++; return nil }
func (p *executorTestChild) kill() error             { p.killed++; return nil }
func (p *executorTestChild) wait(time.Duration) bool { return !p.running }
func (p *executorTestChild) pid() int                { return 12345 }
func executorTestDependencies(s *Settings, c *CatalogueClient, system, owner Account) executorDependencies {
	return executorDependencies{inventory: func() (Inventory, error) { return Inventory{System: system, Accounts: []Account{system, owner}}, nil }, now: c.now, get: func(context.Context, Account, string, http.Header) (map[string]any, error) {
		return map[string]any{"profile": map[string]any{"id": "secondary-profile", "active_root_thread_id": "secondary-root"}, "selection": map[string]any{"available": true, "thread_id": "secondary-root"}}, nil
	}, request: func(context.Context, Account, string, string, string, []byte) (int, string, []byte, error) {
		return 200, "application/json", []byte(`{"executor_id":"secondary-environment"}`), nil
	}, launch: func(*Settings, string, int, string, string, bool, bool) (executorProcess, error) {
		return &executorTestChild{}, nil
	}}
}
func TestSecondaryExecutorStableInstallationAndPrivateProcess(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	deps := executorTestDependencies(s, c, system, owner)
	installations := []string{}
	deps.request = func(_ context.Context, bound Account, host, method, path string, body []byte) (int, string, []byte, error) {
		var value map[string]any
		if json.Unmarshal(body, &value) != nil {
			t.Fatal("invalid registration")
		}
		installations = append(installations, value["installation_id"].(string))
		if len(value["selected_capability_roots"].([]any)) != 0 || bound.AccountID != owner.AccountID || host != "chatgpt.com" || path != "/backend-api/flora/cca/executor" {
			t.Fatal("registration expanded owner or capabilities")
		}
		return 200, "application/json", []byte(`{"executor_id":"secondary-environment"}`), nil
	}
	deps.launch = func(_ *Settings, root string, port int, environment, capability string, sandbox, forward bool) (executorProcess, error) {
		if root != filepath.Join(s.RuntimeRoot, "executors", owner.Alias) || port <= 0 || environment != "secondary-environment" || capability == "" || sandbox || forward {
			t.Fatal("wrong secondary child isolation")
		}
		return &executorTestChild{running: true}, nil
	}
	for i := 0; i < 2; i++ {
		worker, err := startSecondaryExecutor(context.Background(), s, owner, "primary-environment", deps)
		if err != nil {
			t.Fatal(err)
		}
		closeSecondaryExecutor(worker)
	}
	if len(installations) != 2 || installations[0] != installations[1] || !nativeValidUUID(installations[0]) {
		t.Fatal("installation identity was not retained")
	}
	if _, err := startSecondaryExecutor(context.Background(), s, system, "primary-environment", deps); err == nil {
		t.Fatal("started system owner")
	}
}
func TestSecondaryExecutorRegistrationFailureNeverStartsProcess(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	for _, body := range []string{`{}`, `{"executor_id":"../bad"}`, `{"executor_id":"primary-environment"}`} {
		deps := executorTestDependencies(s, c, system, owner)
		deps.request = func(context.Context, Account, string, string, string, []byte) (int, string, []byte, error) {
			return 200, "application/json", []byte(body), nil
		}
		deps.launch = func(*Settings, string, int, string, string, bool, bool) (executorProcess, error) {
			t.Fatal("launched failed registration")
			return nil, nil
		}
		if _, err := startSecondaryExecutor(context.Background(), s, owner, "primary-environment", deps); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
func TestSecondaryExecutorAttachesOnceOnlyAfterRealRegistrySuccess(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	deps := executorTestDependencies(s, c, system, owner)
	broker, err := newExecutorRegistryBroker(s, owner, "secondary-environment", owner.Alias, deps.request)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.listener.Close()
	child := &executorTestChild{running: true}
	worker := &secondaryExecutorWorker{owner: owner, broker: broker, child: child, record: map[string]any{"profile_id": "secondary-profile", "environment_id": "secondary-environment"}}
	calls := 0
	deps.request = func(_ context.Context, bound Account, host, method, path string, _ []byte) (int, string, []byte, error) {
		calls++
		if path != "/backend-api/tbo/secondary-profile/computers/secondary-environment/connect?expected_thread_id=secondary-root" || bound.AccountID != owner.AccountID {
			t.Fatalf("wrong connect target %s", path)
		}
		return 204, "", nil, nil
	}
	if refreshSecondaryExecutor(context.Background(), s, worker, deps)["registered"] != false || calls != 0 {
		t.Fatal("attached before registry success")
	}
	broker.events = append(broker.events, map[string]any{"operation": "register", "status": 200})
	for i := 0; i < 2; i++ {
		refreshSecondaryExecutor(context.Background(), s, worker, deps)
	}
	if calls != 1 || worker.record["connect_http"] != 204 {
		t.Fatal("attachment was retried or lost")
	}
	child.running = false
	if refreshSecondaryExecutor(context.Background(), s, worker, deps)["active"] != false {
		t.Fatal("reported stopped executor active")
	}
}
func TestSecondaryExecutorMappingAndProjectionPreserveRealStatus(t *testing.T) {
	s, _, _, owner := executorTestSetup(t)
	root, err := executorDirectory(s, "executors")
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"owner_alias": owner.Alias, "profile_id": "secondary-profile", "environment_id": "secondary-environment", "primary_environment_id": "primary-environment", "active": true, "registered": true}
	save := func(row map[string]any) {
		t.Helper()
		if err := installSave(filepath.Join(root, "state.json"), map[string]any{"owners": map[string]any{owner.Alias: row}}); err != nil {
			t.Fatal(err)
		}
	}
	save(record)
	if row, _ := SecondaryExecutorMapping(s, owner.Alias); !reflect.DeepEqual(row, record) {
		t.Fatalf("wrong mapping %+v", row)
	}
	source := []byte(`{"thread_id":"root","computers":[{"kind":"openai_cloud","environment_id":"cloud","is_attached":true},{"kind":"user","environment_id":"secondary-environment","status":"offline","is_attached":false,"can_use_for_tasks":false},{"kind":"user","environment_id":"other","status":"connected"}]}`)
	data, err := ProjectExecutorComputers(s, owner.Alias, "secondary-profile", source)
	if err != nil {
		t.Fatal(err)
	}
	var actual, expected map[string]any
	_ = json.Unmarshal(data, &actual)
	_ = json.Unmarshal(source, &expected)
	expected["computers"].([]any)[1].(map[string]any)["environment_id"] = "primary-environment"
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("projection fabricated status: %s", data)
	}
	for field, value := range map[string]any{"owner_alias": "foreign", "profile_id": "main-profile", "environment_id": "../bad", "primary_environment_id": "é", "active": false, "registered": false} {
		copy := map[string]any{}
		for k, v := range record {
			copy[k] = v
		}
		copy[field] = value
		save(copy)
		if row, _ := SecondaryExecutorMapping(s, owner.Alias); row != nil {
			t.Fatalf("accepted invalid %s", field)
		}
	}
	save(record)
	if err := os.Chmod(filepath.Join(root, "state.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if row, _ := SecondaryExecutorMapping(s, owner.Alias); row != nil {
		t.Fatal("accepted shared state")
	}
}
func TestSecondaryExecutorPrivateDirectoryAndIdentityFailClosed(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	deps := executorTestDependencies(s, c, system, owner)
	root, err := executorDirectory(s, "executors")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, owner.Alias)); err != nil {
		t.Fatal(err)
	}
	if _, err := startSecondaryExecutor(context.Background(), s, owner, "primary-environment", deps); err == nil {
		t.Fatal("accepted symlink executor home")
	}
	if err := os.Remove(filepath.Join(root, owner.Alias)); err != nil {
		t.Fatal(err)
	}
	ownerRoot, err := executorDirectory(s, "executors", owner.Alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := installSave(filepath.Join(ownerRoot, "installation.json"), map[string]any{"id": "invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := startSecondaryExecutor(context.Background(), s, owner, "primary-environment", deps); err == nil {
		t.Fatal("accepted invalid persisted installation")
	}
}
func TestProofExecutorRequiresStoppedOwnerBoundProofAndPersistsExit(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	deps := executorTestDependencies(s, c, system, owner)
	root, err := executorDirectory(s, "null-executor")
	if err != nil {
		t.Fatal(err)
	}
	proof := map[string]any{"environment_id": "secondary-environment", "owner_alias": owner.Alias, "executor_stopped": true, "root_thread_id": "secondary-root", "registration_accepted": true}
	path := filepath.Join(root, "proof.json")
	if err := installSave(path, proof); err != nil {
		t.Fatal(err)
	}
	child := &executorTestChild{code: 7}
	deps.launch = func(_ *Settings, _ string, _ int, environment, capability string, sandbox, forward bool) (executorProcess, error) {
		if !sandbox || forward || environment != "secondary-environment" || capability == "" {
			t.Fatal("proof process scope lost")
		}
		return child, nil
	}
	args := []string{"exec-server", "--environment-id", "secondary-environment"}
	if err := runProofExecutor(context.Background(), s, args, deps, false); err != nil {
		t.Fatal(err)
	}
	saved, err := executorRead(path, s.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if saved["executor_stopped"] != true || saved["executor_exit_code"] != float64(7) || saved["executor_pid"] != float64(12345) || child.closed != 1 {
		t.Fatalf("exit proof missing: %+v", saved)
	}
	policy, err := os.ReadFile(filepath.Join(root, "isolation.sb"))
	if err != nil || !strings.Contains(string(policy), "(deny signal (target others))") || !strings.Contains(string(policy), "(deny file-read*") {
		t.Fatal("outer isolation missing")
	}
	for field, value := range map[string]any{"owner_alias": s.SystemAlias, "root_thread_id": "foreign", "executor_stopped": false, "environment_id": "../bad"} {
		copy := map[string]any{}
		for k, v := range proof {
			copy[k] = v
		}
		copy[field] = value
		if err := installSave(path, copy); err != nil {
			t.Fatal(err)
		}
		if err := runProofExecutor(context.Background(), s, args, deps, false); err == nil {
			t.Fatalf("accepted invalid proof %s", field)
		}
	}
}
func TestSecondaryExecutorOwnerRequestRereadsWithoutRefreshing(t *testing.T) {
	s, c, _, owner := executorTestSetup(t)
	path := filepath.Join(s.CQAccounts, "secondary.auth.json")
	catalogueTestWrite(t, path, catalogueTestDocument(owner.AccountID, c.now().Unix()+3600))
	calls := 0
	var token string
	c.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		calls++
		token = r.Header.Get("Authorization")
		return catalogueTestResponse(200, map[string]any{}), nil
	})
	deps := executorDefaults(s, c, owner.Alias)
	if _, _, _, err := deps.request(context.Background(), owner, "chatgpt.com", "GET", "/bound", nil); err != nil {
		t.Fatal(err)
	}
	old := token
	catalogueTestWrite(t, path, catalogueTestDocument(owner.AccountID, c.now().Unix()+7200))
	if _, _, _, err := deps.request(context.Background(), owner, "chatgpt.com", "GET", "/bound", nil); err != nil {
		t.Fatal(err)
	}
	if token == old {
		t.Fatal("stale owner token reused")
	}
	catalogueTestWrite(t, path, catalogueTestDocument("foreign", c.now().Unix()+7200))
	if _, _, _, err := deps.request(context.Background(), owner, "chatgpt.com", "GET", "/bound", nil); err == nil || calls != 2 {
		t.Fatal("changed owner forwarded")
	}
}
func TestSecondaryExecutorLaunchFailureClosesBroker(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	deps := executorTestDependencies(s, c, system, owner)
	deps.launch = func(*Settings, string, int, string, string, bool, bool) (executorProcess, error) {
		return nil, errors.New("launch failed")
	}
	if _, err := startSecondaryExecutor(context.Background(), s, owner, "primary-environment", deps); err == nil {
		t.Fatal("ignored launch failure")
	}
}

func TestNativeExecutorReuseChecksExistingStatusWithoutRegistration(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	catalogueTestWrite(t, filepath.Join(s.CQAccounts, "secondary.auth.json"), catalogueTestDocument(owner.AccountID, c.now().Unix()+3600))
	root, err := executorDirectory(s, "null-executor")
	if err != nil {
		t.Fatal(err)
	}
	proof := map[string]any{"owner_alias": owner.Alias, "registration_accepted": true, "root_thread_id": "secondary-root", "environment_id": "secondary-environment"}
	if err := installSave(filepath.Join(root, "proof.json"), proof); err != nil {
		t.Fatal(err)
	}
	statusEnvironment := "secondary-environment"
	calls := []string{}
	c.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Method+" "+r.URL.Host+r.URL.RequestURI())
		if r.Method != "GET" {
			t.Fatal("native reuse sent a write")
		}
		var value map[string]any
		switch r.URL.Host {
		case "chatgpt.com":
			account := r.Header.Get("ChatGPT-Account-Id")
			if account == system.AccountID {
				value = map[string]any{"profile": map[string]any{"id": "main-profile"}, "selection": map[string]any{"thread_id": "main-root"}}
			} else if account == owner.AccountID {
				value = map[string]any{"profile": map[string]any{"id": "secondary-profile", "active_root_thread_id": "secondary-root"}, "selection": map[string]any{"available": true, "thread_id": "secondary-root"}}
			} else {
				t.Fatal("foreign request owner")
			}
		case executorRegistry:
			if r.Header.Get("ChatGPT-Account-Id") != owner.AccountID || r.URL.Path != "/api/cloud/environment/secondary-environment/status" {
				t.Fatal("wrong registry status owner or target")
			}
			value = map[string]any{"environment_id": statusEnvironment}
		default:
			t.Fatal("foreign request host")
		}
		response := catalogueTestResponse(200, value)
		response.Header = http.Header{"Content-Type": []string{"application/json"}}
		return response, nil
	})
	installation, err := nativeUUID()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"displayName": "Native Mac", "installation_id": installation, "selected_capability_roots": []any{"proposed"}})
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+system.Token)
	headers.Set("ChatGPT-Account-Id", system.AccountID)
	result, metadata, err := ReuseNativeExecutor(context.Background(), s, c, headers, body)
	if err != nil {
		t.Fatal(err)
	}
	if result["executor_id"] != "secondary-environment" || metadata["new_registration_sent"] != false || metadata["capability_roots_granted"] != 0 || metadata["native_proposed_roots"] != 1 || len(calls) != 3 {
		t.Fatalf("wrong reuse result %+v %+v calls=%v", result, metadata, calls)
	}
	statusEnvironment = "foreign"
	if _, _, err := ReuseNativeExecutor(context.Background(), s, c, headers, body); err == nil {
		t.Fatal("accepted foreign registry environment")
	}
}

func TestSecondaryExecutorManagerStopsAllChildrenAndPersistsInactiveOwners(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	document := catalogueTestDocument("another-secondary", c.now().Unix()+3600)
	data, _ := json.Marshal(document)
	second, err := ParseCredential(data, c.now())
	if err != nil {
		t.Fatal(err)
	}
	s.Dots["third"] = Dot{OwnerAlias: second.Alias, OwnerAccountID: second.AccountID, ProfileID: "third-profile", RootThreadID: "third-root"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	children := []*executorTestChild{}
	installations := map[string]string{}
	registered := []string{}
	defaults := func(_ *Settings, _ *CatalogueClient, alias string) executorDependencies {
		deps := executorTestDependencies(s, c, system, owner)
		deps.inventory = func() (Inventory, error) {
			return Inventory{System: system, Accounts: []Account{system, owner, second}}, nil
		}
		deps.request = func(_ context.Context, bound Account, host, method, path string, body []byte) (int, string, []byte, error) {
			if bound.Alias == system.Alias {
				t.Fatal("registered system owner")
			}
			var value map[string]any
			_ = json.Unmarshal(body, &value)
			installations[bound.Alias] = value["installation_id"].(string)
			registered = append(registered, bound.Alias)
			response, _ := json.Marshal(map[string]any{"executor_id": "env-" + bound.Alias})
			return 200, "application/json", response, nil
		}
		deps.launch = func(_ *Settings, root string, _ int, environment, capability string, sandbox, forward bool) (executorProcess, error) {
			if sandbox || forward || !strings.HasSuffix(root, alias) {
				t.Fatal("owner process scope changed")
			}
			child := &executorTestChild{running: true}
			children = append(children, child)
			if len(children) == 2 {
				cancel()
			}
			return child, nil
		}
		return deps
	}
	primary := func(context.Context, *Settings, NativeEvaluator) (string, error) { return "primary-environment", nil }
	if err := runSecondaryExecutors(ctx, s, c, nil, defaults, primary); err != nil {
		t.Fatal(err)
	}
	if len(registered) != 2 || len(installations) != 2 || installations[owner.Alias] == installations[second.Alias] {
		t.Fatal("secondary installation identities were shared")
	}
	for _, child := range children {
		if child.running || child.closed != 1 {
			t.Fatal("child not stopped")
		}
	}
	state, err := executorRead(filepath.Join(s.RuntimeRoot, "executors", "state.json"), s.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	owners := state["owners"].(map[string]any)
	if len(owners) != 2 {
		t.Fatal("missing persisted owner records")
	}
	for _, item := range owners {
		row := item.(map[string]any)
		if row["active"] != false || row["registered"] != false {
			t.Fatal("shutdown invented a live registration")
		}
	}
}

func TestSecondaryExecutorManagerRejectsDuplicatePrivateInstallationIDs(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	document := catalogueTestDocument("another-secondary", c.now().Unix()+3600)
	data, _ := json.Marshal(document)
	second, err := ParseCredential(data, c.now())
	if err != nil {
		t.Fatal(err)
	}
	s.Dots["third"] = Dot{OwnerAlias: second.Alias, ProfileID: "third-profile", RootThreadID: "third-root"}
	id, err := nativeUUID()
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []Account{owner, second} {
		root, err := executorDirectory(s, "executors", account.Alias)
		if err != nil {
			t.Fatal(err)
		}
		if err := installSave(filepath.Join(root, "installation.json"), map[string]any{"id": id}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launches := 0
	defaults := func(_ *Settings, _ *CatalogueClient, alias string) executorDependencies {
		deps := executorTestDependencies(s, c, system, owner)
		deps.inventory = func() (Inventory, error) {
			return Inventory{System: system, Accounts: []Account{system, owner, second}}, nil
		}
		deps.launch = func(*Settings, string, int, string, string, bool, bool) (executorProcess, error) {
			launches++
			cancel()
			return &executorTestChild{}, nil
		}
		return deps
	}
	if err := runSecondaryExecutors(ctx, s, c, nil, defaults, func(context.Context, *Settings, NativeEvaluator) (string, error) { return "primary-environment", nil }); err != nil {
		t.Fatal(err)
	}
	if launches != 1 {
		t.Fatalf("started %d workers for shared installation ID", launches)
	}
	state, err := executorRead(filepath.Join(s.RuntimeRoot, "executors", "state.json"), s.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(state["failures"].(map[string]any)) != 1 {
		t.Fatal("duplicate installation failure not persisted")
	}
}

func TestNativeExecutorStdinEOFStopsChildAndPreservesProof(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	deps := executorTestDependencies(s, c, system, owner)
	root, err := executorDirectory(s, "null-executor")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "proof.json")
	proof := map[string]any{"environment_id": "secondary-environment", "owner_alias": owner.Alias, "executor_stopped": true, "root_thread_id": "secondary-root", "registration_accepted": true}
	if err := installSave(path, proof); err != nil {
		t.Fatal(err)
	}
	child := &executorTestChild{running: true}
	deps.stdin = strings.NewReader("")
	deps.launch = func(_ *Settings, _ string, _ int, _ string, _ string, sandbox, forward bool) (executorProcess, error) {
		if !sandbox || !forward {
			t.Fatal("native wrapper isolation/streams lost")
		}
		return child, nil
	}
	if err := runProofExecutor(context.Background(), s, []string{"exec-server", "--environment-id", "secondary-environment"}, deps, true); err != nil {
		t.Fatal(err)
	}
	saved, err := executorRead(path, s.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if child.running || child.closed != 1 || saved["executor_stopped"] != true || saved["native_singleton_managed"] != true {
		t.Fatal("stdin EOF did not stop native child")
	}
}

type executorTestEvaluator struct {
	value      any
	expression string
}

func (e *executorTestEvaluator) EvaluateAll(_ context.Context, expression string) ([]any, error) {
	e.expression = expression
	return []any{e.value}, nil
}
func TestSecondaryPrimaryEnvironmentUsesOnlyRealReadyNativeState(t *testing.T) {
	s, _, _, _ := executorTestSetup(t)
	e := &executorTestEvaluator{value: map[string]any{"enabled": true, "ready": true, "environment_id": "native-environment"}}
	if actual, err := primaryExecutorEnvironment(context.Background(), s, e); err != nil || actual != "native-environment" {
		t.Fatalf("wrong native environment %q %v", actual, err)
	}
	if !strings.Contains(e.expression, `principal.profile?.id!=="main-profile"`) || !strings.Contains(e.expression, "m.VAn().getState()") || strings.Contains(e.expression, "PLACEHOLDER") {
		t.Fatal("native primary identity was not pinned")
	}
	for _, value := range []any{nil, map[string]any{"enabled": true, "ready": false, "environment_id": "native-environment"}, map[string]any{"enabled": false, "ready": true, "environment_id": "native-environment"}, map[string]any{"enabled": true, "ready": true, "environment_id": "../foreign"}} {
		e.value = value
		if _, err := primaryExecutorEnvironment(context.Background(), s, e); err == nil {
			t.Fatalf("accepted fake native readiness %+v", value)
		}
	}
	s.Dots["duplicate"] = Dot{OwnerAlias: s.SystemAlias, ProfileID: "second-system-profile"}
	if _, err := primaryExecutorEnvironment(context.Background(), s, e); err == nil {
		t.Fatal("accepted ambiguous system Dot")
	}
}

type executorStuckChild struct {
	executorTestChild
	waits []time.Duration
}

func (p *executorStuckChild) closeStdin() error { p.closed++; return nil }
func (p *executorStuckChild) wait(delay time.Duration) bool {
	p.waits = append(p.waits, delay)
	return false
}
func TestProofExecutorDoesNotClaimStoppedWhenProcessCannotBeReaped(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	deps := executorTestDependencies(s, c, system, owner)
	root, err := executorDirectory(s, "null-executor")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "proof.json")
	if err := installSave(path, map[string]any{"environment_id": "secondary-environment", "owner_alias": owner.Alias, "executor_stopped": true, "root_thread_id": "secondary-root"}); err != nil {
		t.Fatal(err)
	}
	child := &executorStuckChild{executorTestChild: executorTestChild{running: true, code: -1}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps.launch = func(*Settings, string, int, string, string, bool, bool) (executorProcess, error) {
		cancel()
		return child, nil
	}
	if err := runProofExecutor(ctx, s, []string{"exec-server", "--environment-id", "secondary-environment"}, deps, false); err == nil {
		t.Fatal("unreaped child was reported stopped")
	}
	saved, err := executorRead(path, s.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if saved["executor_stopped"] != false || child.terminated != 1 || child.killed != 1 || !reflect.DeepEqual(child.waits, []time.Duration{10 * time.Second, 10 * time.Second, 5 * time.Second}) {
		t.Fatalf("wrong shutdown state %+v", saved)
	}
}
