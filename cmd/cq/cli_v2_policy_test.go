package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func init() {
	registerV2Fixture("session-digest-pass", func(t *testing.T) *v2Fixture { return &v2Fixture{Lookup: lookupV2Policy, In: noAccessV2Input{}} })
}
func TestCLIV2PolicyContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "digest pass-through needs no control", Scenario: "session-digest-pass", Args: []string{"codex", "proxy", "session", "digest", "--digest", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--json"}, Exit: 0, Command: "codex proxy session digest", WantJSON: `{"session_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, Forbid: []string{"network", "credentials", "filesystem-write"}})
}

func newV2PolicyRig(t *testing.T) (*proxy.ProxyResilienceState, proxyPolicyDependencies) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "authority")
	options := proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: proxyPolicyNow}
	if err := proxy.InitialiseProxyResilienceState(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	state, err := proxy.OpenProxyResilienceState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := state.Routing.PublishDocument(proxy.RoutingPolicyDocument{SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1, Pools: []proxy.AccountPoolDocument{{Name: "Work", Value: 7, Members: []codex.AccountKey{"account-a"}}}}); err != nil {
		t.Fatal(err)
	}
	handler, err := (&proxy.Server{Config: &proxy.Config{ClaudeUpstream: "https://example.test"}, RoutingPolicy: state.Routing, SessionPolicy: state.Routing.Resolver()}).RuntimeHandler()
	if err != nil {
		t.Fatal(err)
	}
	return state, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic-policy-token"}, nil }, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	}), ListInventory: func(context.Context) (codex.Inventory, error) {
		return codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "account-a", Identity: codex.AccountIdentity{Email: "alice@example.test"}}, {Key: "account-b", Identity: codex.AccountIdentity{Email: "bob@example.test"}}}}, nil
	}, LoadAliasIndex: func() (codex.AccountAliasIndex, error) { return codex.AccountAliasIndex{}, nil }}
}
func policyInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.Parse(append([]string{"codex", "proxy"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}
func TestCLIV2PolicyRawSessionBytes(t *testing.T) {
	state, deps := newV2PolicyRig(t)
	for _, raw := range []string{"identifier", "identifier\n", strings.Repeat("Ω", 2048)} {
		for _, stdin := range []bool{false, true} {
			args := []string{"session", "digest", "--session-id", raw}
			if stdin {
				args = []string{"session", "digest", "--session-id-stdin"}
			}
			out := handleV2PolicyWithDependencies(context.Background(), policyInvocation(t, args...), &cli.Session{In: strings.NewReader(raw)}, deps)
			want, _ := json.Marshal(map[string]string{"session_digest": state.Routing.SessionDigest([]byte(raw))})
			if out.ExitCode != 0 || !bytes.Equal(out.Data, want) {
				t.Fatalf("stdin=%t length=%d outcome=%+v want=%s", stdin, len(raw), out, want)
			}
		}
	}
}

func policyRun(t *testing.T, deps proxyPolicyDependencies, input io.Reader, args ...string) (cli.Outcome, []byte) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var outcome cli.Outcome
	lookup := func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Policy(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			outcome = handleV2PolicyWithDependencies(ctx, inv, s, deps)
			return outcome
		}, ok
	}
	full := append(append([]string{"codex", "proxy"}, args...), "--json")
	exit := cli.Run(context.Background(), full, &cli.Session{In: input, Out: &stdout, Err: &stderr}, lookup)
	command := "codex proxy " + args[0] + " " + args[1]
	code := ""
	if len(outcome.Errors) > 0 {
		code = outcome.Errors[0].Code
	}
	if err := validateV2Envelope(stdout.Bytes(), v2Case{Command: command, Exit: exit, Code: code}); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	if exit != outcome.ExitCode || stderr.Len() != 0 {
		t.Fatalf("%v exit=%d outcome=%+v stderr=%s", args, exit, outcome, &stderr)
	}
	return outcome, stdout.Bytes()
}
func requirePolicyCode(t *testing.T, out cli.Outcome, exit int, code string) {
	t.Helper()
	if out.ExitCode != exit || (code != "" && (len(out.Errors) != 1 || out.Errors[0].Code != code)) {
		t.Fatalf("outcome=%+v want exit=%d code=%s", out, exit, code)
	}
}
func TestCLIV2PolicyTenLeafLifecycle(t *testing.T) {
	state, deps := newV2PolicyRig(t)
	digest := strings.Repeat("a", 64)
	name := `Research Ω; "night"`
	run := func(args ...string) cli.Outcome {
		t.Helper()
		out, _ := policyRun(t, deps, nil, args...)
		requirePolicyCode(t, out, 0, "")
		return out
	}
	run("policy", "show")
	run("pool", "set", name, "--account", "account-b", "--account", "alice@example.test", "--value", "4294967295")
	current := state.Routing.Current()
	poolID := current.Pools[0].ID
	for _, p := range current.Pools {
		if p.Name == name {
			poolID = p.ID
			if p.Value != 4294967295 || !reflect.DeepEqual(p.Members, []codex.AccountKey{"account-a", "account-b"}) {
				t.Fatal(p)
			}
		}
	}
	run("session", "bind", "--pool", strings.ToLower(name), "--digest", digest)
	run("session", "bind", "--pool", name, "--digest", digest)
	if len(state.Routing.Current().SessionBindings) != 1 {
		t.Fatal("equivalent bind duplicated binding")
	}
	out := run("session", "show", "--digest", digest)
	want, _ := json.Marshal(map[string]any{"binding": proxy.SessionBindingDocument{SessionDigest: digest, Pool: name}})
	if !bytes.Equal(out.Data, want) {
		t.Fatalf("binding=%s", out.Data)
	}
	run("pool", "set", strings.ToLower(name), "--account", "account-b")
	for _, p := range state.Routing.Current().Pools {
		if p.ID == poolID && (p.Value != 4294967295 || p.Name != name) {
			t.Fatal("omission or casing lost", p)
		}
	}
	run("pool", "value", name, "0")
	run("pool", "rename", name, "Renamed Ω")
	for _, p := range state.Routing.Current().Pools {
		if p.Name == "Renamed Ω" && (p.ID != poolID || p.Value != 0) {
			t.Fatal("rename lost identity", p)
		}
	}
	out = run("session", "list")
	want, _ = json.Marshal(map[string]any{"bindings": []proxy.SessionBindingDocument{{SessionDigest: digest, Pool: "Renamed Ω"}}})
	if !bytes.Equal(out.Data, want) {
		t.Fatalf("bindings=%s", out.Data)
	}
	run("session", "digest", "--digest", digest)
	run("session", "unbind", "--digest", digest)
	out, _ = policyRun(t, deps, nil, "session", "unbind", "--digest", digest)
	requirePolicyCode(t, out, 3, "session_binding_not_found")
	out, _ = policyRun(t, deps, nil, "session", "show", "--digest", digest)
	requirePolicyCode(t, out, 3, "session_binding_not_found")
	out, _ = policyRun(t, deps, nil, "session", "bind", "--digest", digest, "--pool", "missing")
	requirePolicyCode(t, out, 3, "pool_not_found")
	prior, _ := state.Routing.Document()
	replacement := proxy.RoutingPolicyDocument{SchemaVersion: 1, AuthorityGeneration: prior.AuthorityGeneration + 1, RoutingGeneration: prior.RoutingGeneration + 1, EffectiveGeneration: prior.EffectiveGeneration}
	body, _ := json.Marshal(replacement)
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	out = run("policy", "apply", "--file", file)
	if len(state.Routing.Current().Pools) != 0 {
		t.Fatal("apply was a patch, not replacement")
	}
	out, _ = policyRun(t, deps, nil, "policy", "apply", "--file", file)
	requirePolicyCode(t, out, 6, "policy_generation_conflict")
}
func TestCLIV2PolicyValidationBeforeAccess(t *testing.T) {
	for _, args := range [][]string{
		{"policy", "apply"}, {"policy", "show", "--state-dir", "/missing", "--port", "19280"}, {"policy", "show", "--state-dir", "relative"},
		{"pool", "set", "work"}, {"pool", "set", "work", "--account", "a", "--value", "4294967296"}, {"pool", "value", "work", "-1"}, {"pool", "rename", "work", "\n"},
		{"session", "digest"}, {"session", "digest", "--session-id", "x", "--session-id-stdin"}, {"session", "digest", "--digest", strings.Repeat("A", 64)},
		{"session", "list", "--session-id-stdin"}, {"session", "list", "--digest", strings.Repeat("a", 64)}, {"session", "list", "--session-id", "x"},
		{"session", "digest", "--session-id", strings.Repeat("x", 4097)}, {"session", "digest", "--session-id", string([]byte{0xff})},
		{"session", "bind", "--session-id", "x"}, {"policy", "show", "--port", "65536"}, {"policy", "show", "--timeout", "0s"},
	} {
		runV2Case(t, v2Case{Name: fmt.Sprint(args), Scenario: "no-access", Args: append(append([]string{"codex", "proxy"}, args...), "--json"), Command: "codex proxy " + args[0] + " " + args[1], Exit: 2, Code: "routing_invalid_argument", Forbid: []string{"lookup"}})
	}
	for _, raw := range []string{"", strings.Repeat("x", 4097), string([]byte{0xff})} {
		inv := policyInvocation(t, "session", "digest", "--session-id-stdin")
		out := handleV2PolicyWithDependencies(context.Background(), inv, &cli.Session{In: strings.NewReader(raw)}, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { t.Fatal("invalid stdin read config"); return nil, nil }})
		requirePolicyCode(t, out, 2, "routing_invalid_argument")
	}
}
func TestCLIV2PolicyPoolFailuresBeforeWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []string
		exit int
		code string
	}{
		{"duplicate", []string{"account-a", "alice@example.test"}, 2, "routing_invalid_argument"},
		{"missing", []string{"account-a", "missing"}, 3, "routing_account_not_found"},
		{"ambiguous", []string{"shared@example.test"}, 6, "routing_account_ambiguous"},
		{"inventory", []string{"account-a"}, 4, "routing_inventory_unavailable"},
		{"aliases", []string{"account-a"}, 4, "routing_inventory_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, deps := newV2PolicyRig(t)
			deps.Doer = testDoer(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid account reached control")
				return nil, nil
			})
			if tc.name == "inventory" {
				deps.ListInventory = func(context.Context) (codex.Inventory, error) { return codex.Inventory{}, errors.New("private") }
			}
			if tc.name == "aliases" {
				deps.LoadAliasIndex = func() (codex.AccountAliasIndex, error) { return codex.AccountAliasIndex{}, errors.New("private") }
			}
			if tc.name == "ambiguous" {
				deps.ListInventory = func(context.Context) (codex.Inventory, error) {
					return codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "a", Identity: codex.AccountIdentity{Email: "shared@example.test"}}, {Key: "b", Identity: codex.AccountIdentity{Email: "shared@example.test"}}}}, nil
				}
			}
			args := []string{"pool", "set", "Work"}
			for _, ref := range tc.refs {
				args = append(args, "--account", ref)
			}
			out, _ := policyRun(t, deps, nil, args...)
			requirePolicyCode(t, out, tc.exit, tc.code)
		})
	}
	state, deps := newV2PolicyRig(t)
	before := state.Routing.Current()
	for _, args := range [][]string{{"pool", "value", "missing", "1"}, {"pool", "rename", "missing", "other"}} {
		out, _ := policyRun(t, deps, nil, args...)
		requirePolicyCode(t, out, 3, "pool_not_found")
	}
	out, _ := policyRun(t, deps, nil, "pool", "set", "Other", "--account", "account-a")
	requirePolicyCode(t, out, 0, "")
	out, _ = policyRun(t, deps, nil, "pool", "rename", "Work", "other")
	requirePolicyCode(t, out, 6, "pool_name_conflict")
	if state.Routing.Current().RoutingGeneration != before.RoutingGeneration+1 {
		t.Fatal("rejected mutation changed generation")
	}
}
func TestCLIV2PolicyStrictDocumentsAndOfflineAuthority(t *testing.T) {
	state, deps := newV2PolicyRig(t)
	before := state.Routing.Current()
	valid := `{"schema_version":1,"authority_generation":2,"routing_generation":2,"effective_generation":1}`
	for _, body := range []string{valid + `{}`, strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), strings.Replace(valid, `"schema_version":1`, `"unknown":1,"schema_version":1`, 1), `null`, strings.Repeat("x", proxyPolicyMaxBytes+1), valid[:len(valid)-1] + `,"pools":[{"name":"Work","members":[]}]}`, strings.Replace(valid, `"authority_generation":2`, `"authority_generation":18446744073709551616`, 1)} {
		file := filepath.Join(t.TempDir(), "policy.json")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, _ := policyRun(t, deps, nil, "policy", "apply", "--file", file)
		requirePolicyCode(t, out, 2, "policy_document_invalid")
		if !reflect.DeepEqual(before, state.Routing.Current()) {
			t.Fatal("invalid policy partially committed")
		}
	}
	root := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"show", "apply"} {
		args := []string{"policy", action, "--state-dir", root}
		if action == "apply" {
			args = append(args, "--file", file)
		}
		out, _ := policyRun(t, proxyPolicyDependencies{}, nil, args...)
		requirePolicyCode(t, out, 1, "routing_io_failed")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("offline command bootstrapped missing root", err)
	}
	options := proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: proxyPolicyNow}
	if err := proxy.InitialiseProxyResilienceState(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	owner, err := proxy.OpenProxyResilienceState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := policyRun(t, proxyPolicyDependencies{}, nil, "policy", "apply", "--file", file, "--state-dir", root)
	requirePolicyCode(t, out, 6, "routing_conflict")
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	out, _ = policyRun(t, proxyPolicyDependencies{}, nil, "policy", "apply", "--file", file, "--state-dir", root)
	requirePolicyCode(t, out, 0, "")
	out, _ = policyRun(t, proxyPolicyDependencies{}, nil, "policy", "show", "--state-dir", root)
	requirePolicyCode(t, out, 0, "")
	out, _ = policyRun(t, proxyPolicyDependencies{}, nil, "policy", "apply", "--file", file, "--state-dir", root)
	requirePolicyCode(t, out, 6, "policy_generation_conflict")
}
func TestCLIV2PolicyBudgetAndPorts(t *testing.T) {
	inv := policyInvocation(t, "policy", "show", "--timeout", "1ms")
	for _, phase := range []string{"prepare", "config", "request"} {
		t.Run(phase, func(t *testing.T) {
			reads, requests := 0, 0
			out := handleV2PolicyWithPreparation(context.Background(), inv, nil, func(ctx context.Context) (proxyPolicyDependencies, error) {
				if phase == "prepare" {
					<-ctx.Done()
					return proxyPolicyDependencies{}, errors.New("private")
				}
				return proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) {
					reads++
					if phase == "config" {
						<-ctx.Done()
					}
					return &proxy.Config{LocalToken: "synthetic"}, nil
				}, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
					requests++
					<-r.Context().Done()
					return nil, r.Context().Err()
				})}, nil
			})
			requirePolicyCode(t, out, 7, "routing_timeout")
			if phase == "prepare" && reads != 0 || phase != "request" && requests != 0 {
				t.Fatalf("reads=%d requests=%d", reads, requests)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := handleV2PolicyWithPreparation(ctx, inv, nil, func(context.Context) (proxyPolicyDependencies, error) {
		t.Fatal("cancelled preparation")
		return proxyPolicyDependencies{}, nil
	})
	requirePolicyCode(t, out, 130, "interrupted")
	for _, tc := range []struct{ configured, explicit, want int }{{0, 0, 19280}, {12345, 0, 12345}, {12345, 23456, 23456}} {
		args := []string{"policy", "show"}
		if tc.explicit != 0 {
			args = append(args, "--port", fmt.Sprint(tc.explicit))
		}
		out := handleV2PolicyWithDependencies(context.Background(), policyInvocation(t, args...), nil, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: tc.configured, LocalToken: "synthetic"}, nil }, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != fmt.Sprintf("127.0.0.1:%d", tc.want) {
				t.Fatal(r.URL.Host)
			}
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > 10*time.Second {
				t.Fatal("missing total budget")
			}
			return nil, errors.New("offline")
		})})
		requirePolicyCode(t, out, 4, "routing_control_unavailable")
	}
}

