package keyring

import (
	"os"
	"testing"
)

// File fixtures opt into setKeyringTestHome. Unconfigured tests cannot access
// the authenticated subject's native credentials on any platform.
func TestMain(m *testing.M) {
	resolveCredentialHome = func() (string, error) { return "", os.ErrNotExist }
	os.Exit(m.Run())
}
