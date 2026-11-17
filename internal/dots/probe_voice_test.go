package dots

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestNativeVoiceOnlyRetriesDefinitiveQuotaRejection(t *testing.T) {
	const limit = `{"error":{"type":"usage_limit_reached"},"voice_limit_message":{"title":"Limit reached"}}`
	const offer = `{"sdp":"v=0 offer","session":{"delegation":{"type":"client"},"voice_session_id":"voice-session"}}`
	for _, item := range []struct {
		name   string
		status int
		body   string
		bearer string
		want   bool
	}{
		{name: "quota rejection", status: 429, body: limit, bearer: "native-token", want: true},
		{name: "uncertain throttling", status: 429, body: `{"error":{"type":"rate_limit_exceeded"}}`, bearer: "native-token"},
		{name: "unauthenticated caller", status: 429, body: limit, bearer: "foreign"},
		{name: "success", status: 201, body: "original-answer", bearer: "native-token"},
	} {
		t.Run(item.name, func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			p.options.VoiceProxy = "http://127.0.0.1:19280"
			calls := 0
			path := proxy.CodexNativeVoiceCallPath + "?intent=quicksilver&architecture=avas"
			p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != offer || r.URL.RequestURI() != path {
					t.Fatal("native voice offer changed")
				}
				if calls == 1 {
					response := probeSuccess(item.body)
					response.StatusCode = item.status
					return response, nil
				}
				if calls != 2 || r.URL.Host != "127.0.0.1:19280" ||
					r.Header.Get("Authorization") != "Bearer native-token" || r.Header.Get("Cookie") != "" {
					t.Fatal("unexpected native voice fallback authority")
				}
				response := probeSuccess("alternate-answer")
				response.StatusCode = http.StatusCreated
				response.Header.Set("Location", "/v1/realtime/calls/rtc_alternate")
				return response, nil
			})
			r := probeRequest(http.MethodPost, path, offer)
			r.Header.Set("Authorization", "Bearer "+item.bearer)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("OpenAI-Alpha", "quicksilver=v2")
			r.Header.Set("Cookie", "primary=fixture")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if item.want {
				if calls != 2 || w.Code != http.StatusCreated || w.Body.String() != "alternate-answer" ||
					w.Header().Get("Location") != "/v1/realtime/calls/rtc_alternate" {
					t.Fatalf("native voice failover: calls=%d code=%d", calls, w.Code)
				}
			} else if calls != 1 || w.Code != item.status || w.Body.String() != item.body {
				t.Fatalf("native voice passthrough changed: calls=%d code=%d", calls, w.Code)
			}
		})
	}
}

func TestProbeSecondaryVoiceLifecycleUsedOwnerIdentity(t *testing.T) {
	for _, operation := range []struct{ path, body string }{
		{"", `{"sdp":"v=0 offer","instant_connect":true,"debug_prompt_overrides":{}}`},
		{"/rtc_call/attach", ""},
		{"/rtc_call/refresh", `{"sdp":"v=0 new offer","ufrag":"ice-fragment"}`},
		{"/rtc_call/stop", `{"client_duration_ms":120000}`},
		{"/rtc_call/stop", ""},
	} {
		t.Run(operation.path+operation.body, func(t *testing.T) {
			p, _, owner := probeFixtureForTest(t)
			path := "/backend-api/tbo/profile-owner/voice/calls" + operation.path
			calls := 0
			p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.URL.RequestURI() != path || string(body) != operation.body {
					t.Error("voice request changed")
				}
				if r.Header.Get("Authorization") != "Bearer "+owner.Token || r.Header.Get("ChatGPT-Account-Id") != owner.AccountID {
					t.Error("voice request used foreign authority")
				}
				for _, key := range []string{"Cookie", "X-OpenAI-Account-Routing-Override", "X-OpenAI-Expected-Account-Id"} {
					if r.Header.Get(key) != "" {
						t.Errorf("voice request retained %s", key)
					}
				}
				response := probeSuccess("v=0 answer")
				response.StatusCode = http.StatusCreated
				response.Header.Set("Content-Type", "application/sdp")
				response.Header.Set("Location", "/rtc_call")
				response.Header.Set("OpenAI-Voice-Thread-ID", "voice-thread")
				response.Header.Set("OpenAI-Root-Thread-ID", "root-owner")
				response.Header.Set("Set-Cookie", "secondary=secret")
				return response, nil
			})
			r := probeRequest("POST", path, operation.body)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Cookie", "primary=secret")
			r.Header.Set("X-OpenAI-Account-Routing-Override", "foreign")
			r.Header.Set("X-OpenAI-Expected-Account-Id", "foreign")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusCreated || calls != 1 || w.Body.String() != "v=0 answer" || w.Header().Get("Content-Type") != "application/sdp" || w.Header().Get("Location") != "/rtc_call" || w.Header().Get("OpenAI-Voice-Thread-ID") != "voice-thread" || w.Header().Get("OpenAI-Root-Thread-ID") != "root-owner" || w.Header().Get("Set-Cookie") != "" {
				t.Fatalf("voice response unavailable or changed: status=%d calls=%d", w.Code, calls)
			}
		})
	}
}

