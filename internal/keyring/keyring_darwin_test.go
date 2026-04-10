//go:build darwin

package keyring

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeSecurityBin writes a fake `security` shell script to binDir and returns
// the path of the commands log. The script models macOS keychain semantics where
// a single service can have multiple accounts (keyed by "-a <account>").
//
// State file format: stateDir/keychain.json holds:
//
//	{ "<service>": ["<blob1>", "<blob2>", ...], ... }
//
// Commands modelled:
//
//	find-generic-password -s <service> -w      → prints first blob for service; exits 44 if absent
//	delete-generic-password -s <service>       → removes ONLY the first blob for service
//	                                             (leaving additional blobs for the same service!)
//	add-generic-password …                     → no-op success
//
// The "delete removes only the first entry" behaviour is the faithful model of
// the real macOS keychain bug that RemovePlatformClaudeKeychainAccountsByEmail
// must handle: after one delete-generic-password call, a second call to
// find-generic-password for the same service can still return a surviving entry.
//
// All invoked command lines are appended to stateDir/commands.log one per line.
func makeSecurityBin(t *testing.T, binDir, stateDir string, initial map[string][]string) string {
	t.Helper()

	// Write initial keychain state (service → ordered list of blobs).
	keychainPath := filepath.Join(stateDir, "keychain.json")
	raw, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("marshal initial keychain: %v", err)
	}
	if err := os.WriteFile(keychainPath, raw, 0o600); err != nil {
		t.Fatalf("write keychain state: %v", err)
	}

	commandsLog := filepath.Join(stateDir, "commands.log")

	script := `#!/bin/sh
KEYCHAIN="` + keychainPath + `"
COMMANDS_LOG="` + commandsLog + `"

# Record the full invocation.
echo "$*" >> "$COMMANDS_LOG"

# Locate -s argument.
service=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-s" ]; then
    service="$arg"
  fi
  prev="$arg"
done

cmd="$1"

if [ "$cmd" = "find-generic-password" ]; then
  # Return the first blob for this service; exit 44 if none.
  python3 -c "
import json,sys
d=json.load(open('` + keychainPath + `'))
k=sys.argv[1]
blobs=d.get(k,[])
if not blobs:
  sys.exit(44)
sys.stdout.write(blobs[0])
" "$service" 2>/dev/null
  exit $?
fi

if [ "$cmd" = "delete-generic-password" ]; then
  # Delete ONLY the first blob for this service (leaves the rest intact).
  python3 -c "
import json,sys
path='` + keychainPath + `'
d=json.load(open(path))
k=sys.argv[1]
blobs=d.get(k,[])
if blobs:
  d[k]=blobs[1:]  # remove only the first entry
  if not d[k]:
    del d[k]
  json.dump(d, open(path,'w'))
" "$service" 2>/dev/null
  exit 0
fi

if [ "$cmd" = "add-generic-password" ]; then
  exit 0
fi

exit 1
`

	scriptPath := filepath.Join(binDir, "security")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake security: %v", err)
	}
	return commandsLog
}

