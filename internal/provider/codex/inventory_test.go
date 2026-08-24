package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestAuthoritativeInventoryRejectsHighFileIDChange(t *testing.T) {
	mem := fsutil.NewMemFS()
	if err := mem.MkdirAll("/codex", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/codex/auth.json", []byte(`{"tokens":{"access_token":"token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := &changingFileIDFS{MemFS: mem}
	if _, _, err := readAuthoritativeCredentialFile(fsys, "/codex/auth.json"); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("changed high file ID error = %v, want unsafe", err)
	}
}

func TestAuthoritativeInventoryAcceptsPrincipalInspectionWithoutUID(t *testing.T) {
	mem := fsutil.NewMemFS()
	if err := mem.MkdirAll("/codex", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/codex/auth.json", []byte(`{"tokens":{"access_token":"token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	principal := fsutil.SecurePrincipal{Kind: fsutil.SecurePrincipalSID, SIDLength: 4, SID: [68]byte{1, 2, 3, 4}}
	fsys := &principalOnlyInventoryFS{MemFS: mem, principal: principal}
	data, found, err := readAuthoritativeCredentialFile(fsys, "/codex/auth.json")
	if err != nil || !found || len(data) == 0 {
		t.Fatalf("principal-only credential read = (%q, %v, %v)", data, found, err)
	}
}

type principalOnlyInventoryFS struct {
	*fsutil.MemFS
	principal fsutil.SecurePrincipal
}

func (*principalOnlyInventoryFS) EffectiveUID() uint64                    { return 0 }
func (*principalOnlyInventoryFS) FileOwnerUID(os.FileInfo) (uint64, bool) { return 0, false }

func (fsys *principalOnlyInventoryFS) EffectivePrincipal() (fsutil.SecurePrincipal, bool) {
	return fsys.principal, true
}

func (fsys *principalOnlyInventoryFS) FileOwnerPrincipal(os.FileInfo) (fsutil.SecurePrincipal, bool) {
	return fsys.principal, true
}

type changingFileIDFS struct {
	*fsutil.MemFS
	regularCalls int
}

func (fsys *changingFileIDFS) FileIdentity(info os.FileInfo) (fsutil.SecureFileIdentity, bool) {
	identity, ok := fsys.MemFS.FileIdentity(info)
	if ok && info.Mode().IsRegular() {
		fsys.regularCalls++
		if fsys.regularCalls > 2 {
			identity.FileID[15] = 1
		}
	}
	return identity, ok
}

type fakeExternalCredentialSource struct {
	name         string
	candidates   []ExternalCandidate
	material     CredentialMaterial
	listErr      error
	listErrAfter int
	resolveRef   ExternalCandidateRef
	listCalls    int
	resolves     int
}

type panickingExternalCredentialSource struct {
	operation  string
	candidates []ExternalCandidate
}

type panickingExternalSourceError struct{}

func (panickingExternalSourceError) Error() string {
	return "private external source list error"
}

func (panickingExternalSourceError) Is(error) bool {
	panic("private external source error classification panic")
}

func (s panickingExternalCredentialSource) Name() string {
	if s.operation == "name" {
		panic("private external source name panic")
	}
	return "panicking-external"
}

func (s panickingExternalCredentialSource) List(context.Context) ([]ExternalCandidate, error) {
	if s.operation == "list" {
		panic("private external source list panic")
	}
	if s.operation == "list_error" {
		return nil, panickingExternalSourceError{}
	}
	return append([]ExternalCandidate(nil), s.candidates...), nil
}

func (s panickingExternalCredentialSource) Resolve(context.Context, ExternalCandidateRef) (CredentialMaterial, error) {
	if s.operation == "resolve_error" {
		return CredentialMaterial{}, errors.New("private external source resolve error")
	}
	panic("private external source resolve panic")
}

func (s *fakeExternalCredentialSource) Name() string {
	if s.name != "" {
		return s.name
	}
	return "external-test"
}
func (s *fakeExternalCredentialSource) List(context.Context) ([]ExternalCandidate, error) {
	s.listCalls++
	if s.listErrAfter > 0 && s.listCalls <= s.listErrAfter {
		return append([]ExternalCandidate(nil), s.candidates...), nil
	}
	return append([]ExternalCandidate(nil), s.candidates...), s.listErr
}
func (s *fakeExternalCredentialSource) Resolve(_ context.Context, ref ExternalCandidateRef) (CredentialMaterial, error) {
	s.resolves++
	s.resolveRef = ref
	if ref != s.candidates[0].Ref {
		return CredentialMaterial{}, ErrStaleRevision
	}
	return s.material, nil
}

func TestInventoryRejectsExternalCandidateSourceMismatch(t *testing.T) {
	source := &fakeExternalCredentialSource{
		name: "source-a",
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "source-b", RecordID: "record-1", Revision: "revision-1"},
			Identity: AccountIdentity{AccountID: "account-1", UserID: "user-1"}, Routable: true,
		}},
	}
	inventory := DiscoverInventoryWithSources(context.Background(), newFakeFS(), source)
	if len(inventory.Accounts) != 0 {
		t.Fatalf("mismatched external source entered inventory: %+v", inventory.Accounts)
	}
	if len(inventory.ExternalSources) != 1 || inventory.ExternalSources[0].ErrorCode != "invalid" || inventory.ExternalSources[0].CandidateCount != 0 {
		t.Fatalf("external source status = %+v, want invalid with zero candidates", inventory.ExternalSources)
	}
}

