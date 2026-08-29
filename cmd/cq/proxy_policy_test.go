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
	policy := proxy.RoutingPolicyDocument{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools:              []proxy.AccountPoolDocument{{Name: "Selected", Value: 3, Members: []codex.AccountKey{"account-a"}}},
		SessionBindings:    []proxy.SessionBindingDocument{{SessionDigest: digest, Pool: "Selected"}},
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
	var got proxy.RoutingPolicyDocument
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RoutingGeneration != 1 || len(got.Pools) != 1 || got.Pools[0].Name != "Selected" || got.Pools[0].Value != 3 {
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
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"pool", "set", "Cyber", "--account", "a@example.test", "--value", "10", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	poolID := state.Routing.Current().Pools[0].ID
	if bytes.Contains(output.Bytes(), []byte(poolID)) {
		t.Fatalf("pool set leaked UUID: %q", output.String())
	}
	output.Reset()
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"pool", "set", "cyber", "--account", "a@example.test", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	if current := state.Routing.Current(); current.Pools[0].Name != "Cyber" || current.Pools[0].Value != 10 || current.Pools[0].ID != poolID {
		t.Fatalf("pool set did not preserve identity/value: %#v", current.Pools[0])
	}
	output.Reset()
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"pool", "value", "CYBER", "12", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"session", "bind", "--pool", "cyber", "--session-id-stdin", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
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
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"pool", "rename", "Cyber", "Security Research", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	if current := state.Routing.Current(); current.Pools[0].Name != "Security Research" || current.Pools[0].Value != 12 || current.Pools[0].ID != poolID || current.SessionBindings[0].PoolID != poolID {
		t.Fatalf("rename changed pool identity: %#v", current)
	}
	output.Reset()
	if err := runProxyPolicyWithDependencies(context.Background(), []string{"session", "list", "--port", strconv.Itoa(port)}, &output, deps); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"pool":"Security Research"`)) || bytes.Contains(output.Bytes(), []byte("private-session")) || bytes.Contains(output.Bytes(), []byte(poolID)) {
		t.Fatalf("session list = %q", output.String())
	}
	var bindings []proxy.SessionBindingDocument
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

func TestProxyPolicyPoolRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "4294967296", "ten"} {
		err := runProxyPolicyWithDependencies(context.Background(), []string{"pool", "value", "Cyber", value}, &bytes.Buffer{}, proxyPolicyDependencies{})
		if err == nil {
			t.Fatalf("value %q accepted", value)
		}
	}
}

func TestProxyPolicyPoolMutationErrorsNamePools(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		response  string
		want      string
	}{
		{name: "missing rename source", arguments: []string{"pool", "rename", "Missing", "Cyber"}, response: `{"error":"pool_not_found"}`, want: `proxy policy pool rename "Missing" to "Cyber": pool not found`},
		{name: "duplicate rename target", arguments: []string{"pool", "rename", "Cyber", "Research"}, response: `{"error":"pool_name_conflict"}`, want: `proxy policy pool rename "Cyber" to "Research": pool name already exists`},
		{name: "invalid rename target", arguments: []string{"pool", "rename", "Cyber", "Bad\nName"}, response: `{"error":"invalid_pool_name"}`, want: `proxy policy pool rename "Cyber" to "Bad\nName": invalid pool name`},
		{name: "missing value target", arguments: []string{"pool", "value", "Missing", "10"}, response: `{"error":"pool_not_found"}`, want: `proxy policy pool value "Missing": pool not found`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusConflict)
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()
			port := server.Listener.Addr().(*net.TCPAddr).Port
			arguments := append(append([]string(nil), test.arguments...), "--port", strconv.Itoa(port))
			err := runProxyPolicyWithDependencies(context.Background(), arguments, &bytes.Buffer{}, proxyPolicyDependencies{
				LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local-token"}, nil },
				Doer:       server.Client(),
			})
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %q, want %q", err, test.want)
			}
		})
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
