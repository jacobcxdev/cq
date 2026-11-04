package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// SessionStore owns fresh browser logins under CQ's private configuration tree.
// CipherSecret is supplied by the caller and is excluded from public output.
type SessionStore struct {
	FS           fsutil.FileSystem
	Path         string
	CipherSecret string `json:"-"`
}

type storedLoginSession struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Email       string `json:"email,omitempty"`
	Name        string `json:"name,omitempty"`
	Label       string `json:"label,omitempty"`
	Credentials string `json:"credentials"`
}

type loginSecrets struct {
	UserID            string `json:"user_id"`
	ZCodeJWT          string `json:"zcode_jwt"`
	RawOAuth          string `json:"oauth_access_token"`
	BusinessJWT       string `json:"business_jwt"`
	RefreshToken      string `json:"refresh_token,omitempty"`
	BusinessExpiresAt int64  `json:"business_expires_at,omitempty"`
	DeviceMid         string `json:"device_mid,omitempty"`
}

type loginSessionFile struct {
	SchemaVersion int                  `json:"schema_version"`
	Accounts      []storedLoginSession `json:"accounts"`
}

func (s SessionStore) Accounts() ([]LoginSession, error) {
	if s.FS == nil || s.Path == "" || s.CipherSecret == "" {
		return nil, errors.New("ZCode session store unavailable")
	}
	data, err := s.FS.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("read CQ ZCode sessions")
	}
	if len(data) > httputil.MaxResponseBody {
		return nil, errors.New("invalid CQ ZCode sessions")
	}
	var file loginSessionFile
	if json.Unmarshal(data, &file) != nil || file.SchemaVersion != 1 || file.Accounts == nil {
		return nil, errors.New("invalid CQ ZCode sessions")
	}
	accounts := make([]LoginSession, 0, len(file.Accounts))
	seen := make(map[string]bool)
	for _, stored := range file.Accounts {
		account := LoginSession{ID: stored.ID, UserID: stored.UserID, Email: stored.Email, Name: stored.Name, Label: stored.Label}
		if !validLoginIdentity(account) || stored.ID != loginAccountID(stored.UserID) || seen[stored.ID] {
			return nil, errors.New("invalid CQ ZCode sessions")
		}
		seen[stored.ID] = true
		plain, err := s.decrypt(stored.ID, stored.Credentials)
		if err != nil {
			return nil, err
		}
		var secrets loginSecrets
		err = json.Unmarshal(plain, &secrets)
		clear(plain)
		if err != nil || secrets.UserID != stored.UserID || !validLoginSecrets(secrets) {
			return nil, errors.New("invalid CQ ZCode session credentials")
		}
		account.ZCodeJWT, account.RawOAuth, account.BusinessJWT, account.RefreshToken = secrets.ZCodeJWT, secrets.RawOAuth, secrets.BusinessJWT, secrets.RefreshToken
		account.BusinessExpiresAt = secrets.BusinessExpiresAt
		account.DeviceMid = secrets.DeviceMid
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func (s SessionStore) Save(session LoginSession, label string, activate bool) (LoginSession, error) {
	label = strings.TrimSpace(label)
	if !validLoginIdentity(session) || !validProfileText(label, 128) || !validLoginSecrets(loginSecrets{UserID: session.UserID, ZCodeJWT: session.ZCodeJWT, RawOAuth: session.RawOAuth, BusinessJWT: session.BusinessJWT, RefreshToken: session.RefreshToken, BusinessExpiresAt: session.BusinessExpiresAt}) {
		return LoginSession{}, errors.New("invalid ZCode login session")
	}
	accounts, err := s.Accounts()
	if err != nil {
		return LoginSession{}, err
	}
	session.ID = loginAccountID(session.UserID)
	index := -1
	for i, account := range accounts {
		if account.ID == session.ID {
			index = i
			if label == "" {
				label = account.Label
			}
			break
		}
	}
	if label == "" {
		label = session.Label
	}
	if label == "" {
		label = session.Email
	}
	if !validProfileText(label, 128) {
		return LoginSession{}, errors.New("invalid ZCode account label")
	}
	session.Label = label
	if index < 0 {
		accounts = append(accounts, session)
		index = len(accounts) - 1
	} else {
		accounts[index] = session
	}
	if activate && index > 0 {
		copy(accounts[1:index+1], accounts[:index])
		accounts[0] = session
	}
	if err := s.write(accounts); err != nil {
		return LoginSession{}, err
	}
	return session, nil
}

func (s SessionStore) SetActive(reference string) error {
	accounts, err := s.Accounts()
	if err != nil {
		return err
	}
	index, err := loginSessionIndex(accounts, reference)
	if err != nil {
		return err
	}
	account := accounts[index]
	copy(accounts[1:index+1], accounts[:index])
	accounts[0] = account
	return s.write(accounts)
}

func (s SessionStore) Remove(reference string) error {
	accounts, err := s.Accounts()
	if err != nil {
		return err
	}
	index, err := loginSessionIndex(accounts, reference)
	if err != nil {
		return err
	}
	return s.write(append(accounts[:index], accounts[index+1:]...))
}

func (s SessionStore) write(accounts []LoginSession) error {
	file := loginSessionFile{SchemaVersion: 1, Accounts: make([]storedLoginSession, 0, len(accounts))}
	for _, account := range accounts {
		plain, _ := json.Marshal(loginSecrets{UserID: account.UserID, ZCodeJWT: account.ZCodeJWT, RawOAuth: account.RawOAuth, BusinessJWT: account.BusinessJWT, RefreshToken: account.RefreshToken, BusinessExpiresAt: account.BusinessExpiresAt, DeviceMid: account.DeviceMid})
		encrypted, err := s.encrypt(account.ID, plain)
		clear(plain)
		if err != nil {
			return err
		}
		file.Accounts = append(file.Accounts, storedLoginSession{ID: account.ID, UserID: account.UserID, Email: account.Email, Name: account.Name, Label: account.Label, Credentials: encrypted})
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return errors.New("encode CQ ZCode sessions")
	}
	if err := fsutil.SecureAtomicWrite(s.FS, s.Path, append(data, '\n')); err != nil {
		return errors.New("save CQ ZCode sessions")
	}
	return nil
}

func (s SessionStore) aead() (cipher.AEAD, error) {
	if s.CipherSecret == "" {
		return nil, errors.New("CQ ZCode session cipher unavailable")
	}
	key := sha256.Sum256([]byte(s.CipherSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, errors.New("CQ ZCode session cipher unavailable")
	}
	return cipher.NewGCM(block)
}

func (s SessionStore) encrypt(id string, plain []byte) (string, error) {
	aead, err := s.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("encrypt CQ ZCode session")
	}
	sealed := aead.Seal(nil, nonce, plain, []byte("cq-zcode-session-v1:"+id))
	return "cqenc:v1:" + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func (s SessionStore) decrypt(id, value string) ([]byte, error) {
	invalid := errors.New("decrypt CQ ZCode session")
	if !strings.HasPrefix(value, "cqenc:v1:") {
		return nil, invalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(value, "cqenc:v1:"))
	if err != nil {
		return nil, invalid
	}
	aead, err := s.aead()
	if err != nil || len(data) < aead.NonceSize()+aead.Overhead() {
		return nil, invalid
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("cq-zcode-session-v1:"+id))
	if err != nil {
		return nil, invalid
	}
	return plain, nil
}

func validLoginSecrets(s loginSecrets) bool {
	return validHeaderValue(s.UserID, 512) && validHeaderValue(s.ZCodeJWT, 16<<10) && validHeaderValue(s.RawOAuth, 16<<10) && validHeaderValue(s.BusinessJWT, 16<<10) && (s.RefreshToken == "" || validHeaderValue(s.RefreshToken, 16<<10)) && s.BusinessExpiresAt >= 0
}

func loginSessionIndex(accounts []LoginSession, reference string) (int, error) {
	for i, account := range accounts {
		if account.ID == reference {
			return i, nil
		}
	}
	index := -1
	for i, account := range accounts {
		if reference != "" && account.Label == reference {
			if index >= 0 {
				return 0, errors.New("ambiguous ZCode account label; use account ID")
			}
			index = i
		}
	}
	if index < 0 {
		return 0, errors.New("ZCode account not found")
	}
	return index, nil
}
