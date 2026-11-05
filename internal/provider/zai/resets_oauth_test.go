package zai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func resetOAuthAccount() Account {
	return Account{ID: "zcode-stable", OAuth: &zcode.LoginSession{UserID: "stable-user", ZCodeJWT: "private-zcode", BusinessJWT: "private-business", RawOAuth: "private-oauth", DeviceMid: "private-device"}}
}

func TestResetOAuthUsedBusinessIdentityAndExactCard(t *testing.T) {
	account := resetOAuthAccount()
	fsys := fsutil.NewMemFS()
	posts := 0
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "private-business" || r.Header.Get("User-Agent") != "ZCode/"+zcode.ClientVersion || r.Header.Get("X-Title") != "Z Code@electron" || r.Header.Get("X-Device-Mid") != "" {
			t.Fatal("reset request crossed credential or source boundaries")
		}
		if r.Method == http.MethodGet {
			return resetResponse(200, resetFixture), nil
		}
		posts++
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["recordId"]) != "900719925474099313" || string(body["requestId"]) != `"stable-request"` || string(body["grantType"]) != `"CAMPAIGN"` {
			t.Fatal("OAuth reset changed exact card or journal identity")
		}
		digest := sha256.Sum256([]byte(account.ID))
		data, err := fsys.ReadFile(filepath.Join("/state/zai-resets", hex.EncodeToString(digest[:])+".json"))
		var attempt resetAttempt
		if err != nil || json.Unmarshal(data, &attempt) != nil || attempt.State != "pending" || attempt.CreditID != "5h:900719925474099313" || attempt.RequestID != "stable-request" {
			t.Fatal("OAuth reset dispatched without its exact durable intent")
		}
		return resetResponse(200, `{"code":200,"success":true}`), nil
	})}
	executor := resetTestExecutor(fsys, client)
	result, err := executor.Use(context.Background(), account, "5h", "5h:900719925474099313", func(ResetCredit) (bool, error) { return true, nil })
	if err != nil || !result.Accepted || posts != 1 {
		t.Fatalf("OAuth reset failed: %v", err)
	}
	result, err = executor.Use(context.Background(), account, "5h", "5h:900719925474099313", func(ResetCredit) (bool, error) { t.Fatal("accepted receipt re-confirmed"); return true, nil })
	if err != nil || !result.AlreadyAccepted || posts != 1 {
		t.Fatal("accepted OAuth reset replayed")
	}
}

func TestResetOAuthRefreshedKnownExpiryOrOneReadOnly401(t *testing.T) {
	for _, knownExpiry := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-only-401", true: "known-expiry"}[knownExpiry], func(t *testing.T) {
			account := resetOAuthAccount()
			original := account.OAuth
			if knownExpiry {
				account.OAuth.BusinessExpiresAt = resetNow.Add(-time.Second).Unix()
			}
			refreshes, gets, posts := 0, 0, 0
			client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/auth/z/login" {
					refreshes++
					body, _ := io.ReadAll(r.Body)
					if string(body) != `{"token":"private-oauth"}` || r.Header.Get("Authorization") != "" {
						t.Fatal("business refresh used a PAT, API key or ZCode JWT")
					}
					return resetResponse(200, `{"code":200,"data":{"access_token":"fresh-business","expires_in":3600}}`), nil
				}
				if r.Method == http.MethodGet {
					gets++
					if !knownExpiry && gets == 1 {
						return resetResponse(401, "private-error"), nil
					}
				} else {
					posts++
				}
				if r.Header.Get("Authorization") != "fresh-business" {
					t.Fatal("refreshed business identity was not reused")
				}
				if r.Method == http.MethodGet {
					return resetResponse(200, resetFixture), nil
				}
				return resetResponse(200, `{"code":200,"success":true}`), nil
			})}
			inventory, err := client.ListAccount(context.Background(), &account, resetNow)
			if err != nil {
				t.Fatal(err)
			}
			card, _ := SelectResetCredit(inventory, "5h", "5h:earlier", resetNow)
			if err := client.UseAccount(context.Background(), &account, card, "request", resetNow); err != nil {
				t.Fatal(err)
			}
			if refreshes != 1 || posts != 1 || account.OAuth.BusinessJWT != "fresh-business" || original.BusinessJWT != "private-business" {
				t.Fatal("business refresh changed shared session or repeated exchanges")
			}
		})
	}
}

