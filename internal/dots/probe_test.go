package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type probeDoer func(*http.Request) (*http.Response, error)

func (f probeDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func probeFixtureForTest(t *testing.T) (*Probe, Account, Account) {
	t.Helper()
	system := Account{Alias: "aaaaaaaa", AccountID: "system", Token: "native-token", ViewerIDs: map[string]bool{"system-user": true}}
	owner := Account{Alias: "bbbbbbbb", AccountID: "owner", Token: "owner-token", ViewerIDs: map[string]bool{"owner-user": true}}
	s := Settings{RuntimeRoot: t.TempDir(), SystemAlias: system.Alias, IntegrationMode: "native", Dots: map[string]Dot{"one": {OwnerAlias: system.Alias, ProfileID: "profile-system", RootThreadID: "root-system"}, "two": {OwnerAlias: owner.Alias, ProfileID: "profile-owner", RootThreadID: "root-owner"}}}
	if err := os.Chmod(s.RuntimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := NewProbe(s, ProbeOptions{RequireNative: func(_ context.Context, h http.Header) error {
		if h.Get("Authorization") != "Bearer native-token" || h.Get("ChatGPT-Account-Id") != "system" {
			return fmt.Errorf("native auth required")
		}
		return nil
	}, Event: func(map[string]any) {}})
	if err != nil {
		t.Fatal(err)
	}
	p.fixture = &CatalogueFixture{System: system, Profiles: []map[string]any{{"id": "profile-system", "active_root_thread_id": "root-system", "messaging_room_id": "room-system"}, {"id": "profile-owner", "active_root_thread_id": "root-owner", "messaging_room_id": "room-owner"}}, Owners: map[string]Account{"profile-system": system, "root-system": system, "room-system": system, "profile-owner": owner, "root-owner": owner, "room-owner": owner}}
	return p, system, owner
}
func probeRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer native-token")
	r.Header.Set("ChatGPT-Account-Id", "system")
	return r
}
func probeSuccess(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestProbeOwnerHeadersStripsAllForeignAuthority(t *testing.T) {
	_, _, owner := probeFixtureForTest(t)
	h := http.Header{}
	for _, key := range []string{"Authorization", "ChatGPT-Account-Id", "Cookie", "X-OpenAI-Account-Routing-Override", "X-OpenAI-Expected-Account-Id", "X-OpenAI-Orbit-Selection"} {
		h.Set(key, "foreign")
	}
	h.Set("X-Sentinel-Dc", "device-proof")
	result := OwnerHeaders(h, owner)
	if result.Get("Authorization") != "Bearer owner-token" || result.Get("ChatGPT-Account-Id") != "owner" || result.Get("X-Sentinel-Dc") != "device-proof" {
		t.Fatal("owner/device identity not preserved")
	}
	for _, key := range []string{"Cookie", "X-OpenAI-Account-Routing-Override", "X-OpenAI-Expected-Account-Id", "X-OpenAI-Orbit-Selection"} {
		if result.Get(key) != "" {
			t.Errorf("foreign %s leaked", key)
		}
	}
	if h.Get("Authorization") != "foreign" {
		t.Fatal("input mutated")
	}
}

func TestProbeProfileValidationUsesIDsOwnersNotNames(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	for _, profile := range p.fixture.Profiles {
		profile["display_name"] = "duplicate"
	}
	if _, err := p.validateFixture(p.fixture); err != nil {
		t.Fatal(err)
	}
	p.fixture.Profiles[1]["active_root_thread_id"] = "root-system"
	if _, err := p.validateFixture(p.fixture); err == nil {
		t.Fatal("accepted cross-owner root")
	}
}

func TestProbeResourceOwnership(t *testing.T) {
	p, _, owner := probeFixtureForTest(t)
	for _, path := range []string{"/backend-api/tbo/profile-owner", "/backend-api/tbo/by-thread/root-owner/activity", "/backend-api/messaging/rooms/room-owner/history"} {
		got, err := p.resourceOwner(path, p.fixture)
		if err != nil || got == nil || !probeEqual(*got, owner) {
			t.Errorf("%s ownership missing: %v", path, err)
		}
	}
	if got, err := p.resourceOwner("/backend-api/tbo/unknown", p.fixture); got != nil || err != nil {
		t.Fatal("unknown resource claimed")
	}
	p.fixture.Owners["root-owner"] = p.fixture.System
	if _, err := p.resourceOwner("/backend-api/tbo/profile-owner", p.fixture); err == nil {
		t.Fatal("cross-owner resource accepted")
	}
}

func TestProbeSecondaryGateBlocksBeforeUpstream(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	calls := 0
	p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) { calls++; return probeSuccess("{}"), nil })
	r := probeRequest("GET", "/backend-api/tbo/profile-owner", "")
	r.Header.Set("Authorization", "Bearer foreign")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 401 || calls != 0 {
		t.Fatalf("code=%d calls=%d", w.Code, calls)
	}
}

