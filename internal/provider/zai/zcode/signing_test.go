package zcode

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// Independent Node crypto.hkdfSync/createHmac/createCipheriv vectors. Private
// key contains the public test seed 000102...1f, not an account credential.
const signingFixtureCipher = "AAECAwQFBgcICQoL1gnsLrlYxd5MjOmaq8dWfVv9tDIkLsDx9F1ECw3j4N68H5WZFZ6p4JyM3/UEoWXFjTxZTjjjBwqM2jwcS7vHrA7wco7WjGGLnE0TSt1Peko="
const signingFixturePublic = "03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8"

func signingResponse(ciphertext string) *http.Response {
	body, _ := json.Marshal(map[string]any{"code": 200, "data": map[string]string{"privateCipher": ciphertext}})
	return jsonResponse(string(body))
}

func TestSigningIndependentDerivationAndCipherVectors(t *testing.T) {
	for _, test := range []struct{ info, want string }{
		{"getSignKey_hmac", "a749a0140477e49397c13227b121ab1c64bc6bb3f1277f6c232f05f384bd2adb"},
		{"ed25519_priv", "667f854e18691eaadcb3eddd0741befcf6c7d1ecbdf41654881fbfb3fa4ef1bc"},
	} {
		key, err := signingDerivedKey("fixture-secret", test.info)
		if err != nil || hex.EncodeToString(key) != test.want {
			t.Fatal("HKDF did not match independent Node vector")
		}
	}
	mac, err := signingHandshakeMAC("fixture-id", "fixture-secret", "1800000000123", "000102030405060708090a0b0c0d0e0f")
	if err != nil || mac != "u9v0BVuQDQ5dPZovgBioQGQ/8PnF7D9talBHAejrwAg=" {
		t.Fatal("handshake HMAC did not match independent Node vector")
	}
	key, err := decryptSigningKey("fixture-id", "fixture-secret", signingFixtureCipher)
	if err != nil || hex.EncodeToString(key.Public().(ed25519.PublicKey)) != signingFixturePublic {
		t.Fatal("private key decryption did not match independent Node vector")
	}
}

func verifySigningHeaders(t *testing.T, header http.Header, id, session string, public ed25519.PublicKey) {
	t.Helper()
	if header.Get("X-App-Id") != "zcode" || header.Get("X-Client-Version") != "3.17.2" || header.Get("X-Session-Id") != session || header.Get("X-Client-Sign-Verified") != "" || header.Get("Authorization") != "" {
		t.Fatal("signing headers asserted another application or leaked authentication")
	}
	ts, nonce := header.Get("X-Client-Ts"), header.Get("X-Client-Nonce")
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Fatal("invalid millisecond timestamp")
	}
	if decoded, err := hex.DecodeString(nonce); err != nil || len(decoded) != 16 || nonce != strings.ToLower(nonce) {
		t.Fatal("invalid request nonce")
	}
	signature, err := base64.StdEncoding.DecodeString(header.Get("X-Client-Sig"))
	message := strings.Join([]string{id, ts, "3.17.2", session, nonce}, "\n")
	if err != nil || !ed25519.Verify(public, []byte(message), signature) {
		t.Fatal("request signature failed independent public-key verification")
	}
	proof := header.Get("X-Client-Pow")
	if decoded, err := hex.DecodeString(proof); err != nil || len(decoded) != 16 || proof != strings.ToLower(proof) {
		t.Fatal("invalid proof candidate")
	}
	seed := sha256.Sum256([]byte(strings.Join([]string{id, "zcode", session, ts}, "\n")))
	digest := sha256.Sum256([]byte(hex.EncodeToString(seed[:])[:32] + "\n" + proof))
	if digest[0] != 0 {
		t.Fatal("proof did not satisfy eight-bit work requirement")
	}
}

