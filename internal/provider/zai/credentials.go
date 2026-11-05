package zai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// Account is one subscription credential. API keys are excluded from public JSON.
type Account struct {
	ID        string              `json:"id"`
	Label     string              `json:"label,omitempty"`
	APIKey    string              `json:"-"`
	Transport string              `json:"transport,omitempty"`
	OAuth     *zcode.LoginSession `json:"-"`
}

type storedAccount struct {
	ID             string `json:"id"`
	Label          string `json:"label,omitempty"`
	APIKey         string `json:"api_key"`
	Transport      string `json:"transport,omitempty"`
	ZCodeAccountID string `json:"zcode_account_id,omitempty"`
}

type credentialFile struct {
	// Keep the preferred key readable by older CQ versions during upgrades.
	APIKey   string           `json:"api_key"`
	Accounts *[]storedAccount `json:"accounts,omitempty"`
}

// ReadAPIKey reads ZAI_API_KEY or CQ's private zai.json credential file.
// Environment credentials take precedence. A missing file is not an error.
func ReadAPIKey(fsys fsutil.FileSystem, env func(string) string, configDir string) (string, error) {
	accounts, err := ReadAccounts(fsys, env, configDir)
	if err != nil || len(accounts) == 0 {
		return "", err
	}
	return accounts[0].APIKey, nil
}

// ReadAccounts reads subscriptions in preferred-account order. An environment
// key deliberately overrides all stored accounts, as it did before multi-account support.
func ReadAccounts(fsys fsutil.FileSystem, env func(string) string, configDir string) ([]Account, error) {
	if key := strings.TrimSpace(env("ZAI_API_KEY")); key != "" {
		key, err := validateAPIKey(key)
		if err != nil {
			return nil, err
		}
		return []Account{newAccount(key, "Environment")}, nil
	}
	data, err := fsys.ReadFile(filepath.Join(configDir, "zai.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("read Z.ai credentials")
	}
	var credentials credentialFile
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, errors.New("invalid Z.ai credential file")
	}
	if credentials.Accounts == nil {
		key, err := validateAPIKey(strings.TrimSpace(credentials.APIKey))
		if err != nil || key == "" {
			return nil, err
		}
		return []Account{newAccount(key, "")}, nil
	}
	accounts := make([]Account, 0, len(*credentials.Accounts))
	seen := make(map[string]bool)
	var sessions map[string]zcode.LoginSession
	for _, stored := range *credentials.Accounts {
		if stored.ZCodeAccountID != "" {
			if stored.APIKey != "" || !validLabel(stored.Label) || (stored.Transport != "zcode" && stored.Transport != "zcode-start") {
				return nil, errors.New("invalid Z.ai credential file")
			}
			if sessions == nil {
				store, err := zcode.OpenSessionStore(fsys, configDir, false)
				if err != nil {
					return nil, errors.New("ZCode login unavailable; run cq zai login --zcode")
				}
				all, err := store.Accounts()
				if err != nil {
					return nil, err
				}
				sessions = make(map[string]zcode.LoginSession, len(all))
				for _, session := range all {
					sessions[session.ID] = session
				}
			}
			session, ok := sessions[stored.ZCodeAccountID]
			if !ok {
				return nil, errors.New("ZCode login unavailable; run cq zai login --zcode")
			}
			account := newZCodeAccount(session)
			if stored.ID != account.ID {
				return nil, errors.New("invalid Z.ai credential file")
			}
			account.Label, account.Transport = stored.Label, stored.Transport
			if !seen[account.ID] {
				accounts = append(accounts, account)
				seen[account.ID] = true
			}
			continue
		}
		key, err := validateAPIKey(strings.TrimSpace(stored.APIKey))
		if err != nil || key == "" || !validLabel(stored.Label) || (stored.Transport != "" && stored.Transport != "zcode") {
			return nil, errors.New("invalid Z.ai credential file")
		}
		account := newAccount(key, stored.Label)
		account.Transport = stored.Transport
		if stored.ID != "" && stored.ID != account.ID {
			return nil, errors.New("invalid Z.ai credential file")
		}
		if !seen[account.ID] {
			accounts = append(accounts, account)
			seen[account.ID] = true
		}
	}
	return accounts, nil
}