func TestInventoryExternalSourcePanicsFailClosedPrivately(t *testing.T) {
	for _, operation := range []string{"name", "list", "list_error"} {
		t.Run(operation, func(t *testing.T) {
			inventory := DiscoverInventoryWithSources(
				context.Background(), newFakeFS(), panickingExternalCredentialSource{operation: operation},
			)
			if len(inventory.Accounts) != 0 || len(inventory.ExternalSources) != 1 || inventory.ExternalSources[0].ErrorCode != "invalid" {
				t.Fatalf("inventory after %s panic = %+v", operation, inventory)
			}
			if strings.Contains(fmt.Sprintf("%+v", inventory), "private external source") {
				t.Fatalf("inventory disclosed %s panic", operation)
			}
		})
	}
}

func TestInventoryRejectsMalformedExternalCandidateSet(t *testing.T) {
	valid := ExternalCandidate{
		Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
		Identity: AccountIdentity{AccountID: "account-1", UserID: "user-1"}, Routable: true,
	}
	tests := []struct {
		name       string
		candidates []ExternalCandidate
	}{
		{
			name: "empty record ID",
			candidates: []ExternalCandidate{{
				Ref: ExternalCandidateRef{Source: "external-test", Revision: "revision-1"},
			}},
		},
		{
			name: "empty revision",
			candidates: []ExternalCandidate{{
				Ref: ExternalCandidateRef{Source: "external-test", RecordID: "record-1"},
			}},
		},
		{
			name: "duplicate record ID",
			candidates: []ExternalCandidate{
				valid,
				{
					Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-2"},
					Identity: AccountIdentity{AccountID: "account-2", UserID: "user-2"}, Routable: true,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &fakeExternalCredentialSource{candidates: test.candidates}
			inventory := DiscoverInventoryWithSources(context.Background(), newFakeFS(), source)
			if len(inventory.Accounts) != 0 {
				t.Fatalf("malformed external source entered inventory: %+v", inventory.Accounts)
			}
			if len(inventory.ExternalSources) != 1 || inventory.ExternalSources[0].ErrorCode != "invalid" || inventory.ExternalSources[0].CandidateCount != 0 {
				t.Fatalf("external source status = %+v, want invalid with zero candidates", inventory.ExternalSources)
			}
		})
	}
}

func inventoryAuth(accessToken, accountID, idToken string, expiresAt int64) []byte {
	data, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": accessToken, "refresh_token": "synthetic-refresh",
			"id_token": idToken, "account_id": accountID,
		},
		"cq_expires_at": expiresAt,
	})
	return data
}

