package proxy

import (
	"crypto/rand"
	"errors"
	"io"
	"os"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// Secret material lives only in an unlinked private descriptor, allowing a
// failed candidate to exec its predecessor without persisting an auth key.
func NewRuntimeUpgradeSecretFile(root string) (*os.File, *RuntimeSecret, error) {
	if err := fsutil.EnsureSecureDirectory(fsutil.OSFileSystem{}, root); err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(root, ".upgrade-secret-")
	if err != nil {
		return nil, nil, err
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return nil, nil, err
	}
	material := make([]byte, RuntimeSecretSize)
	defer zeroRuntimeBytes(material)
	if _, err := io.ReadFull(rand.Reader, material); err != nil {
		file.Close()
		return nil, nil, err
	}
	if _, err := file.Write(material); err != nil {
		file.Close()
		return nil, nil, err
	}
	secret, err := NewRuntimeSecret(material)
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, secret, nil
}
func ReadRuntimeUpgradeSecretFile(file *os.File) (*RuntimeSecret, error) {
	if file == nil {
		return nil, ErrRuntimeControlFrame
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != RuntimeSecretSize {
		return nil, ErrRuntimeControlFrame
	}
	material := make([]byte, RuntimeSecretSize)
	defer zeroRuntimeBytes(material)
	if _, err := file.ReadAt(material, 0); err != nil {
		return nil, errors.Join(ErrRuntimeControlFrame, err)
	}
	return NewRuntimeSecret(material)
}