func newAccount(key, label string) Account {
	digest := sha256.Sum256([]byte(key))
	return Account{ID: "zai_" + hex.EncodeToString(digest[:8]), Label: label, APIKey: key}
}

func newZCodeAccount(session zcode.LoginSession) Account {
	digest := sha256.Sum256([]byte("zcode:" + session.UserID))
	label := session.Label
	if label == "" {
		label = session.Email
	}
	if label == "" {
		label = session.Name
	}
	return Account{ID: "zai_" + hex.EncodeToString(digest[:8]), Label: label, Transport: "zcode", OAuth: &session}
}

// StoreZCodeAccount saves a fresh browser login without replacing API-key accounts.
func StoreZCodeAccount(fsys fsutil.FileSystem, configDir string, session zcode.LoginSession, label string, activate bool) (Account, error) {
	store, err := zcode.OpenSessionStore(fsys, configDir, true)
	if err != nil {
		return Account{}, err
	}
	session, err = store.Save(session, label, activate)
	if err != nil {
		return Account{}, err
	}
	accounts, err := ReadAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return Account{}, err
	}
	account := newZCodeAccount(session)
	index := -1
	for i, existing := range accounts {
		if existing.ID == account.ID {
			index = i
			account.Transport = existing.Transport
			break
		}
	}
	if index < 0 {
		accounts = append(accounts, account)
		index = len(accounts) - 1
	} else {
		accounts[index] = account
	}
	if activate && index != 0 {
		copy(accounts[1:index+1], accounts[:index])
		accounts[0] = account
	}
	if err := writeAccounts(fsys, configDir, accounts); err != nil {
		return Account{}, err
	}
	return account, nil
}

// StoreAccount adds or updates one subscription without discarding existing ones.
func StoreAccount(fsys fsutil.FileSystem, configDir, key, label string, activate bool) (Account, error) {
	key, err := validateAPIKey(strings.TrimSpace(key))
	if err != nil || key == "" {
		return Account{}, errors.New("invalid Z.ai API key")
	}
	label = strings.TrimSpace(label)
	if !validLabel(label) {
		return Account{}, errors.New("invalid Z.ai account label")
	}
	accounts, err := ReadAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return Account{}, err
	}
	account := newAccount(key, label)
	index := -1
	for i, existing := range accounts {
		if existing.ID == account.ID {
			index = i
			account.Transport = existing.Transport
			if label == "" {
				account.Label = existing.Label
			}
			break
		}
	}
	if index < 0 {
		accounts = append(accounts, account)
		index = len(accounts) - 1
	} else {
		accounts[index] = account
	}
	if activate && index != 0 {
		copy(accounts[1:index+1], accounts[:index])
		accounts[0] = account
	}
	if err := writeAccounts(fsys, configDir, accounts); err != nil {
		return Account{}, err
	}
	return account, nil
}

// SetActiveAccount selects the preferred subscription by exact ID or unique label.
func SetActiveAccount(fsys fsutil.FileSystem, configDir, reference string) error {
	accounts, err := ReadAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return err
	}
	index, err := accountIndex(accounts, reference)
	if err != nil {
		return err
	}
	account := accounts[index]
	copy(accounts[1:index+1], accounts[:index])
	accounts[0] = account
	return writeAccounts(fsys, configDir, accounts)
}

