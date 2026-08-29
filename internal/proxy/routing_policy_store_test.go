package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestRoutingPolicyStoreMigratesAuthenticatedV1Once(t *testing.T) {
	inspector, directory, publisher, key := newRoutingPolicyAuthorityForTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	predicate := CapabilityPredicateCoreV1{SchemaVersion: 1, Capability: "model.invoke", ProductSurface: "desktop", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5", EffectiveModel: "gpt-5"}
	legacy := RoutingPolicyV1{
		SchemaVersion: 1, AuthorityGeneration: 7, RoutingGeneration: 7, EffectiveGeneration: 6,
		Pools:                     []AccountPoolV1{{Name: "cyber", Members: []providerCodex.AccountKey{"acct-a", "acct-b"}}},
		SessionBindings:           []SessionBindingV1{{SessionDigest: strings.Repeat("a", 64), Pool: "cyber"}},
		CapabilityPool:            "cyber",
		CapabilityPredicates:      []CapabilityPredicateCoreV1{predicate},
		CapabilityRoutingEvidence: []CapabilityRoutingEvidenceV1{capabilityEvidenceForTest("acct-a", "workspace", predicate, "migration", nil, CapabilityEvidenceEligible, 7, now)},
	}
	seedRoutingPolicyV1(t, directory, publisher, key, legacy)

	store, err := OpenRoutingPolicyStore(context.Background(), inspector, directory, publisher, bytes.NewReader(bytes.Repeat([]byte{0x51}, 64)), key)
	if err != nil {
		t.Fatal(err)
	}
	current := store.Current()
	if current.SchemaVersion != 2 || current.AuthorityGeneration != 7 || current.RoutingGeneration != 7 || current.EffectiveGeneration != 6 {
		t.Fatalf("migrated generations = %#v", current)
	}
	if len(current.Pools) != 1 || current.Pools[0].Name != "Cyber" || current.Pools[0].Value != 0 || !validPoolID(current.Pools[0].ID) {
		t.Fatalf("migrated pool = %#v", current.Pools)
	}
	if current.SessionBindings[0].PoolID != current.Pools[0].ID || current.CapabilityPool != current.Pools[0].ID {
		t.Fatalf("migrated references = %#v / %q", current.SessionBindings, current.CapabilityPool)
	}
	firstID := current.Pools[0].ID

	reopened, err := OpenRoutingPolicyStore(context.Background(), inspector, directory, publisher, bytes.NewReader(bytes.Repeat([]byte{0x62}, 64)), key)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Current().Pools[0].ID; got != firstID {
		t.Fatalf("reopened pool ID = %q, want %q", got, firstID)
	}
	document, err := reopened.Document()
	if err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != 1 || document.Pools[0].Name != "Cyber" || document.SessionBindings[0].Pool != "Cyber" || document.CapabilityPool != "Cyber" {
		t.Fatalf("public document = %#v", document)
	}
}

func TestRoutingPolicyMigrationFailureLeavesV1Authoritative(t *testing.T) {
	for _, stage := range []string{"object", "anchor"} {
		t.Run(stage, func(t *testing.T) {
			inspector, directory, publisher, key := newRoutingPolicyAuthorityForTest(t)
			seedRoutingPolicyV1(t, directory, publisher, key, RoutingPolicyV1{
				SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
				Pools: []AccountPoolV1{{Name: "cyber", Members: []providerCodex.AccountKey{"acct-a"}}},
			})
			failing := &failingRoutingPolicyPublisher{DurableObjectPublisher: publisher, stage: stage}
			if _, err := OpenRoutingPolicyStore(context.Background(), inspector, directory, failing, bytes.NewReader(bytes.Repeat([]byte{0x71}, 64)), key); err == nil {
				t.Fatal("migration failure accepted")
			}
			anchorBody, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, routingPolicyAnchorName, routingPolicyMaxBytes)
			if err != nil {
				t.Fatal(err)
			}
			var anchor struct {
				SchemaVersion int `json:"schema_version"`
			}
			if err := json.Unmarshal(anchorBody, &anchor); err != nil || anchor.SchemaVersion != 1 {
				t.Fatalf("authoritative anchor = %q, error = %v", anchorBody, err)
			}
			recovered, err := OpenRoutingPolicyStore(context.Background(), inspector, directory, publisher, bytes.NewReader(bytes.Repeat([]byte{0x72}, 64)), key)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Current().SchemaVersion != 2 || recovered.Current().Pools[0].Name != "Cyber" {
				t.Fatalf("recovered policy = %#v", recovered.Current())
			}
		})
	}
}

type failingRoutingPolicyPublisher struct {
	DurableObjectPublisher
	stage string
}

func (publisher *failingRoutingPolicyPublisher) PublishImmutable(ctx context.Context, directory fsutil.SecureDirectory, name string, body []byte, mode fs.FileMode) (StableObjectIdentity, error) {
	if publisher.stage == "object" && strings.HasPrefix(name, "routing-policy-") {
		return StableObjectIdentity{}, errors.New("injected object publication failure")
	}
	return publisher.DurableObjectPublisher.PublishImmutable(ctx, directory, name, body, mode)
}

