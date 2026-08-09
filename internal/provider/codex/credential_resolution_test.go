package codex

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type countingCredentialInventory struct {
	inventory Inventory
	err       error
	calls     int
}

func (i *countingCredentialInventory) List(context.Context) (Inventory, error) {
	i.calls++
	return i.inventory, i.err
}

type scriptedExactResolver struct {
	plans []PlannedCandidate
}

func (r *scriptedExactResolver) ResolveExact(_ context.Context, planned PlannedCandidate) (CredentialMaterial, error) {
	r.plans = append(r.plans, planned)
	if len(r.plans) == 1 {
		return CredentialMaterial{}, ErrStaleRevision
	}
	return testCredentialMaterial(planned.Identity, "rotated-secret"), nil
}

type exactResolverFunc func(context.Context, PlannedCandidate) (CredentialMaterial, error)

func (f exactResolverFunc) ResolveExact(ctx context.Context, planned PlannedCandidate) (CredentialMaterial, error) {
	return f(ctx, planned)
}

func testCredentialMaterial(identity AccountIdentity, accessToken string) CredentialMaterial {
	email := identity.Email
	if email == "" {
		email = "synthetic@example.test"
	}
	return CredentialMaterial{
		AccessToken: accessToken,
		AccountID:   identity.AccountID,
		IDToken:     fakeCodexJWT(email, identity.AccountID, identity.UserID, identity.PlanType),
	}
}

func TestPlanCandidateCarriesOnlyStrongIdentity(t *testing.T) {
	logical := LogicalAccount{
		Key: "logical",
		Identity: AccountIdentity{
			AccountID: "account", UserID: "user", Email: "private@example.test",
			PlanType: "pro", RecordKey: "private-record",
		},
	}
	candidate := CredentialCandidate{
		Ref:      CandidateRef{AccountKey: logical.Key, CandidateID: "candidate"},
		Revision: "revision", Source: SourceExternal, Routable: true,
	}
	planned := PlanCandidate(logical, candidate)
	if planned.Ref != candidate.Ref || planned.Revision != candidate.Revision || planned.Source != candidate.Source {
		t.Fatalf("planned candidate = %+v", planned)
	}
	wantIdentity := AccountIdentity{AccountID: "account", UserID: "user"}
	if planned.Identity != wantIdentity {
		t.Fatalf("planned identity = %+v, want %+v", planned.Identity, wantIdentity)
	}
}

func TestCredentialMaterialMatchesIdentityAllowsAccountIDOutsideIDToken(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	material := CredentialMaterial{
		AccessToken: "secret", AccountID: identity.AccountID,
		IDToken: fakeCodexJWT("synthetic@example.test", "", identity.UserID, "plus"),
	}
	if !credentialMaterialMatchesIdentity(material, identity) {
		t.Fatal("matching token account ID and JWT user ID were rejected when JWT omitted account ID")
	}
}

func TestResolvePlannedCandidateRejectsIncompleteStrongIdentityBeforeResolution(t *testing.T) {
	planned := PlannedCandidate{
		Ref:      CandidateRef{AccountKey: "logical-1", CandidateID: "system-1"},
		Revision: "revision-1", Source: SourceSystem,
		Identity: AccountIdentity{AccountID: "account-1"},
	}
	resolverCalls := 0
	resolver := exactResolverFunc(func(context.Context, PlannedCandidate) (CredentialMaterial, error) {
		resolverCalls++
		return testCredentialMaterial(planned.Identity, "secret"), nil
	})

	material, resolved, err := ResolvePlannedCandidate(context.Background(), nil, resolver, planned)
	if err == nil {
		t.Fatal("ResolvePlannedCandidate accepted an incomplete strong identity")
	}
	if material != (CredentialMaterial{}) || resolved != planned {
		t.Fatalf("resolution leaked material or changed plan: material=%+v resolved=%+v", material, resolved)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolverCalls)
	}
}

func TestResolvePlannedCandidateHonoursCancellationAroundExactResolution(t *testing.T) {
	planned := PlannedCandidate{
		Ref:      CandidateRef{AccountKey: "logical-1", CandidateID: "system-1"},
		Revision: "revision-1", Source: SourceSystem,
		Identity: AccountIdentity{AccountID: "account-1", UserID: "user-1"},
	}

	t.Run("before resolver", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resolverCalls := 0
		resolver := exactResolverFunc(func(context.Context, PlannedCandidate) (CredentialMaterial, error) {
			resolverCalls++
			return testCredentialMaterial(planned.Identity, "secret"), nil
		})

		_, _, err := ResolvePlannedCandidate(ctx, nil, resolver, planned)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ResolvePlannedCandidate error = %v, want context.Canceled", err)
		}
		if resolverCalls != 0 {
			t.Fatalf("resolver calls = %d, want 0", resolverCalls)
		}
	})

	t.Run("as resolver returns", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		resolver := exactResolverFunc(func(context.Context, PlannedCandidate) (CredentialMaterial, error) {
			cancel()
			return testCredentialMaterial(planned.Identity, "secret"), nil
		})

		material, _, err := ResolvePlannedCandidate(ctx, nil, resolver, planned)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ResolvePlannedCandidate error = %v, want context.Canceled", err)
		}
		if material != (CredentialMaterial{}) {
			t.Fatalf("ResolvePlannedCandidate returned material after cancellation: %+v", material)
		}
	})
}

