package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func backfillEdgeSource(t *testing.T, fs *fsutil.MemFS, key string) string {
	t.Helper()
	home, _ := fs.UserHomeDir()
	path := filepath.Join(home, ".codexbar", "config.json")
	data, err := json.Marshal(map[string]any{"version": 1, "providers": []any{map[string]any{"id": "zai", "tokenAccounts": map[string]any{"version": 1, "activeIndex": 0, "accounts": []any{map[string]string{"id": "borrowed", "label": "Same label", "externalIdentifier": "same@example.com", "token": key}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = fs.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func backfillEdgeBytes(t *testing.T, fs *fsutil.MemFS, paths ...string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, path := range paths {
		data, err := fs.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[path] = data
	}
	return out
}

func assertBackfillEdgeUnchanged(t *testing.T, fs *fsutil.MemFS, before map[string][]byte) {
	t.Helper()
	for path, data := range before {
		after, err := fs.ReadFile(path)
		if err != nil || !bytes.Equal(data, after) {
			t.Fatalf("unowned source or saved credential state changed: %s", path)
		}
	}
}

func TestRefreshAccountsWrongSecretsAndLabelsDidNotProveOwnership(t *testing.T) {
	for _, key := range []string{"key-id.wrong-secret", "different-prefix.secret"} {
		t.Run(key, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			session := oauthAccountFixture("one", "initial")
			session.BusinessJWT = "business-one"
			session.Email = "same@example.com"
			owner, err := StoreZCodeAccount(fs, "/config", session, "Same label", true)
			if err != nil {
				t.Fatal(err)
			}
			path := backfillEdgeSource(t, fs, key)
			before := backfillEdgeBytes(t, fs, path, "/config/zai.json", "/config/zcode-accounts.json", "/config/zcode-accounts.key")
			got, err := RefreshAccounts(t.Context(), ownershipFixture(t, "secret"), fs, noEnvironment, "/config")
			if err != nil || len(got) != 1 || got[0].ID != owner.ID || len(got[0].verifiedAPIKeyIDs) != 0 {
				t.Fatalf("unproved duplicate acquired identity: count=%d err=%v", len(got), err)
			}
			assertBackfillEdgeUnchanged(t, fs, before)
		})
	}
}

func TestRefreshAccountsIdenticalLabelsBoundOnlyProvedOwner(t *testing.T) {
	fs := fsutil.NewMemFS()
	first := oauthAccountFixture("one", "first")
	first.BusinessJWT = "business-one"
	first.Email = "same@example.com"
	second := oauthAccountFixture("two", "second")
	second.BusinessJWT = "business-two"
	second.Email = "same@example.com"
	a, err := StoreZCodeAccount(fs, "/config", first, "Same label", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := StoreZCodeAccount(fs, "/config", second, "Same label", false)
	if err != nil {
		t.Fatal(err)
	}
	path := backfillEdgeSource(t, fs, "key-id.secret")
	before := backfillEdgeBytes(t, fs, path, "/config/zcode-accounts.json", "/config/zcode-accounts.key")
	client := ownershipDoer(func(r *http.Request) (*http.Response, error) {
		copied := r.Clone(r.Context())
		copied.Header = r.Header.Clone()
		secret := "wrong-secret"
		if strings.Contains(r.Header.Get("Authorization"), "business-two") {
			secret = "secret"
			copied.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "business-two", "business-one", 1))
		}
		return ownershipFixture(t, secret).Do(copied)
	})
	got, err := RefreshAccounts(t.Context(), client, fs, noEnvironment, "/config")
	if err != nil || len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID || len(got[0].verifiedAPIKeyIDs) != 0 || !containsAlias(got[1].verifiedAPIKeyIDs, newAccount("key-id.secret", "").ID) {
		t.Fatalf("same-label proof attached to wrong owner: count=%d err=%v", len(got), err)
	}
	assertBackfillEdgeUnchanged(t, fs, before)
}

type backfillEdgeReadError struct{}

func (backfillEdgeReadError) Read([]byte) (int, error) {
	return 0, errors.New("synthetic read failure")
}
func (backfillEdgeReadError) Close() error { return nil }

func TestRefreshAccountsReadAndAuthFailuresPreservedOwnedState(t *testing.T) {
	for _, failure := range []string{"transport", "read", "unauthorised"} {
		t.Run(failure, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			session := oauthAccountFixture("one", "initial")
			session.BusinessJWT = "business-one"
			owner, err := StoreZCodeAccount(fs, "/config", session, "Owner", true)
			if err != nil {
				t.Fatal(err)
			}
			path := backfillEdgeSource(t, fs, "key-id.secret")
			before := backfillEdgeBytes(t, fs, path, "/config/zai.json", "/config/zcode-accounts.json", "/config/zcode-accounts.key")
			client := ownershipDoer(func(*http.Request) (*http.Response, error) {
				switch failure {
				case "transport":
					return nil, errors.New("synthetic network failure")
				case "read":
					return &http.Response{StatusCode: 200, Body: backfillEdgeReadError{}}, nil
				default:
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"code":401}`))}, nil
				}
			})
			got, err := RefreshAccounts(t.Context(), client, fs, noEnvironment, "/config")
			if err != nil || len(got) != 1 || got[0].ID != owner.ID || len(got[0].verifiedAPIKeyIDs) != 0 {
				t.Fatalf("verification failure changed usable ownership: count=%d err=%v", len(got), err)
			}
			assertBackfillEdgeUnchanged(t, fs, before)
		})
	}
}

func TestRefreshAccountsCancelledBeforeDiscoveryMadeNoRequestsOrWrites(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "initial")
	session.BusinessJWT = "business-one"
	if _, err := StoreZCodeAccount(fs, "/config", session, "Owner", true); err != nil {
		t.Fatal(err)
	}
	path := backfillEdgeSource(t, fs, "key-id.secret")
	before := backfillEdgeBytes(t, fs, path, "/config/zai.json", "/config/zcode-accounts.json", "/config/zcode-accounts.key")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requests := 0
	_, _ = RefreshAccounts(ctx, ownershipDoer(func(r *http.Request) (*http.Response, error) { requests++; return nil, r.Context().Err() }), fs, noEnvironment, "/config")
	if requests != 0 {
		t.Fatalf("cancelled discovery still invoked HTTP: %d", requests)
	}
	assertBackfillEdgeUnchanged(t, fs, before)
}

func TestRefreshAccountsExpiredBusinessRefreshedOnlyInMemory(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "initial")
	session.BusinessJWT = "business-old"
	session.BusinessExpiresAt = 1
	owner, err := StoreZCodeAccount(fs, "/config", session, "Owner", true)
	if err != nil {
		t.Fatal(err)
	}
	path := backfillEdgeSource(t, fs, "key-id.secret")
	before := backfillEdgeBytes(t, fs, path, "/config/zcode-accounts.json", "/config/zcode-accounts.key")
	exchanges := 0
	client := ownershipDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/auth/z/login" {
			exchanges++
			var body struct {
				Token string `json:"token"`
			}
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != session.RawOAuth {
				t.Fatal("business refresh crossed identity or protocol boundary")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"access_token":"business-one","expires_in":3600}}`))}, nil
		}
		return ownershipFixture(t, "secret").Do(r)
	})
	got, err := RefreshAccounts(t.Context(), client, fs, noEnvironment, "/config")
	if err != nil || exchanges != 1 || len(got) != 1 || got[0].ID != owner.ID || !containsAlias(got[0].verifiedAPIKeyIDs, newAccount("key-id.secret", "").ID) {
		t.Fatalf("expired business credential did not produce exact proof: count=%d exchanges=%d err=%v", len(got), exchanges, err)
	}
	assertBackfillEdgeUnchanged(t, fs, before)
}

func TestRefreshAccountsPreservedPreferenceAndRouteChangedDuringProof(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "initial")
	session.BusinessJWT = "business-one"
	owner, err := StoreZCodeAccount(fs, "/config", session, "Owner", true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := StoreZCodeAccount(fs, "/config", oauthAccountFixture("two", "other"), "Other", false)
	if err != nil {
		t.Fatal(err)
	}
	path := backfillEdgeSource(t, fs, "key-id.secret")
	before := backfillEdgeBytes(t, fs, path, "/config/zcode-accounts.json", "/config/zcode-accounts.key")
	changed := false
	client := ownershipDoer(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/copy/") && !changed {
			changed = true
			if err := SetActiveAccount(fs, "/config", other.ID); err != nil {
				t.Fatal(err)
			}
			if err := SetTransport(fs, "/config", owner.ID, "zcode-start"); err != nil {
				t.Fatal(err)
			}
		}
		return ownershipFixture(t, "secret").Do(r)
	})
	got, err := RefreshAccounts(t.Context(), client, fs, noEnvironment, "/config")
	if err != nil || !changed || len(got) != 2 || got[0].ID != other.ID || got[1].ID != owner.ID || got[1].Transport != "zcode-start" || len(got[1].verifiedAPIKeyIDs) != 1 {
		t.Fatalf("proof overwrote intervening preference or route: count=%d err=%v", len(got), err)
	}
	assertBackfillEdgeUnchanged(t, fs, before)
}

func TestRefreshAccountsRemovedOwnerDuringProofWasNotResurrected(t *testing.T) {
	fs := fsutil.NewMemFS()
	session := oauthAccountFixture("one", "initial")
	session.BusinessJWT = "business-one"
	owner, err := StoreZCodeAccount(fs, "/config", session, "Owner", true)
	if err != nil {
		t.Fatal(err)
	}
	path := backfillEdgeSource(t, fs, "key-id.secret")
	borrowedBefore := backfillEdgeBytes(t, fs, path, "/config/zcode-accounts.key")
	var removedState map[string][]byte
	client := ownershipDoer(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/copy/") && removedState == nil {
			if err := RemoveAccount(fs, "/config", owner.ID); err != nil {
				t.Fatal(err)
			}
			removedState = backfillEdgeBytes(t, fs, "/config/zai.json", "/config/zcode-accounts.json")
		}
		return ownershipFixture(t, "secret").Do(r)
	})
	got, err := RefreshAccounts(t.Context(), client, fs, noEnvironment, "/config")
	if err != nil || len(got) != 0 || removedState == nil {
		t.Fatalf("removed owner resurrected or unresolved key admitted: count=%d err=%v", len(got), err)
	}
	assertBackfillEdgeUnchanged(t, fs, borrowedBefore)
	assertBackfillEdgeUnchanged(t, fs, removedState)
}

