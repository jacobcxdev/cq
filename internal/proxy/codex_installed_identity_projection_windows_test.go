//go:build windows

package proxy

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCodexInstalledProtectedIdentityWindowsUsesFullFileID(t *testing.T) {
	first := fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 3, FileID: [16]byte{15: 9}}
	second := first
	second.FileID[15] = 10
	var firstEncoded bytes.Buffer
	var secondEncoded bytes.Buffer
	writeCodexInstalledProtectedIdentityDomain(&firstEncoded)
	writeCodexInstalledProtectedIdentity(&firstEncoded, first)
	writeCodexInstalledProtectedIdentityDomain(&secondEncoded)
	writeCodexInstalledProtectedIdentity(&secondEncoded, second)
	if bytes.Equal(firstEncoded.Bytes(), secondEncoded.Bytes()) {
		t.Fatal("different Windows file IDs produced identical projection")
	}
	const wantIdentity = "00000000000000010000000000000000000000000000000900000000000000020000000000000003"
	domainLength := len("cq/codex-installed/protected-identity/windows/v1\x00")
	if got := hex.EncodeToString(firstEncoded.Bytes()[domainLength:]); got != wantIdentity {
		t.Fatalf("Windows identity projection = %s, want %s", got, wantIdentity)
	}
}
