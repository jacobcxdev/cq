//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package codex

func OpenCredentialControl(string, *CredentialCoordinator) (*CredentialControl, error) {
	return nil, ErrCredentialControlDisabled
}