func TestSignerHandshakeAndFreshHeaders(t *testing.T) {
	const credential = "fixture-id.fixture-secret"
	var calls int
	s := Signer{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.String() != "https://api.z.ai/api/paas/c1f3a7e2/v2/client" || r.Header.Get("Authorization") != credential || r.Header.Get("User-Agent") != "ZCode/3.17.2" || r.Header.Get("X-App-Id") != "zcode" || r.Header.Get("X-Client-Version") != "3.17.2" {
			t.Fatal("handshake used wrong origin or application identity")
		}
		data, err := httputil.ReadBody(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		if json.Unmarshal(data, &body) != nil || len(body) != 4 || body["apiKey"] != credential || body["accessToken"] != "" {
			t.Fatal("wrong API-key handshake fields")
		}
		key, _ := hex.DecodeString("a749a0140477e49397c13227b121ab1c64bc6bb3f1277f6c232f05f384bd2adb")
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte("get_sign_key\nfixture-id\n" + body["ts"] + "\n" + body["nonce"]))
		if !hmac.Equal([]byte(body["sig"]), []byte(base64.StdEncoding.EncodeToString(mac.Sum(nil)))) {
			t.Fatal("invalid handshake proof")
		}
		return signingResponse(signingFixtureCipher), nil
	})}
	first, err := s.Headers(context.Background(), credential, "cq-session")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Headers(context.Background(), credential, "cq-session")
	if err != nil || calls != 1 || first.Get("X-Client-Nonce") == second.Get("X-Client-Nonce") {
		t.Fatal("key cache failed or per-attempt nonce was reused")
	}
	public, _ := hex.DecodeString(signingFixturePublic)
	verifySigningHeaders(t, first, "fixture-id", "cq-session", public)
	verifySigningHeaders(t, second, "fixture-id", "cq-session", public)
	encoded, err := json.Marshal(&s)
	if err != nil || strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), signingFixtureCipher) || strings.Contains(string(encoded), signingFixturePublic) {
		t.Fatal("signer JSON exposed cached key material")
	}
	s.Invalidate(credential)
	if _, err := s.Headers(context.Background(), credential, "cq-session"); err != nil || calls != 2 {
		t.Fatal("explicit invalidation did not renew the handshake")
	}
}

func fixtureProjectToken(header, payload any) string {
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p) + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
}

func TestSignerProjectAccessTokenContract(t *testing.T) {
	credential := fixtureProjectToken(map[string]string{"alg": "HS256", "typ": "JWT", "sign_type": "SIGN"}, map[string]string{"token_use": "project_access", "api_key": "fixture-id"})
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	s := Signer{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]string
		data, _ := httputil.ReadBody(r.Body)
		if json.Unmarshal(data, &body) != nil || body["accessToken"] != credential || body["apiKey"] != "" || r.Header.Get("Authorization") != credential {
			t.Fatal("project token was confused with ordinary API-key authentication")
		}
		derived, _ := hkdf.Key(sha256.New, []byte(credential), []byte("WD_CLIENT_SIGN_KDF_SALT"), "getSignKey_hmac", 32)
		mac := hmac.New(sha256.New, derived)
		_, _ = mac.Write([]byte("get_sign_key\nfixture-id\n" + body["ts"] + "\n" + body["nonce"]))
		if body["sig"] != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Fatal("project token did not use the entire token as derivation secret")
		}
		return signingResponse(encryptSigningFixture(t, "fixture-id", credential, private)), nil
	})}
	header, err := s.Headers(context.Background(), credential, "project-session")
	if err != nil {
		t.Fatal(err)
	}
	verifySigningHeaders(t, header, "fixture-id", "project-session", private.Public().(ed25519.PublicKey))
}

func encryptSigningFixture(t *testing.T, id, secret string, private ed25519.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return encryptSigningPlaintext(t, id, secret, []byte(base64.StdEncoding.EncodeToString(der)))
}

func encryptSigningPlaintext(t *testing.T, id, secret string, plain []byte) string {
	t.Helper()
	key, _ := hkdf.Key(sha256.New, []byte(secret), []byte("WD_CLIENT_SIGN_KDF_SALT"), "ed25519_priv", 32)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	return base64.StdEncoding.EncodeToString(append(nonce, gcm.Seal(nil, nonce, plain, []byte(id))...))
}