func TestRefreshAccountsLegacyDuplicateKeptEarliestLogicalPreferenceAndStoredKey(t *testing.T) {
	fs := fsutil.NewMemFS()
	legacy, err := StoreAccount(fs, "/config", "key-id.secret", "Preferred legacy", true)
	if err != nil {
		t.Fatal(err)
	}
	otherSession := oauthAccountFixture("other", "other")
	otherSession.BusinessJWT = "business-other"
	other, err := StoreZCodeAccount(fs, "/config", otherSession, "Other", false)
	if err != nil {
		t.Fatal(err)
	}
	session := oauthAccountFixture("one", "initial")
	session.BusinessJWT = "business-one"
	owner, err := StoreZCodeAccount(fs, "/config", session, "Owner", false)
	if err != nil {
		t.Fatal(err)
	}
	path := backfillEdgeSource(t, fs, "key-id.secret")
	before := backfillEdgeBytes(t, fs, path, "/config/zcode-accounts.json", "/config/zcode-accounts.key")
	client := ownershipDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "business-other" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"organizations":[]}}`))}, nil
		}
		return ownershipFixture(t, "secret").Do(r)
	})
	got, err := RefreshAccounts(t.Context(), client, fs, noEnvironment, "/config")
	if err != nil || len(got) != 2 || got[0].ID != owner.ID || got[1].ID != other.ID {
		t.Fatalf("alias folding changed preferred logical account: count=%d err=%v", len(got), err)
	}
	stored, err := readOwnedAccounts(fs, noEnvironment, "/config")
	if err != nil || len(stored) != 3 || stored[0].ID != legacy.ID || stored[0].APIKey != "key-id.secret" {
		t.Fatalf("discovery removed or reordered stored legacy key: count=%d err=%v", len(stored), err)
	}
	assertBackfillEdgeUnchanged(t, fs, before)
}

func backfillEdgeStartWithProvedAlias(t *testing.T, fs *fsutil.MemFS, key string) Account {
	t.Helper()
	session := oauthAccountFixture("one", "initial")
	session.BusinessJWT = "business-one"
	proof, err := VerifyAPIKeyOwnership(t.Context(), ownershipFixture(t, "secret"), session, []Account{newAccount(key, "")})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := StoreVerifiedZCodeAccount(fs, "/config", session, "Start pool", false, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTransport(fs, "/config", owner.ID, "zcode-start"); err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestRefreshAccountsSameOwnerStartDidNotReplacePreferredPaidPool(t *testing.T) {
	for _, route := range []string{"", "zcode"} {
		t.Run("paid_route_"+route, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			paid, err := StoreAccount(fs, "/config", "key-id.secret", "Preferred paid pool", true)
			if err != nil {
				t.Fatal(err)
			}
			if route != "" {
				if err := SetTransport(fs, "/config", paid.ID, route); err != nil {
					t.Fatal(err)
				}
			}
			session := oauthAccountFixture("one", "initial")
			session.BusinessJWT = "business-one"
			start, err := StoreZCodeAccount(fs, "/config", session, "Independent Start pool", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetTransport(fs, "/config", start.ID, "zcode-start"); err != nil {
				t.Fatal(err)
			}
			path := backfillEdgeSource(t, fs, "key-id.secret")
			before := backfillEdgeBytes(t, fs, path, "/config/zcode-accounts.json", "/config/zcode-accounts.key")
			got, err := RefreshAccounts(t.Context(), ownershipFixture(t, "secret"), fs, noEnvironment, "/config")
			if err != nil || len(got) != 2 || got[0].ID != paid.ID || got[0].OAuth != nil || got[0].Transport != route || got[1].ID != start.ID || got[1].Transport != "zcode-start" {
				t.Fatalf("same owner folded independent paid and Start pools: count=%d err=%v", len(got), err)
			}
			stored, err := readOwnedAccounts(fs, noEnvironment, "/config")
			if err != nil || len(stored) != 2 || stored[0].ID != paid.ID || stored[0].APIKey != "key-id.secret" || stored[0].Transport != route {
				t.Fatal("proof altered saved paid pool")
			}
			assertBackfillEdgeUnchanged(t, fs, before)
		})
	}
}

func TestReadAccountsProvedEnvironmentKeyDidNotChooseStartPool(t *testing.T) {
	fs := fsutil.NewMemFS()
	backfillEdgeStartWithProvedAlias(t, fs, "key-id.secret")
	path := backfillEdgeSource(t, fs, "key-id.secret")
	before := backfillEdgeBytes(t, fs, path, "/config/zai.json", "/config/zcode-accounts.json", "/config/zcode-accounts.key")
	got, err := ReadAccounts(fs, func(name string) string {
		if name == "ZAI_API_KEY" {
			return "key-id.secret"
		}
		return ""
	}, "/config")
	if err != nil || len(got) != 1 || got[0].ID != newAccount("key-id.secret", "").ID || got[0].APIKey != "key-id.secret" || got[0].OAuth != nil || got[0].Transport != "" {
		t.Fatalf("environment paid key selected independent Start pool: count=%d err=%v", len(got), err)
	}
	assertBackfillEdgeUnchanged(t, fs, before)
}

func TestStoreAccountProvedKeyAddedPaidPoolInsteadOfReturningStart(t *testing.T) {
	fs := fsutil.NewMemFS()
	start := backfillEdgeStartWithProvedAlias(t, fs, "key-id.secret")
	path := backfillEdgeSource(t, fs, "key-id.secret")
	before := backfillEdgeBytes(t, fs, path, "/config/zcode-accounts.json", "/config/zcode-accounts.key")
	paid, err := StoreAccount(fs, "/config", "key-id.secret", "Paid pool", true)
	if err != nil || paid.OAuth != nil || paid.APIKey != "key-id.secret" || paid.ID == start.ID || paid.Transport != "" {
		t.Fatalf("explicit paid-key login returned Start pool: err=%v", err)
	}
	got, err := ReadAccounts(fs, noEnvironment, "/config")
	if err != nil || len(got) != 2 || got[0].ID != paid.ID || got[1].ID != start.ID || got[1].Transport != "zcode-start" {
		t.Fatalf("explicit paid-key login lost independent pool or preference: count=%d err=%v", len(got), err)
	}
	assertBackfillEdgeUnchanged(t, fs, before)
}

func TestStoreVerifiedZCodeAccountRenewedStartWithoutRemovingPaidPool(t *testing.T) {
	for _, route := range []string{"", "zcode"} {
		t.Run("paid_route_"+route, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			paid, err := StoreAccount(fs, "/config", "key-id.secret", "Preferred paid", true)
			if err != nil {
				t.Fatal(err)
			}
			if route != "" {
				if err := SetTransport(fs, "/config", paid.ID, route); err != nil {
					t.Fatal(err)
				}
			}
			session := oauthAccountFixture("one", "initial")
			session.BusinessJWT = "business-one"
			start, err := StoreZCodeAccount(fs, "/config", session, "Start pool", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetTransport(fs, "/config", start.ID, "zcode-start"); err != nil {
				t.Fatal(err)
			}
			path := backfillEdgeSource(t, fs, "key-id.secret")
			before := backfillEdgeBytes(t, fs, path)
			proof, err := VerifyAPIKeyOwnership(t.Context(), ownershipFixture(t, "secret"), session, []Account{paid})
			if err != nil {
				t.Fatal(err)
			}
			renewed, err := StoreVerifiedZCodeAccount(fs, "/config", oauthAccountFixture("one", "renewed"), "Start renewed", false, proof)
			if err != nil || renewed.ID != start.ID || renewed.Transport != "zcode-start" {
				t.Fatalf("renewal changed Start pool identity or route: err=%v", err)
			}
			got, err := ReadAccounts(fs, noEnvironment, "/config")
			if err != nil || len(got) != 2 || got[0].ID != paid.ID || got[0].Transport != route || got[0].APIKey != "key-id.secret" || got[1].ID != start.ID || got[1].Transport != "zcode-start" {
				t.Fatalf("Start renewal removed paid pool or changed preference: count=%d err=%v", len(got), err)
			}
			stored, err := readOwnedAccounts(fs, noEnvironment, "/config")
			if err != nil || len(stored) != 2 || stored[0].ID != paid.ID || stored[0].APIKey != "key-id.secret" || stored[0].Transport != route {
				t.Fatal("Start renewal removed or changed stored paid-key credentials")
			}
			assertBackfillEdgeUnchanged(t, fs, before)
		})
	}
}
