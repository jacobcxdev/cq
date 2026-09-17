package keyring

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/userdirs"
	gokeyring "github.com/zalando/go-keyring"
)

// resolveCredentialHome preserves authenticated native ownership; tests inject isolated homes.
var resolveCredentialHome = userdirs.UserHomeDir

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
	return DiscoverClaudeAccountsContext(context.Background())
}
func DiscoverClaudeAccountsContext(ctx context.Context) []ClaudeOAuth {
	if ctx.Err() != nil {
		return nil
	}
	var accounts []ClaudeOAuth

	// De-dup within each source only. Cross-source de-dup would let a stale
	// credentials-file record suppress a fresher cq-keyring record before we
	// can compare them by freshness.
	accounts = append(accounts, discoverCredentialsFileContext(ctx, make(map[string]bool))...)

	// Platform-specific keychain discovery (macOS: security CLI for backward compat).
	// Claude Code refreshes the keychain token but not the credentials file,
	// and the keychain entry often lacks email/UUID metadata.
	if ctx.Err() != nil {
		return nil
	}
	accounts = append(accounts, discoverPlatformKeychainContext(ctx, make(map[string]bool))...)

	// cq-managed accounts via go-keyring (cross-platform)
	accounts = append(accounts, discoverCQKeyringContext(ctx, make(map[string]bool))...)

	// Merge identified accounts from different sources by freshness, keyed by
	// AccountUUID then Email. This prevents a stale credentials-file record
	// from suppressing a fresher cq-keyring record for the same logical account.
	accounts = mergeIdentifiedByFreshness(accounts)

	// Merge anonymous keychain entries (no email/UUID) with identified entries
	// from the credentials file or cq keyring. Must run after all sources are
	// discovered so token affinity can match against cq keyring entries.
	accounts = mergeAnonymousFresh(accounts)

	// Drop phantom anonymous entries: an expired, identity-less keychain slot
	// left behind by a removed account whose tokens drifted from the stored
	// copies (so neither remove-by-affinity nor mergeAnonymousFresh could
	// attribute it). Runs after the merge so genuine fresh anonymous logins are
	// already absorbed into their identified account.
	accounts = dropOrphanAnonymousExpired(accounts, time.Now().UnixMilli())

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

// dropOrphanAnonymousExpired removes anonymous entries (no Email, no AccountUUID)
// that are expired, but only when at least one identified account is also present.
//
// Such an entry is a phantom: typically a leftover "Claude Code" keychain slot
// from an account removed via `cq claude remove`, whose live tokens had already
// drifted from the stored copies — so removal-by-token-affinity could not delete
// it and mergeAnonymousFresh could not attribute it to any identified account. It
// carries no identity and cannot be merged, so it would otherwise resurface as a
// standalone "auth expired" account on every run.
//
// Guards:
//   - No identified account present → the anonymous entry is the user's only
//     login; preserve it (a legitimately expired session still worth surfacing).
//   - ExpiresAt == 0 (unknown expiry) → preserve; absence of data is not expiry.
//   - Non-expired anonymous entry → preserve; may be a freshly written Claude Code
//     login cq has not yet matched to an identity.
//
// The input slice is not mutated; a new slice is returned.
func dropOrphanAnonymousExpired(accounts []ClaudeOAuth, nowMs int64) []ClaudeOAuth {
	hasIdentified := false
	for i := range accounts {
		if accounts[i].Email != "" || accounts[i].AccountUUID != "" {
			hasIdentified = true
			break
		}
	}
	if !hasIdentified {
		return accounts
	}

	var result []ClaudeOAuth
	for _, a := range accounts {
		if a.Email == "" && a.AccountUUID == "" && a.ExpiresAt != 0 && a.ExpiresAt <= nowMs {
			continue // phantom: identity-less and expired
		}
		result = append(result, a)
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
	return discoverCredentialsFileContext(context.Background(), seen)
}
func discoverCredentialsFileContext(ctx context.Context, seen map[string]bool) []ClaudeOAuth {
	if ctx.Err() != nil {
		return nil
	}
	home, err := resolveCredentialHome()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	if ctx.Err() != nil {
		return nil
	}
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
	return discoverCQKeyringContext(context.Background(), seen)
}
func discoverCQKeyringContext(ctx context.Context, seen map[string]bool) []ClaudeOAuth {
	if ctx.Err() != nil {
		return nil
	}
	// cq-managed accounts are stored with known service names.
	// We track them in a manifest file since go-keyring doesn't support enumeration.
	manifestPath, err := defaultCQManifestPath()
	if err != nil {
		return nil
	}
	manifest := loadManifestContext(ctx, manifestPath)
	var accounts []ClaudeOAuth
	for _, entry := range manifest {
		if ctx.Err() != nil {
			return accounts
		}
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
	return loadManifestContext(context.Background(), path)
}
func loadManifestContext(ctx context.Context, path string) []manifestEntry {
	if ctx.Err() != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		credentialDiagnostic(ctx, "manifest_decode_failed", "Claude credential manifest could not be decoded.", "cq: loadManifest: unmarshal %s: %v\n", path, err)
		return nil
	}
	return entries
}

func saveManifest(path string, entries []manifestEntry) error {
	return saveManifestContext(context.Background(), path, entries)
}
func saveManifestContext(ctx context.Context, path string, entries []manifestEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		credentialDiagnostic(ctx, "manifest_directory_failed", "Claude credential manifest directory could not be created.", "cq: saveManifest: mkdir: %v\n", err)
		return fmt.Errorf("saveManifest: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		credentialDiagnostic(ctx, "manifest_encode_failed", "Claude credential manifest could not be encoded.", "cq: saveManifest: marshal: %v\n", err)
		return fmt.Errorf("saveManifest: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		credentialDiagnostic(ctx, "manifest_write_failed", "Claude credential manifest could not be written.", "cq: saveManifest: write: %v\n", err)
		return fmt.Errorf("saveManifest: write: %w", err)
	}
	if err := ctx.Err(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		credentialDiagnostic(ctx, "manifest_replace_failed", "Claude credential manifest could not be replaced.", "cq: saveManifest: rename: %v\n", err)
		return fmt.Errorf("saveManifest: rename: %w", err)
	}
	return nil
}

// StoreCQAccount stores credentials in the cross-platform keyring and updates the manifest.
func StoreCQAccount(acct *ClaudeOAuth) error {
	return StoreCQAccountContext(context.Background(), acct)
}
func StoreCQAccountContext(ctx context.Context, acct *ClaudeOAuth) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if acct.AccountUUID == "" {
		return fmt.Errorf("account UUID required for keyring storage")
	}
	manifestPath, err := defaultCQManifestPath()
	if err != nil {
		return fmt.Errorf("resolve manifest path: %w", err)
	}
	service := ServicePrefix + Hash8(acct.AccountUUID)
	data, err := json.Marshal(acct)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := gokeyring.Set(service, acct.AccountUUID, string(data)); err != nil {
		// SecItemAdd fails with errSecDuplicateItem (exit status 45) when the
		// item already exists. Delete and retry once.
		if err := ctx.Err(); err != nil {
			return err
		}
		_ = gokeyring.Delete(service, acct.AccountUUID)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := gokeyring.Set(service, acct.AccountUUID, string(data)); err != nil {
			return err
		}
	}

	// Update manifest
	entries := loadManifestContext(ctx, manifestPath)
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
	if err := saveManifestContext(ctx, manifestPath, entries); err != nil {
		return &ClaudeStoreError{Err: err}
	}
	return nil
}

// RemoveCQClaudeAccountsByEmail deletes cq-managed Claude account state for all
// manifest rows matching the given email.
func RemoveCQClaudeAccountsByEmail(email string) error {
	if email == "" {
		return nil
	}
	manifestPath, err := defaultCQManifestPath()
	if err != nil {
		return fmt.Errorf("resolve manifest path: %w", err)
	}
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
	BackfillCredentialsFileContext(context.Background(), acct)
}
func BackfillCredentialsFileContext(ctx context.Context, acct *ClaudeOAuth) {
	if ctx.Err() != nil {
		return
	}
	home, err := resolveCredentialHome()
	if err != nil {
		credentialDiagnostic(ctx, "credential_home_failed", "Claude credential storage could not be resolved.", "cq: backfill creds: home dir: %v\n", err)
		return
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	if ctx.Err() != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // file missing is normal (e.g. no credentials file on disk)
	}
	var creds ClaudeCredentials
	if err := json.Unmarshal(data, &creds); err != nil || creds.ClaudeAiOauth == nil {
		credentialDiagnostic(ctx, "credential_decode_failed", "Claude credential metadata could not be decoded.", "cq: backfill creds: parse credentials file\n")
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
		credentialDiagnostic(ctx, "credential_encode_failed", "Claude credential metadata could not be encoded.", "cq: backfill creds: marshal: %v\n", err)
		return
	}
	tmp := path + ".tmp"
	if err := ctx.Err(); err != nil {
		return
	}
	if err := os.WriteFile(tmp, serialised, 0o600); err != nil {
		credentialDiagnostic(ctx, "credential_write_failed", "Claude credential metadata could not be written.", "cq: write credentials tmp: %v\n", err)
		return
	}
	if err := ctx.Err(); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		credentialDiagnostic(ctx, "credential_replace_failed", "Claude credential metadata could not be replaced.", "cq: rename credentials: %v\n", err)
		os.Remove(tmp)
	}
}

var (
	updateKeychainEntryForRefresh = UpdateKeychainEntry
	storeCQAccountForRefresh      = StoreCQAccount
)

// PersistRefreshedToken updates stored Claude credentials after a successful refresh.
func PersistRefreshedToken(acct *ClaudeOAuth) {
	PersistRefreshedTokenContext(context.Background(), acct)
}
func PersistRefreshedTokenContext(ctx context.Context, acct *ClaudeOAuth) {
	if ctx.Err() != nil {
		return
	}
	cqAccount := *acct
	writeCredentials := WriteCredentialsFile
	updateKeychain := updateKeychainEntryForRefresh
	storeAccount := storeCQAccountForRefresh
	if _, observed := ctx.Value(diagnosticsKey{}).(func(string, string)); observed {
		writeCredentials = func(creds *ClaudeCredentials) error { return WriteCredentialsFileContext(ctx, creds) }
		updateKeychain = func(service string, creds *ClaudeCredentials) error {
			return updateKeychainEntryContext(ctx, service, creds)
		}
		storeAccount = func(acct *ClaudeOAuth) error { return StoreCQAccountContext(ctx, acct) }
	}

	home, err := resolveCredentialHome()
	if err == nil {
		path := filepath.Join(home, ".claude", ".credentials.json")
		if ctx.Err() != nil {
			return
		}
		data, err := os.ReadFile(path)
		if err == nil {
			var creds ClaudeCredentials
			if json.Unmarshal(data, &creds) == nil && canUpdateStoredAccount(creds.ClaudeAiOauth, acct) {
				stored := creds.ClaudeAiOauth
				updated := mergeRefreshedAccount(stored, acct)
				creds.ClaudeAiOauth = &updated
				cqAccount = updated
				if ctx.Err() != nil {
					return
				}
				if err := writeCredentials(&creds); err != nil {
					credentialDiagnostic(ctx, "credential_refresh_write_failed", "Refreshed Claude credentials could not be written.", "cq: PersistRefreshedToken: write creds: %v\n", err)
				} else if err := updateKeychain("Claude Code-credentials", &creds); err != nil {
					credentialDiagnostic(ctx, "credential_refresh_keychain_failed", "Refreshed Claude credentials could not be stored in the keychain.", "cq: PersistRefreshedToken: update keychain: %v\n", err)
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
		if ctx.Err() != nil {
			return
		}
		if err := storeAccount(&cqAccount); err != nil {
			credentialDiagnostic(ctx, "credential_refresh_store_failed", "Refreshed Claude credentials could not be stored by CQ.", "cq: PersistRefreshedToken: store cq account: %v\n", err)
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
	home, err := resolveCredentialHome()
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
	return WriteCredentialsFileContext(context.Background(), creds)
}
func WriteCredentialsFileContext(ctx context.Context, creds *ClaudeCredentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	home, err := resolveCredentialHome()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude")
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}
	// Enforce permissions even if directory pre-exists with wrong mode.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod credential dir: %w", err)
	}
	path := filepath.Join(dir, ".credentials.json")
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

type diagnosticsKey struct{}

// WithDiagnostics scopes safe best-effort credential diagnostics to one check.
func WithDiagnostics(ctx context.Context, warning func(string, string)) context.Context {
	return context.WithValue(ctx, diagnosticsKey{}, warning)
}
func credentialDiagnostic(ctx context.Context, code, message, legacy string, args ...any) {
	if warning, ok := ctx.Value(diagnosticsKey{}).(func(string, string)); ok {
		if warning != nil && ctx.Err() == nil {
			warning(code, message)
		}
		return
	}
	fmt.Fprintf(os.Stderr, legacy, args...)
}

// ClaudeRemovalResult reports writes that completed before an error.
type ClaudeRemovalResult struct{ Changed, ActiveRemoved bool }

var ErrClaudeIdentityChanged = errors.New("Claude account identity changed")

// SameClaudeIdentity accepts a stable UUID or exact token affinity for legacy
// anonymous sources. Email alone never authorises deleting a different UUID.
func SameClaudeIdentity(a, b ClaudeOAuth) bool {
	if a.AccountUUID != "" && b.AccountUUID != "" {
		return a.AccountUUID == b.AccountUUID
	}
	return a.RefreshToken != "" && a.RefreshToken == b.RefreshToken || a.AccessToken != "" && a.AccessToken == b.AccessToken
}

// RemoveClaudeAccountContext revalidates the exact selected identity, then
// removes matching platform, CQ and native sources without choosing a default.
func RemoveClaudeAccountContext(ctx context.Context, expected ClaudeOAuth) (ClaudeRemovalResult, error) {
	return removeClaudeAccountContext(ctx, expected, claudeRemovalIO{
		inspect: InspectClaudeAccounts, manifest: defaultCQManifestPath, home: resolveCredentialHome,
		read: os.ReadFile, get: gokeyring.Get, delete: gokeyring.Delete,
		saveManifest: saveManifestContext, write: WriteCredentialsFileContext, platform: removePlatformClaudeAccountContext,
	})
}

type claudeRemovalIO struct {
	inspect      func(context.Context) ([]ClaudeAccountInspection, error)
	manifest     func() (string, error)
	home         func() (string, error)
	read         func(string) ([]byte, error)
	get          func(string, string) (string, error)
	delete       func(string, string) error
	saveManifest func(context.Context, string, []manifestEntry) error
	write        func(context.Context, *ClaudeCredentials) error
	platform     func(context.Context, []ClaudeOAuth) (bool, error)
}

func removeClaudeAccountContext(ctx context.Context, expected ClaudeOAuth, ops claudeRemovalIO) (ClaudeRemovalResult, error) {
	result := ClaudeRemovalResult{}
	evidence := []ClaudeOAuth{expected}
	rows, err := ops.inspect(ctx)
	if err != nil {
		return result, err
	}
	count := 0
	for _, row := range rows {
		if strings.EqualFold(strings.TrimSpace(row.Account.Email), strings.TrimSpace(expected.Email)) {
			count++
			if !SameClaudeIdentity(row.Account, expected) {
				return result, ErrClaudeIdentityChanged
			}
		}
	}
	if count != 1 {
		return result, ErrClaudeIdentityChanged
	}
	path, err := ops.manifest()
	if err != nil {
		return result, err
	}
	data, err := ops.read(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	var entries []manifestEntry
	if err == nil && json.Unmarshal(data, &entries) != nil {
		return result, errors.New("Claude manifest unavailable")
	}
	retained := make([]manifestEntry, 0, len(entries))
	var selected []manifestEntry
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if entry.UUID == "" || entry.UUID != expected.AccountUUID && !strings.EqualFold(strings.TrimSpace(entry.Email), strings.TrimSpace(expected.Email)) {
			retained = append(retained, entry)
			continue
		}
		raw, err := ops.get(ServicePrefix+Hash8(entry.UUID), entry.UUID)
		if err != nil {
			return result, err
		}
		var credentials ClaudeOAuth
		if json.Unmarshal([]byte(raw), &credentials) != nil || !SameClaudeIdentity(credentials, expected) {
			return result, ErrClaudeIdentityChanged
		}
		selected = append(selected, entry)
		evidence = append(evidence, credentials)
	}
	home, err := ops.home()
	if err != nil {
		return result, err
	}
	native, err := ops.read(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	var current ClaudeCredentials
	if err == nil && json.Unmarshal(native, &current) != nil {
		return result, errors.New("Claude credentials unavailable")
	}
	active := current.ClaudeAiOauth != nil && SameClaudeIdentity(*current.ClaudeAiOauth, expected)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if active {
		evidence = append(evidence, *current.ClaudeAiOauth)
	}
	changed, err := ops.platform(ctx, evidence)
	result.Changed = changed
	if err != nil {
		return result, err
	}
	for _, entry := range selected {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := ops.delete(ServicePrefix+Hash8(entry.UUID), entry.UUID); err != nil && !errors.Is(err, gokeyring.ErrNotFound) {
			return result, err
		}
		result.Changed = true
	}
	if len(selected) > 0 {
		if err := ops.saveManifest(ctx, path, retained); err != nil {
			return result, err
		}
	}
	if active {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Re-read immediately before the native write; another client may have
		// selected a different account while platform cleanup was in progress.
		latest, err := ops.read(filepath.Join(home, ".claude", ".credentials.json"))
		if err != nil {
			return result, err
		}
		var credentials ClaudeCredentials
		if json.Unmarshal(latest, &credentials) != nil || credentials.ClaudeAiOauth == nil || !SameClaudeIdentity(*credentials.ClaudeAiOauth, expected) {
			return result, ErrClaudeIdentityChanged
		}
		if err := ops.write(ctx, &ClaudeCredentials{}); err != nil {
			return result, err
		}
		result.Changed, result.ActiveRemoved = true, true
	}
	return result, nil
}

// UpdateKeychainEntryContext is the context-aware variant for explicit account
// mutations. It does not print diagnostics or credential material.
func UpdateKeychainEntryContext(ctx context.Context, service string, creds *ClaudeCredentials) error {
	return updateKeychainEntryContext(ctx, service, creds)
}

// ClaudeStoreError retains durable keyring persistence when manifest publication
// fails. Callers must not imply the credential write was rolled back.
type ClaudeStoreError struct{ Err error }

func (e *ClaudeStoreError) Error() string {
	return "Claude credentials saved; manifest publication failed"
}
func (e *ClaudeStoreError) Unwrap() error { return e.Err }

func matchesClaudeEvidence(account ClaudeOAuth, evidence []ClaudeOAuth) bool {
	for _, known := range evidence {
		if SameClaudeIdentity(account, known) {
			return true
		}
	}
	return false
}