func TestSignerConcurrentCredentialCacheIsolation(t *testing.T) {
	credentials := []string{"fixture-id.fixture-secret", "other-id.other-secret"}
	private := []ed25519.PrivateKey{ed25519.NewKeyFromSeed(make([]byte, 32)), ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))}
	ciphers := []string{encryptSigningFixture(t, "fixture-id", "fixture-secret", private[0]), encryptSigningFixture(t, "other-id", "other-secret", private[1])}
	var mu sync.Mutex
	counts := make(map[string]int)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	s := Signer{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		credential := r.Header.Get("Authorization")
		mu.Lock()
		counts[credential]++
		mu.Unlock()
		started <- struct{}{}
		<-release
		if credential == credentials[0] {
			return signingResponse(ciphers[0]), nil
		}
		return signingResponse(ciphers[1]), nil
	})}
	type result struct {
		index  int
		header http.Header
		err    error
	}
	results := make(chan result, 24)
	var group sync.WaitGroup
	for i := range 24 {
		group.Add(1)
		go func() {
			defer group.Done()
			defer func() {
				if recover() != nil {
					results <- result{err: errors.New("signing panicked")}
				}
			}()
			index := i % 2
			header, err := s.Headers(context.Background(), credentials[index], "concurrent-session")
			results <- result{index: index, header: header, err: err}
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("distinct credentials did not acquire independent handshakes")
		}
	}
	close(release)
	group.Wait()
	close(results)
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		id := strings.SplitN(credentials[r.index], ".", 2)[0]
		verifySigningHeaders(t, r.header, id, "concurrent-session", private[r.index].Public().(ed25519.PublicKey))
	}
	for _, credential := range credentials {
		if counts[credential] != 1 {
			t.Fatalf("expected one handshake per credential, got %d", counts[credential])
		}
	}
}

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestSignerRejectsInvalidCredentialShapesBeforeNetwork(t *testing.T) {
	goodHeader := map[string]string{"alg": "HS256", "typ": "JWT", "sign_type": "SIGN"}
	goodPayload := map[string]string{"token_use": "project_access", "api_key": "fixture-id"}
	for _, credential := range []string{
		"", "no-dot", ".secret", "id.", "id.secret.more", "id.secret\n", "Bearer id.secret", strings.Repeat("x", 17<<10),
		fixtureProjectToken(map[string]string{"alg": "none", "typ": "JWT", "sign_type": "SIGN"}, goodPayload),
		fixtureProjectToken(map[string]string{"alg": "HS256", "typ": "JWT"}, goodPayload),
		fixtureProjectToken(goodHeader, map[string]string{"token_use": "other", "api_key": "fixture-id"}),
		fixtureProjectToken(goodHeader, map[string]string{"token_use": "project_access"}),
		fixtureProjectToken(goodHeader, map[string]string{"token_use": "project_access", "api_key": "bad\nidentity"}),
	} {
		s := Signer{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid credential reached handshake")
			return nil, nil
		})}
		if _, err := s.Headers(context.Background(), credential, "session"); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid credential accepted or exposed")
		}
	}
}

func TestSignerRejectsMalformedPrivateCipher(t *testing.T) {
	for _, ciphertext := range []string{
		"", "not-base64", signingFixtureCipher + "\n", base64.StdEncoding.EncodeToString(make([]byte, 12)),
		base64.StdEncoding.EncodeToString(make([]byte, 40)),
		encryptSigningPlaintext(t, "wrong-id", "fixture-secret", []byte("not-base64")),
		encryptSigningPlaintext(t, "fixture-id", "fixture-secret", []byte("not-base64")),
		encryptSigningPlaintext(t, "fixture-id", "fixture-secret", []byte(base64.StdEncoding.EncodeToString([]byte("not a private key")))),
	} {
		if _, err := decryptSigningKey("fixture-id", "fixture-secret", ciphertext); err == nil || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), ciphertext) && ciphertext != "" {
			t.Fatal("malformed key cipher accepted or exposed")
		}
	}
}

func TestSignerCancellationDoesNotPoisonCache(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s := Signer{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return signingResponse(signingFixtureCipher), nil
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("signer panicked")
			}
		}()
		_, err := s.Headers(ctx, "fixture-id.fixture-secret", "session")
		done <- err
	}()
	<-started
	// Observe entry into the pending-cache wait before cancelling that waiter.
	waiter, stopWaiter := context.WithCancel(context.Background())
	observed := &observedDoneContext{Context: waiter, observed: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				waiterDone <- errors.New("waiter panicked")
			}
		}()
		_, err := s.Headers(observed, "fixture-id.fixture-secret", "session")
		waiterDone <- err
	}()
	select {
	case <-observed.observed:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never joined pending handshake")
	}
	stopWaiter()
	select {
	case err := <-waiterDone:
		if err == nil || calls.Load() != 1 {
			t.Fatal("cancelled waiter disturbed the active handshake")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled caller received signing headers")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	releaseOnce.Do(func() { close(release) })
	if _, err := s.Headers(context.Background(), "fixture-id.fixture-secret", "session"); err != nil || calls.Load() != 1 {
		t.Fatal("cancelled callers poisoned shared handshake or subsequent cache access")
	}
}

func TestSignerCancelledLeaderDoesNotCancelSurvivingWaiter(t *testing.T) {
	const credential = "fixture-id.fixture-secret"
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s := Signer{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 15*time.Second {
			return nil, errors.New("shared handshake lacks its bounded deadline")
		}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return signingResponse(signingFixtureCipher), nil
		}
	})}
	leader, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderDone := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				leaderDone <- errors.New("leader panicked")
			}
		}()
		_, err := s.Headers(leader, credential, "leader-session")
		leaderDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not start signing handshake")
	}
	observed := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	type result struct {
		headers http.Header
		err     error
	}
	waiterDone := make(chan result, 1)
	go func() {
		defer func() {
			if recover() != nil {
				waiterDone <- result{err: errors.New("waiter panicked")}
			}
		}()
		headers, err := s.Headers(observed, credential, "waiter-session")
		waiterDone <- result{headers: headers, err: err}
	}()
	select {
	case <-observed.observed:
	case <-time.After(2 * time.Second):
		t.Fatal("independent waiter did not join signing handshake")
	}
	cancel()
	select {
	case err := <-leaderDone:
		if err == nil {
			t.Fatal("cancelled leader received signing headers")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled leader kept waiting for shared handshake")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case got := <-waiterDone:
		if got.err != nil {
			t.Fatal("leader cancellation failed independent signing waiter")
		}
		public, _ := hex.DecodeString(signingFixturePublic)
		verifySigningHeaders(t, got.headers, "fixture-id", "waiter-session", ed25519.PublicKey(public))
	case <-time.After(2 * time.Second):
		t.Fatal("independent waiter did not receive shared signing key")
	}
	if _, err := s.Headers(context.Background(), credential, "cached-session"); err != nil || calls.Load() != 1 {
		t.Fatal("shared signing key was not cached after leader cancellation")
	}
}

