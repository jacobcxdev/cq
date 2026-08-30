//go:build windows

package proxy

import (
	"encoding/binary"
	"io"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func writeCodexInstalledProtectedIdentityDomain(writer io.Writer) {
	_, _ = writer.Write([]byte("cq/codex-installed/protected-identity/windows/v1\x00"))
}

func writeCodexInstalledProtectedIdentity(writer io.Writer, identity fsutil.SecureFileIdentity) {
	var encoded [5 * 8]byte
	binary.BigEndian.PutUint64(encoded[0:8], identity.Device)
	copy(encoded[8:24], identity.FileID[:])
	binary.BigEndian.PutUint64(encoded[24:32], identity.Inode)
	binary.BigEndian.PutUint64(encoded[32:40], identity.Links)
	_, _ = writer.Write(encoded[:])
}
