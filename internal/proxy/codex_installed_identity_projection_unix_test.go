//go:build unix

package proxy

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCodexInstalledProtectedIdentityUnixGolden(t *testing.T) {
	identity := fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 3, FileID: [16]byte{15: 9}}
	var encoded bytes.Buffer
	writeCodexInstalledProtectedIdentityDomain(&encoded)
	writeCodexInstalledProtectedIdentity(&encoded, identity)
	const want = "000000000000000100000000000000020000000000000003"
	if got := hex.EncodeToString(encoded.Bytes()); got != want {
		t.Fatalf("Unix identity projection = %s, want %s", got, want)
	}
}