func (publisher *failingRoutingPolicyPublisher) ReplaceSelectorExactPrior(context.Context, fsutil.SecureDirectory, string, *StableObjectIdentity, []byte) (StableObjectIdentity, error) {
	if publisher.stage == "anchor" {
		return StableObjectIdentity{}, errors.New("injected anchor replacement failure")
	}
	panic("unexpected selector replacement")
}

func newRoutingPolicyAuthorityForTest(t *testing.T) (fsutil.SecurePathInspector, fsutil.SecureDirectory, DurableObjectPublisher, []byte) {
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
	return fsys, directory, publisher, bytes.Repeat([]byte{0x31}, 32)
}

func seedRoutingPolicyV1(t *testing.T, directory fsutil.SecureDirectory, publisher DurableObjectPublisher, key []byte, policy RoutingPolicyV1) {
	t.Helper()
	body, err := sealRoutingPolicy(policy, key)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	objectDigest := hex.EncodeToString(digest[:])
	if _, err := publisher.PublishImmutable(context.Background(), directory, "routing-policy-"+objectDigest+".json", body, fs.FileMode(0o600)); err != nil {
		t.Fatal(err)
	}
	anchor, err := sealRoutingAnchor(routingPolicyAnchorV1{SchemaVersion: 1, ObjectDigest: objectDigest}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishImmutable(context.Background(), directory, routingPolicyAnchorName, anchor, fs.FileMode(0o600)); err != nil {
		t.Fatal(err)
	}
}

func routingPolicyV2ForTest(legacy RoutingPolicyV1) RoutingPolicyV2 {
	policy := RoutingPolicyV2{
		SchemaVersion:             2,
		AuthorityGeneration:       legacy.AuthorityGeneration,
		RoutingGeneration:         legacy.RoutingGeneration,
		EffectiveGeneration:       legacy.EffectiveGeneration,
		Pools:                     make([]AccountPoolV2, len(legacy.Pools)),
		SessionBindings:           make([]SessionBindingV2, len(legacy.SessionBindings)),
		CapabilityEvidence:        append([]CapabilityEvidenceV1(nil), legacy.CapabilityEvidence...),
		CapabilityPredicates:      append([]CapabilityPredicateCoreV1(nil), legacy.CapabilityPredicates...),
		CapabilityRoutingEvidence: cloneCapabilityRoutingEvidence(legacy.CapabilityRoutingEvidence),
		Delegations:               append([]CallerDelegationV1(nil), legacy.Delegations...),
	}
	ids := make(map[string]PoolID, len(legacy.Pools))
	for index, pool := range legacy.Pools {
		digest := sha256.Sum256([]byte(pool.Name))
		id, err := newPoolID(bytes.NewReader(digest[:]))
		if err != nil {
			panic(err)
		}
		ids[pool.Name] = id
		policy.Pools[index] = AccountPoolV2{ID: id, Name: pool.Name, Members: append([]providerCodex.AccountKey(nil), pool.Members...)}
	}
	for index, binding := range legacy.SessionBindings {
		policy.SessionBindings[index] = SessionBindingV2{SessionDigest: binding.SessionDigest, PoolID: ids[binding.Pool]}
	}
	capabilityPool := legacy.CapabilityPool
	if capabilityPool == "" && len(legacy.CapabilityPredicates) > 0 && len(legacy.Pools) == 1 {
		capabilityPool = legacy.Pools[0].Name
	}
	policy.CapabilityPool = ids[capabilityPool]
	return policy
}

func TestRoutingPolicyStorePersistsKeyedSessionsAndReopens(t *testing.T) {
	store, directory := newRoutingPolicyStoreForTest(t)
	rawSession := []byte("private-session-123")
	digest := store.SessionDigest(rawSession)
	policy := RoutingPolicyV2{
		SchemaVersion:       2,
		AuthorityGeneration: 1,
		RoutingGeneration:   1,
		EffectiveGeneration: 0,
		Pools:               []AccountPoolV2{{ID: testPoolIDA, Name: "interactive", Members: []providerCodex.AccountKey{"acct-a", "acct-b"}}},
		SessionBindings:     []SessionBindingV2{{SessionDigest: digest, PoolID: testPoolIDA}},
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

	reopened, err := OpenRoutingPolicyStore(context.Background(), store.inspector, directory, store.publisher, bytes.NewReader(bytes.Repeat([]byte{0x43}, 64)), store.key[:])
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.Resolver().Resolve(rawSession, []providerCodex.AccountKey{"acct-a"}); got.Status != PolicyDecisionSelected {
		t.Fatalf("reopened Resolve = %#v", got)
	}
}

func TestRoutingPolicyStoreRejectsBroadeningAndInactiveDelegation(t *testing.T) {
	store, _ := newRoutingPolicyStoreForTest(t)
	invalid := RoutingPolicyV2{SchemaVersion: 2, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 2}
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
	store, err := OpenRoutingPolicyStore(context.Background(), fsys, directory, publisher, bytes.NewReader(bytes.Repeat([]byte{0x43}, 4096)), key)
	if err != nil {
		t.Fatal(err)
	}
	return store, directory
}
