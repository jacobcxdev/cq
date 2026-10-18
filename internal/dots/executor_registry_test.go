package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func executorTestSetup(t *testing.T) (*Settings, *CatalogueClient, Account, Account) {
	t.Helper()
	c := catalogueTestClient(t)
	s := c.Settings
	root, err := CanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.RuntimeRoot = root
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	s.AppPath = "/Applications/ChatGPT.app"
	s.AppVersion = "test"
	s.ModuleAsset = "app-initial-test.js"
	s.IntegrationMode = "native"
	ownerDocument := catalogueTestDocument("secondary", c.now().Unix()+3600)
	data, _ := json.Marshal(ownerDocument)
	owner, err := ParseCredential(data, c.now())
	if err != nil {
		t.Fatal(err)
	}
	s.SecondaryAlias = owner.Alias
	s.Dots = map[string]Dot{"main": {OwnerAlias: s.SystemAlias, ProfileID: "main-profile", RootThreadID: "main-root"}, "other": {OwnerAlias: owner.Alias, OwnerAccountID: owner.AccountID, ProfileID: "secondary-profile", RootThreadID: "secondary-root"}}
	inventory, err := c.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	return s, c, inventory.System, owner
}
func TestExecutorHeadersRejectSystemForeignAndExpiredOwners(t *testing.T) {
	s, c, system, owner := executorTestSetup(t)
	if _, err := executorOwnerHeaders(s, owner, owner.Alias, c.now()); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		owner Account
		alias string
	}{{system, system.Alias}, {owner, "foreign"}, {owner, s.SystemAlias}} {
		if _, err := executorOwnerHeaders(s, row.owner, row.alias, c.now()); err == nil {
			t.Fatalf("accepted alias %s", row.alias)
		}
	}
	if _, err := executorOwnerHeaders(s, owner, owner.Alias, c.now().Add(time.Hour)); err == nil {
		t.Fatal("accepted expired owner")
	}
}
func TestExecutorChildHasOnlyLocalCapabilityAndPrivateHome(t *testing.T) {
	s, _, _, _ := executorTestSetup(t)
	for _, key := range []string{"CODEX_ACCESS_TOKEN", "CODEX_AUTH", "BROWSER_USE_ACCESS_PIPE_PATH", "CODEX_CLI_PATH", "HTTPS_PROXY", "SSL_CERT_FILE"} {
		t.Setenv(key, "must-not-inherit")
	}
	root := s.RuntimeRoot + "/executors/secondary"
	cmd, err := executorCommandFor(s, root, 12345, "environment", "local-capability", false, false)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, value := range cmd.Env {
		key, value, _ := strings.Cut(value, "=")
		env[key] = value
	}
	if env["CODEX_API_KEY"] != "local-capability" || env["CODEX_HOME"] != root+"/home" || env["TMPDIR"] != root+"/tmp" || cmd.Dir != root+"/work" {
		t.Fatalf("wrong child environment: %#v", env)
	}
	for _, key := range []string{"CODEX_ACCESS_TOKEN", "CODEX_AUTH", "BROWSER_USE_ACCESS_PIPE_PATH", "CODEX_CLI_PATH", "HTTPS_PROXY", "SSL_CERT_FILE"} {
		if _, ok := env[key]; ok {
			t.Fatalf("inherited forbidden key %s", key)
		}
	}
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "sandbox-exec") || !strings.Contains(args, "--exit-on-stdin-close") || !strings.Contains(args, "--environment-id environment") {
		t.Fatalf("wrong secondary command %s", args)
	}
	isolated, err := executorCommandFor(s, root, 12345, "environment", "local-capability", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Path != "/usr/bin/sandbox-exec" || isolated.Stdout == nil || isolated.Stderr == nil {
		t.Fatal("singleton process isolation or stdio missing")
	}
	for _, invalid := range []string{"../bad", "", "é"} {
		if _, err := executorCommandFor(s, root, 12345, invalid, "local", false, false); err == nil {
			t.Fatalf("accepted environment %q", invalid)
		}
	}
}
func TestExecutorRegistryRendezvousAuthority(t *testing.T) {
	for _, raw := range []string{`{"environment_id":"foreign","url":"wss://chatgpt.com/x"}`, `{"environment_id":"test","url":"wss://chatgpt.com.evil.test/x"}`, `{"environment_id":"test","url":"wss://user@chatgpt.com/x"}`, `{"environment_id":"test","url":"wss://chatgpt.com:80/x"}`, `{"environment_id":"test","url":"https://chatgpt.com/x"}`} {
		if validateExecutorRegistryResponse([]byte(raw), "test") == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, host := range []string{"chatgpt.com", "sub.openai.com", "sub.openai.org"} {
		raw, _ := json.Marshal(map[string]any{"environment_id": "test", "url": "wss://" + host + "/x"})
		if err := validateExecutorRegistryResponse(raw, "test"); err != nil {
			t.Fatal(err)
		}
	}
}
func TestExecutorRegistryBrokerBlocksForeignPathsCapabilitiesAndFraming(t *testing.T) {
	s, _, system, owner := executorTestSetup(t)
	for _, bad := range []Account{system, {Alias: "foreign"}} {
		if _, err := newExecutorRegistryBroker(s, bad, "test", bad.Alias, nil); err == nil {
			t.Fatal("accepted forbidden owner")
		}
	}
	calls := 0
	request := func(context.Context, Account, string, string, string, []byte) (int, string, []byte, error) {
		calls++
		return 200, "application/json", []byte(`{"environment_id":"test","url":"wss://chatgpt.com/x"}`), nil
	}
	b, err := newExecutorRegistryBroker(s, owner, "test", owner.Alias, request)
	if err != nil {
		t.Fatal(err)
	}
	defer b.listener.Close()
	for _, row := range []struct{ path, capability string }{{"/api/cloud/environment/main/register", b.capability}, {"/api/cloud/environment/test/register", "wrong"}} {
		r := httptest.NewRequest("POST", row.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+row.capability)
		w := httptest.NewRecorder()
		b.serve(w, r)
		if w.Code != 403 {
			t.Fatalf("status %d", w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("forwarded unauthorised request")
	}
	r := httptest.NewRequest("POST", "/api/cloud/environment/test/register", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+b.capability)
	w := httptest.NewRecorder()
	b.serve(w, r)
	if w.Code != 502 || calls != 0 {
		t.Fatal("invalid registration was forwarded")
	}
	stopped, _ := b.snapshot()
	if !stopped {
		t.Fatal("malformed request did not stop broker")
	}
}
func TestExecutorRegistryBrokerStopsAfterCredentialRejectionAndRecordsRealSuccess(t *testing.T) {
	s, _, _, owner := executorTestSetup(t)
	for _, status := range []int{200, 401, 403} {
		calls := 0
		b, err := newExecutorRegistryBroker(s, owner, "test", owner.Alias, func(_ context.Context, a Account, host, method, path string, body []byte) (int, string, []byte, error) {
			calls++
			if a.AccountID != owner.AccountID || host != executorRegistry || method != "POST" {
				t.Fatal("owner or registry changed")
			}
			return status, "application/json", []byte(`{"environment_id":"test","url":"wss://chatgpt.com/x"}`), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/api/cloud/environment/test/register", strings.NewReader(`{"security_profile":"noise_hybrid_ik_v1","executor_public_key":{}}`))
		r.Header.Set("Authorization", "Bearer "+b.capability)
		w := httptest.NewRecorder()
		b.serve(w, r)
		stopped, registered := b.snapshot()
		if w.Code != status || registered != (status == 200) || stopped != (status != 200) {
			t.Fatalf("wrong state after %d: stopped=%v registered=%v", status, stopped, registered)
		}
		if status != 200 {
			w = httptest.NewRecorder()
			b.serve(w, r)
			if w.Code != 403 || calls != 1 {
				t.Fatal("retried rejected owner")
			}
		}
		_ = b.listener.Close()
	}
}
func TestExecutorDirectRequestBoundsResponseAndRejectsOtherHosts(t *testing.T) {
	s, c, _, owner := executorTestSetup(t)
	calls := 0
	doer := catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != executorRegistry || r.Header.Get("Authorization") != "Bearer "+owner.Token {
			t.Fatal("request identity changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(make([]byte, (1<<20)+1)))}, nil
	})
	request := executorDirectRequest(s, doer, c.now, owner.Alias)
	if _, _, _, err := request(context.Background(), owner, "evil.test", "POST", "/api", nil); err == nil || calls != 0 {
		t.Fatal("foreign host forwarded")
	}
	if _, _, _, err := request(context.Background(), owner, executorRegistry, "GET", "/api", nil); err == nil || calls != 1 {
		t.Fatal("oversize response accepted")
	}
}
