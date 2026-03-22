package keyring

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	gokeyring "github.com/zalando/go-keyring"
)

// ServicePrefix is the keyring service prefix for cq-managed accounts.
const ServicePrefix = "cq-claude-"

// Hash8 returns first 8 hex chars of SHA-256 for use in service names.
func Hash8(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}

// ClaudeCredentials is the format stored in Claude Code's keychain entry.
type ClaudeCredentials struct {
	ClaudeAiOauth *ClaudeOAuth `json:"claudeAiOauth,omitempty"`
}

type ClaudeOAuth struct {
	AccessToken      string          `json:"accessToken"`
	RefreshToken     string          `json:"refreshToken"`
	ExpiresAt        int64           `json:"expiresAt"`
	Scopes           []string        `json:"scopes,omitempty"`
	SubscriptionType string          `json:"subscriptionType,omitempty"`
	RateLimitTier    string          `json:"rateLimitTier,omitempty"`
	Email            string          `json:"email,omitempty"`
	AccountUUID      string          `json:"accountUUID,omitempty"`
	Profile          json.RawMessage `json:"profile,omitempty"`
	TokenAccount     *TokenAccount   `json:"tokenAccount,omitempty"`
}

type TokenAccount struct {
	UUID             string `json:"uuid"`
	EmailAddress     string `json:"emailAddress"`
	OrganizationUUID string `json:"organizationUuid"`
}

// DiscoverClaudeAccounts finds all Claude accounts from:
// 1. ~/.claude/.credentials.json (active account)
// 2. Platform keychain (macOS: "Claude Code-credentials*", all: cq-claude-* via go-keyring)
func DiscoverClaudeAccounts() []ClaudeOAuth {
	var accounts []ClaudeOAuth
	seen := make(map[string]bool)

	// Credentials file first — has email/UUID but token may be stale.
	accounts = append(accounts, discoverCredentialsFile(seen)...)

	// Platform-specific keychain discovery (macOS: security CLI for backward compat).
	// Claude Code refreshes the keychain token but not the credentials file,
	// and the keychain entry often lacks email/UUID metadata.
	accounts = append(accounts, discoverPlatformKeychain(seen)...)

	// Merge anonymous keychain entries (no email/UUID) with identified entries
	// from the credentials file. Claude Code's active account lives in both
	// places but with different tokens after a refresh.
	accounts = mergeAnonymousFresh(accounts)

	// cq-managed accounts via go-keyring (cross-platform)
	accounts = append(accounts, discoverCQKeyring(seen)...)

	// Post-discovery dedup: entries from different sources may represent
	// the same account with different tokens (e.g. before/after refresh).
	// Merge by email when available.
	return dedupByEmail(accounts)
}

// mergeAnonymousFresh finds anonymous entries (no email/UUID, typically from keychain
// after Claude Code refreshed tokens) and merges their fresh tokens into identified
// entries (from credentials file or cq keyring). The anonymous entry is removed after
// merging.
//
// With a single identified account the anonymous entry is merged unconditionally
// (there is only one candidate). With 2+ identified accounts, token affinity
// (sameStoredAccount) is used to find the right target — this prevents cross-wiring
// while still allowing the merge when a clear match exists.
func mergeAnonymousFresh(accounts []ClaudeOAuth) []ClaudeOAuth {
	if len(accounts) <= 1 {
		return accounts
	}

	merged := make(map[int]bool)
	for i, a := range accounts {
		if a.Email != "" || a.AccountUUID != "" {
			continue // not anonymous
		}

		// Find identified entries that could be the same account.
		var matchIdx []int
		for j := range accounts {
			if j == i || (accounts[j].Email == "" && accounts[j].AccountUUID == "") {
				continue
			}
			matchIdx = append(matchIdx, j)
		}

		// Use token affinity to find the right merge target. Even with a
		// single identified entry we require a token match — blind merging
		// cross-wires credentials when an identified entry is missing from
		// a source (e.g. a deleted cq keyring item).
		target := -1
		for _, j := range matchIdx {
			if sameStoredAccount(&a, &accounts[j]) {
				if target != -1 {
					target = -1 // ambiguous — matches multiple
					break
				}
				target = j
			}
		}

		if target >= 0 && a.ExpiresAt > accounts[target].ExpiresAt {
			updated := accounts[target]
			updated.AccessToken = a.AccessToken
			updated.RefreshToken = a.RefreshToken
			updated.ExpiresAt = a.ExpiresAt
			if len(a.Scopes) > 0 {
				updated.Scopes = a.Scopes
			}
			accounts[target] = updated
			merged[i] = true
		}
	}

	if len(merged) == 0 {
		return accounts
	}
	var result []ClaudeOAuth
	for i, a := range accounts {
		if !merged[i] {
			result = append(result, a)
		}
	}
	return result
}

