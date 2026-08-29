//go:build unix

package proxy

import (
	"encoding/binary"
	"io"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func writeCodexInstalledProtectedIdentityDomain(io.Writer) {}

func writeCodexInstalledProtectedIdentity(writer io.Writer, identity fsutil.SecureFileIdentity) {
	var encoded [3 * 8]byte
	binary.BigEndian.PutUint64(encoded[0:8], identity.Device)
	binary.BigEndian.PutUint64(encoded[8:16], identity.Inode)
	binary.BigEndian.PutUint64(encoded[16:24], identity.Links)
	_, _ = writer.Write(encoded[:])
}