func TestCLIV2PolicyErrorReceiptsAndStatusPrecedence(t *testing.T) {
	for _, tc := range []struct {
		status, exit  int
		receipt, code string
	}{
		{401, 5, "routing_io_failed", "routing_auth_failed"}, {403, 5, "policy_document_invalid", "routing_auth_failed"}, {503, 4, "routing_io_failed", "routing_control_unavailable"},
		{409, 6, "", "routing_conflict"}, {409, 6, "private-unknown", "routing_conflict"}, {409, 6, "policy_generation_conflict", "policy_generation_conflict"},
		{409, 2, "policy_document_invalid", "policy_document_invalid"}, {409, 1, "routing_io_failed", "routing_io_failed"}, {400, 2, "policy_document_invalid", "policy_document_invalid"}, {400, 2, "", "routing_invalid_argument"},
	} {
		for _, args := range [][]string{{"policy", "show"}, {"session", "digest", "--session-id", "private"}} {
			t.Run(fmt.Sprintf("%v/%d/%s", args, tc.status, tc.receipt), func(t *testing.T) {
				body := newReserveErrorBody(false)
				deps := proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "private-secret"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tc.status, Header: http.Header{"X-Cq-Policy-Error": []string{tc.receipt}}, Body: body}, nil
				})}
				out, _ := policyRun(t, deps, nil, args...)
				wantExit, wantCode := tc.exit, tc.code
				if args[0] == "session" && (tc.code == "policy_generation_conflict" || tc.code == "policy_document_invalid") {
					wantExit, wantCode = 6, "routing_conflict"
				}
				requirePolicyCode(t, out, wantExit, wantCode)
				if body.reads != 0 || body.closes != 1 {
					t.Fatalf("body reads=%d closes=%d", body.reads, body.closes)
				}
			})
		}
	}
	for _, tc := range []struct {
		body, code string
		exit       int
	}{{`{"error":"pool_not_found"}`, "pool_not_found", 3}, {`{"error":"pool_name_conflict"}`, "pool_name_conflict", 6}, {`{"error":"invalid_pool_name"}`, "routing_invalid_argument", 2}, {`{"error":"pool_mutation_rejected"}`, "routing_conflict", 6}, {"private-secret", "routing_conflict", 6}} {
		deps := proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})}
		out, _ := policyRun(t, deps, nil, "pool", "rename", "Work", "Other")
		requirePolicyCode(t, out, tc.exit, tc.code)
	}
	for _, tc := range []struct {
		name string
		cfg  *proxy.Config
		err  error
		exit int
		code string
	}{{"no config", nil, os.ErrNotExist, 1, "routing_io_failed"}, {"no token", &proxy.Config{}, nil, 5, "routing_auth_failed"}, {"nil config", nil, nil, 5, "routing_auth_failed"}} {
		deps := proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return tc.cfg, tc.err }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid config reached transport")
			return nil, nil
		})}
		out, _ := policyRun(t, deps, nil, "policy", "show")
		requirePolicyCode(t, out, tc.exit, tc.code)
	}
	for _, body := range []string{"invalid", strings.Repeat("x", proxyPolicyMaxBytes+1)} {
		deps := proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		out, _ := policyRun(t, deps, nil, "policy", "show")
		requirePolicyCode(t, out, 1, "routing_io_failed")
	}
}
func TestCLIV2PolicyGenerationRaceAndExactIntegers(t *testing.T) {
	state, deps := newV2PolicyRig(t)
	underlying := deps.Doer
	puts := 0
	deps.Doer = testDoer(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut {
			puts++
			current, _ := state.Routing.Document()
			next := nextProxyRoutingPolicy(current)
			if err := state.Routing.PublishDocument(next); err != nil {
				t.Fatal(err)
			}
		}
		return underlying.Do(r)
	})
	out, _ := policyRun(t, deps, nil, "pool", "set", "Work", "--account", "account-b")
	requirePolicyCode(t, out, 6, "routing_conflict")
	if puts != 1 || state.Routing.Current().Pools[0].Members[0] != "account-a" {
		t.Fatal("race overwrote concurrent state")
	}
	deps.Doer = underlying
	body := `{"schema_version":1,"authority_generation":18446744073709551615,"routing_generation":18446744073709551615,"effective_generation":9007199254740993,"pools":[{"name":"Ω","value":4294967295,"members":["a"]}]}`
	deps.Doer = testDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	out, raw := policyRun(t, deps, nil, "policy", "show")
	requirePolicyCode(t, out, 0, "")
	for _, number := range []string{"18446744073709551615", "9007199254740993", "4294967295"} {
		if !bytes.Contains(raw, []byte(number)) {
			t.Fatalf("rounded integer %s: %s", number, raw)
		}
	}
	want := `{"policy":{"schema_version":1,"authority_generation":18446744073709551615,"routing_generation":18446744073709551615,"effective_generation":9007199254740993,"pools":[{"name":"Ω","value":4294967295,"members":["a"]}],"session_bindings":[],"capability_evidence":[],"capability_pool":null,"capability_predicates":[],"capability_routing_evidence":[],"delegations":[]}}`
	if string(out.Data) != want {
		t.Fatalf("public resource=%s", out.Data)
	}
	pretty := new(bytes.Buffer)
	if err := json.Indent(pretty, []byte(strings.TrimSuffix(strings.TrimPrefix(want, `{"policy":`), "}")), "", "  "); err != nil {
		t.Fatal(err)
	}
	if out.Human != pretty.String()+"\n" {
		t.Fatalf("human=%q want=%q", out.Human, pretty.String()+"\n")
	}
}
func TestCLIV2PolicyAllHelpAndAliases(t *testing.T) {
	for _, path := range []string{"policy apply", "policy show", "pool rename", "pool set", "pool value", "session bind", "session digest", "session list", "session show", "session unbind"} {
		filename := filepath.Join("..", "..", "specs", "cli-v2", "help", "codex-proxy-"+strings.ReplaceAll(path, " ", "-")+".txt")
		want, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := append([]string{"codex", "proxy"}, strings.Fields(path)...)
		args = append(args, "--help", "--json")
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) { t.Fatal("help looked up handler"); return nil, false })
		if exit != 0 || !bytes.Equal(stdout.Bytes(), want) || stderr.Len() != 0 {
			t.Fatalf("help %s exit=%d", path, exit)
		}
	}
	state, deps := newV2PolicyRig(t)
	_ = state
	for _, args := range [][]string{{"proxy", "policy", "status"}, {"proxy", "policy", "pool", "set", "Alias Pool", "--account", "account-a"}, {"proxy", "policy", "session", "digest", "--digest", strings.Repeat("a", 64)}} {
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), append(args, "--json"), &cli.Session{Out: &stdout, Err: &stderr}, func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Policy(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2PolicyWithDependencies(ctx, inv, s, deps)
			}, ok
		})
		if exit != 0 || !bytes.Contains(stdout.Bytes(), []byte(`"schema_version":2`)) || !bytes.Contains(stdout.Bytes(), []byte(`"command":"codex proxy`)) {
			t.Fatalf("alias %v exit=%d stdout=%s", args, exit, &stdout)
		}
	}
}
func TestCLIV2PolicyProductionDoesNotBootstrap(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	inv := policyInvocation(t, "policy", "show")
	out := handleV2Policy(context.Background(), inv, nil)
	requirePolicyCode(t, out, 1, "routing_io_failed")
	inv = policyInvocation(t, "session", "digest", "--digest", strings.Repeat("a", 64))
	out = handleV2PolicyWithPreparation(context.Background(), inv, nil, func(context.Context) (proxyPolicyDependencies, error) {
		t.Fatal("digest resolved roots")
		return proxyPolicyDependencies{}, nil
	})
	requirePolicyCode(t, out, 0, "")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("bootstrapped authority: %v %v", entries, err)
	}
}

