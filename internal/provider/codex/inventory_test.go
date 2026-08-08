package codex

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

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
		{Revision: "fresh", AccessExpiresAt: now.Add(time.Hour), Ref: CandidateRef{CandidateID: "fresh"}},
		{Revision: "accepted", AccessExpiresAt: now.Add(-time.Hour), Ref: CandidateRef{CandidateID: "accepted"}},
	}}
	ordered := ResolveCandidate(logical, "accepted", now)
	if ordered[0].Revision != "accepted" {
		t.Fatalf("first revision = %q, want accepted", ordered[0].Revision)
	}
}