func TestInventoryRetainsLiveAndManagedCandidatesAndChoosesFreshest(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	now := time.Now()
	fs.files["/fake/home/.codex/auth.json"] = inventoryAuth("system", "acct-1", jwt, now.Add(time.Hour).UnixMilli())
	fs.files["/fake/home/.codex/accounts/user-1::acct-1.auth.json"] = inventoryAuth("managed", "acct-1", jwt, now.Add(2*time.Hour).UnixMilli())
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "user-1::acct-1.auth.json"}},
	}

	inventory := DiscoverInventory(fs)
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 2 {
		t.Fatalf("inventory = %+v, want one account with two candidates", inventory)
	}
	logical := inventory.Accounts[0]
	if !logical.Active || !logical.Routable || !logical.Unstable {
		t.Fatalf("logical flags = %+v", logical)
	}
	ordered := ResolveCandidate(logical, "", now)
	if ordered[0].Source != SourceManaged {
		t.Fatalf("preferred source = %v, want fresher managed", ordered[0].Source)
	}
	compat := DiscoverAccounts(fs)
	if len(compat) != 1 || compat[0].AccessToken != "managed" || !compat[0].IsActive {
		t.Fatalf("compatibility accounts = %+v", compat)
	}
}

func TestInventoryChoosesFreshLiveOverStaleManaged(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	now := time.Now()
	fs.files["/fake/home/.codex/auth.json"] = inventoryAuth("system", "acct-1", jwt, now.Add(2*time.Hour).UnixMilli())
	fs.files["/fake/home/.codex/accounts/user-1::acct-1.auth.json"] = inventoryAuth("managed", "acct-1", jwt, now.Add(time.Hour).UnixMilli())
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "user-1::acct-1.auth.json"}},
	}

	ordered := ResolveCandidate(DiscoverInventory(fs).Accounts[0], "", now)
	if ordered[0].Source != SourceSystem {
		t.Fatalf("preferred source = %v, want fresher system", ordered[0].Source)
	}
}

func TestInventoryMergesPartialAccountIDIntoRicherIdentity(t *testing.T) {
	fs := newFakeFS()
	partialJWT := fakeCodexJWT("", "", "", "")
	richJWT := fakeCodexJWT("user@test.com", "acct-1", "user-1", "pro")
	fs.files["/fake/home/.codex/auth.json"] = inventoryAuth("partial", "acct-1", partialJWT, 0)
	fs.files["/fake/home/.codex/accounts/rich.auth.json"] = inventoryAuth("rich", "acct-1", richJWT, 0)
	fs.dirEntries = map[string][]fakeDirEntry{"/fake/home/.codex/accounts": {{name: "rich.auth.json"}}}

	inventory := DiscoverInventory(fs)
	if len(inventory.Accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want merged logical account", len(inventory.Accounts))
	}
	identity := inventory.Accounts[0].Identity
	if identity.AccountID != "acct-1" || identity.UserID != "user-1" || identity.Email != "user@test.com" {
		t.Fatalf("identity = %+v, want enriched claims", identity)
	}
	for _, candidate := range inventory.Accounts[0].Candidates {
		if candidate.Source == SourceSystem && candidate.Routable {
			t.Fatal("partial system candidate without a strong user identity is routable")
		}
		if candidate.Source == SourceManaged && !candidate.Routable {
			t.Fatal("managed candidate with a complete strong identity is not routable")
		}
	}
}

