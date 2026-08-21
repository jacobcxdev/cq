package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyPolicyInitialiseApplyAndStatus(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	root := filepath.Join(configHome, "state")
	var output bytes.Buffer
	if err := runProxyPolicy([]string{"initialise", "--state-root", root}, &output); err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyResilienceStateDir != root {
		t.Fatalf("configured state root = %q", cfg.ProxyResilienceStateDir)
	}
	state, err := proxy.OpenProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: bytes.NewReader(bytes.Repeat([]byte{0x61}, 1024)), Now: proxyPolicyNow})
	if err != nil {
		t.Fatal(err)
	}
	digest := state.Routing.SessionDigest([]byte("session"))
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	policy := proxy.RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:              []proxy.AccountPoolV1{{Name: "selected", Members: []codex.AccountKey{"account-a"}}},
		SessionBindings:    []proxy.SessionBindingV1{{SessionDigest: digest, Pool: "selected"}},
		CapabilityEvidence: []proxy.CapabilityEvidenceV1{{AccountKey: "account-a", State: proxy.CapabilitySupported}},
	}
	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(configHome, "policy.json")
	if err := os.WriteFile(policyPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runProxyPolicy([]string{"apply", "--file", policyPath, "--state-root", root}, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runProxyPolicy([]string{"status", "--state-root", root}, &output); err != nil {
		t.Fatal(err)
	}
	var got proxy.RoutingPolicyV1
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RoutingGeneration != 1 || len(got.Pools) != 1 || got.Pools[0].Name != "selected" {
		t.Fatalf("status = %#v", got)
	}
}

func TestProxyPolicyLivePoolAndSessionLifecycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := proxy.InitialiseProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: root, Random: bytes.NewReader(bytes.Repeat([]byte{0x61}, 4096)), Now: proxyPolicyNow,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := proxy.OpenProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: root, Random: bytes.NewReader(bytes.Repeat([]byte{0x62}, 4096)), Now: proxyPolicyNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	resolver := state.Routing.Resolver()
	handler, err := (&proxy.Server{Config: &proxy.Config{ClaudeUpstream: "https://example.test"}, RoutingPolicy: state.Routing, SessionPolicy: resolver}).RuntimeHandler()
	if err != nil {
		t.Fatal(err)
	}
	var gotAuthorization string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotAuthorization = request.Header.Get("Authorization")
		handler.ServeHTTP(writer, request)
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	deps := proxyPolicyDependencies{
		LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local-token"}, nil },
		Doer:       server.Client(),
		Stdin:      bytes.NewBufferString("private-session"),
		ListInventory: func(context.Context) (codex.Inventory, error) {
			return codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "account-a", Identity: codex.AccountIdentity{Email: "a@example.test"}}}}, nil
		},
		LoadAliasIndex: func() (codex.AccountAliasIndex, error) { return codex.AccountAliasIndex{}, nil },
	}
	var output bytes.Buffer
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"pool", "set", "team", "--account", "a@example.test", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"session", "bind", "--pool", "team", "--session-id-stdin", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	if gotAuthorization != "Bearer local-token" {
		t.Fatalf("authorization = %q", gotAuthorization)
	}
	decision := resolver.Resolve([]byte("private-session"), []codex.AccountKey{"account-a"})
	if decision.Status != proxy.PolicyDecisionSelected || len(decision.Allowed) != 1 {
		t.Fatalf("decision = %#v", decision)
	}
	output.Reset()
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"session", "list", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"pool":"team"`)) || bytes.Contains(output.Bytes(), []byte("private-session")) {
		t.Fatalf("session list = %q", output.String())
	}
	var bindings []proxy.SessionBindingV1
	if err := json.Unmarshal(output.Bytes(), &bindings); err != nil || len(bindings) != 1 {
		t.Fatalf("session list = %q, error = %v", output.String(), err)
	}
	output.Reset()
	selector := []string{"--digest", bindings[0].SessionDigest, "--port", strconv.Itoa(port)}
	if err := runProxyPolicyWithDependencies(context.Background(), append([]string{"session", "show"}, selector...), &output, deps); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("private-session")) {
		t.Fatalf("session show disclosed raw ID: %q", output.String())
	}
	output.Reset()
	if err := runProxyPolicyWithDependencies(context.Background(), append([]string{"session", "unbind"}, selector...), &output, deps); err != nil {
		t.Fatal(err)
	}
	decision = resolver.Resolve([]byte("private-session"), []codex.AccountKey{"account-a"})
	if decision.Status != proxy.PolicyDecisionUnbound {
		t.Fatalf("unbound decision = %#v", decision)
	}
}

func TestProxyPolicyRejectsUnknownAndOversizedInput(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := runProxyPolicy([]string{"apply", "--unknown", "x"}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted unknown option")
	}
	path := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runProxyPolicy([]string{"apply", "--state-root", filepath.Join(t.TempDir(), "missing"), "--file", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted oversized policy")
	}
}
