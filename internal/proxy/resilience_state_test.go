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
	policy := RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools: []AccountPoolV1{{Name: "fast", Members: []codex.AccountKey{"account-a"}}},
	}
	policy.SessionBindings = []SessionBindingV1{{SessionDigest: state.Routing.SessionDigest([]byte("session-1")), Pool: "fast"}}
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
}

func TestProxyResilienceStateRejectsUnsafeRoot(t *testing.T) {
	options := ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: "relative", Random: bytes.NewReader(make([]byte, 128)), Now: time.Now}
	if err := InitialiseProxyResilienceState(context.Background(), options); err == nil {
		t.Fatal("accepted relative state root")
	}
}
