package dots

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// NativeGate verifies the native principal before substituting an owner's credentials.
type NativeGate struct {
	settings *Settings
	mu       sync.Mutex
	cached   [32]byte
	expires  time.Time
	now      func() time.Time
	load     func() (Account, error)
	get      func(context.Context, Account, string, http.Header) (map[string]any, error)
}

func NewNativeGate(s *Settings, c *CatalogueClient) *NativeGate {
	g := &NativeGate{settings: s, now: time.Now}
	g.load = func() (Account, error) {
		data, err := ReadOwned(s.SystemAuth, "")
		if err != nil {
			return Account{}, err
		}
		return ParseCredential(data, c.now())
	}
	g.get = c.Get
	return g
}
func (g *NativeGate) Require(ctx context.Context, headers http.Header) error {
	token := headers.Get("Authorization")
	account := headers.Get("ChatGPT-Account-Id")
	if !strings.HasPrefix(token, "Bearer ") || len(token) <= 8 || len(token) > 16384 || len(account) <= 1 || len(account) > 256 {
		return errors.New("native authentication required")
	}
	fingerprint := sha256.Sum256([]byte(account + "\x00" + token))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.now().Before(g.expires) && subtle.ConstantTimeCompare(fingerprint[:], g.cached[:]) == 1 {
		return nil
	}
	system, err := g.load()
	if err != nil {
		return err
	}
	if system.Alias != g.settings.SystemAlias || account != system.AccountID {
		return errors.New("native principal mismatch")
	}
	if g.settings.IntegrationMode == "native" {
		if subtle.ConstantTimeCompare([]byte(token[7:]), []byte(system.Token)) != 1 {
			return errors.New("native bearer mismatch")
		}
	} else {
		system.Token = token[7:]
		value, err := g.get(ctx, system, "/backend-api/tbo/primary", headers)
		if err != nil {
			return err
		}
		profile, _ := value["profile"].(map[string]any)
		selection, _ := value["selection"].(map[string]any)
		root, ok := profile["active_root_thread_id"].(string)
		if profile["id"] != g.settings.ProfileIDs()[system.Alias] || selection["available"] != true || !ok || selection["thread_id"] != root {
			return errors.New("native principal verification failed")
		}
	}
	g.cached = fingerprint
	g.expires = g.now().Add(time.Minute)
	return nil
}

// ValidateNativeRegistration permits reusing only the existing secondary registration.
func ValidateNativeRegistration(s *Settings, headers http.Header, body []byte, system Account, proof map[string]any, currentRoot string) (map[string]any, error) {
	environment, _ := proof["environment_id"].(string)
	if system.Alias != s.SystemAlias || headers.Get("ChatGPT-Account-Id") != system.AccountID || subtle.ConstantTimeCompare([]byte(headers.Get("Authorization")), []byte("Bearer "+system.Token)) != 1 || proof["owner_alias"] != s.SecondaryAlias || proof["registration_accepted"] != true || proof["root_thread_id"] != currentRoot || environment == "" {
		return nil, errors.New("isolated native executor identity mismatch")
	}
	var value map[string]any
	if len(body) > nativeMaxRequest || json.Unmarshal(body, &value) != nil || len(value) != 3 {
		return nil, errors.New("native registration body invalid")
	}
	_, display := value["displayName"].(string)
	_, roots := value["selected_capability_roots"].([]any)
	id, idOK := value["installation_id"].(string)
	if !display || !roots || !idOK || !nativeValidUUID(id) {
		return nil, errors.New("native registration body invalid")
	}
	return value, nil
}
func nativeValidUUID(value string) bool {
	value = strings.TrimPrefix(value, "urn:uuid:")
	value = strings.TrimPrefix(strings.TrimSuffix(value, "}"), "{")
	value = strings.ReplaceAll(value, "-", "")
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