// dedupByEmail removes duplicate accounts that share an email address,
// preferring fresher tokens before falling back to richer metadata.
func dedupByEmail(accounts []ClaudeOAuth) []ClaudeOAuth {
	if len(accounts) <= 1 {
		return accounts
	}
	seen := make(map[string]int) // email -> index in result
	var result []ClaudeOAuth
	for _, a := range accounts {
		if a.Email != "" {
			if idx, ok := seen[a.Email]; ok {
				existing := result[idx]
				if a.ExpiresAt > existing.ExpiresAt {
					result[idx] = a
				} else if a.AccountUUID != "" && existing.AccountUUID == "" {
					result[idx] = a
				}
				continue
			}
			seen[a.Email] = len(result)
		}
		result = append(result, a)
	}
	return result
}

func sameStoredAccount(stored, acct *ClaudeOAuth) bool {
	if stored == nil || acct == nil {
		return false
	}
	if stored.Email != "" && acct.Email != "" && stored.Email == acct.Email {
		return true
	}
	if stored.AccountUUID != "" && acct.AccountUUID != "" && stored.AccountUUID == acct.AccountUUID {
		return true
	}
	if stored.RefreshToken != "" && acct.RefreshToken != "" && stored.RefreshToken == acct.RefreshToken {
		return true
	}
	// Access-token matching is a last resort. After a token refresh the stored
	// access token changes, so this check may fail for the same logical account.
	// It is kept as a fallback for accounts lacking UUID, email, and refresh token.
	if stored.AccessToken != "" && acct.AccessToken != "" && stored.AccessToken == acct.AccessToken {
		return true
	}
	return false
}

// accountKey returns a stable dedup key for an account.
// Falls back to access token when no stable identifier (UUID, refresh token)
// is available. This fallback is fragile: after a token refresh the key changes,
// potentially producing duplicate entries for the same logical account.
func accountKey(a *ClaudeOAuth) string {
	if a.AccountUUID != "" {
		return "uuid:" + a.AccountUUID
	}
	if a.RefreshToken != "" {
		return "rt:" + Hash8(a.RefreshToken)
	}
	return "at:" + Hash8(a.AccessToken)
}

func discoverCredentialsFile(seen map[string]bool) []ClaudeOAuth {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var creds ClaudeCredentials
	if json.Unmarshal(data, &creds) != nil {
		return nil
	}
	if creds.ClaudeAiOauth == nil || creds.ClaudeAiOauth.AccessToken == "" {
		return nil
	}
	key := accountKey(creds.ClaudeAiOauth)
	if seen[key] {
		return nil
	}
	seen[key] = true
	return []ClaudeOAuth{*creds.ClaudeAiOauth}
}

func discoverCQKeyring(seen map[string]bool) []ClaudeOAuth {
	// cq-managed accounts are stored with known service names.
	// We track them in a manifest file since go-keyring doesn't support enumeration.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	manifest := loadManifest(filepath.Join(home, ".cache", "cq", "accounts.json"))
	var accounts []ClaudeOAuth
	for _, entry := range manifest {
		service := ServicePrefix + Hash8(entry.UUID)
		raw, err := gokeyring.Get(service, entry.UUID)
		if err != nil {
			continue
		}
		var acct ClaudeOAuth
		if json.Unmarshal([]byte(raw), &acct) != nil {
			continue
		}
		key := accountKey(&acct)
		if seen[key] {
			continue
		}
		seen[key] = true
		accounts = append(accounts, acct)
	}
	return accounts
}

type manifestEntry struct {
	UUID  string `json:"uuid"`
	Email string `json:"email"`
}

func loadManifest(path string) []manifestEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		fmt.Fprintf(os.Stderr, "cq: loadManifest: unmarshal %s: %v\n", path, err)
		return nil
	}
	return entries
}

func saveManifest(path string, entries []manifestEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "cq: saveManifest: mkdir: %v\n", err)
		return fmt.Errorf("saveManifest: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: saveManifest: marshal: %v\n", err)
		return fmt.Errorf("saveManifest: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "cq: saveManifest: write: %v\n", err)
		return fmt.Errorf("saveManifest: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "cq: saveManifest: rename: %v\n", err)
		return fmt.Errorf("saveManifest: rename: %w", err)
	}
	return nil
}

