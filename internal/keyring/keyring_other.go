//go:build !darwin

package keyring

import "context"

// discoverPlatformKeychain is a no-op on non-macOS platforms.
// Account discovery uses the credentials file and cq-managed go-keyring entries.
func discoverPlatformKeychain(seen map[string]bool) []ClaudeOAuth {
	return discoverPlatformKeychainContext(context.Background(), seen)
}
func discoverPlatformKeychainContext(ctx context.Context, seen map[string]bool) []ClaudeOAuth {
	return nil
}

// UpdateKeychainEntry is a no-op on non-macOS platforms.
// Use StoreCQAccount for cross-platform keyring storage.
func UpdateKeychainEntry(service string, creds *ClaudeCredentials) error {
	return updateKeychainEntryContext(context.Background(), service, creds)
}
func updateKeychainEntryContext(ctx context.Context, service string, creds *ClaudeCredentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// RemovePlatformClaudeKeychainAccountsByEmail is a no-op on non-macOS platforms.
func RemovePlatformClaudeKeychainAccountsByEmail(email string) error {
	return nil
}
