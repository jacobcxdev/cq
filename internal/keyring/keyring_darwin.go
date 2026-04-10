//go:build darwin

package keyring

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// discoverPlatformKeychain discovers existing Claude Code keychain entries on macOS.
func discoverPlatformKeychain(seen map[string]bool) []ClaudeOAuth {
	var accounts []ClaudeOAuth

	services := []string{"Claude Code-credentials"}
	for i := 2; i <= 10; i++ {
		services = append(services, fmt.Sprintf("Claude Code-credentials-%d", i))
	}

	for _, service := range services {
		out, err := exec.Command("security", "find-generic-password",
			"-s", service, "-w").Output()
		if err != nil {
			if service != "Claude Code-credentials" {
				break
			}
			continue
		}
		acct := parseKeychainEntry(strings.TrimSpace(string(out)))
		if acct == nil {
			continue
		}
		key := accountKey(acct)
		if seen[key] {
			continue
		}
		seen[key] = true
		accounts = append(accounts, *acct)
	}

	return accounts
}

func parseKeychainEntry(raw string) *ClaudeOAuth {
	if raw == "" {
		return nil
	}
	var creds ClaudeCredentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return nil
	}
	if creds.ClaudeAiOauth == nil || creds.ClaudeAiOauth.AccessToken == "" {
		return nil
	}
	return creds.ClaudeAiOauth
}

// UpdateKeychainEntry updates a macOS keychain entry with plaintext JSON,
// matching Claude Code's `security add-generic-password -w` writes.
func UpdateKeychainEntry(service string, creds *ClaudeCredentials) error {
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	return exec.Command("security", "add-generic-password",
		"-U", "-s", service, "-a", user, "-w", string(data)).Run()
}

// RemovePlatformClaudeKeychainAccountsByEmail deletes matching Claude Code
// keychain entries from the macOS login keychain.
//
// Two classes of entries are removed:
//  1. Identified entries whose Email field matches the target email.
//  2. Anonymous entries (no Email, no AccountUUID) whose RefreshToken or
//     AccessToken matches a token from an entry that was already removed in
//     this call. This covers the case where Claude Code wrote a post-refresh
//     keychain entry without identity metadata — such entries would otherwise
//     survive and be re-adopted as the target account on the next run.
func RemovePlatformClaudeKeychainAccountsByEmail(email string) error {
	if email == "" {
		return nil
	}
	services := []string{"Claude Code-credentials"}
	for i := 2; i <= 10; i++ {
		services = append(services, fmt.Sprintf("Claude Code-credentials-%d", i))
	}

	// Collect tokens from all identified entries we remove so that anonymous
	// entries sharing those tokens can be matched later.
	removedRefreshTokens := make(map[string]bool)
	removedAccessTokens := make(map[string]bool)

	// Pre-seed token sets from non-platform sources for the target email. This
	// handles the case where an anonymous platform entry appears before the
	// identified platform entry in the same service slot: without pre-seeding,
	// the anonymous entry fails the affinity check (tokens not yet recorded) and
	// the loop breaks before ever reaching the identified entry. We only seed from
	// entries that are confidently identified as the target email; we never delete
	// from these sources here — this function is the platform-keychain helper only.
	for _, acct := range discoverCredentialsFile(make(map[string]bool)) {
		if acct.Email == email {
			if acct.RefreshToken != "" {
				removedRefreshTokens[acct.RefreshToken] = true
			}
			if acct.AccessToken != "" {
				removedAccessTokens[acct.AccessToken] = true
			}
		}
	}
	for _, acct := range discoverCQKeyring(make(map[string]bool)) {
		if acct.Email == email {
			if acct.RefreshToken != "" {
				removedRefreshTokens[acct.RefreshToken] = true
			}
			if acct.AccessToken != "" {
				removedAccessTokens[acct.AccessToken] = true
			}
		}
	}

slotLoop:
	for _, service := range services {
		// A single service slot can hold multiple accounts (e.g. two OS users
		// both wrote to the same keychain service name). Loop until find returns
		// no entry matching the target email so all duplicates are cleared.
		firstForService := true
		for {
			out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
			if err != nil {
				// No more entries for this service.
				if firstForService && service != "Claude Code-credentials" {
					// Gap in the numbered slots — stop scanning further slots.
					break slotLoop
				}
				break
			}
			firstForService = false
			acct := parseKeychainEntry(strings.TrimSpace(string(out)))
			if acct == nil {
				break
			}
			if acct.Email == email {
				// Identified match — record its tokens and delete.
				if acct.RefreshToken != "" {
					removedRefreshTokens[acct.RefreshToken] = true
				}
				if acct.AccessToken != "" {
					removedAccessTokens[acct.AccessToken] = true
				}
				if err := exec.Command("security", "delete-generic-password", "-s", service).Run(); err != nil {
					return err
				}
				continue
			}
			if acct.Email == "" && acct.AccountUUID == "" {
				// Anonymous entry — check token affinity with removed accounts.
				if (acct.RefreshToken != "" && removedRefreshTokens[acct.RefreshToken]) ||
					(acct.AccessToken != "" && removedAccessTokens[acct.AccessToken]) {
					if err := exec.Command("security", "delete-generic-password", "-s", service).Run(); err != nil {
						return err
					}
					continue
				}
			}
			// Entry exists but is not the target — leave it and stop
			// looking in this slot (can't skip to a deeper entry).
			break
		}
	}
	return nil
}
