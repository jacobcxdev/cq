//go:build !windows

package codex

func (s *ManagedStore) nativeDurableReplace(string, []byte) (bool, error) {
	return false, nil
}

func (s *ManagedStore) recoverManagedPermissions() error { return nil }

func (s *ManagedStore) nativeReadManaged(string) ([]byte, bool, error) { return nil, false, nil }
