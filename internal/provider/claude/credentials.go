package claude

import (
	"github.com/jacobcxdev/cq/internal/keyring"
)

// persistRefreshedToken updates stored credentials after a successful token
// refresh. Tests may replace it with a stub to keep persistence hermetic.
var persistRefreshedToken = keyring.PersistRefreshedToken

// backfillCredentialsFile updates the credentials file and cross-platform
// keyring with profile data (email, UUID, plan, tier). Tests may replace it
// with a stub to keep persistence hermetic.
var backfillCredentialsFile = func(acct *keyring.ClaudeOAuth) error {
	keyring.BackfillCredentialsFile(acct)
	if acct.AccountUUID != "" {
		return keyring.StoreCQAccount(acct)
	}
	return nil
}
