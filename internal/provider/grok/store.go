package grok

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// Store holds only CQ-owned Grok logins; it never writes native application state.
type Store struct {
	FS        fsutil.FileSystem
	ConfigDir string
	directory fsutil.SecureDirectory
	inspector fsutil.SecurePathInspector
	fence     func() error
}
type privateTokens struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token,omitempty"`
	ID      string    `json:"id_token,omitempty"`
	Expiry  time.Time `json:"expires_at"`
}
type storedOperation struct {
	Attempted      bool           `json:"attempted"`
	ResultRecorded bool           `json:"result_recorded"`
	Result         *privateTokens `json:"result,omitempty"`
	Failed         bool           `json:"failed,omitempty"`
	Committed      bool           `json:"committed,omitempty"`
}
type storedAccount struct {
	Account    Account          `json:"account"`
	Generation string           `json:"generation"`
	Tokens     privateTokens    `json:"tokens"`
	Operation  *storedOperation `json:"operation,omitempty"`
}
type storeData struct {
	Accounts []storedAccount `json:"accounts"`
}
type storeEnvelope struct {
	Version    int    `json:"version"`
	Ciphertext string `json:"ciphertext"`
}

const storeFilename = "grok-accounts.json"
const keyFilename = "grok-accounts.key"

func (s Store) read() (storeData, error) {
	if s.FS == nil || s.ConfigDir == "" {
		return storeData{}, errors.New("Grok credential store unavailable")
	}
	data, err := s.readFile(storeFilename, httputil.MaxResponseBody)
	if errors.Is(err, os.ErrNotExist) {
		return storeData{Accounts: []storedAccount{}}, nil
	}
	if err != nil {
		return storeData{}, errors.New("read CQ Grok accounts")
	}
	var envelope storeEnvelope
	if json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 {
		return storeData{}, errors.New("invalid CQ Grok accounts")
	}
	aead, err := s.aead(false)
	if err != nil {
		return storeData{}, err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Ciphertext)
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return storeData{}, errors.New("invalid CQ Grok account encryption")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte("cq:grok:accounts:v1"))
	if err != nil {
		return storeData{}, errors.New("invalid CQ Grok account encryption")
	}
	defer clear(plain)
	var records storeData
	if json.Unmarshal(plain, &records) != nil || records.Accounts == nil {
		return storeData{}, errors.New("invalid CQ Grok accounts")
	}
	seen := map[AccountID]bool{}
	for _, record := range records.Accounts {
		if !validAccount(record.Account) || record.Account.ID != accountID(record.Account) || record.Generation == "" || seen[record.Account.ID] || !validToken(record.Tokens.Access) || record.Tokens.Refresh != "" && !validToken(record.Tokens.Refresh) || record.Tokens.ID != "" && !validToken(record.Tokens.ID) || record.Tokens.Expiry.IsZero() {
			return storeData{}, errors.New("invalid CQ Grok account")
		}
		seen[record.Account.ID] = true
	}
	return records, nil
}
func (s Store) aead(create bool) (cipher.AEAD, error) {
	key, err := s.readFile(keyFilename, 128)
	if errors.Is(err, os.ErrNotExist) && create {
		if _, e := s.FS.Stat(filepath.Join(s.ConfigDir, storeFilename)); !errors.Is(e, os.ErrNotExist) {
			return nil, errors.New("Grok account key unavailable")
		}
		material := make([]byte, 32)
		if _, err := rand.Read(material); err != nil {
			return nil, errors.New("create Grok account key")
		}
		key = []byte(base64.RawURLEncoding.EncodeToString(material))
		clear(material)
		if err := s.writeFile(keyFilename, key); err != nil {
			return nil, errors.New("save Grok account key")
		}
	} else if err != nil {
		return nil, errors.New("read Grok account key")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(string(key))
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("invalid Grok account key")
	}
	defer clear(decoded)
	block, err := aes.NewCipher(decoded)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s Store) write(records storeData) error {
	aead, err := s.aead(true)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(records)
	if err != nil {
		return errors.New("encode Grok accounts")
	}
	defer clear(plain)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("create Grok account nonce")
	}
	sealed := aead.Seal(append([]byte(nil), nonce...), nonce, plain, []byte("cq:grok:accounts:v1"))
	data, _ := json.Marshal(storeEnvelope{Version: 1, Ciphertext: base64.RawURLEncoding.EncodeToString(sealed)})
	if err := s.writeFile(storeFilename, data); err != nil {
		return errors.New("save CQ Grok accounts")
	}
	return nil
}

type storeLock struct {
	fsutil.ExclusiveLock
	directory fsutil.SecureDirectory
	store     Store
}

