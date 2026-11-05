// Read-only installed-session inspection is qualification tooling only.
package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// SessionReader reads only the active Z.ai login from ZCode's credential file.
// CipherSecret is injected; the caller owns platform-specific secret discovery.
type SessionReader struct {
	FS           fsutil.FileSystem
	Path         string
	CipherSecret string `json:"-"`
}

func (r SessionReader) Read() (Session, error) {
	if r.FS == nil || r.Path == "" {
		return Session{}, errors.New("ZCode credential reader unavailable")
	}
	data, err := r.FS.ReadFile(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, ErrNotConfigured
	}
	if err != nil {
		return Session{}, errors.New("read ZCode credentials")
	}
	var stored map[string]string
	if len(data) > httputil.MaxResponseBody || json.Unmarshal(data, &stored) != nil || stored == nil {
		return Session{}, errors.New("invalid ZCode credential file")
	}
	family, err := decryptCredential(stored["oauth:active_provider"], r.CipherSecret)
	if err != nil {
		return Session{}, err
	}
	if family == "" {
		return Session{}, ErrNotConfigured
	}
	if family != "zai" {
		return Session{}, errors.New("ZCode active login is not Z.ai")
	}
	zcodeRaw, businessRaw := stored["zcodejwttoken"], stored["oauth:zai:access_token"]
	zcodeJWT, err := decryptCredential(zcodeRaw, r.CipherSecret)
	if err != nil {
		return Session{}, err
	}
	businessJWT, err := decryptCredential(businessRaw, r.CipherSecret)
	if err != nil {
		return Session{}, err
	}
	if zcodeJWT == "" || businessJWT == "" {
		return Session{}, ErrNotConfigured
	}
	if !validHeaderValue(zcodeJWT, 16<<10) || !validHeaderValue(businessJWT, 16<<10) {
		return Session{}, errors.New("invalid ZCode session credentials")
	}
	return Session{
		Family: family, ZCodeJWT: zcodeJWT, BusinessJWT: businessJWT,
		PlaintextCredentials: !strings.HasPrefix(zcodeRaw, "enc:v1:") || !strings.HasPrefix(businessRaw, "enc:v1:"),
	}, nil
}

// ZCode's public credentialCipherProvider format is AES-256-GCM with a
// SHA-256(secret) key and enc:v1:<base64url(iv)>.<tag>.<ciphertext> framing.
func decryptCredential(value, secret string) (string, error) {
	invalid := errors.New("decrypt ZCode credential")
	if !strings.HasPrefix(value, "enc:v1:") {
		if strings.HasPrefix(value, "enc:") {
			return "", invalid
		}
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, "enc:v1:"), ".")
	if secret == "" || len(parts) != 3 {
		return "", invalid
	}
	encoding := base64.RawURLEncoding.Strict()
	iv, err := encoding.DecodeString(parts[0])
	if err != nil || len(iv) != 12 {
		return "", invalid
	}
	tag, err := encoding.DecodeString(parts[1])
	if err != nil || len(tag) != 16 {
		return "", invalid
	}
	encrypted, err := encoding.DecodeString(parts[2])
	if err != nil {
		return "", invalid
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", invalid
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", invalid
	}
	plain, err := gcm.Open(nil, iv, append(encrypted, tag...), nil)
	if err != nil {
		return "", invalid
	}
	defer clear(plain)
	return string(plain), nil
}
