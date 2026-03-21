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
// Credentials are passed via stdin (not as a -w argument value) to prevent
// exposure in process listings (ps output).
func UpdateKeychainEntry(service string, creds *ClaudeCredentials) error {
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	cmd := exec.Command("security", "add-generic-password",
		"-U", "-s", service, "-a", user, "-w")
	cmd.Stdin = strings.NewReader(string(data))
	return cmd.Run()
}