func (l *storeLock) Close() error { return errors.Join(l.ExclusiveLock.Close(), l.directory.Close()) }
func (s Store) readFile(name string, maxBytes int64) ([]byte, error) {
	if s.directory != nil {
		if err := s.fence(); err != nil {
			return nil, err
		}
		return fsutil.ReadSecureFileInDirectory(s.inspector, s.directory, name, maxBytes)
	}
	path := filepath.Join(s.ConfigDir, name)
	inspector, ok := s.FS.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	// Optional discovery must recognise an absent entry before applying the
	// stricter private-directory contract needed to read existing credentials.
	if _, err := inspector.Lstat(path); err != nil {
		return nil, err
	}
	return fsutil.ReadSecureFile(s.FS, path, maxBytes)
}
func (s Store) writeFile(name string, data []byte) error {
	if s.directory == nil {
		return errors.New("Grok credential mutation requires owner lock")
	}
	return fsutil.SecureAtomicWriteInDirectoryChecked(s.inspector, s.directory, name, data, s.fence)
}
func (s Store) lock(ctx context.Context) (*storeLock, error) {
	if s.FS == nil || s.ConfigDir == "" || !filepath.IsAbs(s.ConfigDir) {
		return nil, errors.New("Grok credential store unavailable")
	}
	if err := fsutil.EnsureSecureDirectory(s.FS, s.ConfigDir); err != nil {
		return nil, err
	}
	inspector, ok := s.FS.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := s.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(s.ConfigDir)
	if err != nil {
		return nil, err
	}
	closeError := func(err error) (*storeLock, error) { directory.Close(); return nil, err }
	for {
		if err := ctx.Err(); err != nil {
			return closeError(err)
		}
		lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, "grok-accounts.lock")
		if err == nil {
			info, err := lock.Stat()
			if err != nil {
				lock.Close()
				return closeError(err)
			}
			identity, ok := inspector.FileIdentity(info)
			if !ok {
				lock.Close()
				return closeError(fsutil.ErrSecureCapabilityUnavailable)
			}
			s.directory, s.inspector = directory, inspector
			s.fence = func() error {
				if err := fsutil.ValidateSecureDirectoryHandle(inspector, directory, s.ConfigDir); err != nil {
					return err
				}
				return fsutil.ValidateExclusiveLockHeldInDirectory(inspector, directory, "grok-accounts.lock", identity)
			}
			if err := s.fence(); err != nil {
				lock.Close()
				return closeError(err)
			}
			return &storeLock{ExclusiveLock: lock, directory: directory, store: s}, nil
		}
		if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
			return closeError(err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return closeError(ctx.Err())
		case <-timer.C:
		}
	}
}

func findAccount(records storeData, reference string) (int, error) {
	index := -1
	for i, record := range records.Accounts {
		if string(record.Account.ID) == reference || record.Account.Label == reference || record.Account.Email == reference {
			if index != -1 {
				return -1, errors.New("ambiguous Grok account reference")
			}
			index = i
		}
	}
	if index < 0 {
		return -1, errors.New("Grok account not found")
	}
	return index, nil
}
func (a *SessionAuthority) SaveLogin(ctx context.Context, login Login, label string, activate bool) (Account, error) {
	if !validAccount(login.Account) || !validToken(login.AccessToken) || login.RefreshToken != "" && !validToken(login.RefreshToken) || login.IDToken != "" && !validToken(login.IDToken) || login.ExpiresAt.IsZero() || !validText(strings.TrimSpace(label), 128) {
		return Account{}, errors.New("invalid Grok login")
	}
	lock, err := a.Store.lock(ctx)
	if err != nil {
		return Account{}, err
	}
	defer lock.Close()
	records, err := lock.store.read()
	if err != nil {
		return Account{}, err
	}
	account := login.Account
	account.Source = "cq_oauth"
	account.ReportingOnly = false
	account.ID = accountID(account)
	account.Label = strings.TrimSpace(label)
	account.Active = false
	if account.Label == "" {
		account.Label = account.Email
	}
	generation := make([]byte, 16)
	if _, err := rand.Read(generation); err != nil {
		return Account{}, errors.New("create Grok credential generation")
	}
	record := storedAccount{Account: account, Generation: base64.RawURLEncoding.EncodeToString(generation), Tokens: privateTokens{Access: login.AccessToken, Refresh: login.RefreshToken, ID: login.IDToken, Expiry: login.ExpiresAt}}
	index := -1
	for i, old := range records.Accounts {
		if old.Account.ID == account.ID {
			index = i
			break
		}
	}
	if index < 0 {
		records.Accounts = append(records.Accounts, record)
		index = len(records.Accounts) - 1
	} else {
		records.Accounts[index] = record
	}
	if activate && index > 0 {
		copy(records.Accounts[1:index+1], records.Accounts[:index])
		records.Accounts[0] = record
	}
	if err := lock.store.write(records); err != nil {
		return Account{}, err
	}
	a.retainedMu.Lock()
	delete(a.retained, account.ID)
	a.retainedMu.Unlock()
	account.Active = activate || index == 0
	return account, nil
}
func (a *SessionAuthority) SetPreferred(ctx context.Context, reference string) error {
	lock, err := a.Store.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	records, err := lock.store.read()
	if err != nil {
		return err
	}
	index, err := findAccount(records, reference)
	if err != nil {
		return err
	}
	record := records.Accounts[index]
	copy(records.Accounts[1:index+1], records.Accounts[:index])
	records.Accounts[0] = record
	return lock.store.write(records)
}
func (a *SessionAuthority) Remove(ctx context.Context, reference string) error {
	lock, err := a.Store.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	records, err := lock.store.read()
	if err != nil {
		return err
	}
	index, err := findAccount(records, reference)
	if err != nil {
		return err
	}
	removed := records.Accounts[index].Account.ID
	records.Accounts = append(records.Accounts[:index], records.Accounts[index+1:]...)
	if err := lock.store.write(records); err != nil {
		return err
	}
	a.retainedMu.Lock()
	delete(a.retained, removed)
	a.retainedMu.Unlock()
	return nil
}