func TestInventoryNeverMergesConflictingStrongClaims(t *testing.T) {
	fs := newFakeFS()
	jwt1 := fakeCodexJWT("one@test.com", "acct-1", "user-1", "plus")
	jwt2 := fakeCodexJWT("two@test.com", "acct-1", "user-2", "plus")
	fs.files["/fake/home/.codex/accounts/one.auth.json"] = inventoryAuth("one", "acct-1", jwt1, 0)
	fs.files["/fake/home/.codex/accounts/two.auth.json"] = inventoryAuth("two", "acct-1", jwt2, 0)
	fs.dirEntries = map[string][]fakeDirEntry{"/fake/home/.codex/accounts": {{name: "two.auth.json"}, {name: "one.auth.json"}}}

	inventory := DiscoverInventory(fs)
	if len(inventory.Accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want conflicting identities separate", len(inventory.Accounts))
	}
	if inventory.Accounts[0].Key == inventory.Accounts[1].Key {
		t.Fatal("conflicting identities received same generation key")
	}
}

func TestInventoryKeepsWeakOnlyEmailCandidatesSeparateAndUnroutable(t *testing.T) {
	fs := newFakeFS()
	jwt := fakeCodexJWT("same@test.com", "", "", "")
	fs.files["/fake/home/.codex/accounts/one.auth.json"] = inventoryAuth("one", "", jwt, 0)
	fs.files["/fake/home/.codex/accounts/two.auth.json"] = inventoryAuth("two", "", jwt, 0)
	fs.dirEntries = map[string][]fakeDirEntry{"/fake/home/.codex/accounts": {{name: "one.auth.json"}, {name: "two.auth.json"}}}

	inventory := DiscoverInventory(fs)
	if len(inventory.Accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want weak candidates separate", len(inventory.Accounts))
	}
	for _, logical := range inventory.Accounts {
		if logical.Routable {
			t.Fatalf("weak-only logical account is routable: %+v", logical)
		}
	}
}

func TestInventoryOrderingIgnoresReadDirOrder(t *testing.T) {
	makeInventory := func(order []fakeDirEntry) Inventory {
		fs := newFakeFS()
		jwt1 := fakeCodexJWT("one@test.com", "acct-1", "user-1", "plus")
		jwt2 := fakeCodexJWT("two@test.com", "acct-2", "user-2", "pro")
		fs.files["/fake/home/.codex/accounts/z.auth.json"] = inventoryAuth("one", "acct-1", jwt1, 0)
		fs.files["/fake/home/.codex/accounts/a.auth.json"] = inventoryAuth("two", "acct-2", jwt2, 0)
		fs.dirEntries = map[string][]fakeDirEntry{"/fake/home/.codex/accounts": order}
		return DiscoverInventory(fs)
	}
	forward := makeInventory([]fakeDirEntry{{name: "z.auth.json"}, {name: "a.auth.json"}})
	reverse := makeInventory([]fakeDirEntry{{name: "a.auth.json"}, {name: "z.auth.json"}})
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("inventory changed with directory order:\nforward=%+v\nreverse=%+v", forward, reverse)
	}
}

func TestInventoryRestoresPersistedOpaqueAccountAndCandidateKeys(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	credential := testLoginCredential()
	credential.Tokens.IDToken = fakeCodexJWT("user@test.com", "acct-1", "user-1", "plus")
	record, err := store.SaveNew(credential)
	if err != nil {
		t.Fatal(err)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(record.Metadata.CandidateID) + ".auth.json"}},
	}
	first := DiscoverInventory(fs)
	second := DiscoverInventory(fs)
	if len(first.Accounts) != 1 || first.Accounts[0].Key != record.Metadata.AccountKey {
		t.Fatalf("first inventory = %+v", first)
	}
	if !reflect.DeepEqual(first, second) || first.Accounts[0].Candidates[0].Ref.CandidateID != record.Metadata.CandidateID || first.Accounts[0].Unstable {
		t.Fatalf("persisted inventory changed across restart: first=%+v second=%+v", first, second)
	}
}

func TestResolveCandidateKeepsAcceptedRevisionFirst(t *testing.T) {
	now := time.Now()
	logical := LogicalAccount{Candidates: []CredentialCandidate{
		{Revision: "fresh", AccessExpiresAt: now.Add(time.Hour), Ref: CandidateRef{CandidateID: "fresh"}, Routable: true},
		{Revision: "accepted", AccessExpiresAt: now.Add(-time.Hour), Ref: CandidateRef{CandidateID: "accepted"}, Routable: true},
	}}
	ordered := ResolveCandidate(logical, "accepted", now)
	if ordered[0].Revision != "accepted" {
		t.Fatalf("first revision = %q, want accepted", ordered[0].Revision)
	}
}

