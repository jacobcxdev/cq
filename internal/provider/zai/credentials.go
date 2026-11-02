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
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// Account is one subscription credential. API keys are excluded from public JSON.
type Account struct {
	ID     string `json:"id"`
	Label  string `json:"label,omitempty"`
	APIKey string `json:"-"`
}

type storedAccount struct {
	ID     string `json:"id"`
	Label  string `json:"label,omitempty"`
	APIKey string `json:"api_key"`
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
	for _, stored := range *credentials.Accounts {
		key, err := validateAPIKey(strings.TrimSpace(stored.APIKey))
		if err != nil || key == "" || !validLabel(stored.Label) {
			return nil, errors.New("invalid Z.ai credential file")
		}
		account := newAccount(key, stored.Label)
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
	accounts = append(accounts[:index], accounts[index+1:]...)
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
		stored = append(stored, storedAccount{ID: account.ID, Label: account.Label, APIKey: account.APIKey})
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
