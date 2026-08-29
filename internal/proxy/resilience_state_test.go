package proxy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestProxyResilienceStateOpenIsNonCreatingAndReopensPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	options := ProxyResilienceStateOptions{
		FS:     fsutil.OSFileSystem{},
		Root:   root,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x41}, 4096)),
		Now:    func() time.Time { return time.Unix(100, 0) },
	}
	if _, err := OpenProxyResilienceState(context.Background(), options); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open absent state error = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open created state: %v", err)
	}
	if err := InitialiseProxyResilienceState(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	state, err := OpenProxyResilienceState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	policy := RoutingPolicyV2{
		SchemaVersion: 2, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools: []AccountPoolV2{{ID: testPoolIDA, Name: "fast", Members: []codex.AccountKey{"account-a"}}},
	}
	policy.SessionBindings = []SessionBindingV2{{SessionDigest: state.Routing.SessionDigest([]byte("session-1")), PoolID: testPoolIDA}}
	policy.CapabilityEvidence = []CapabilityEvidenceV1{{AccountKey: "account-a", State: CapabilitySupported}}
	if err := state.Routing.Publish(policy); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenProxyResilienceState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	decision := reopened.Routing.Resolver().Resolve([]byte("session-1"), []codex.AccountKey{"account-a", "account-b"})
	if decision.Status != PolicyDecisionSelected || len(decision.Allowed) != 1 || decision.Allowed[0] != "account-a" {
		t.Fatalf("reopened decision = %#v", decision)
	}
	if reopened.DispatchPermits == nil || reopened.RuntimeMode == nil {
		t.Fatal("state owner omitted production authorities")
	}
	if err := reopened.RuntimeMode.Commit(context.Background(), RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: 1, DesiredMode: TrafficModeRescue, EffectiveMode: TrafficModeRescueDraining, Phase: RuntimeModePhaseIntent}); err != nil {
		t.Fatalf("runtime mode commit: %v", err)
	}
}

func TestProxyResilienceStateRejectsUnsafeRoot(t *testing.T) {
	options := ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: "relative", Random: bytes.NewReader(make([]byte, 128)), Now: time.Now}
	if err := InitialiseProxyResilienceState(context.Background(), options); err == nil {
		t.Fatal("accepted relative state root")
	}
}

func TestProxyRescueStateOpensWhenNormalRoutingStateIsCorrupt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	options := ProxyResilienceStateOptions{
		FS:     fsutil.OSFileSystem{},
		Root:   root,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x42}, 4096)),
		Now:    time.Now,
	}
	if err := InitialiseProxyResilienceState(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, proxyRoutingDirectoryName, routingPolicyAnchorName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := OpenProxyResilienceState(context.Background(), options); err == nil {
		_ = state.Close()
		t.Fatal("full state opened corrupt routing authority")
	}
	rescue, err := OpenProxyRescueState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer rescue.Close()
	if rescue.RuntimeMode == nil {
		t.Fatal("rescue authority incomplete")
	}
}

func TestProxyWorkerStateCoexistsWithRescueAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	options := ProxyResilienceStateOptions{
		FS:     fsutil.OSFileSystem{},
		Root:   root,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x43}, 8192)),
		Now:    time.Now,
	}
	if err := InitialiseProxyResilienceState(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	rescue, err := OpenProxyRescueState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer rescue.Close()

	options.SkipRuntimeMode = true
	worker, err := OpenProxyResilienceState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if worker.Routing == nil || worker.DispatchPermits == nil {
		t.Fatal("worker routing authority incomplete")
	}
	if worker.RuntimeMode != nil {
		t.Fatal("worker acquired supervisor-owned runtime mode authority")
	}
}