// readCommandsLog returns all command lines recorded by the fake security binary.
func readCommandsLog(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read commands log: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// credBlob builds the JSON blob that the fake security binary returns for a
// service entry with the given email and access token.
func credBlob(t *testing.T, email, accessToken string) string {
	t.Helper()
	creds := ClaudeCredentials{
		ClaudeAiOauth: &ClaudeOAuth{
			AccessToken:  accessToken,
			RefreshToken: "rt-" + email,
			ExpiresAt:    9999999999,
			Email:        email,
		},
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal cred blob: %v", err)
	}
	return string(raw)
}

// anonCredBlob builds a JSON blob with no email/UUID fields — matching the
// shape that Claude Code writes after it refreshes tokens (stripping identity
// metadata). The refreshToken is deliberately shared with an identified entry
// so that sameStoredAccount can later re-adopt the anonymous entry.
func anonCredBlob(t *testing.T, accessToken, refreshToken string) string {
	t.Helper()
	creds := ClaudeCredentials{
		ClaudeAiOauth: &ClaudeOAuth{
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    9999999999,
			// Email and AccountUUID intentionally absent — this is anonymous.
		},
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal anon cred blob: %v", err)
	}
	return string(raw)
}

// TestAnonymousPlatformEntryCanBeReidentifiedAfterRemove is a regression test
// for the anonymous-entry re-adoption gap.
//
// Scenario:
//  1. "Claude Code-credentials" holds two entries in the same service slot:
//     - slot[0]: an identified entry for user@example.com (has Email set)
//     - slot[1]: an anonymous entry (no Email, no UUID) whose RefreshToken
//     matches the identified entry — as happens when Claude Code refreshes
//     tokens and writes a new keychain entry without identity metadata.
//
//  2. RemovePlatformClaudeKeychainAccountsByEmail("user@example.com") is called.
//     It must delete slot[0] (the identified entry) and then detect slot[1] as
//     a token-affinity match (shared RefreshToken) and delete it too.
//
//  3. discoverPlatformKeychain is called afterwards.
//     Expected: returns nothing — both the identified and anonymous entries have
//     been removed.
//
//  4. If any entry survives, sameStoredAccount checks whether it can be
//     re-adopted as user@example.com, which would confirm the bug is present.
func TestAnonymousPlatformEntryCanBeReidentifiedAfterRemove(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; fake security binary requires it")
	}

	t.Run("anonymous entry survives removal and remains re-adoptable via token affinity", func(t *testing.T) {
		binDir := t.TempDir()
		stateDir := t.TempDir()

		// The identified entry has both Email and a RefreshToken.
		// The anonymous entry shares that RefreshToken (simulates a post-refresh
		// keychain write by Claude Code that dropped identity metadata).
		sharedRefreshToken := "rt-user@example.com"
		initial := map[string][]string{
			"Claude Code-credentials": {
				// slot[0]: identified entry — this is what Remove targets.
				credBlob(t, "user@example.com", "at-identified"),
				// slot[1]: anonymous entry written by Claude Code after a token
				// refresh — no Email, no UUID, but same RefreshToken.
				anonCredBlob(t, "at-anonymous-refreshed", sharedRefreshToken),
			},
		}
		makeSecurityBin(t, binDir, stateDir, initial)

		origPath := os.Getenv("PATH")
		t.Setenv("PATH", binDir+":"+origPath)

		if err := RemovePlatformClaudeKeychainAccountsByEmail("user@example.com"); err != nil {
			t.Fatalf("RemovePlatformClaudeKeychainAccountsByEmail returned error: %v", err)
		}

		// Post-removal: the anonymous entry should NOT be discoverable.
		// The removal loop records tokens from the identified entry and then
		// matches and deletes the anonymous slot[1] by token affinity.
		found := discoverPlatformKeychain(make(map[string]bool))
		if len(found) == 0 {
			// Correct outcome — no surviving entries.
			return
		}

		// If anything was found, check whether it can be re-adopted as user@example.com.
		identifiedAccount := &ClaudeOAuth{
			Email:        "user@example.com",
			AccountUUID:  "uuid-user",
			AccessToken:  "at-identified",
			RefreshToken: sharedRefreshToken,
			ExpiresAt:    9999999999,
		}
		for _, surviving := range found {
			entry := surviving // capture for clarity
			if sameStoredAccount(&entry, identifiedAccount) {
				t.Errorf("post-removal: anonymous keychain entry survived and can be re-adopted "+
					"as user@example.com via token affinity (RefreshToken match): %+v", entry)
			}
		}
		// Even if sameStoredAccount doesn't match, any surviving entry after removal
		// of user@example.com's account is itself a bug worth surfacing.
		t.Errorf("post-removal discoverPlatformKeychain returned %d surviving entry/entries "+
			"that should have been cleared: %+v", len(found), found)
	})
}