func TestResetOAuthDidNotRetryMutationOrRepeatedRead401(t *testing.T) {
	for _, operation := range []string{"read", "use"} {
		t.Run(operation, func(t *testing.T) {
			account := resetOAuthAccount()
			requests, refreshes := 0, 0
			client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/auth/z/login" {
					refreshes++
					return resetResponse(200, `{"code":200,"data":{"access_token":"fresh-business"}}`), nil
				}
				requests++
				return resetResponse(401, "private-business upstream"), nil
			})}
			var err error
			if operation == "read" {
				_, err = client.ListAccount(context.Background(), &account, resetNow)
				if requests != 2 || refreshes != 1 {
					t.Fatal("read-only auth recovery exceeded one retry")
				}
			} else {
				err = client.UseAccount(context.Background(), &account, ResetCredit{ID: "5h:exact", Window: "5h", Available: true, recordID: json.RawMessage(`"exact"`)}, "request", resetNow)
				if requests != 1 || refreshes != 0 {
					t.Fatal("mutation was refreshed or replayed after 401")
				}
			}
			if err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatal("failed OAuth reset leaked or succeeded")
			}
		})
	}
}

func TestResetOAuthCancellationAndMixedIdentityFailedClosed(t *testing.T) {
	client := ResetClient{HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached upstream")
		return nil, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	account := resetOAuthAccount()
	_, err := client.ListAccount(ctx, &account, resetNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled read did not stop")
	}
	account.APIKey = "private-api-key"
	if _, err := client.ListAccount(context.Background(), &account, resetNow); err == nil {
		t.Fatal("mixed OAuth/API-key identity accepted")
	}
}

func TestResetOAuthExecutorCancellationAndScheduledPendingDidNotPost(t *testing.T) {
	for _, scenario := range []string{"cancelled", "pending"} {
		t.Run(scenario, func(t *testing.T) {
			posts := 0
			client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					posts++
					return nil, errors.New("private transport outcome")
				}
				return resetResponse(200, resetFixture), nil
			})}
			executor := resetTestExecutor(fsutil.NewMemFS(), client)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			account := resetOAuthAccount()
			_, err := executor.Use(ctx, account, "5h", "5h:earlier", func(ResetCredit) (bool, error) {
				if scenario == "cancelled" {
					cancel()
				}
				return true, nil
			})
			if err == nil {
				t.Fatal("cancelled or uncertain reset accepted")
			}
			if scenario == "cancelled" {
				if posts != 0 {
					t.Fatal("cancelled OAuth reset dispatched")
				}
				return
			}
			_, err = executor.UseScheduled(context.Background(), account, "5h", "5h:earlier")
			if !errors.Is(err, ErrResetPending) || posts != 1 {
				t.Fatal("scheduler replayed unresolved OAuth reset")
			}
		})
	}
}

func TestResetOAuthCredentialRequestsDidNotFollowRedirects(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(map[bool]string{false: "inventory", true: "use"}[mutation], func(t *testing.T) {
			calls := 0
			transport := resetOAuthTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Hostname() != "api.z.ai" {
					t.Fatal("reset credential followed redirect")
				}
				response := resetResponse(http.StatusTemporaryRedirect, "")
				response.Header = http.Header{"Location": []string{"https://untrusted.invalid/collect"}}
				return response, nil
			})
			httpClient := &http.Client{Transport: transport}
			client := ResetClient{HTTP: httpClient}
			account := resetOAuthAccount()
			var err error
			if mutation {
				err = client.UseAccount(context.Background(), &account, ResetCredit{Window: "5h", Available: true, recordID: json.RawMessage(`"exact"`)}, "request", resetNow)
			} else {
				_, err = client.ListAccount(context.Background(), &account, resetNow)
			}
			if err == nil || calls != 1 || httpClient.CheckRedirect != nil {
				t.Fatal("redirect safety failed or injected client was changed")
			}
		})
	}
}

type resetOAuthTransport func(*http.Request) (*http.Response, error)

func (f resetOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