func TestSignerInvalidationDoesNotRestoreOldHandshake(t *testing.T) {
	for _, oldFailed := range []bool{false, true} {
		const credential = "fixture-id.fixture-secret"
		newPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
		newCipher := encryptSigningFixture(t, "fixture-id", "fixture-secret", newPrivate)
		var calls atomic.Int32
		started, release := make(chan struct{}), make(chan struct{})
		s := Signer{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
				if oldFailed {
					return nil, errors.New("fixture-secret")
				}
				return signingResponse(signingFixtureCipher), nil
			}
			return signingResponse(newCipher), nil
		})}
		oldDone := make(chan error, 1)
		go func() {
			defer func() {
				if recover() != nil {
					oldDone <- errors.New("old handshake panicked")
				}
			}()
			_, err := s.Headers(context.Background(), credential, "session")
			oldDone <- err
		}()
		<-started
		s.Invalidate(credential)
		if _, err := s.Headers(context.Background(), credential, "session"); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-oldDone; oldFailed != (err != nil) {
			t.Fatal("old handshake outcome changed")
		}
		header, err := s.Headers(context.Background(), credential, "session")
		if err != nil || calls.Load() != 2 {
			t.Fatal("old handshake replaced or deleted the new cache entry")
		}
		verifySigningHeaders(t, header, "fixture-id", "session", newPrivate.Public().(ed25519.PublicKey))
	}
}

func TestSignerPanicDoesNotLeavePendingCache(t *testing.T) {
	calls := 0
	s := Signer{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			panic("fixture-id.fixture-secret")
		}
		return signingResponse(signingFixtureCipher), nil
	})}
	if _, err := s.Headers(context.Background(), "fixture-id.fixture-secret", "session"); err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("panic was not classified safely")
	}
	if _, err := s.Headers(context.Background(), "fixture-id.fixture-secret", "session"); err != nil || calls != 2 {
		t.Fatal("panic left a permanently pending cache entry")
	}
}

func TestSignerHandshakeFailuresRedactedAndRetryable(t *testing.T) {
	for _, response := range []*http.Response{
		jsonResponse(`{"code":401,"msg":"fixture-id.fixture-secret"}`),
		jsonResponse(`{"code":"200","data":{"privateCipher":"fixture-id.fixture-secret"}}`),
		jsonResponse(`{"code":200,"data":{}}`),
		jsonResponse(strings.Repeat("x", httputil.MaxResponseBody+1)),
		{StatusCode: http.StatusOK, Body: io.NopCloser(failureReader{}), Header: http.Header{}},
	} {
		calls := 0
		s := Signer{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return response, nil
			}
			return signingResponse(signingFixtureCipher), nil
		})}
		if _, err := s.Headers(context.Background(), "fixture-id.fixture-secret", "session"); err == nil || strings.Contains(err.Error(), "fixture-id") || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatal("failed handshake accepted or exposed credentials")
		}
		if _, err := s.Headers(context.Background(), "fixture-id.fixture-secret", "session"); err != nil || calls != 2 {
			t.Fatal("failed handshake remained cached")
		}
	}
}

func TestSignerNeverFollowsHandshakeRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls != 1 {
			t.Fatal("signing credentials followed a redirect")
		}
		response := jsonResponse("")
		response.StatusCode = http.StatusTemporaryRedirect
		response.Header.Set("Location", "https://other.example/key")
		return response, nil
	})}
	s := Signer{Version: "3.17.2", HTTP: client}
	if _, err := s.Headers(context.Background(), "fixture-id.fixture-secret", "session"); err == nil || calls != 1 || client.CheckRedirect != nil {
		t.Fatal("redirect did not fail closed without changing caller client")
	}
}
