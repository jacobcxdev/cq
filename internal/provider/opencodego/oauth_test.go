package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type oauthTransport func(*http.Request) (*http.Response, error)

func (f oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func oauthReply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const deviceFixture = `{
  "device_code": "poll-credential",
  "user_code": "TEST-CODE",
  "verification_uri_complete": "device?user_code=TEST-CODE",
  "expires_in": 60,
  "interval": 2
}`

func TestOAuthDeviceLoginPollsAndVerifiesWorkspace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var polls int
		start := time.Now()
		wantScope := map[string]any{"client_id": consoleClientID, "supports_org_scope": true}
		wantPoll := map[string]any{"client_id": consoleClientID, "grant_type": deviceGrant, "device_code": "poll-credential"}
		wantTime := []time.Duration{2 * time.Second, 4 * time.Second, 11 * time.Second, 26 * time.Second}
		client := OAuthClient{Client: doUsage(func(req *http.Request) (*http.Response, error) {
			if req.URL.Scheme != "https" || req.URL.Host != "opencode.ai" {
				t.Fatal("OAuth request left the OpenCode origin")
			}
			switch req.URL.Path {
			case "/console/auth/device/code":
				assertOAuthJSON(t, req, wantScope)
				return oauthReply(http.StatusOK, deviceFixture), nil
			case "/console/auth/device/token":
				assertOAuthJSON(t, req, wantPoll)
				if polls >= len(wantTime) || time.Since(start) != wantTime[polls] {
					t.Fatalf("poll %d ran after %v", polls, time.Since(start))
				}
				polls++
				switch polls {
				case 1:
					return oauthReply(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil
				case 2:
					return oauthReply(http.StatusBadRequest, `{"error":"slow_down"}`), nil
				case 3:
					response := oauthReply(http.StatusTooManyRequests, "slow")
					response.Header.Set("Retry-After", "15")
					return response, nil
				default:
					return oauthReply(http.StatusOK, `{"access_token":"member-access","refresh_token":"member-refresh","expires_in":600,"org_id":"selected-workspace"}`), nil
				}
			case "/console/api/user":
				assertOAuthIdentityRequest(t, req)
				return oauthReply(http.StatusOK, `{"id":"member-123","email":"member@example.test"}`), nil
			case "/console/api/orgs":
				assertOAuthIdentityRequest(t, req)
				return oauthReply(http.StatusOK, `[{"id":"other-workspace"},{"id":"selected-workspace"}]`), nil
			default:
				t.Fatalf("unexpected OAuth path: %s", req.URL.Path)
				return nil, nil
			}
		})}
		got, err := client.Login(t.Context(), func(verification, code string) error {
			if verification != "https://opencode.ai/console/device?user_code=TEST-CODE" || code != "TEST-CODE" {
				t.Fatal("incorrect verification URL or code")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.AccessToken != "member-access" || got.RefreshToken != "member-refresh" ||
			got.WorkspaceID != "selected-workspace" || got.MemberID != "member-123" || got.Email != "member@example.test" ||
			!got.ExpiresAt.Equal(time.Now().Add(10*time.Minute)) || polls != 4 {
			t.Fatal("OAuth login did not bind the verified workspace and member")
		}
	})
}

func TestOAuthLoginRejectsUntrustedVerificationURL(t *testing.T) {
	for _, uri := range []string{
		"https://example.test/device",
		"http://opencode.ai/console/device",
		"https://opencode.ai@example.test/device",
		"https://approval@opencode.ai/console/device",
	} {
		t.Run(uri, func(t *testing.T) {
			client := OAuthClient{Client: doUsage(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/console/auth/device/code" {
					t.Fatal("OAuth login continued after an unsafe verification link")
				}
				return oauthReply(http.StatusOK, strings.Replace(deviceFixture, "device?user_code=TEST-CODE", uri, 1)), nil
			})}
			_, err := client.Login(t.Context(), func(string, string) error {
				t.Fatal("unsafe verification URL reached the browser callback")
				return nil
			})
			if err == nil || strings.Contains(err.Error(), uri) {
				t.Fatal("OAuth login failed to return a safe verification error")
			}
		})
	}
}

func TestOAuthIdentifyRequiresAWorkspaceMember(t *testing.T) {
	for _, tt := range []struct {
		name, selected, workspaces, want string
	}{
		{"single workspace", "", `[{"id":"only"}]`, "only"},
		{"ambiguous workspace", "", `[{"id":"one"},{"id":"two"}]`, ""},
		{"unavailable workspace", "missing", `[{"id":"other"}]`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := OAuthClient{Client: doUsage(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/console/api/user" {
					return oauthReply(http.StatusOK, `{"id":"member","email":"member@example.test"}`), nil
				}
				return oauthReply(http.StatusOK, tt.workspaces), nil
			})}
			token := consoleToken{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 600, WorkspaceID: tt.selected}
			got, err := client.identify(t.Context(), token)
			if got.WorkspaceID != tt.want || (err == nil) != (tt.want != "") {
				t.Errorf("unexpected workspace or error: %q, %v", got.WorkspaceID, err)
			}
		})
	}
}

