package claude

import (
	"github.com/jacobcxdev/cq/internal/keyring"
)

// persistRefreshedToken updates stored credentials after a successful token
// refresh. It delegates to keyring.PersistRefreshedToken using the shared
// keyring.ClaudeOAuth type.
func persistRefreshedToken(acct *keyring.ClaudeOAuth) {
	keyring.PersistRefreshedToken(acct)
}

// backfillCredentialsFile updates the credentials file and cross-platform
// keyring with profile data (email, UUID, plan, tier). It delegates to the
// keyring package functions which handle the file I/O.
func backfillCredentialsFile(acct *keyring.ClaudeOAuth) error {
	keyring.BackfillCredentialsFile(acct)
	if acct.AccountUUID != "" {
		return keyring.StoreCQAccount(acct)
	}
	return nil
}