// StoreCQAccount stores credentials in the cross-platform keyring and updates the manifest.
func StoreCQAccount(acct *ClaudeOAuth) error {
	if acct.AccountUUID == "" {
		return fmt.Errorf("account UUID required for keyring storage")
	}
	service := ServicePrefix + Hash8(acct.AccountUUID)
	data, err := json.Marshal(acct)
	if err != nil {
		return err
	}
	if err := gokeyring.Set(service, acct.AccountUUID, string(data)); err != nil {
		return err
	}

	// Update manifest
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home dir: %w", err)
	}
	manifestPath := filepath.Join(home, ".cache", "cq", "accounts.json")
	entries := loadManifest(manifestPath)
	found := false
	for i, e := range entries {
		if e.UUID == acct.AccountUUID {
			entries[i].Email = acct.Email
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, manifestEntry{UUID: acct.AccountUUID, Email: acct.Email})
	}
	if err := saveManifest(manifestPath, entries); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	return nil
}

// BackfillCredentialsFile updates the active credentials file with profile data
// (email, UUID, plan, tier) without overwriting the tokens.
func BackfillCredentialsFile(acct *ClaudeOAuth) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: backfill creds: home dir: %v\n", err)
		return
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: backfill creds: read: %v\n", err)
		return
	}
	var creds ClaudeCredentials
	if err := json.Unmarshal(data, &creds); err != nil || creds.ClaudeAiOauth == nil {
		fmt.Fprintf(os.Stderr, "cq: backfill creds: parse credentials file\n")
		return
	}
	// Only update if this is the same account.
	stored := creds.ClaudeAiOauth
	if !sameStoredAccount(stored, acct) {
		return
	}
	updated := *stored
	changed := false
	if acct.Email != "" && stored.Email != acct.Email {
		updated.Email = acct.Email
		changed = true
	}
	if acct.AccountUUID != "" && stored.AccountUUID != acct.AccountUUID {
		updated.AccountUUID = acct.AccountUUID
		changed = true
	}
	if acct.SubscriptionType != "" && stored.SubscriptionType != acct.SubscriptionType {
		updated.SubscriptionType = acct.SubscriptionType
		changed = true
	}
	if acct.RateLimitTier != "" && stored.RateLimitTier != acct.RateLimitTier {
		updated.RateLimitTier = acct.RateLimitTier
		changed = true
	}
	if !changed {
		return
	}
	creds.ClaudeAiOauth = &updated
	serialised, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: backfill creds: marshal: %v\n", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, serialised, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "cq: write credentials tmp: %v\n", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "cq: rename credentials: %v\n", err)
		os.Remove(tmp)
	}
}

// PersistRefreshedToken updates stored Claude credentials after a successful refresh.
func PersistRefreshedToken(acct *ClaudeOAuth) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var creds ClaudeCredentials
	if json.Unmarshal(data, &creds) != nil || creds.ClaudeAiOauth == nil {
		return
	}
	stored := creds.ClaudeAiOauth
	if !sameStoredAccount(stored, acct) {
		return
	}

	updated := *stored
	changed := false
	if acct.AccessToken != "" && stored.AccessToken != acct.AccessToken {
		updated.AccessToken = acct.AccessToken
		changed = true
	}
	if acct.ExpiresAt > 0 && stored.ExpiresAt != acct.ExpiresAt {
		updated.ExpiresAt = acct.ExpiresAt
		changed = true
	}
	if acct.RefreshToken != "" && stored.RefreshToken != acct.RefreshToken {
		updated.RefreshToken = acct.RefreshToken
		changed = true
	}
	if len(acct.Scopes) > 0 && len(stored.Scopes) == 0 {
		updated.Scopes = acct.Scopes
		changed = true
	}
	if !changed {
		return
	}
	creds.ClaudeAiOauth = &updated

	if err := WriteCredentialsFile(&creds); err != nil {
		fmt.Fprintf(os.Stderr, "cq: PersistRefreshedToken: write creds: %v\n", err)
		return
	}
	if err := UpdateKeychainEntry("Claude Code-credentials", &creds); err != nil {
		fmt.Fprintf(os.Stderr, "cq: PersistRefreshedToken: update keychain: %v\n", err)
	}
	if updated.AccountUUID != "" {
		if err := StoreCQAccount(&updated); err != nil {
			fmt.Fprintf(os.Stderr, "cq: PersistRefreshedToken: store cq account: %v\n", err)
		}
	}
}

// WriteCredentialsFile atomically writes credentials to ~/.claude/.credentials.json.
func WriteCredentialsFile(creds *ClaudeCredentials) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}
	// Enforce permissions even if directory pre-exists with wrong mode.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod credential dir: %w", err)
	}
	path := filepath.Join(dir, ".credentials.json")
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