func TestOAuthDeviceLoginHonoursExpiryAndCancellation(t *testing.T) {
	for _, tt := range []struct {
		name string
		stop bool
		want error
	}{
		{"expiry", false, context.DeadlineExceeded},
		{"cancellation", true, context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				client := OAuthClient{Client: doUsage(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path != "/console/auth/device/code" {
						t.Fatal("device token was polled after expiry or cancellation")
					}
					return oauthReply(http.StatusOK, strings.Replace(deviceFixture, `"expires_in": 60`, `"expires_in": 1`, 1)), nil
				})}
				_, err := client.Login(ctx, func(string, string) error {
					if tt.stop {
						cancel()
					}
					return nil
				})
				if !errors.Is(err, tt.want) {
					t.Errorf("login error = %v, want %v", err, tt.want)
				}
			})
		})
	}
}

func TestOAuthRefreshKeepsWorkspaceAndClassifiesRejection(t *testing.T) {
	for _, tt := range []struct {
		name, body    string
		status        int
		rejected, bad bool
	}{
		{"retains workspace", `{"access_token":"new","refresh_token":"rotated","expires_in":600}`, 200, false, false},
		{"changed workspace", `{"access_token":"new","refresh_token":"rotated","expires_in":600,"org_id":"changed"}`, 200, false, true},
		{"malformed successful reply", `{"access_token":"new","expires_in":600}`, 200, false, true},
		{"invalid grant", `{"error":"invalid_grant","description":"credential-detail"}`, 400, true, true},
		{"temporarily unavailable", `{"error":"invalid_grant","description":"credential-detail"}`, 503, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := OAuthSession{AccessToken: "old", RefreshToken: "old-refresh", WorkspaceID: "selected", MemberID: "member", Email: "member@example.test"}
			client := OAuthClient{Client: doUsage(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != consoleURL+deviceTokenPath {
					t.Fatal("unexpected OAuth refresh endpoint")
				}
				assertOAuthJSON(t, req, map[string]any{
					"client_id": consoleClientID, "grant_type": "refresh_token", "refresh_token": old.RefreshToken,
				})
				return oauthReply(tt.status, tt.body), nil
			})}
			got, err := client.Refresh(t.Context(), old)
			if errors.Is(err, ErrOAuthRejected) != tt.rejected || (err != nil) != tt.bad {
				t.Fatal("incorrect OAuth refresh result")
			}
			if err != nil {
				if got != (OAuthSession{}) || strings.Contains(err.Error(), "credential-detail") {
					t.Fatal("OAuth refresh leaked response data")
				}
			} else if got.WorkspaceID != old.WorkspaceID || got.MemberID != old.MemberID || got.Email != old.Email ||
				got.AccessToken != "new" || got.RefreshToken != "rotated" || time.Until(got.ExpiresAt) <= 9*time.Minute {
				t.Fatal("OAuth refresh did not preserve identity or rotate tokens")
			}
		})
	}
}

func TestOAuthRefreshNeverFollowsRedirect(t *testing.T) {
	var calls int
	client := OAuthClient{Client: &http.Client{
		Transport: oauthTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.URL.String() != consoleURL+deviceTokenPath {
				t.Fatal("OAuth token body followed a redirect")
			}
			response := oauthReply(http.StatusTemporaryRedirect, "")
			response.Header.Set("Location", "https://redirect.example.test/token")
			return response, nil
		}),
	}}
	_, err := client.Refresh(t.Context(), OAuthSession{RefreshToken: "refresh", MemberID: "member", WorkspaceID: "workspace"})
	if calls != 1 || err == nil {
		t.Fatalf("redirect made %d calls, error = %v", calls, err)
	}
}

func assertOAuthJSON(t *testing.T, req *http.Request, want map[string]any) {
	t.Helper()
	var got map[string]any
	if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" ||
		req.Header.Get("Authorization") != "" || json.NewDecoder(req.Body).Decode(&got) != nil ||
		!reflect.DeepEqual(got, want) {
		t.Fatal("incorrect OpenCode OAuth request")
	}
}

func assertOAuthIdentityRequest(t *testing.T, req *http.Request) {
	t.Helper()
	if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer member-access" ||
		req.Header.Get("Accept") != "application/json" {
		t.Fatal("incorrect OpenCode identity request")
	}
}
