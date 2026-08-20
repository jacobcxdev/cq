package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestRoutingPolicyStorePersistsKeyedSessionsAndReopens(t *testing.T) {
	store, directory := newRoutingPolicyStoreForTest(t)
	rawSession := []byte("private-session-123")
	digest := store.SessionDigest(rawSession)
	policy := RoutingPolicyV1{
		SchemaVersion:       1,
		AuthorityGeneration: 1,
		RoutingGeneration:   1,
		EffectiveGeneration: 0,
		Pools:               []AccountPoolV1{{Name: "interactive", Members: []providerCodex.AccountKey{"acct-a", "acct-b"}}},
		SessionBindings:     []SessionBindingV1{{SessionDigest: digest, Pool: "interactive"}},
		CapabilityEvidence: []CapabilityEvidenceV1{
			{AccountKey: "acct-a", State: CapabilitySupported},
			{AccountKey: "acct-b", State: CapabilityUnknown},
		},
	}
	if err := store.Publish(policy); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	decision := store.Resolver().Resolve(rawSession, []providerCodex.AccountKey{"acct-a", "acct-c"})
	if decision.Status != PolicyDecisionSelected || decision.Pool != "interactive" || len(decision.Allowed) != 1 || decision.Allowed[0] != "acct-a" {
		t.Fatalf("Resolve = %#v", decision)
	}
	entries, err := directory.(fsutil.SecureDirectoryReader).ReadDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		file, err := directory.OpenNoFollow(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		_, _ = body.ReadFrom(file)
		_ = file.Close()
		if bytes.Contains(body.Bytes(), rawSession) {
			t.Fatalf("%s persisted raw session", entry.Name())
		}
	}

	reopened, err := OpenRoutingPolicyStore(context.Background(), store.inspector, directory, store.publisher, store.key[:])
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.Resolver().Resolve(rawSession, []providerCodex.AccountKey{"acct-a"}); got.Status != PolicyDecisionSelected {
		t.Fatalf("reopened Resolve = %#v", got)
	}
}

func TestRoutingPolicyStoreRejectsBroadeningAndInactiveDelegation(t *testing.T) {
	store, _ := newRoutingPolicyStoreForTest(t)
	invalid := RoutingPolicyV1{SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 2}
	if err := store.Publish(invalid); err == nil {
		t.Fatal("Publish accepted effective generation newer than desired")
	}
	before, _ := json.Marshal(store.Current())
	if err := store.PublishDelegation(CallerDelegationV1{Caller: "caller", Accounts: []providerCodex.AccountKey{"acct-a"}}); !errors.Is(err, ErrRoutingFeatureInactive) {
		t.Fatalf("PublishDelegation error = %v", err)
	}
	after, _ := json.Marshal(store.Current())
	if !bytes.Equal(before, after) {
		t.Fatal("inactive delegation mutated store")
	}
}

func newRoutingPolicyStoreForTest(t *testing.T) (*RoutingPolicyStore, fsutil.SecureDirectory) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if err := fsutil.EnsureSecureDirectory(fsys, "/routing"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/routing")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireSelectorCASLock(fsys, directory, "routing-policy.lock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close(); _ = directory.Close() })
	publisher := NewAuthorityObjectPublisher(fsys, bytes.NewReader(bytes.Repeat([]byte{0x42}, 4096)), lock)
	key := bytes.Repeat([]byte{0x31}, 32)
	store, err := OpenRoutingPolicyStore(context.Background(), fsys, directory, publisher, key)
	if err != nil {
		t.Fatal(err)
	}
	return store, directory
}