func TestCredentialCoordinatorResolveExactRejectsCurrentUnroutableCandidate(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	source := &fakeExternalCredentialSource{
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
			Identity: AccountIdentity{AccountID: "account-1", UserID: "user-1"},
			Routable: true,
		}},
		material: CredentialMaterial{AccessToken: "secret", AccountID: "account-1"},
	}
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	planned := PlanCandidate(inventory.Accounts[0], inventory.Accounts[0].Candidates[0])
	source.candidates[0].Routable = false

	material, err := coordinator.ResolveExact(context.Background(), planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolveExact error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) || source.resolveRef != (ExternalCandidateRef{}) {
		t.Fatalf("unroutable candidate resolved: material=%+v ref=%+v", material, source.resolveRef)
	}
}

func TestCredentialCoordinatorResolveExactRejectsUnroutableCandidateWithRoutableSibling(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	source := &fakeExternalCredentialSource{
		candidates: []ExternalCandidate{
			{
				Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
				Identity: identity, Routable: true,
			},
			{
				Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-2", Revision: "revision-1"},
				Identity: identity, Routable: true,
			},
		},
		material: CredentialMaterial{AccessToken: "secret", AccountID: identity.AccountID},
	}
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logical := inventory.Accounts[0]
	var target CredentialCandidate
	for _, candidate := range logical.Candidates {
		if candidate.Ref.CandidateID == CandidateID(SourceExternal.String()+":"+shortHash("external-test:record-1")) {
			target = candidate
			break
		}
	}
	if target.Ref.CandidateID == "" {
		t.Fatal("target external candidate not found")
	}
	planned := PlanCandidate(logical, target)
	source.candidates[0].Routable = false

	material, err := coordinator.ResolveExact(context.Background(), planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolveExact error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) || source.resolveRef != (ExternalCandidateRef{}) {
		t.Fatalf("unroutable target resolved: material=%+v ref=%+v", material, source.resolveRef)
	}
}

func TestCredentialCoordinatorResolveExactRejectsExternalSourceSwitch(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	sourceA := &fakeExternalCredentialSource{
		name: "source-a",
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "source-a", RecordID: "record-1", Revision: "revision-1"},
			Identity: identity, Routable: true,
		}},
		material: testCredentialMaterial(identity, "source-a-secret"),
	}
	coordinator.ExternalSources = []ExternalCredentialSource{sourceA}
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	planned := PlanCandidate(inventory.Accounts[0], inventory.Accounts[0].Candidates[0])

	sourceB := &fakeExternalCredentialSource{
		name: "source-b",
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "source-b", RecordID: "record-1", Revision: "revision-1"},
			Identity: identity, Routable: true,
		}},
		material: testCredentialMaterial(identity, "source-b-secret"),
	}
	sourceA.candidates[0].Ref.Source = "source-b"
	coordinator.ExternalSources = []ExternalCredentialSource{sourceA, sourceB}

	material, err := coordinator.ResolveExact(context.Background(), planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolveExact error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) || sourceA.resolves != 0 || sourceB.resolves != 0 {
		t.Fatalf("source switch resolved: material=%+v source calls=%d/%d", material, sourceA.resolves, sourceB.resolves)
	}
}

func TestCredentialCoordinatorResolveExactRejectsDuplicateExternalSourceNames(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	sourceA := &fakeExternalCredentialSource{
		name: "duplicate",
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "duplicate", RecordID: "record-1", Revision: "revision-1"},
			Identity: identity, Routable: true,
		}},
		material: testCredentialMaterial(identity, "source-a-secret"),
	}
	coordinator.ExternalSources = []ExternalCredentialSource{sourceA}
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	planned := PlanCandidate(inventory.Accounts[0], inventory.Accounts[0].Candidates[0])

	sourceB := &fakeExternalCredentialSource{
		name:       "duplicate",
		candidates: append([]ExternalCandidate(nil), sourceA.candidates...),
		material:   testCredentialMaterial(identity, "source-b-secret"),
	}
	coordinator.ExternalSources = []ExternalCredentialSource{sourceA, sourceB}
	material, err := coordinator.ResolveExact(context.Background(), planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolveExact error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) || sourceA.resolves != 0 || sourceB.resolves != 0 {
		t.Fatalf("duplicate source resolved: material=%+v source calls=%d/%d", material, sourceA.resolves, sourceB.resolves)
	}
}

