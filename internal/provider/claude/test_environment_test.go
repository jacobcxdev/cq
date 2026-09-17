package claude

import (
	"os"
	"testing"

	"github.com/jacobcxdev/cq/internal/keyring"
)

// Provider tests exercise provider behaviour. Keyring tests exercise real
// credential file writes against explicitly injected temporary homes.
func TestMain(m *testing.M) {
	resolveActiveCredentialHome = func() (string, error) { return "", os.ErrNotExist }
	discoverClaudeAccounts = func() []keyring.ClaudeOAuth { return nil }
	persistRefreshedToken = func(*keyring.ClaudeOAuth) {}
	backfillCredentialsFile = func(*keyring.ClaudeOAuth) error { return nil }
	os.Exit(m.Run())
}