// RemoveAccount removes one stored subscription by exact ID or unique label.
func RemoveAccount(fsys fsutil.FileSystem, configDir, reference string) error {
	accounts, err := ReadAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return err
	}
	index, err := accountIndex(accounts, reference)
	if err != nil {
		return err
	}
	removed := accounts[index]
	accounts = append(accounts[:index], accounts[index+1:]...)
	if err := writeAccounts(fsys, configDir, accounts); err != nil {
		return err
	}
	if removed.OAuth != nil {
		store, err := zcode.OpenSessionStore(fsys, configDir, false)
		if err != nil {
			return err
		}
		return store.Remove(removed.OAuth.ID)
	}
	return nil
}

// RemoveAPIKeyAccounts removes stored API-key entries only after a saved browser
// login can be loaded. It preserves browser-login order and never revokes tokens
// or changes the encrypted session store.
func RemoveAPIKeyAccounts(fsys fsutil.FileSystem, configDir string) error {
	accounts, err := ReadAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return err
	}
	kept := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		if account.OAuth != nil {
			kept = append(kept, account)
		}
	}
	if len(kept) == 0 {
		return errors.New("save a browser login with cq zai login before removing API keys")
	}
	if len(kept) == len(accounts) {
		return nil
	}
	return writeAccounts(fsys, configDir, kept)
}

// SetTransport chooses an upstream protocol for one saved account. Empty is the
// original native Responses route; ZCode uses the authenticated Messages gateway.
func SetTransport(fsys fsutil.FileSystem, configDir, reference, transport string) error {
	if transport != "responses" && transport != "zcode" && transport != "zcode-start" {
		return errors.New("Z.ai transport must be responses, zcode or zcode-start")
	}
	accounts, err := ReadAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return err
	}
	index, err := accountIndex(accounts, reference)
	if err != nil {
		return err
	}
	if accounts[index].OAuth != nil && transport == "responses" || accounts[index].OAuth == nil && transport == "zcode-start" {
		return errors.New("this route requires a matching ZCode browser login")
	}
	accounts[index].Transport = transport
	if transport == "responses" {
		accounts[index].Transport = ""
	}
	return writeAccounts(fsys, configDir, accounts)
}

func accountIndex(accounts []Account, reference string) (int, error) {
	for i, account := range accounts {
		if account.ID == reference {
			return i, nil
		}
	}
	index := -1
	for i, account := range accounts {
		if reference != "" && account.Label == reference {
			if index >= 0 {
				return 0, errors.New("ambiguous Z.ai account label; use its account ID")
			}
			index = i
		}
	}
	if index < 0 {
		return 0, errors.New("Z.ai account not found")
	}
	return index, nil
}

func writeAccounts(fsys fsutil.FileSystem, configDir string, accounts []Account) error {
	stored := make([]storedAccount, 0, len(accounts))
	credentials := credentialFile{Accounts: &stored}
	for _, account := range accounts {
		entry := storedAccount{ID: account.ID, Label: account.Label, APIKey: account.APIKey, Transport: account.Transport}
		if account.OAuth != nil {
			entry.ZCodeAccountID = account.OAuth.ID
		}
		stored = append(stored, entry)
	}
	if len(accounts) > 0 {
		credentials.APIKey = accounts[0].APIKey
	}
	data, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return errors.New("encode Z.ai credentials")
	}
	if err := fsutil.SecureAtomicWrite(fsys, filepath.Join(configDir, "zai.json"), append(data, '\n')); err != nil {
		return errors.New("save Z.ai credentials")
	}
	return nil
}

func validLabel(label string) bool {
	if len(label) > 128 {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validateAPIKey(key string) (string, error) {
	for _, r := range key {
		if r <= ' ' || r >= 127 {
			return "", errors.New("invalid Z.ai API key")
		}
	}
	return key, nil
}

// DefaultAPIKey reads credentials from the current user's CQ configuration.
func DefaultAPIKey() (string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return "", errors.New("resolve Z.ai credential directory")
	}
	return ReadAPIKey(fsutil.OSFileSystem{}, os.Getenv, roots.Config)
}
