package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	if err := runProxyPolicy([]string{"apply", "--file", policyPath}, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runProxyPolicy([]string{"status"}, &output); err != nil {
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
