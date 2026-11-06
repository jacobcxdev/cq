package dots

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeStartupNeverSentKnownSecondaryResourcesAsPrimary(t *testing.T) {
	for _, path := range []string{
		"/backend-api/tbo/profile-owner", "/backend-api/tbo/profile-owner/threads",
		"/backend-api/tbo/by-thread/root-owner", "/backend-api/tbo/by-thread/root-owner/activity",
	} {
		t.Run(path, func(t *testing.T) {
			p, _, owner := probeFixtureForTest(t)
			ready := p.fixture
			p.fixture = nil
			calls := 0
			p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer "+owner.Token {
					t.Error("secondary routed with foreign credentials")
				}
				return probeSuccess("{}"), nil
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, probeRequest("GET", path, ""))
			if w.Code != http.StatusConflict || calls != 0 {
				t.Fatalf("unready owner escaped: status=%d calls=%d", w.Code, calls)
			}
			p.fixture = ready
			w = httptest.NewRecorder()
			p.ServeHTTP(w, probeRequest("GET", path, ""))
			if w.Code != http.StatusOK || calls != 1 {
				t.Fatalf("ready owner unavailable: status=%d calls=%d", w.Code, calls)
			}
		})
	}
}

func TestProbeStartupPreservedPrimaryAndUnknownPassthrough(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	p.fixture = nil
	calls := 0
	p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer native-token" {
			t.Error("primary authority changed")
		}
		return probeSuccess("{}"), nil
	})
	for _, path := range []string{"/backend-api/tbo/primary", "/backend-api/tbo/profile-system", "/backend-api/tbo/by-thread/root-system", "/backend-api/tbo/unknown", "/backend-api/wham/settings/user"} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, probeRequest("GET", path, ""))
		if w.Code != http.StatusOK {
			t.Fatalf("primary blocked: %s status%d", path, w.Code)
		}
	}
	if calls != 5 {
		t.Fatalf("passthrough calls %d", calls)
	}
}
