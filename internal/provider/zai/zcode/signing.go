package zcode

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const signingClientURL = "https://api.z.ai/api/paas/c1f3a7e2/v2/client"
const signingSalt = "WD_CLIENT_SIGN_KDF_SALT"

// Signer obtains an account's own Ed25519 key through authenticated challenge
// exchange. Keys stay in memory; failed handshakes are not cached. Version is
// the caller-selected ZCode protocol version. Do not copy after first use.
type Signer struct {
	HTTP    httputil.Doer `json:"-"`
	Version string        `json:"version"`

	mu      sync.Mutex
	entries map[[32]byte]*signingEntry
}

type signingEntry struct {
	done chan struct{}
	key  ed25519.PrivateKey
	err  error
}

type signingCredential struct {
	id, secret string
	project    bool
}

// Headers generates a fresh signature and proof for one caller-owned session.
// It does not send inference, set inference Authorization, or retry model calls.
func (s *Signer) Headers(ctx context.Context, credential, sessionID string) (http.Header, error) {
	if s == nil || s.HTTP == nil || !validHeaderValue(s.Version, 80) || !validHeaderValue(sessionID, 256) {
		return nil, &Error{Code: "signing_unavailable"}
	}
	if ctx.Err() != nil {
		return nil, &Error{Code: "signing_cancelled"}
	}
	parsed, err := parseSigningCredential(credential)
	if err != nil {
		return nil, err
	}
	key, err := s.signingKey(ctx, credential, parsed)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, &Error{Code: "signing_cancelled"}
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce, err := signingNonce(16)
	if err != nil {
		return nil, err
	}
	proof, err := signingProof(ctx, parsed.id, sessionID, ts)
	if err != nil {
		return nil, err
	}
	message := strings.Join([]string{parsed.id, ts, s.Version, sessionID, nonce}, "\n")
	headers := make(http.Header)
	headers.Set("X-Client-Ts", ts)
	headers.Set("X-Client-Version", s.Version)
	headers.Set("X-Client-Sig", base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(message))))
	headers.Set("X-Session-Id", sessionID)
	headers.Set("X-Client-Nonce", nonce)
	headers.Set("X-App-Id", "zcode")
	headers.Set("X-Client-Pow", proof)
	return headers, nil
}

// Invalidate forgets one cached key. An admitted handshake may finish, but
// cannot repopulate its invalidated entry or replace a newer handshake.
func (s *Signer) Invalidate(credential string) {
	if s == nil {
		return
	}
	digest := sha256.Sum256([]byte(credential))
	s.mu.Lock()
	delete(s.entries, digest)
	s.mu.Unlock()
}

func (s *Signer) signingKey(ctx context.Context, credential string, parsed signingCredential) (ed25519.PrivateKey, error) {
	digest := sha256.Sum256([]byte(credential))
	s.mu.Lock()
	entry := s.entries[digest]
	if entry == nil {
		entry = &signingEntry{done: make(chan struct{})}
		if s.entries == nil {
			s.entries = make(map[[32]byte]*signingEntry)
		}
		s.entries[digest] = entry
		// A shared, bounded handshake belongs to the cache, not its first
		// caller. Cancelling any caller stops only that caller's wait.
		go func() {
			var key ed25519.PrivateKey
			var err error
			defer func() {
				if recover() != nil {
					key, err = nil, &Error{Code: "signing_request_failed"}
				}
				s.mu.Lock()
				defer s.mu.Unlock()
				entry.key, entry.err = key, err
				if err != nil && s.entries[digest] == entry {
					delete(s.entries, digest)
				}
				close(entry.done)
			}()
			key, err = s.fetchSigningKey(context.Background(), credential, parsed)
		}()
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, &Error{Code: "signing_cancelled"}
	case <-entry.done:
		return entry.key, entry.err
	}
}

