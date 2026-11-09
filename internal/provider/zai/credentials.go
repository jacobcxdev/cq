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
	"github.com/jacobcxdev/cq/internal/provider/codexbar"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// Account is one subscription credential. API keys are excluded from public JSON.
type Account struct {
	ID                string              `json:"id"`
	Label             string              `json:"label,omitempty"`
	APIKey            string              `json:"-"`
	Transport         string              `json:"transport,omitempty"`
	OAuth             *zcode.LoginSession `json:"-"`
	Source            string              `json:"source,omitempty"`
	verifiedAPIKeyIDs []string
}

type storedAccount struct {
	ID                string   `json:"id"`
	Label             string   `json:"label,omitempty"`
	APIKey            string   `json:"api_key"`
	Transport         string   `json:"transport,omitempty"`
	ZCodeAccountID    string   `json:"zcode_account_id,omitempty"`
	VerifiedAPIKeyIDs []string `json:"verified_api_key_ids,omitempty"`
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

// ReadAccounts reads usable accounts in preferred order without HTTP or writes.
// An environment key selects its verified OAuth account when available;
// otherwise it overrides stored accounts. Unresolved borrowed credentials are
// candidates for ownership verification, not independent subscription capacity.
func ReadAccounts(fsys fsutil.FileSystem, env func(string) string, configDir string) ([]Account, error) {
	accounts, err := readAccountSources(fsys, env, configDir)
	if err != nil {
		return nil, err
	}
	return usableAccounts(accounts), nil
}

// ReadAPIKeyCandidates supplies local credentials only for exact ownership
// verification. The result must not be used as an account or routing inventory.
func ReadAPIKeyCandidates(fsys fsutil.FileSystem, env func(string) string, configDir string) ([]Account, error) {
	return readAccountSources(fsys, env, configDir)
}

func usableAccounts(accounts []Account) []Account {
	usable := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		if account.Source != "codexbar" {
			usable = append(usable, account)
		}
	}
	return usable
}

func readAccountSources(fsys fsutil.FileSystem, env func(string) string, configDir string) ([]Account, error) {
	accounts, err := readOwnedAccounts(fsys, env, configDir)
	if err != nil {
		return accounts, err
	}
	if strings.TrimSpace(env("ZAI_API_KEY")) != "" {
		owned, readErr := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
		if readErr == nil {
			for _, account := range owned {
				if account.OAuth != nil && account.Transport != "zcode-start" && containsAlias(account.verifiedAPIKeyIDs, accounts[0].ID) {
					return []Account{account}, nil
				}
			}
		}
		return accounts, nil
	}
	accounts = preferVerifiedOAuth(accounts)
	home, err := fsys.UserHomeDir()
	if err != nil {
		if len(accounts) > 0 {
			return append(accounts, Account{ID: "zai_codexbar_unavailable", Label: "CodexBar source unavailable", Source: "codexbar"}), nil
		}
		return nil, errors.New("resolve CodexBar credential directory")
	}
	external, err := codexbar.ReadAccounts(fsys, env, home, "zai")
	if err != nil {
		if len(accounts) > 0 {
			return append(accounts, Account{ID: "zai_codexbar_unavailable", Label: "CodexBar source unavailable", Source: "codexbar"}), nil
		}
		return nil, err
	}
	seen := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		seen[account.ID] = true
		for _, id := range account.verifiedAPIKeyIDs {
			seen[id] = true
		}
	}
	invalidExternal := false
	for _, borrowed := range external {
		// CQ's existing Coding Plan route is personal/global. Team scopes and
		// regional endpoints require their own quota contracts, not key reuse.
		if borrowed.UsageScope != "" && borrowed.UsageScope != "personal" || borrowed.Region != "" && borrowed.Region != "global" {
			continue
		}
		key, err := validateAPIKey(borrowed.Token)
		if err != nil || key == "" {
			invalidExternal = true
			continue
		}
		account := newAccount(key, borrowed.Label)
		if !validLabel(account.Label) {
			invalidExternal = true
			continue
		}
		if !seen[account.ID] {
			account.Source = "codexbar"
			accounts = append(accounts, account)
			seen[account.ID] = true
		}
	}
	if invalidExternal {
		accounts = append(accounts, Account{ID: "zai_codexbar_unavailable", Label: "CodexBar source unavailable", Source: "codexbar"})
	}
	return accounts, nil
}