func TestCLIV2PolicyStdinBudget(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	fallback := time.AfterFunc(250*time.Millisecond, func() { _ = writer.Close() })
	defer fallback.Stop()
	start := time.Now()
	out := handleV2PolicyWithDependencies(context.Background(), policyInvocation(t, "session", "digest", "--session-id-stdin", "--timeout", "10ms"), &cli.Session{In: reader}, proxyPolicyDependencies{})
	requirePolicyCode(t, out, 7, "routing_timeout")
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("stdin read exceeded the whole operation budget")
	}
}

func init() {
	registerV2Fixture("session-stdin-invalid", func(t *testing.T) *v2Fixture { return noAccessV2Fixture(t) })
	registerV2Fixture("pool-unicode", func(t *testing.T) *v2Fixture {
		_, deps := newV2PolicyRig(t)
		f := &v2Fixture{Secrets: []string{"synthetic-policy-token"}}
		control := deps.Doer
		deps.Doer = testDoer(func(r *http.Request) (*http.Response, error) {
			f.Call("control")
			if r.Method == http.MethodPut {
				f.Call("filesystem-write")
			}
			return control.Do(r)
		})
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Policy(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2PolicyWithDependencies(ctx, inv, s, deps)
			}, ok
		}
		return f
	})
	registerV2Fixture("policy-no-root", func(t *testing.T) *v2Fixture {
		base := t.TempDir()
		file := filepath.Join(base, "policy.json")
		if err := os.WriteFile(file, []byte(`{"schema_version":1,"authority_generation":1,"routing_generation":1,"effective_generation":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(base, "missing")
		t.Cleanup(func() {
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Error("missing authority was created")
			}
		})
		return &v2Fixture{Lookup: func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Policy(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				inv.Options["file"] = []string{file}
				inv.Options["state-dir"] = []string{root}
				return handleV2PolicyWithDependencies(ctx, inv, s, proxyPolicyDependencies{})
			}, ok
		}}
	})
}
func TestCLIV2PolicyRequiredFixtures(t *testing.T) {
	runV2Case(t, v2Case{Name: "missing offline authority never initialises", Scenario: "policy-no-root", Args: []string{"codex", "proxy", "policy", "apply", "--file", "fixture-policy", "--state-dir", "/fixture-authority", "--json"}, Command: "codex proxy policy apply", Exit: 1, Code: "routing_io_failed", Forbid: []string{"network", "credentials", "filesystem-write"}})
	runV2Case(t, v2Case{Name: "two selectors never read stdin", Scenario: "session-stdin-invalid", Args: []string{"codex", "proxy", "session", "digest", "--session-id-stdin", "--session-id", "private", "--json"}, Command: "codex proxy session digest", Exit: 2, Code: "routing_invalid_argument", Forbid: []string{"lookup"}})
	runV2Case(t, v2Case{Name: "literal Unicode and quotes", Scenario: "pool-unicode", Args: []string{"codex", "proxy", "pool", "set", `Research Ω; "night"`, "--account", "account-a", "--json"}, Command: "codex proxy pool set", WantJSON: `{"policy":{"schema_version":1,"pools":[{"name":"Research Ω; \"night\"","value":0,"members":["account-a"]},{"name":"Work","value":7,"members":["account-a"]}]}}`, Calls: map[string]int{"control": 2, "filesystem-write": 1}})
}
func TestCLIV2PolicyPublicEvidenceNullabilityAndHumanEscaping(t *testing.T) {
	when := time.Date(2026, 9, 23, 1, 2, 3, 456, time.FixedZone("offset", 3600))
	text := "opaque\u0085identity"
	p := proxy.RoutingPolicyDocument{SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, Pools: []proxy.AccountPoolDocument{{Name: "Work", Members: []codex.AccountKey{codex.AccountKey(text)}}}, CapabilityRoutingEvidence: []proxy.CapabilityRoutingEvidenceV1{{SchemaVersion: 1, AccountKey: codex.AccountKey(text), ObservedAt: when, RoutingGeneration: 1}}, Delegations: []proxy.CallerDelegationV1{{Caller: text, Accounts: []codex.AccountKey{"a"}, ExpiresAt: when}}}
	out := v2PolicyOutcome(p, "policy")
	if !bytes.Contains(out.Data, []byte(`"expires_at":null`)) || !bytes.Contains(out.Data, []byte(`"observed_at":"2026-09-23T00:02:03.000000456Z"`)) || !bytes.Contains(out.Data, []byte(`"value":0`)) {
		t.Fatalf("required fields missing: %s", out.Data)
	}
	if strings.Contains(out.Human, "\u0085") || !strings.Contains(out.Human, `\u0085`) {
		t.Fatal("human controls not escaped")
	}
	var data struct {
		Policy v2PolicyDocument `json:"policy"`
	}
	if err := json.Unmarshal(out.Data, &data); err != nil || string(data.Policy.Pools[0].Members[0]) != text {
		t.Fatal("JSON human escaping altered identity", err)
	}
}

type v2PolicyLateReader struct {
	release, finished chan struct{}
	content           *strings.Reader
}

func (r *v2PolicyLateReader) Read(p []byte) (int, error) {
	<-r.release
	n, err := r.content.Read(p)
	if err == io.EOF {
		close(r.finished)
	}
	return n, err
}
func TestCLIV2PolicyStdinLateCompletionCannotContactControl(t *testing.T) {
	input := &v2PolicyLateReader{make(chan struct{}), make(chan struct{}), strings.NewReader("private-session\n")}
	var calls atomic.Int32
	deps := proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { calls.Add(1); return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unreachable") })}
	out := handleV2PolicyWithDependencies(context.Background(), policyInvocation(t, "session", "bind", "--pool", "Work", "--session-id-stdin", "--timeout", "10ms"), &cli.Session{In: input}, deps)
	close(input.release)
	select {
	case <-input.finished:
	case <-time.After(time.Second):
		t.Fatal("reader did not finish after caller released input")
	}
	requirePolicyCode(t, out, 7, "routing_timeout")
	if calls.Load() != 0 {
		t.Fatal("late read contacted state/control")
	}
}

func TestCLIV2PolicyForeignReceiptDoesNotAssumeRenameArguments(t *testing.T) {
	for _, args := range [][]string{{"policy", "show"}, {"session", "list"}, {"pool", "value", "Work", "1"}} {
		inv := policyInvocation(t, args...)
		out := v2PolicyFailure(&proxyPolicyControlError{status: 409, code: "pool_name_conflict"}, inv)
		want := "routing_conflict"
		if args[0] == "pool" {
			want = "pool_name_conflict"
		}
		requirePolicyCode(t, out, 6, want)
	}
}

func TestCLIV2PolicySessionListOrderingIsReadOnly(t *testing.T) {
	state, deps := newV2PolicyRig(t)
	document, _ := state.Routing.Document()
	document = nextProxyRoutingPolicy(document)
	document.SessionBindings = []proxy.SessionBindingDocument{{SessionDigest: strings.Repeat("b", 64), Pool: "Work"}, {SessionDigest: strings.Repeat("a", 64), Pool: "Work"}}
	if err := state.Routing.PublishDocument(document); err != nil {
		t.Fatal(err)
	}
	before := state.Routing.Current()
	out, _ := policyRun(t, deps, nil, "session", "list")
	requirePolicyCode(t, out, 0, "")
	want, _ := json.Marshal(struct {
		Bindings []proxy.SessionBindingDocument `json:"bindings"`
	}{[]proxy.SessionBindingDocument{document.SessionBindings[1], document.SessionBindings[0]}})
	if !bytes.Equal(out.Data, want) || !reflect.DeepEqual(before, state.Routing.Current()) {
		t.Fatalf("list ordering mutated state or was unsorted: %s", out.Data)
	}
}

func TestCLIV2PolicyRejectsNonNullableNullBeforePublication(t *testing.T) {
	const base = `{"schema_version":1,"authority_generation":2,"routing_generation":2,"effective_generation":1,"pools":[{"name":"Work","value":7,"members":["account-a"]}]}`
	for _, tc := range []struct{ name, old, replacement string }{
		{"schema", "\"schema_version\":1", "\"schema_version\":null"},
		{"authority generation", "\"authority_generation\":2", "\"authority_generation\":null"},
		{"routing generation", "\"routing_generation\":2", "\"routing_generation\":null"},
		{"effective generation", "\"effective_generation\":1", "\"effective_generation\":null"},
		{"pool value", "\"value\":7", "\"value\":null"},
		{"duplicate pool value", "\"value\":7", "\"value\":null,\"value\":7"},
		{"identical duplicate nested value", "\"value\":7", "\"value\":7,\"value\":7"},
		{"duplicate nested value", "\"value\":7", "\"value\":8,\"value\":7"},
		{"identical duplicate root", "\"schema_version\":1", "\"schema_version\":1,\"schema_version\":1"},
		{"pool name", "\"name\":\"Work\"", "\"name\":null"},
		{"pool members", "\"members\":[\"account-a\"]", "\"members\":null"},
		{"member element", "[\"account-a\"]", "[null]"},
		{"pool element", "{\"name\":\"Work\",\"value\":7,\"members\":[\"account-a\"]}", "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, deps := newV2PolicyRig(t)
			before := state.Routing.Current()
			calls := 0
			underlying := deps.Doer
			deps.Doer = testDoer(func(r *http.Request) (*http.Response, error) { calls++; return underlying.Do(r) })
			file := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(file, []byte(strings.Replace(base, tc.old, tc.replacement, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			out, _ := policyRun(t, deps, nil, "policy", "apply", "--file", file)
			requirePolicyCode(t, out, 2, "policy_document_invalid")
			if calls != 0 || !reflect.DeepEqual(before, state.Routing.Current()) {
				t.Fatalf("invalid input accessed control or changed authority: calls=%d", calls)
			}
		})
	}
	for _, field := range []string{"pools", "session_bindings", "capability_evidence", "capability_predicates", "capability_routing_evidence", "delegations"} {
		t.Run(field+" array", func(t *testing.T) {
			state, deps := newV2PolicyRig(t)
			before := state.Routing.Current()
			body := `{"schema_version":1,"authority_generation":2,"routing_generation":2,"effective_generation":1,"` + field + `":null}`
			file := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			deps.Doer = testDoer(func(*http.Request) (*http.Response, error) {
				t.Fatal("null array reached publication")
				return nil, nil
			})
			out, _ := policyRun(t, deps, nil, "policy", "apply", "--file", file)
			requirePolicyCode(t, out, 2, "policy_document_invalid")
			if !reflect.DeepEqual(before, state.Routing.Current()) {
				t.Fatal("null array changed authority")
			}
		})
	}
}

func v2PolicyNullableEvidenceFixture() []byte {
	predicate := proxy.CapabilityPredicateCoreV1{SchemaVersion: 1, Capability: "model.invoke", ProductSurface: "desktop", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5"}
	document := proxy.RoutingPolicyDocument{SchemaVersion: 1, AuthorityGeneration: 2, RoutingGeneration: 2, EffectiveGeneration: 1, Pools: []proxy.AccountPoolDocument{{Name: "Work", Value: 7, Members: []codex.AccountKey{"account-a"}}}, SessionBindings: []proxy.SessionBindingDocument{{SessionDigest: strings.Repeat("a", 64), Pool: "Work"}}, CapabilityEvidence: []proxy.CapabilityEvidenceV1{{AccountKey: "account-a", State: proxy.CapabilitySupported}}, CapabilityPool: "Work", CapabilityPredicates: []proxy.CapabilityPredicateCoreV1{predicate}, CapabilityRoutingEvidence: []proxy.CapabilityRoutingEvidenceV1{{SchemaVersion: 1, AccountKey: "account-a", AccountKeyHMAC: strings.Repeat("a", 64), Workspace: "synthetic", Capability: predicate.Capability, ProductSurface: predicate.ProductSurface, AccessPath: predicate.AccessPath, AuthMode: predicate.AuthMode, RequestedModel: predicate.RequestedModel, EffectiveModel: predicate.EffectiveModel, Source: "synthetic", State: proxy.CapabilityEvidenceEligible, ObservedAt: time.Now().UTC().Add(-time.Minute), RoutingGeneration: 2, Authenticated: true}}, Delegations: []proxy.CallerDelegationV1{{Caller: "fixture", Accounts: []codex.AccountKey{"account-a"}, ExpiresAt: time.Now().UTC().Add(time.Hour)}}}
	body, _ := json.Marshal(v2PublicPolicy(document))
	return body
}
func TestCLIV2PolicyNestedNullabilityBeforePublication(t *testing.T) {
	for _, tc := range []struct{ array, field string }{
		{"session_bindings", "session_digest"}, {"session_bindings", "pool"}, {"capability_evidence", "account_key"}, {"capability_evidence", "state"},
		{"capability_predicates", "schema_version"}, {"capability_predicates", "capability"},
		{"capability_routing_evidence", "schema_version"}, {"capability_routing_evidence", "routing_generation"}, {"capability_routing_evidence", "authenticated"}, {"capability_routing_evidence", "observed_at"},
		{"delegations", "caller"}, {"delegations", "accounts"}, {"delegations", "expires_at"},
	} {
		t.Run(tc.array+"/"+tc.field, func(t *testing.T) {
			state, deps := newV2PolicyRig(t)
			before := state.Routing.Current()
			var document map[string]any
			if err := json.Unmarshal(v2PolicyNullableEvidenceFixture(), &document); err != nil {
				t.Fatal(err)
			}
			document[tc.array].([]any)[0].(map[string]any)[tc.field] = nil
			body, _ := json.Marshal(document)
			file := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(file, body, 0o600); err != nil {
				t.Fatal(err)
			}
			deps.Doer = testDoer(func(*http.Request) (*http.Response, error) {
				t.Fatal("non-nullable nested field reached publication")
				return nil, nil
			})
			out, _ := policyRun(t, deps, nil, "policy", "apply", "--file", file)
			requirePolicyCode(t, out, 2, "policy_document_invalid")
			if !reflect.DeepEqual(before, state.Routing.Current()) {
				t.Fatal("nested null changed authority")
			}
		})
	}
	for _, value := range []string{`"7"`, `true`, `7.5`, `4294967296`} {
		t.Run("integer type "+value, func(t *testing.T) {
			body := `{"schema_version":1,"authority_generation":2,"routing_generation":2,"effective_generation":1,"pools":[{"name":"Work","value":` + value + `,"members":["account-a"]}]}`
			file := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			out, _ := policyRun(t, proxyPolicyDependencies{}, nil, "policy", "apply", "--file", file)
			requirePolicyCode(t, out, 2, "policy_document_invalid")
		})
	}
}
func TestCLIV2PolicyPermittedNullAndOmissionControls(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        []byte
		value       proxy.PoolValue
		hasEvidence bool
	}{
		{"omitted pool value and optional arrays", []byte(`{"schema_version":1,"authority_generation":2,"routing_generation":2,"effective_generation":1,"pools":[{"name":"Work","members":["account-a"]}]}`), 0, false},
		{"nullable capability pool", []byte(`{"schema_version":1,"authority_generation":2,"routing_generation":2,"effective_generation":1,"pools":[{"name":"Work","value":7,"members":["account-a"]}],"capability_pool":null}`), 7, false},
		{"nullable evidence expiry", v2PolicyNullableEvidenceFixture(), 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, deps := newV2PolicyRig(t)
			file := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(file, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			out, _ := policyRun(t, deps, nil, "policy", "apply", "--file", file)
			requirePolicyCode(t, out, 0, "")
			current := state.Routing.Current()
			if current.RoutingGeneration != 2 || current.Pools[0].Value != tc.value {
				t.Fatalf("omission semantics changed: %+v", current)
			}
			if tc.hasEvidence {
				if len(current.CapabilityRoutingEvidence) != 1 || current.CapabilityRoutingEvidence[0].ExpiresAt != nil {
					t.Fatal("nullable evidence expiry lost")
				}
			} else if !bytes.Contains(out.Data, []byte(`"capability_pool":null`)) {
				t.Fatal("nullable capability pool lost")
			}
		})
	}
}
