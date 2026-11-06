package grok

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

type profileDoer func(*http.Request) (*http.Response, error)

func (f profileDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestValidateLoginRequiresSubscription(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{{"paid", `{"userId":"user-1","email":"test@example.com","subscriptionTier":"SuperGrokPro"}`, true}, {"free", `{"userId":"user-1","subscriptionTier":"Free"}`, false}, {"missing", `{"userId":"user-1"}`, false}, {"identity", `{"userId":"other","subscriptionTier":"SuperGrokPro"}`, false}, {"malformed", `{`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			client := profileDoer(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != BaseURL+"/user?include=subscription" || r.Header.Get("Authorization") != "Bearer access-test" {
					t.Fatal("wrong profile request")
				}
				w := httptest.NewRecorder()
				w.WriteString(tc.body)
				return w.Result(), nil
			})
			login, err := ValidateLogin(context.Background(), client, Login{Account: Account{UserID: "user-1"}, AccessToken: "access-test", RefreshToken: "refresh-test", ExpiresAt: time.Now().Add(time.Hour)})
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
			if tc.ok && !strings.Contains(login.Account.Plan, "SuperGrok") {
				t.Fatal("plan missing")
			}
		})
	}
}

func TestReadSubscriptionProfileUsesNativeCamelCaseAndChecksPrincipal(t *testing.T) {
	client := profileDoer(func(*http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.WriteString(`{"userId":"user-1","principalType":"Team","principalId":"principal-1","teamId":"team-1","subscriptionTier":"SuperGrokPro"}`)
		return w.Result(), nil
	})
	profile, err := readSubscriptionProfile(context.Background(), client, "bearer")
	if err != nil || profile.UserID != "user-1" || profile.PrincipalType != "Team" || profile.PrincipalID != "principal-1" || profile.TeamID != "team-1" {
		t.Fatalf("native schema failed: %v", err)
	}
	if _, err := ValidateLogin(context.Background(), client, Login{AccessToken: "bearer", Account: Account{UserID: "user-1", PrincipalType: "Team", PrincipalID: "different", TeamID: "team-1"}}); err == nil {
		t.Fatal("principal mismatch accepted")
	}
}

func TestReadSubscriptionProfileRejectsEmptyResponse(t *testing.T) {
	client := profileDoer(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200}, nil })
	if _, err := readSubscriptionProfile(context.Background(), client, "bearer"); err == nil {
		t.Fatal("empty response accepted")
	}
}

func TestSubscriptionIdentityRejectsCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			client := httputil.NewClientWithTransport(time.Second, "test", transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				w := httptest.NewRecorder()
				if r.URL.Host == "other.example" {
					w.WriteString(`{"userId":"user-1","subscriptionTier":"SuperGrok"}`)
				} else {
					w.Header().Set("Location", "https://other.example/user")
					w.WriteHeader(status)
				}
				return w.Result(), nil
			}))
			if _, err := SubscriptionIdentity(context.Background(), client, "private-bearer"); err == nil {
				t.Fatal("foreign unauthenticated profile accepted as verified identity")
			}
			if calls != 1 {
				t.Fatalf("followed profile redirect: %d requests", calls)
			}
		})
	}
}