func TestProbeNativePrimaryOpaqueAndSafeHeaders(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	body := strings.Repeat("x", (1<<20)+8)
	p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer native-token" || r.Header.Get("Cookie") != "native-cookie" || r.Header.Get("X-Hop") != "" {
			t.Error("primary auth/hop headers changed incorrectly")
		}
		response := probeSuccess(body)
		response.Header.Set("Set-Cookie", "native=yes; Domain=chatgpt.com; Secure")
		return response, nil
	})
	r := probeRequest("POST", "/backend-api/wham/responses", body)
	r.Header.Set("Cookie", "native-cookie")
	r.Header.Set("Connection", "X-Hop")
	r.Header.Set("X-Hop", "secret")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != body || strings.Contains(w.Header().Get("Set-Cookie"), "Domain=") {
		t.Fatal("primary passthrough changed body or cookie")
	}
}

func TestProbeUnknownSecondaryMutationBlockedNoWriteRetry(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	calls := 0
	p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("failure") })
	for _, path := range []string{"/backend-api/tbo/profile-owner/reboot", "/backend-api/messaging/rooms/room-owner/messages"} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, probeRequest("POST", path, `{}`))
		if w.Code != 503 {
			t.Errorf("unexpected code %d", w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("blocked mutation sent")
	}

}