func TestResolveCandidateSkipsDispatchBlockedButRetainsUnroutableForDiscovery(t *testing.T) {
	now := time.Unix(100, 0)
	logical := LogicalAccount{Candidates: []CredentialCandidate{
		{Ref: CandidateRef{CandidateID: "unroutable"}, Revision: "unroutable", Routable: false},
		{Ref: CandidateRef{CandidateID: "blocked"}, Revision: "blocked", Routable: true, DispatchBlocked: true},
		{Ref: CandidateRef{CandidateID: "ready"}, Revision: "ready", Routable: true},
	}}
	ordered := ResolveCandidate(logical, "unroutable", now)
	if len(ordered) != 2 || ordered[0].Ref.CandidateID != "unroutable" || ordered[1].Ref.CandidateID != "ready" {
		t.Fatalf("resolved candidates = %+v", ordered)
	}
}

func TestInventoryFederatesFreshExternalCandidateIntoLogicalAccount(t *testing.T) {
	fs := newFakeFS()
	now := time.Now()
	jwt := fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus")
	fs.files["/fake/home/.codex/accounts/stale.auth.json"] = inventoryAuth("stale-managed", "acct-1", jwt, now.Add(-time.Hour).UnixMilli())
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: "stale.auth.json"}},
	}
	source := &fakeExternalCredentialSource{candidates: []ExternalCandidate{{
		Ref: ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "fresh-revision"},
		Identity: AccountIdentity{
			AccountID: "acct-1", UserID: "user-1", Email: "user@example.test",
			PlanType: "plus", RecordKey: "user-1::acct-1",
		},
		AccessExpiresAt: now.Add(time.Hour),
		Routable:        true,
	}}}

	inventory := DiscoverInventoryWithSources(context.Background(), fs, source)
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 2 {
		t.Fatalf("inventory = %+v, want one logical account with two candidates", inventory)
	}
	ordered := ResolveCandidate(inventory.Accounts[0], "", now)
	if ordered[0].Source != SourceExternal || ordered[0].Revision != "fresh-revision" {
		t.Fatalf("preferred candidate = %+v, want fresh external", ordered[0])
	}
	if ordered[0].Credential.AccessToken != "" {
		t.Fatal("external inventory candidate exposed credential material")
	}
	if len(inventory.ExternalSources) != 1 || inventory.ExternalSources[0].CandidateCount != 1 || inventory.ExternalSources[0].ErrorCode != "" {
		t.Fatalf("external source status = %+v", inventory.ExternalSources)
	}
}

func TestInventoryKeepsCompleteExternalIdentityConflictsRoutable(t *testing.T) {
	source := &fakeExternalCredentialSource{candidates: []ExternalCandidate{
		{
			Ref: ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
			Identity: AccountIdentity{
				AccountID: "acct-1", UserID: "user-1", Email: "same@example.test",
			},
			Routable: true,
		},
		{
			Ref: ExternalCandidateRef{Source: "external-test", RecordID: "record-2", Revision: "revision-2"},
			Identity: AccountIdentity{
				AccountID: "acct-2", UserID: "user-2", Email: "same@example.test",
			},
			Routable: true,
		},
	}}

	inventory := DiscoverInventoryWithSources(context.Background(), newFakeFS(), source)
	if len(inventory.Accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want two strong identities", len(inventory.Accounts))
	}
	for _, logical := range inventory.Accounts {
		if !logical.Routable {
			t.Fatalf("complete strong identity was quarantined: %+v", logical.Identity)
		}
		if len(logical.Candidates) != 1 {
			t.Fatalf("candidates = %+v, want one per strong identity", logical.Candidates)
		}
	}
}