func TestCredentialCoordinatorResolveExactRejectsCandidateWithoutSourceStrongIdentity(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	systemPath := "/fake/home/.codex/auth.json"
	fs.files[systemPath] = codexAuthJSON(
		"system-secret",
		"account-1",
		fakeCodexJWT("system@example.test", "account-1", "", "plus"),
	)
	fs.modes[systemPath] = 0o600
	source := &fakeExternalCredentialSource{
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
			Identity: AccountIdentity{AccountID: "account-1", UserID: "user-1"},
			Routable: true,
		}},
		material: CredentialMaterial{AccessToken: "external-secret", AccountID: "account-1"},
	}
	coordinator.ExternalSources = []ExternalCredentialSource{source}
	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1 merged logical account", len(inventory.Accounts))
	}
	logical := inventory.Accounts[0]
	var system CredentialCandidate
	for _, candidate := range logical.Candidates {
		if candidate.Source == SourceSystem {
			system = candidate
			break
		}
	}
	if system.Ref.CandidateID == "" {
		t.Fatal("system candidate not found")
	}
	planned := PlanCandidate(logical, system)

	material, err := coordinator.ResolveExact(context.Background(), planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolveExact error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) {
		t.Fatalf("ResolveExact returned incomplete-identity candidate: %+v", material)
	}
}

func TestCredentialCoordinatorResolveExactRejectsSystemIdentitySwitch(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	systemPath := "/fake/home/.codex/auth.json"
	firstJWT := fakeCodexJWT("first@example.test", "account-first", "user-first", "plus")
	fs.files[systemPath] = codexAuthJSON("first-secret", "account-first", firstJWT)
	fs.modes[systemPath] = 0o600

	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logical := inventory.Accounts[0]
	planned := PlanCandidate(logical, logical.Candidates[0])

	secondJWT := fakeCodexJWT("second@example.test", "account-second", "user-second", "plus")
	fs.files[systemPath] = codexAuthJSON("second-secret", "account-second", secondJWT)

	material, err := coordinator.ResolveExact(context.Background(), planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolveExact error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) {
		t.Fatalf("ResolveExact returned replacement identity material: %+v", material)
	}
}

func TestCredentialCoordinatorResolveNeverRebindsStaleSystemReference(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	systemPath := "/fake/home/.codex/auth.json"
	firstJWT := fakeCodexJWT("first@example.test", "account-first", "user-first", "plus")
	fs.files[systemPath] = codexAuthJSON("first-secret", "account-first", firstJWT)
	fs.modes[systemPath] = 0o600

	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staleRef := inventory.Accounts[0].Candidates[0].Ref

	secondJWT := fakeCodexJWT("second@example.test", "account-second", "user-second", "plus")
	fs.files[systemPath] = codexAuthJSON("second-secret", "account-second", secondJWT)

	material, err := coordinator.Resolve(context.Background(), staleRef)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Resolve error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) {
		t.Fatalf("Resolve rebound stale reference to replacement identity: %+v", material)
	}
}

func TestResolvePlannedCandidateRelistsOnceForSameIdentityRevision(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "system-1"}
	planned := PlannedCandidate{Ref: ref, Revision: "revision-1", Source: SourceSystem, Identity: identity}
	inventory := &countingCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{{
		Key: "logical-1", Identity: identity, Routable: true,
		Candidates: []CredentialCandidate{{
			Ref: ref, Revision: "revision-2", Source: SourceSystem, Routable: true,
		}},
	}}}}
	resolver := &scriptedExactResolver{}

	material, resolved, err := ResolvePlannedCandidate(context.Background(), inventory, resolver, planned)
	if err != nil {
		t.Fatal(err)
	}
	if material.AccessToken != "rotated-secret" || resolved.Revision != "revision-2" {
		t.Fatalf("resolved = %+v material = %+v", resolved, material)
	}
	if inventory.calls != 1 {
		t.Fatalf("inventory calls = %d, want 1", inventory.calls)
	}
	wantPlans := []PlannedCandidate{planned, {
		Ref: ref, Revision: "revision-2", Source: SourceSystem, Identity: identity,
	}}
	if !reflect.DeepEqual(resolver.plans, wantPlans) {
		t.Fatalf("resolver plans = %+v, want %+v", resolver.plans, wantPlans)
	}
}