func TestProbeChallengeOneUseOwnerBoundExpiry(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	now := time.Now()
	p.options.Now = func() time.Time { return now }
	if err := p.rememberChallenge([]byte(`{"attestation_challenge":"native","expire_after":2}`), []byte(`{"attestation_challenge":"owner","expire_after":5}`), "bbbbbbbb", now); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"app_attest_challenge":"native"}`)
	if _, err := p.ownerMessage(body, "cccccccc"); err == nil {
		t.Fatal("other owner challenge accepted")
	}
	result, err := p.ownerMessage(body, "bbbbbbbb")
	if err != nil || !bytes.Contains(result, []byte(`"owner"`)) {
		t.Fatal("owner replacement unavailable")
	}
	if _, err := p.ownerMessage(body, "bbbbbbbb"); err == nil {
		t.Fatal("challenge reused")
	}
	if err := p.rememberChallenge([]byte(`{"attestation_challenge":"native"}`), []byte(`{"attestation_challenge":"owner","expire_after":1}`), "bbbbbbbb", now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := p.ownerMessage(body, "bbbbbbbb"); err == nil {
		t.Fatal("expired challenge accepted")
	}
}

func TestProbeSecondarySendProjectsOnlyPresentationAndNeverRetries(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	now := time.Now()
	if err := p.rememberChallenge([]byte(`{"attestation_challenge":"native"}`), []byte(`{"attestation_challenge":"owner"}`), "bbbbbbbb", now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := probeRead(r.Body, 1<<20)
		v, _ := probeObject(body)
		if v["app_attest_challenge"] != "owner" || r.Header.Get("Authorization") != "Bearer owner-token" || r.Header.Get("Cookie") != "" {
			t.Error("send isolation failed")
		}
		return probeSuccess(`{"id":"unchanged","account_user_id":"owner-user","role":"user"}`), nil
	})
	body := `{"content":{"text":"hello"},"request_id":"request","idempotency_token":"request","app_attest_challenge":"native"}`
	r := probeRequest("POST", "/backend-api/messaging/rooms/room-owner/messages", body)
	r.Header.Set("Cookie", "foreign")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	v, _ := probeObject(w.Body.Bytes())
	if w.Code != 200 || v["is_self"] != true || v["id"] != "unchanged" || calls != 1 {
		t.Fatalf("send result %d calls=%d", w.Code, calls)
	}
	w = httptest.NewRecorder()
	p.ServeHTTP(w, probeRequest("POST", "/backend-api/messaging/rooms/room-owner/messages", body))
	if w.Code != 409 || calls != 1 {
		t.Fatal("consumed challenge sent again")
	}
}

func TestProbeDownloadsUseFreshCatalogueCredential(t *testing.T) {
	p, _, owner := probeFixtureForTest(t)
	if err := p.rememberDownload("https://chatgpt.com/backend-api/files/download/file-one?token=opaque", owner); err != nil {
		t.Fatal(err)
	}
	if err := p.rememberDownload("https://evil.test/backend-api/files/download/file-two", owner); err != nil {
		t.Fatal(err)
	}
	owner.Token = "fresh"
	p.fixture.Owners["profile-owner"] = owner
	got := p.downloadOwner("/backend-api/files/download/file-one", p.fixture)
	if got == nil || got.Token != "fresh" {
		t.Fatal("download credential frozen or missing")
	}
	if p.downloadOwner("/backend-api/files/download/file-two", p.fixture) != nil {
		t.Fatal("foreign URL granted")
	}
	if err := p.rememberDownload("https://chatgpt.com/backend-api/files/download/file-one", p.fixture.System); err == nil {
		t.Fatal("download collision accepted")
	}
}

func TestProbeValidationRejectsInvalidInputs(t *testing.T) {
	for _, body := range []string{`{}`, `{"last_read_at":"2026-10-18T12:00:00"}`, `{"last_read_at":"2026-10-18T12:00:00Z","extra":1}`} {
		if probeReceipt([]byte(body)) {
			t.Errorf("receipt accepted %s", body)
		}
	}
	if !probeReceipt([]byte(`{"last_read_at":"2026-10-18T12:00:00Z"}`)) {
		t.Fatal("valid receipt rejected")
	}
	for _, body := range []string{`{"display_name":" "}`, `{"display_name":"bad\\name"}`, `{"display_name":"ok","extra":0}`} {
		if probeName("/backend-api/tbo/profile", []byte(body)) {
			t.Errorf("invalid name %s", body)
		}
	}
	if !probeMessage([]byte(`{"content":{"text":"","attachments":[{"type":"file","file_id":"file-one"}]},"request_id":"r","idempotency_token":"r"}`), true) {
		t.Fatal("native attachment message rejected")
	}
	if probeMessage([]byte(`{"content":{"text":"text","attachments":[{"type":"file","file_id":"../bad"}]},"request_id":"r","idempotency_token":"r"}`), true) {
		t.Fatal("invalid attachment accepted")
	}
}

func TestProbeBoundedWorkersReturnBeforeLateState(t *testing.T) {
	p, _, owner := probeFixtureForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var lock sync.Mutex
	calls := 0
	results := p.ownerResults(ctx, []Account{owner}, func(ctx context.Context, _ Account) (any, error) {
		lock.Lock()
		calls++
		lock.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if results[owner.Alias].err == nil {
		t.Fatal("cancelled operation accepted")
	}
}

func TestProbeRequestUploadBoundAndContentValidation(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	calls := 0
	p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) { calls++; return probeSuccess(`{}`), nil })
	r := probeRequest("POST", "/backend-api/messaging/rooms/room-owner/files", "x")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=native")
	r.ContentLength = nativeUploadLimit + 1
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 413 || calls != 0 {
		t.Fatal("oversized upload forwarded")
	}
	r = probeRequest("POST", "/backend-api/messaging/rooms/room-owner/files", "x")
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 503 || calls != 0 {
		t.Fatal("invalid upload format forwarded")
	}
}

func TestProbeStatsigPreservesIdentityAndOtherEvaluations(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	payload := `{"user":{"id":"native"},"feature_gates":{"other":{"value":false}},"dynamic_configs":{"x":1}}`
	envelope, _ := json.Marshal(map[string]any{"user": map[string]any{"id": "native"}, "statsigPayload": payload})
	r := probeRequest("GET", "/backend-api/wham/statsig/bootstrap", "")
	data, err := p.transform(r, probeSuccess(""), envelope, nil, nil, 0, false, true)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := probeObject(data)
	inner, _ := probeObject([]byte(probeString(v["statsigPayload"])))
	gates := inner["feature_gates"].(map[string]any)
	if gates["other"].(map[string]any)["value"] != false || gates["2929219922"].(map[string]any)["value"] != true || inner["user"].(map[string]any)["id"] != "native" {
		t.Fatal("identity or other evaluation changed")
	}
}

func TestProbeCatalogueVerifiedFallbackAndChangedPrimaryPassthrough(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			primary := map[string]any{"id": "profile-system", "active_root_thread_id": "root-system", "messaging_room_id": "room-system"}
			if changed {
				primary["active_root_thread_id"] = "changed-root"
			}
			body, _ := json.Marshal(map[string]any{"items": []any{primary}, "cursor": nil})
			data, err := p.transform(probeRequest("GET", "/backend-api/tbo", ""), probeSuccess(""), body, nil, p.fixture, 0, false, true)
			if err != nil {
				t.Fatal(err)
			}
			value, _ := probeObject(data)
			items := value["items"].([]any)
			expected := 2
			if changed {
				expected = 1
			}
			if len(items) != expected {
				t.Fatalf("got %d profiles wanted %d", len(items), expected)
			}
		})
	}
}

func TestProbeNativePrimaryRefreshRejectsCrossOwnerAndPublishesNewRoot(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	headers := probeRequest("GET", "/", "").Header
	for _, root := range []string{"root-owner", "fresh-system-root"} {
		body, _ := json.Marshal(map[string]any{"selection": map[string]any{"available": true, "thread_id": root}, "profile": map[string]any{"id": "profile-system", "active_root_thread_id": root}})
		err := p.observePrimary(context.Background(), headers, body)
		if root == "root-owner" {
			if err == nil {
				t.Fatal("cross-owner root accepted")
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if p.fixture.Profiles[0]["active_root_thread_id"] != "fresh-system-root" || p.generation != 1 {
		t.Fatal("native root not refreshed")
	}
	if _, ok := p.fixture.Owners["root-system"]; ok {
		t.Fatal("stale primary grant retained")
	}
}

func TestProbeAttestationFanoutPreservesNativeProofAndBindsEachOwner(t *testing.T) {
	p, _, owner := probeFixtureForTest(t)
	calls := map[string]int{}
	var lock sync.Mutex
	p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
		alias := r.Header.Get("ChatGPT-Account-Id")
		lock.Lock()
		calls[alias]++
		lock.Unlock()
		if r.Header.Get("X-Sentinel-Dc") != "native-device-proof" {
			t.Error("device attestation stripped")
		}
		challenge := "native-challenge"
		if alias == owner.AccountID {
			challenge = "owner-challenge"
			if r.Header.Get("Authorization") != "Bearer owner-token" || r.Header.Get("Cookie") != "" {
				t.Error("owner attestation identity leaked")
			}
		}
		return probeSuccess(`{"attestation_challenge":"` + challenge + `"}`), nil
	})
	r := probeRequest("GET", "/backend-api/ios/attestation_challenge", "")
	r.Header.Set("X-Sentinel-Dc", "native-device-proof")
	r.Header.Set("Cookie", "native-cookie")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || calls["system"] != 1 || calls[owner.AccountID] != 1 {
		t.Fatalf("challenge calls=%v code=%d", calls, w.Code)
	}
	data, err := p.ownerMessage([]byte(`{"app_attest_challenge":"native-challenge"}`), owner.Alias)
	if err != nil || !bytes.Contains(data, []byte("owner-challenge")) {
		t.Fatal("fanout owner challenge unavailable")
	}
}

func TestProbeSecondaryBackendFailureDoesNotRetryWrite(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	calls := 0
	p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("unavailable") })
	w := httptest.NewRecorder()
	p.ServeHTTP(w, probeRequest("POST", "/backend-api/tbo/profile-owner/runtime/pause", ""))
	if w.Code != 502 || calls != 1 {
		t.Fatalf("write attempts=%d code=%d", calls, w.Code)
	}
}