func TestInventoryKeepsWeakExternalAmbiguityUnroutable(t *testing.T) {
	source := &fakeExternalCredentialSource{candidates: []ExternalCandidate{
		{
			Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
			Identity: AccountIdentity{Email: "same@example.test"}, Routable: true,
		},
		{
			Ref:      ExternalCandidateRef{Source: "external-test", RecordID: "record-2", Revision: "revision-2"},
			Identity: AccountIdentity{Email: "same@example.test"}, Routable: true,
		},
	}}

	inventory := DiscoverInventoryWithSources(context.Background(), newFakeFS(), source)
	if len(inventory.Accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want separate weak candidates", len(inventory.Accounts))
	}
	for _, logical := range inventory.Accounts {
		if logical.Routable {
			t.Fatalf("weak-only external identity is routable: %+v", logical)
		}
	}
}

func TestInventoryExternalSourceOrderDoesNotChangeIdentity(t *testing.T) {
	richIdentity := AccountIdentity{
		AccountID: "acct-1", UserID: "user-1", Email: "user@example.test",
		RecordKey: "user-1::acct-1",
	}
	sourceA := &fakeExternalCredentialSource{
		name: "source-a",
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "source-a", RecordID: "record-a", Revision: "revision-a"},
			Identity: AccountIdentity{AccountID: "acct-1", Email: "user@example.test"}, Routable: true,
		}},
	}
	sourceB := &fakeExternalCredentialSource{
		name: "source-b",
		candidates: []ExternalCandidate{{
			Ref:      ExternalCandidateRef{Source: "source-b", RecordID: "record-b", Revision: "revision-b"},
			Identity: richIdentity, Routable: true,
		}},
	}

	forward := DiscoverInventoryWithSources(context.Background(), newFakeFS(), sourceA, sourceB)
	reverse := DiscoverInventoryWithSources(context.Background(), newFakeFS(), sourceB, sourceA)
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("inventory changed with external source order:\nforward=%+v\nreverse=%+v", forward, reverse)
	}
	if len(forward.Accounts) != 1 || len(forward.Accounts[0].Candidates) != 2 {
		t.Fatalf("inventory = %+v, want one enriched account with two candidates", forward)
	}
	for _, candidate := range forward.Accounts[0].Candidates {
		if candidate.externalRef == nil {
			t.Fatalf("external candidate has no source reference: %+v", candidate)
		}
		switch candidate.externalRef.Source {
		case "source-a":
			if candidate.Routable {
				t.Fatal("weak external candidate became routable after logical identity enrichment")
			}
		case "source-b":
			if !candidate.Routable {
				t.Fatal("strong external candidate is not routable")
			}
		}
	}
}

func TestCredentialCoordinatorResolvesExternalCandidateWithoutListingSecrets(t *testing.T) {
	fs := newDurableFakeFS()
	coordinator, err := NewCredentialCoordinator(testManagedStore(t, fs), testCQStateDir())
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeExternalCredentialSource{
		candidates: []ExternalCandidate{{
			Ref:             ExternalCandidateRef{Source: "external-test", RecordID: "record-1", Revision: "revision-1"},
			Identity:        AccountIdentity{AccountID: "acct-1", UserID: "user-1", RecordKey: "user-1::acct-1"},
			AccessExpiresAt: time.Now().Add(time.Hour), Routable: true,
		}},
		material: CredentialMaterial{AccessToken: "external-secret", AccountID: "acct-1"},
	}
	coordinator.ExternalSources = []ExternalCredentialSource{source}

	inventory, err := coordinator.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 1 {
		t.Fatalf("inventory = %+v", inventory)
	}
	candidate := inventory.Accounts[0].Candidates[0]
	if candidate.Credential.AccessToken != "" {
		t.Fatal("coordinator list exposed external secret")
	}
	material, err := coordinator.Resolve(context.Background(), candidate.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if material.AccessToken != "external-secret" || source.resolveRef.Revision != "revision-1" {
		t.Fatalf("resolved material/access revision = %t/%q", material.AccessToken != "", source.resolveRef.Revision)
	}
}