func (s *Signer) fetchSigningKey(ctx context.Context, credential string, parsed signingCredential) (key ed25519.PrivateKey, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	nonce, err := signingNonce(16)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac, err := signingHandshakeMAC(parsed.id, parsed.secret, ts, nonce)
	if err != nil {
		return nil, err
	}
	field := "apiKey"
	if parsed.project {
		field = "accessToken"
	}
	body, err := json.Marshal(map[string]string{field: credential, "nonce": nonce, "ts": ts, "sig": mac})
	if err != nil {
		return nil, &Error{Code: "signing_invalid_request"}
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signingClientURL, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Code: "signing_invalid_request"}
	}
	req.Header.Set("Authorization", credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	SetSourceHeaders(req.Header, s.Version)
	req.Header.Set("X-App-Id", "zcode")
	req.Header.Set("X-Client-Version", s.Version)
	doer := s.HTTP
	if client, ok := doer.(*http.Client); ok {
		private := *client
		private.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &private
	}
	response, err := doer.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, &Error{Code: "signing_request_failed"}
	}
	if response == nil || response.Body == nil {
		return nil, &Error{Code: "signing_invalid_response"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &Error{Code: "signing_http_error", HTTPStatus: response.StatusCode}
	}
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, &Error{Code: "signing_read_failed"}
	}
	var envelope struct {
		Code    *int  `json:"code"`
		Success *bool `json:"success"`
		Data    *struct {
			PrivateCipher string `json:"privateCipher"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Code == nil || *envelope.Code != 200 || envelope.Success != nil && !*envelope.Success || envelope.Data == nil {
		return nil, &Error{Code: "signing_invalid_response"}
	}
	return decryptSigningKey(parsed.id, parsed.secret, envelope.Data.PrivateCipher)
}

func parseSigningCredential(credential string) (signingCredential, error) {
	invalid := &Error{Code: "signing_invalid_credential"}
	if !validHeaderValue(credential, 16<<10) {
		return signingCredential{}, invalid
	}
	parts := strings.Split(credential, ".")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return signingCredential{id: parts[0], secret: parts[1]}, nil
	}
	if len(parts) != 3 {
		return signingCredential{}, invalid
	}
	header, err := strictBase64(parts[0], base64.RawURLEncoding)
	if err != nil {
		return signingCredential{}, invalid
	}
	payload, err := strictBase64(parts[1], base64.RawURLEncoding)
	if err != nil {
		return signingCredential{}, invalid
	}
	signature, err := strictBase64(parts[2], base64.RawURLEncoding)
	if err != nil || len(signature) != sha256.Size {
		return signingCredential{}, invalid
	}
	var h struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		SignType  string `json:"sign_type"`
	}
	var p struct {
		TokenUse string `json:"token_use"`
		APIKey   string `json:"api_key"`
	}
	if json.Unmarshal(header, &h) != nil || json.Unmarshal(payload, &p) != nil || h.Algorithm != "HS256" || h.Type != "JWT" || h.SignType != "SIGN" || p.TokenUse != "project_access" || !validHeaderValue(p.APIKey, 256) {
		return signingCredential{}, invalid
	}
	return signingCredential{id: p.APIKey, secret: credential, project: true}, nil
}

func signingDerivedKey(secret, info string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, []byte(secret), []byte(signingSalt), info, 32)
	if err != nil {
		return nil, &Error{Code: "signing_key_derivation_failed"}
	}
	return key, nil
}

func signingHandshakeMAC(id, secret, ts, nonce string) (string, error) {
	key, err := signingDerivedKey(secret, "getSignKey_hmac")
	if err != nil {
		return "", err
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(strings.Join([]string{"get_sign_key", id, ts, nonce}, "\n")))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decryptSigningKey(id, secret, encoded string) (ed25519.PrivateKey, error) {
	invalid := &Error{Code: "signing_invalid_key"}
	encrypted, err := strictBase64(encoded, base64.StdEncoding)
	if err != nil || len(encrypted) < 12+16 {
		return nil, invalid
	}
	key, err := signingDerivedKey(secret, "ed25519_priv")
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, invalid
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, invalid
	}
	plain, err := gcm.Open(nil, encrypted[:12], encrypted[12:], []byte(id))
	if err != nil {
		return nil, invalid
	}
	defer clear(plain)
	der, err := strictBase64(string(plain), base64.StdEncoding)
	if err != nil {
		return nil, invalid
	}
	defer clear(der)
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, invalid
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(private) != ed25519.PrivateKeySize {
		return nil, invalid
	}
	return private, nil
}

func strictBase64(value string, encoding *base64.Encoding) ([]byte, error) {
	if value == "" || len(value) > httputil.MaxResponseBody {
		return nil, &Error{Code: "signing_invalid_encoding"}
	}
	decoded, err := encoding.Strict().DecodeString(value)
	if err != nil || encoding.EncodeToString(decoded) != value {
		return nil, &Error{Code: "signing_invalid_encoding"}
	}
	return decoded, nil
}

func signingNonce(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", &Error{Code: "signing_entropy_unavailable"}
	}
	return hex.EncodeToString(data), nil
}

func signingProof(ctx context.Context, id, session, ts string) (string, error) {
	seedHash := sha256.Sum256([]byte(strings.Join([]string{id, "zcode", session, ts}, "\n")))
	seed := hex.EncodeToString(seedHash[:])[:32]
	prefix, err := signingNonce(12)
	if err != nil {
		return "", err
	}
	for counter := uint32(0); ; counter++ {
		if ctx.Err() != nil {
			return "", &Error{Code: "signing_cancelled"}
		}
		candidate := prefix + fmt.Sprintf("%08x", counter)
		digest := sha256.Sum256([]byte(seed + "\n" + candidate))
		if digest[0] == 0 {
			return candidate, nil
		}
	}
}