// TestRemovePlatformKeychainAnonymousSlotInDifferentService proves the gap
// extends beyond slot collocation: when an identified entry is in slot-1 and
// an anonymous entry sharing token affinity is in slot-2, removal of the
// identified entry leaves the anonymous slot-2 entry intact and re-adoptable.
func TestRemovePlatformKeychainAnonymousSlotInDifferentService(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; fake security binary requires it")
	}

	t.Run("anonymous entry in slot-2 survives removal of identified entry in slot-1", func(t *testing.T) {
		binDir := t.TempDir()
		stateDir := t.TempDir()

		sharedRefreshToken := "rt-user@example.com"
		initial := map[string][]string{
			// Slot-1 holds the identified entry.
			"Claude Code-credentials": {
				credBlob(t, "user@example.com", "at-identified"),
			},
			// Slot-2 holds an anonymous entry whose RefreshToken matches.
			"Claude Code-credentials-2": {
				anonCredBlob(t, "at-anon-refreshed", sharedRefreshToken),
			},
		}
		makeSecurityBin(t, binDir, stateDir, initial)

		origPath := os.Getenv("PATH")
		t.Setenv("PATH", binDir+":"+origPath)

		if err := RemovePlatformClaudeKeychainAccountsByEmail("user@example.com"); err != nil {
			t.Fatalf("RemovePlatformClaudeKeychainAccountsByEmail returned error: %v", err)
		}

		found := discoverPlatformKeychain(make(map[string]bool))
		if len(found) == 0 {
			// Correct outcome — nothing left to re-adopt.
			return
		}

		identifiedAccount := &ClaudeOAuth{
			Email:        "user@example.com",
			RefreshToken: sharedRefreshToken,
		}
		for _, surviving := range found {
			entry := surviving
			if sameStoredAccount(&entry, identifiedAccount) {
				t.Errorf("post-removal: anonymous entry in slot-2 survived and can be "+
					"re-adopted as user@example.com (RefreshToken match): %+v", entry)
			}
		}
		t.Errorf("post-removal discoverPlatformKeychain returned %d surviving entry/entries: %+v",
			len(found), found)
	})
}

// TestRemovePlatformKeychainLeavesNoSurvivingEntry is a regression test for the
// re-adoption gap: after RemovePlatformClaudeKeychainAccountsByEmail, a subsequent
// discoverPlatformKeychain call must not find the removed account.
//
// The fake security binary models macOS keychain semantics where a single service
// can hold multiple accounts (e.g. two OS users both wrote to "Claude Code-credentials").
// A single delete-generic-password -s <service> call removes only the FIRST entry;
// the second entry survives and is returned by the next find-generic-password call.
//
// The removal loop within each service slot repeatedly calls find-generic-password
// and delete-generic-password until the slot is empty, so all duplicate entries
// (even multiple entries for the same email under the same service) are cleared.
//
// Expected outcome: PASS — post-removal discovery returns nothing for the target email.
func TestRemovePlatformKeychainLeavesNoSurvivingEntry(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; fake security binary requires it")
	}
	t.Run("two entries under same service slot both cleared after removal", func(t *testing.T) {
		binDir := t.TempDir()
		stateDir := t.TempDir()

		// Two distinct credential blobs for the same email stored under the same
		// service (models two OS users / two Claude Code sessions writing to the
		// same keychain service name).
		initial := map[string][]string{
			"Claude Code-credentials": {
				credBlob(t, "user@example.com", "at-user-session-1"),
				credBlob(t, "user@example.com", "at-user-session-2"),
			},
		}
		makeSecurityBin(t, binDir, stateDir, initial)

		origPath := os.Getenv("PATH")
		t.Setenv("PATH", binDir+":"+origPath)

		if err := RemovePlatformClaudeKeychainAccountsByEmail("user@example.com"); err != nil {
			t.Fatalf("Remove returned error: %v", err)
		}

		// After removal, no further discovery of user@example.com is permitted.
		// The removal loop continues within a service slot until find-generic-password
		// returns no entry, so all duplicates under the same service are cleared.
		found := discoverPlatformKeychain(make(map[string]bool))
		for _, a := range found {
			if a.Email == "user@example.com" {
				t.Errorf("post-removal discoverPlatformKeychain re-adopted user@example.com "+
					"(second keychain entry survived single-delete call): %+v", a)
			}
		}
	})

	t.Run("slot-2 entry cleared when slot-1 holds different email", func(t *testing.T) {
		binDir := t.TempDir()
		stateDir := t.TempDir()

		// Slot-1 has a different user; slot-2 has the target. Production code
		// must reach and delete slot-2 even though slot-1 is a non-match.
		initial := map[string][]string{
			"Claude Code-credentials":   {credBlob(t, "different@example.com", "at-different")},
			"Claude Code-credentials-2": {credBlob(t, "user@example.com", "at-user")},
		}
		makeSecurityBin(t, binDir, stateDir, initial)

		origPath := os.Getenv("PATH")
		t.Setenv("PATH", binDir+":"+origPath)

		if err := RemovePlatformClaudeKeychainAccountsByEmail("user@example.com"); err != nil {
			t.Fatalf("Remove returned error: %v", err)
		}

		found := discoverPlatformKeychain(make(map[string]bool))
		for _, a := range found {
			if a.Email == "user@example.com" {
				t.Errorf("post-removal discovery still found user@example.com in slot-2: %+v", a)
			}
		}
		// different@example.com must still be discoverable (non-target entry preserved).
		keptDifferent := false
		for _, a := range found {
			if a.Email == "different@example.com" {
				keptDifferent = true
			}
		}
		if !keptDifferent {
			t.Errorf("different@example.com was incorrectly removed; found = %+v", found)
		}
	})
}

