//go:build unix && !darwin && !linux

package fsutil

func renameNoReplaceAt(int, string, int, string) error {
	return ErrSecureCapabilityUnavailable
}