func TestProbeSecondaryVoiceNeverRetriedUnknownOutcome(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	calls := 0
	p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("unknown voice create outcome")
	})
	r := probeRequest("POST", "/backend-api/tbo/profile-owner/voice/calls", `{"sdp":"v=0 offer"}`)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || calls != 1 {
		t.Fatalf("status=%d calls=%d", w.Code, calls)
	}
}

func TestProbeSecondaryVoiceAcceptedCompositeProfileID(t *testing.T) {
	p, _, owner := probeFixtureForTest(t)
	const profileID = "profile-owner~workspace-owner"
	p.fixture.Profiles[1]["id"] = profileID
	delete(p.fixture.Owners, "profile-owner")
	p.fixture.Owners[profileID] = owner
	dot := p.settings.Dots["two"]
	dot.ProfileID = profileID
	p.settings.Dots["two"] = dot
	if _, err := p.validateFixture(p.fixture); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+owner.Token || r.Header.Get("ChatGPT-Account-Id") != owner.AccountID || !strings.Contains(r.URL.Path, "/"+profileID+"/voice/calls") {
			t.Error("composite profile lost owner identity")
		}
		return probeSuccess("{}"), nil
	})
	for _, operation := range []struct{ suffix, body string }{
		{"", `{"sdp":"v=0 offer"}`},
		{"/rtc_call/attach", ""},
		{"/rtc_call/refresh", `{"sdp":"v=0 new offer","ufrag":"ice-fragment"}`},
		{"/rtc_call/stop", `{"client_duration_ms":120000}`},
	} {
		r := probeRequest("POST", "/backend-api/tbo/"+profileID+"/voice/calls"+operation.suffix, operation.body)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("composite profile operation %q returned %d", operation.suffix, w.Code)
		}
	}
	if calls != 4 {
		t.Fatalf("composite profile lifecycle reached upstream %d times, want 4", calls)
	}
}

func TestProbeSecondaryVoiceRejectedInvalidWrites(t *testing.T) {
	for _, invalid := range []struct{ method, suffix, body, contentType string }{
		{"DELETE", "", "", ""},
		{"POST", "?extra=x", `{"sdp":"v=0"}`, "application/json"},
		{"POST", "/rtc_call/unknown", `{}`, "application/json"},
		{"POST", "/other_call/stop", "", ""},
		{"POST", "/rtc%2Fcall/stop", "", ""},
		{"POST", "/rtc_call/attach/extra", "", ""},
		{"POST", "/rtc_call/attach", `{}`, "application/json"},
		{"POST", "", `{"sdp":"v=0"}`, "text/plain"},
		{"POST", "", `{"sdp":`, "application/json"},
		{"POST", "", `[]`, "application/json"},
		{"POST", "", "", "application/json"},
		{"POST", "/rtc_call/refresh", "", "application/json"},
		{"POST", "/rtc_call/stop", `null`, "application/json"},
		{"POST", "", strings.Repeat("x", (1<<20)+1), "application/json"},
	} {
		t.Run(invalid.method+invalid.suffix+invalid.body[:min(len(invalid.body), 15)], func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			calls := 0
			p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) { calls++; return probeSuccess("{}"), nil })
			r := probeRequest(invalid.method, "/backend-api/tbo/profile-owner/voice/calls"+invalid.suffix, invalid.body)
			r.Header.Set("Content-Type", invalid.contentType)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code < 400 || calls != 0 {
				t.Fatalf("invalid voice write escaped: status=%d calls=%d", w.Code, calls)
			}
		})
	}
}

func TestProbeSecondaryVoicePreservedNativeOwnershipGates(t *testing.T) {
	for _, mode := range []string{"foreign-auth", "loading", "conflicting-owner", "research"} {
		t.Run(mode, func(t *testing.T) {
			p, system, _ := probeFixtureForTest(t)
			r := probeRequest("POST", "/backend-api/tbo/profile-owner/voice/calls", `{"sdp":"v=0 offer"}`)
			r.Header.Set("Content-Type", "application/json")
			want := http.StatusConflict
			switch mode {
			case "foreign-auth":
				r.Header.Set("Authorization", "Bearer foreign")
				want = http.StatusUnauthorized
			case "loading":
				p.fixture = nil
			case "conflicting-owner":
				p.fixture.Owners["root-owner"] = system
			case "research":
				p.settings.IntegrationMode = "research"
				want = http.StatusServiceUnavailable
			}
			calls := 0
			p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) { calls++; return probeSuccess("{}"), nil })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != want || calls != 0 {
				t.Fatalf("voice bypassed native ownership gate: status=%d calls=%d", w.Code, calls)
			}
		})
	}
}