// TestAnonymousEntryBeforeIdentifiedBlocksRemoval is a RED-phase regression test
// for the verified blocker: when an anonymous entry occupies slot[0] and the
// identified platform entry for the target email is in slot[1], the removal
// loop currently breaks out at slot[0] (anonymous, no token affinity yet) and
// never reaches slot[1]. The identified entry survives and remains re-adoptable.
//
// Scenario:
//  1. "Claude Code-credentials" holds two blobs in order:
//     - slot[0]: anonymous blob (no Email, no UUID) whose RefreshToken matches
//     the target account's refresh token. Claude Code wrote this blob after a
//     token refresh, stripping identity metadata.
//     - slot[1]: the identified platform entry for user@example.com (has Email).
//
//  2. ~/.claude/.credentials.json is pre-seeded with user@example.com's tokens
//     (same refresh token). This mirrors real-world state and is what the
//     suspected fix will use to seed removedRefreshTokens before scanning.
//
//  3. RemovePlatformClaudeKeychainAccountsByEmail("user@example.com") is called.
//     Current behaviour: encounters slot[0] (anonymous), token-affinity check
//     fails (removedRefreshTokens still empty), breaks — slot[1] never deleted.
//     Expected behaviour after fix: identified entry in slot[1] must be deleted.
//
//  4. After removal, discoverPlatformKeychain must return nothing (or at most
//     entries that cannot be re-adopted as user@example.com). If the identified
//     entry is still there sameStoredAccount will match it — proving the bug.
func TestAnonymousEntryBeforeIdentifiedBlocksRemoval(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; fake security binary requires it")
	}

	binDir := t.TempDir()
	stateDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	const (
		targetEmail  = "user@example.com"
		sharedRT     = "rt-user@example.com" // matches what credBlob generates: "rt-" + email
		identifiedAT = "at-identified"
		anonymousAT  = "at-anonymous-refreshed"
	)

	// Seed ~/.claude/.credentials.json so the suspected fix can read token data
	// for the target email before scanning platform slots. This is required
	// because the fix direction is to pre-load removedRefreshTokens from
	// non-platform records (credentials file and/or CQ keyring).
	claudeDir := homeDir + "/.claude"
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.claude: %v", err)
	}
	seedCreds := ClaudeCredentials{
		ClaudeAiOauth: &ClaudeOAuth{
			Email:        targetEmail,
			AccountUUID:  "uuid-user",
			AccessToken:  identifiedAT,
			RefreshToken: sharedRT,
			ExpiresAt:    9999999999,
		},
	}
	seedData, err := json.Marshal(seedCreds)
	if err != nil {
		t.Fatalf("marshal seed creds: %v", err)
	}
	if err := os.WriteFile(claudeDir+"/.credentials.json", seedData, 0o600); err != nil {
		t.Fatalf("write seed creds: %v", err)
	}

	// Platform keychain state: anonymous blob is FIRST, identified blob is SECOND.
	// This ordering is the core of the blocker — macOS keychain ordering is not
	// under cq's control, so this arrangement is plausible in production.
	initial := map[string][]string{
		"Claude Code-credentials": {
			// slot[0]: anonymous — no Email, no UUID; shares RefreshToken with target.
			anonCredBlob(t, anonymousAT, sharedRT),
			// slot[1]: identified platform entry for user@example.com.
			credBlob(t, targetEmail, identifiedAT),
		},
	}
	makeSecurityBin(t, binDir, stateDir, initial)

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+":"+origPath)

	if err := RemovePlatformClaudeKeychainAccountsByEmail(targetEmail); err != nil {
		t.Fatalf("RemovePlatformClaudeKeychainAccountsByEmail returned error: %v", err)
	}

	// Post-removal: discoverPlatformKeychain must not return any entry that
	// can be re-adopted as user@example.com.
	found := discoverPlatformKeychain(make(map[string]bool))

	// Build the reference account representing what the removal targeted.
	// sameStoredAccount will return true for any surviving entry that shares
	// the email, UUID, refresh token, or access token with the identified entry.
	identified := &ClaudeOAuth{
		Email:        targetEmail,
		AccountUUID:  "uuid-user",
		AccessToken:  identifiedAT,
		RefreshToken: sharedRT,
		ExpiresAt:    9999999999,
	}

	for _, surviving := range found {
		entry := surviving
		if sameStoredAccount(&entry, identified) {
			t.Errorf(
				"BUG: anonymous entry in slot[0] blocked removal of identified entry in slot[1]; "+
					"surviving entry can be re-adopted as %s via token affinity: %+v",
				targetEmail, entry,
			)
		}
	}

	if len(found) > 0 {
		// Any surviving entry after targeted removal is itself a diagnostic signal.
		// Fail with the full set so the reader can see what was left behind.
		t.Errorf(
			"post-removal discoverPlatformKeychain returned %d surviving entry/entries "+
				"(expected 0 after full removal of %s): %+v",
			len(found), targetEmail, found,
		)
	}
}

