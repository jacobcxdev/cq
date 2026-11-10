package dots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestProbeSecondaryAuthenticationFailurePreservedNativePrincipal(t *testing.T) {
	for _, path := range []string{"/backend-api/tbo/profile-owner", "/backend-api/files/download/file-one", "/backend-api/tbo/by-thread/root-owner/activity"} {
		t.Run(path, func(t *testing.T) {
			p, _, owner := probeFixtureForTest(t)
			if err := p.rememberDownload("https://chatgpt.com/backend-api/files/download/file-one", owner); err != nil {
				t.Fatal(err)
			}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer owner-token" || r.Header.Get("ChatGPT-Account-Id") != "owner" {
					t.Error("secondary request used the native principal")
				}
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail":"private owner failure"}`))
			}))
			defer upstream.Close()
			p.options.Origin, p.options.HTTP = upstream.URL, upstream.Client()
			w := httptest.NewRecorder()
			p.ServeHTTP(w, probeRequest("GET", path, ""))
			if w.Code != http.StatusBadGateway || calls != 1 {
				t.Fatalf("substituted owner failure: code=%d, upstream calls=%d", w.Code, calls)
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != "dot_owner_authentication_unavailable" {
				t.Fatalf("owner failure classification missing: %s", w.Body.String())
			}
			if w.Header().Get("WWW-Authenticate") != "" || strings.Contains(w.Body.String(), "private owner failure") {
				t.Fatal("foreign authentication challenge escaped into the native principal")
			}
		})
	}
}

func TestProbeInvalidNativePrincipalDoesNotUseOwnerCredentials(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	calls := 0
	p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) { calls++; return probeSuccess("{}"), nil })
	r := probeRequest("GET", "/backend-api/tbo/profile-owner", "")
	r.Header.Set("Authorization", "Bearer invalid-native-token")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("native principal rejection changed: code=%d calls=%d", w.Code, calls)
	}
}

// Couple Probe's actual HTTP result to the installed Desktop's authentication
// retry decision. No native credentials or running Desktop state are accessed.
func TestInstalledNativeSecondaryAuthenticationBoundary(t *testing.T) {
	app := os.Getenv("CQ_TEST_NATIVE_DISCOVERY_APP")
	if app == "" {
		t.Skip("set CQ_TEST_NATIVE_DISCOVERY_APP for read-only installed authentication qualification")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required for installed authentication qualification")
	}
	var retry string
	for _, source := range installedBootstrapSourcesForTest(t, app) {
		start := strings.Index(source, `W="never"!==l&&`)
		if start < 0 {
			continue
		}
		end := strings.Index(source[start:], ";if(401===")
		if end < 0 || retry != "" {
			t.Fatal("installed authentication retry contract ambiguous")
		}
		retry = strings.TrimPrefix(source[start:start+end], "W=")
	}
	if retry == "" {
		t.Fatal("installed authentication retry contract changed")
	}
	for _, test := range []struct {
		name string
		path string
		want bool
	}{
		{"secondary-owner", "/backend-api/tbo/profile-owner", false},
		{"native-principal", "/backend-api/ordinary", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) {
				response := probeSuccess(`{"detail":"expired"}`)
				response.StatusCode = http.StatusUnauthorized
				return response, nil
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, probeRequest("GET", test.path, ""))
			input, _ := json.Marshal(map[string]any{"status": w.Code, "want": test.want})
			script := `const assert=require('node:assert/strict'),fs=require('node:fs');const test=JSON.parse(fs.readFileSync(0,'utf8'));const x={status:test.status},b={attachAuth:true,token:'fixture-native-token'},T=null,l='default',i='GET',o='https://chatgpt.com/backend-api/resource',ei={d33:'/uploads'};let u,h;assert.equal(Boolean(` + retry + `),test.want,'foreign-owner auth must not refresh native principal');`
			command := exec.Command(node, "-e", script)
			command.Stdin = strings.NewReader(string(input))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("installed authentication retry: %v\n%s", err, output)
			}
		})
	}
}

func TestProbeAuthenticationFailureStatusBoundaries(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		code int
	}{
		{"native-authentication", "/backend-api/ordinary", http.StatusUnauthorized},
		{"owner-authorization", "/backend-api/tbo/profile-owner", http.StatusForbidden},
		{"owner-missing-resource", "/backend-api/tbo/profile-owner", http.StatusNotFound},
		{"owner-upstream-unavailable", "/backend-api/tbo/profile-owner", http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) {
				response := probeSuccess(`{"detail":"upstream response"}`)
				response.StatusCode = test.code
				return response, nil
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, probeRequest("GET", test.path, ""))
			if w.Code != test.code || w.Body.String() != `{"detail":"upstream response"}` {
				t.Fatalf("unrelated response changed: code=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