func TestResolvePlannedCandidateRejectsUnroutableReplanWithRoutableSibling(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	planned := PlannedCandidate{Ref: ref, Revision: "revision-1", Source: SourceExternal, Identity: identity}
	inventory := &countingCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{{
		Key: "logical-1", Identity: identity, Routable: true,
		Candidates: []CredentialCandidate{
			{Ref: ref, Revision: "revision-2", Source: SourceExternal, Routable: false},
			{
				Ref:      CandidateRef{AccountKey: "logical-1", CandidateID: "external-2"},
				Revision: "revision-1", Source: SourceExternal, Routable: true,
			},
		},
	}}}}
	resolver := &scriptedExactResolver{}

	material, resolved, err := ResolvePlannedCandidate(context.Background(), inventory, resolver, planned)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("ResolvePlannedCandidate error = %v, want ErrStaleRevision", err)
	}
	if material != (CredentialMaterial{}) || resolved != planned {
		t.Fatalf("resolution leaked material or changed plan: material=%+v resolved=%+v", material, resolved)
	}
	if inventory.calls != 1 || len(resolver.plans) != 1 {
		t.Fatalf("inventory/resolver calls = %d/%d, want 1/1", inventory.calls, len(resolver.plans))
	}
}

func TestResolvePlannedCandidateFailsClosedOnUnusableReplan(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	otherIdentity := AccountIdentity{AccountID: "account-1", UserID: "user-2"}
	ref := CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"}
	planned := PlannedCandidate{Ref: ref, Revision: "revision-1", Source: SourceExternal, Identity: identity}
	candidate := func(revision Revision, source CredentialSource) CredentialCandidate {
		return CredentialCandidate{Ref: ref, Revision: revision, Source: source, Routable: true}
	}
	tests := []struct {
		name      string
		inventory Inventory
	}{
		{
			name: "candidate disappeared",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key: "logical-1", Identity: identity, Routable: true,
			}}},
		},
		{
			name: "revision unchanged",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key: "logical-1", Identity: identity, Routable: true,
				Candidates: []CredentialCandidate{candidate("revision-1", SourceExternal)},
			}}},
		},
		{
			name: "source changed",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key: "logical-1", Identity: identity, Routable: true,
				Candidates: []CredentialCandidate{candidate("revision-2", SourceManaged)},
			}}},
		},
		{
			name: "identity changed",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key: "logical-1", Identity: otherIdentity, Routable: true,
				Candidates: []CredentialCandidate{candidate("revision-2", SourceExternal)},
			}}},
		},
		{
			name: "account unroutable",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key: "logical-1", Identity: identity, Routable: false,
				Candidates: []CredentialCandidate{candidate("revision-2", SourceExternal)},
			}}},
		},
		{
			name: "candidate blocked",
			inventory: Inventory{Accounts: []LogicalAccount{{
				Key: "logical-1", Identity: identity, Routable: true,
				Candidates: []CredentialCandidate{{
					Ref: ref, Revision: "revision-2", Source: SourceExternal,
					Routable: true, DispatchBlocked: true,
				}},
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := &countingCredentialInventory{inventory: tt.inventory}
			resolver := &scriptedExactResolver{}
			material, resolved, err := ResolvePlannedCandidate(context.Background(), inventory, resolver, planned)
			if !errors.Is(err, ErrStaleRevision) {
				t.Fatalf("ResolvePlannedCandidate error = %v, want ErrStaleRevision", err)
			}
			if material != (CredentialMaterial{}) || resolved != planned {
				t.Fatalf("resolution leaked material or changed plan: material=%+v resolved=%+v", material, resolved)
			}
			if inventory.calls != 1 || len(resolver.plans) != 1 {
				t.Fatalf("inventory/resolver calls = %d/%d, want 1/1", inventory.calls, len(resolver.plans))
			}
		})
	}
}

func TestResolvePlannedCandidateStopsWhenInventoryRefreshFails(t *testing.T) {
	identity := AccountIdentity{AccountID: "account-1", UserID: "user-1"}
	planned := PlannedCandidate{
		Ref:      CandidateRef{AccountKey: "logical-1", CandidateID: "external-1"},
		Revision: "revision-1", Source: SourceExternal, Identity: identity,
	}
	wantErr := errors.New("synthetic inventory failure")
	inventory := &countingCredentialInventory{err: wantErr}
	resolver := &scriptedExactResolver{}

	material, resolved, err := ResolvePlannedCandidate(context.Background(), inventory, resolver, planned)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ResolvePlannedCandidate error = %v, want inventory failure", err)
	}
	if material != (CredentialMaterial{}) || resolved != planned {
		t.Fatalf("resolution leaked material or changed plan: material=%+v resolved=%+v", material, resolved)
	}
	if inventory.calls != 1 || len(resolver.plans) != 1 {
		t.Fatalf("inventory/resolver calls = %d/%d, want 1/1", inventory.calls, len(resolver.plans))
	}
}
