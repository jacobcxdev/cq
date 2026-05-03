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

	// De-dup within each source only. Cross-source de-dup would let a stale
	// credentials-file record suppress a fresher cq-keyring record before we
	// can compare them by freshness.
	accounts = append(accounts, discoverCredentialsFile(make(map[string]bool))...)

	// Platform-specific keychain discovery (macOS: security CLI for backward compat).
	// Claude Code refreshes the keychain token but not the credentials file,
	// and the keychain entry often lacks email/UUID metadata.
	accounts = append(accounts, discoverPlatformKeychain(make(map[string]bool))...)

	// cq-managed accounts via go-keyring (cross-platform)
	accounts = append(accounts, discoverCQKeyring(make(map[string]bool))...)

	// Merge identified accounts from different sources by freshness, keyed by
	// AccountUUID then Email. This prevents a stale credentials-file record
	// from suppressing a fresher cq-keyring record for the same logical account.
	accounts = mergeIdentifiedByFreshness(accounts)

	// Merge anonymous keychain entries (no email/UUID) with identified entries
	// from the credentials file or cq keyring. Must run after all sources are
	// discovered so token affinity can match against cq keyring entries.
	accounts = mergeAnonymousFresh(accounts)

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

		if target >= 0 && a.ExpiresAt >= accounts[target].ExpiresAt {
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

// pickWinner reports whether candidate should replace current as the "token
// winner" when two entries represent the same logical account. It implements the
// shared tie-break policy:
//  1. Higher ExpiresAt wins outright.
//  2. On a tie: prefer non-empty AccountUUID.
//  3. Then: prefer non-nil TokenAccount.
//  4. Then: prefer richer (longer) Scopes list.
//  5. Otherwise keep current (return false).
func pickWinner(candidate, current ClaudeOAuth) bool {
	if candidate.ExpiresAt > current.ExpiresAt {
		return true
	}
	if candidate.ExpiresAt < current.ExpiresAt {
		return false
	}
	// Equal ExpiresAt — apply tie-break policy.
	if candidate.AccountUUID != "" && current.AccountUUID == "" {
		return true
	}
	if candidate.AccountUUID == "" && current.AccountUUID != "" {
		return false
	}
	if candidate.TokenAccount != nil && current.TokenAccount == nil {
		return true
	}
	if candidate.TokenAccount == nil && current.TokenAccount != nil {
		return false
	}
	if len(candidate.Scopes) > len(current.Scopes) {
		return true
	}
	return false
}

// mergeIdentifiedByFreshness deduplicates identified accounts (those with
// AccountUUID or Email) across discovery sources by preferring the entry with
// the highest ExpiresAt. This fixes the source-order bias bug where a stale
// credentials-file record could suppress a fresher cq-keyring record for the
// same logical account.
//
// Two accounts are considered the same logical account when they share an
// AccountUUID, or when both have an Email that matches (even if only one has a
// UUID). Anonymous entries (no UUID and no Email) pass through unchanged.
//
// The winner keeps its own token fields (AccessToken, RefreshToken, ExpiresAt)
// and is enriched with any metadata the loser has that the winner lacks
// (Scopes, SubscriptionType, RateLimitTier, AccountUUID, Profile, TokenAccount).
func mergeIdentifiedByFreshness(accounts []ClaudeOAuth) []ClaudeOAuth {
	if len(accounts) <= 1 {
		return accounts
	}

	// byUUID and byEmail track where in result each logical account lives.
	byUUID := make(map[string]int)  // uuid -> index
	byEmail := make(map[string]int) // email -> index
	var result []ClaudeOAuth

	for _, a := range accounts {
		if a.AccountUUID == "" && a.Email == "" {
			// Anonymous: pass through unchanged.
			result = append(result, a)
			continue
		}

		// Find existing canonical index — UUID match takes precedence over email.
		idx := -1
		if a.AccountUUID != "" {
			if i, ok := byUUID[a.AccountUUID]; ok {
				idx = i
			}
		}
		if idx < 0 && a.Email != "" {
			if i, ok := byEmail[a.Email]; ok {
				idx = i
			}
		}

		if idx < 0 {
			// First time we see this logical account.
			idx = len(result)
			result = append(result, a)
		} else {
			// Merge: pick the better entry as winner.
			if pickWinner(a, result[idx]) {
				result[idx] = mergeAccountFields(a, result[idx])
			} else {
				result[idx] = mergeAccountFields(result[idx], a)
			}
		}

		// Register both stable identifiers for future lookup.
		if result[idx].AccountUUID != "" {
			byUUID[result[idx].AccountUUID] = idx
		}
		if result[idx].Email != "" {
			byEmail[result[idx].Email] = idx
		}
	}
	return result
}

// mergeAccountFields copies missing metadata from loser into winner.
// Token fields (AccessToken, RefreshToken, ExpiresAt) come from winner;
// everything else is filled in from loser when winner lacks it.
func mergeAccountFields(winner, loser ClaudeOAuth) ClaudeOAuth {
	if len(winner.Scopes) == 0 && len(loser.Scopes) > 0 {
		winner.Scopes = loser.Scopes
	}
	if winner.SubscriptionType == "" && loser.SubscriptionType != "" {
		winner.SubscriptionType = loser.SubscriptionType
	}
	if winner.RateLimitTier == "" && loser.RateLimitTier != "" {
		winner.RateLimitTier = loser.RateLimitTier
	}
	if winner.AccountUUID == "" && loser.AccountUUID != "" {
		winner.AccountUUID = loser.AccountUUID
	}
	if winner.Profile == nil && loser.Profile != nil {
		winner.Profile = loser.Profile
	}
	if winner.TokenAccount == nil && loser.TokenAccount != nil {
		winner.TokenAccount = loser.TokenAccount
	}
	return winner
}

// dedupByEmail removes duplicate accounts that share an email address,
// preferring fresher tokens before falling back to richer metadata.
// Metadata (scopes, plan, tier, UUID, profile) is carried forward from the
// replaced entry when the winner lacks it — prevents silent scope stripping.
func dedupByEmail(accounts []ClaudeOAuth) []ClaudeOAuth {
	if len(accounts) <= 1 {
		return accounts
	}
	seen := make(map[string]int) // email -> index in result
	var result []ClaudeOAuth
	for _, a := range accounts {
		if a.Email != "" {
			if idx, ok := seen[a.Email]; ok {
				if pickWinner(a, result[idx]) {
					result[idx] = mergeAccountFields(a, result[idx])
				} else {
					result[idx] = mergeAccountFields(result[idx], a)
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
		// SecItemAdd fails with errSecDuplicateItem (exit status 45) when the
		// item already exists. Delete and retry once.
		_ = gokeyring.Delete(service, acct.AccountUUID)
		if err := gokeyring.Set(service, acct.AccountUUID, string(data)); err != nil {
			return err
		}
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

// RemoveCQClaudeAccountsByEmail deletes cq-managed Claude account state for all
// manifest rows matching the given email.
func RemoveCQClaudeAccountsByEmail(email string) error {
	if email == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home dir: %w", err)
	}
	manifestPath := filepath.Join(home, ".cache", "cq", "accounts.json")
	entries := loadManifest(manifestPath)
	if len(entries) == 0 {
		return nil
	}

	filtered := make([]manifestEntry, 0, len(entries))
	removed := false
	for _, entry := range entries {
		if entry.Email == email {
			removed = true
			if entry.UUID != "" {
				service := ServicePrefix + Hash8(entry.UUID)
				_ = gokeyring.Delete(service, entry.UUID)
			}
			continue
		}
		filtered = append(filtered, entry)
	}
	if !removed {
		return nil
	}
	if err := saveManifest(manifestPath, filtered); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	return nil
}

// RemoveActiveClaudeCredentialsByEmail clears the active Claude credentials when
// they belong to the given email.
func RemoveActiveClaudeCredentialsByEmail(email string) error {
	if email == "" || ActiveClaudeEmail() != email {
		return nil
	}
	if err := WriteCredentialsFile(&ClaudeCredentials{}); err != nil {
		return fmt.Errorf("clear credentials: %w", err)
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
		return // file missing is normal (e.g. no credentials file on disk)
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

var (
	updateKeychainEntryForRefresh = UpdateKeychainEntry
	storeCQAccountForRefresh      = StoreCQAccount
)

// PersistRefreshedToken updates stored Claude credentials after a successful refresh.
func PersistRefreshedToken(acct *ClaudeOAuth) {
	cqAccount := *acct

	home, err := os.UserHomeDir()
	if err == nil {
		path := filepath.Join(home, ".claude", ".credentials.json")
		data, err := os.ReadFile(path)
		if err == nil {
			var creds ClaudeCredentials
			if json.Unmarshal(data, &creds) == nil && canUpdateStoredAccount(creds.ClaudeAiOauth, acct) {
				stored := creds.ClaudeAiOauth
				updated := mergeRefreshedAccount(stored, acct)
				creds.ClaudeAiOauth = &updated
				cqAccount = updated
				if err := WriteCredentialsFile(&creds); err != nil {
					fmt.Fprintf(os.Stderr, "cq: PersistRefreshedToken: write creds: %v\n", err)
				} else if err := updateKeychainEntryForRefresh("Claude Code-credentials", &creds); err != nil {
					fmt.Fprintf(os.Stderr, "cq: PersistRefreshedToken: update keychain: %v\n", err)
				}
			}
		}
	}

	if cqAccount.AccountUUID == "" {
		cqAccount.AccountUUID = acct.AccountUUID
	}
	if cqAccount.Email == "" {
		cqAccount.Email = acct.Email
	}
	if cqAccount.AccountUUID != "" {
		if err := storeCQAccountForRefresh(&cqAccount); err != nil {
			fmt.Fprintf(os.Stderr, "cq: PersistRefreshedToken: store cq account: %v\n", err)
		}
	}
}

func canUpdateStoredAccount(stored, acct *ClaudeOAuth) bool {
	if stored == nil || acct == nil {
		return false
	}
	if stored.Email != "" && acct.Email != "" && stored.Email != acct.Email {
		return false
	}
	if stored.AccountUUID != "" && acct.AccountUUID != "" && stored.AccountUUID != acct.AccountUUID {
		return false
	}
	if stored.Email != "" && acct.Email != "" {
		return true
	}
	if stored.AccountUUID != "" && acct.AccountUUID != "" {
		return true
	}
	return sameStoredAccount(stored, acct)
}

func mergeRefreshedAccount(stored, acct *ClaudeOAuth) ClaudeOAuth {
	updated := *stored
	if acct.AccessToken != "" {
		updated.AccessToken = acct.AccessToken
	}
	if acct.ExpiresAt > 0 {
		updated.ExpiresAt = acct.ExpiresAt
	}
	if acct.RefreshToken != "" {
		updated.RefreshToken = acct.RefreshToken
	}
	if len(acct.Scopes) > 0 && len(stored.Scopes) == 0 {
		updated.Scopes = acct.Scopes
	}
	if updated.Email == "" {
		updated.Email = acct.Email
	}
	if updated.AccountUUID == "" {
		updated.AccountUUID = acct.AccountUUID
	}
	if updated.SubscriptionType == "" {
		updated.SubscriptionType = acct.SubscriptionType
	}
	if updated.RateLimitTier == "" {
		updated.RateLimitTier = acct.RateLimitTier
	}
	if updated.Profile == nil {
		updated.Profile = acct.Profile
	}
	if updated.TokenAccount == nil {
		updated.TokenAccount = acct.TokenAccount
	}
	return updated
}

// ActiveClaudeEmail returns the email of the currently active Claude account
// from ~/.claude/.credentials.json. Returns "" if unavailable.
func ActiveClaudeEmail() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	var creds ClaudeCredentials
	if json.Unmarshal(data, &creds) != nil || creds.ClaudeAiOauth == nil {
		return ""
	}
	return creds.ClaudeAiOauth.Email
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