// readOwnedAccounts is the mutation boundary: borrowed credentials are never
// persisted, switched, removed or given renewal ownership by CQ store operations.
func readOwnedAccounts(fsys fsutil.FileSystem, env func(string) string, configDir string) ([]Account, error) {
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
			for _, id := range stored.VerifiedAPIKeyIDs {
				if len(id) != 20 || !strings.HasPrefix(id, "zai_") {
					return nil, errors.New("invalid Z.ai credential file")
				}
				if _, err := hex.DecodeString(id[4:]); err != nil {
					return nil, errors.New("invalid Z.ai credential file")
				}
			}
			account.verifiedAPIKeyIDs = stored.VerifiedAPIKeyIDs
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
	return StoreVerifiedZCodeAccount(fsys, configDir, session, label, activate, VerifiedAPIKeyOwnership{})
}

// StoreVerifiedZCodeAccount replaces only exact keys proved to belong to this
// OAuth account. Borrowed source records are never written or removed.
func StoreVerifiedZCodeAccount(fsys fsutil.FileSystem, configDir string, session zcode.LoginSession, label string, activate bool, proof VerifiedAPIKeyOwnership) (Account, error) {
	if proof.userID != "" && proof.userID != session.UserID {
		return Account{}, errors.New("Z.ai key ownership account mismatch")
	}
	store, err := zcode.OpenSessionStore(fsys, configDir, true)
	if err != nil {
		return Account{}, err
	}
	session, err = store.Save(session, label, activate)
	if err != nil {
		return Account{}, err
	}
	accounts, err := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return Account{}, err
	}
	account := newZCodeAccount(session)
	account.verifiedAPIKeyIDs = append([]string(nil), proof.keyIDs...)
	for _, existing := range accounts {
		if existing.ID == account.ID {
			account.Transport = existing.Transport
			break
		}
	}
	filtered := accounts[:0]
	replacementIndex := -1
	for _, existing := range accounts {
		remove := account.Transport != "zcode-start" && existing.OAuth == nil && containsAlias(proof.keyIDs, existing.ID)
		if remove && replacementIndex < 0 {
			replacementIndex = len(filtered)
		}
		if !remove {
			filtered = append(filtered, existing)
		}
	}
	accounts = filtered
	index := -1
	for i, existing := range accounts {
		if existing.ID == account.ID {
			index = i
			for _, id := range existing.verifiedAPIKeyIDs {
				if !containsAlias(account.verifiedAPIKeyIDs, id) {
					account.verifiedAPIKeyIDs = append(account.verifiedAPIKeyIDs, id)
				}
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
	// Replacing a key preserves that logical account's earliest preferred
	// position, even when an OAuth record already exists later in the list.
	if replacementIndex >= 0 && replacementIndex < index {
		copy(accounts[replacementIndex+1:index+1], accounts[replacementIndex:index])
		accounts[replacementIndex] = account
		index = replacementIndex
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
	accounts, err := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return Account{}, err
	}
	account := newAccount(key, label)
	for i, existing := range accounts {
		if existing.OAuth != nil && existing.Transport != "zcode-start" && containsAlias(existing.verifiedAPIKeyIDs, account.ID) {
			if activate && i > 0 {
				copy(accounts[1:i+1], accounts[:i])
				accounts[0] = existing
				if err := writeAccounts(fsys, configDir, accounts); err != nil {
					return Account{}, err
				}
			}
			return existing, nil
		}
	}
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
	accounts, err := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
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
	accounts, err := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
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
	accounts, err := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
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
	accounts, err := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
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
			entry.VerifiedAPIKeyIDs = account.verifiedAPIKeyIDs
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

func containsAlias(aliases []string, id string) bool {
	for _, alias := range aliases {
		if alias == id {
			return true
		}
	}
	return false
}

func preferVerifiedOAuth(accounts []Account) []Account {
	aliases := map[string]Account{}
	for _, a := range accounts {
		if a.OAuth != nil && a.Transport != "zcode-start" {
			for _, id := range a.verifiedAPIKeyIDs {
				aliases[id] = a
			}
		}
	}
	out := make([]Account, 0, len(accounts))
	seen := map[string]bool{}
	for _, a := range accounts {
		if owner, ok := aliases[a.ID]; a.OAuth == nil && ok {
			a = owner
		}
		if !seen[a.ID] {
			seen[a.ID] = true
			out = append(out, a)
		}
	}
	return out
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