// ── parseKeychainEntry ────────────────────────────────────────────────────────

func TestParseKeychainEntry(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		if got := parseKeychainEntry(""); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("valid JSON with access token returns entry", func(t *testing.T) {
		raw := `{"claudeAiOauth":{"accessToken":"tok123","refreshToken":"rt","expiresAt":9999}}`
		got := parseKeychainEntry(raw)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.AccessToken != "tok123" {
			t.Errorf("AccessToken = %q, want tok123", got.AccessToken)
		}
		if got.RefreshToken != "rt" {
			t.Errorf("RefreshToken = %q, want rt", got.RefreshToken)
		}
	})

	t.Run("invalid JSON returns nil", func(t *testing.T) {
		if got := parseKeychainEntry("{not valid json}"); got != nil {
			t.Errorf("expected nil for invalid JSON, got %+v", got)
		}
	})

	t.Run("missing claudeAiOauth key returns nil", func(t *testing.T) {
		if got := parseKeychainEntry(`{"other":"value"}`); got != nil {
			t.Errorf("expected nil for missing claudeAiOauth, got %+v", got)
		}
	})

	t.Run("claudeAiOauth present but empty access token returns nil", func(t *testing.T) {
		raw := `{"claudeAiOauth":{"accessToken":"","refreshToken":"rt","expiresAt":100}}`
		if got := parseKeychainEntry(raw); got != nil {
			t.Errorf("expected nil for empty accessToken, got %+v", got)
		}
	})
}
