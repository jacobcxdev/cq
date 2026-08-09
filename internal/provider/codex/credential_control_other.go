//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package codex

import "context"

func OpenCredentialControl(path string, coordinator *CredentialCoordinator) (*CredentialControl, error) {
	return OpenCredentialControlPrepared(context.Background(), path, coordinator, nil)
}

func OpenCredentialControlPrepared(context.Context, string, *CredentialCoordinator, CredentialOwnerInitializer) (*CredentialControl, error) {
	return nil, ErrCredentialControlDisabled
}

func OpenCredentialControlPreparedWithLegacyMaintenanceVerifier(context.Context, string, *CredentialCoordinator, CredentialOwnerInitializer, LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return nil, ErrCredentialControlDisabled
}

func OpenRecoveringCredentialControl(path string, coordinator *CredentialCoordinator) (*CredentialControl, error) {
	return OpenRecoveringCredentialControlPrepared(context.Background(), path, coordinator, nil)
}

func OpenRecoveringCredentialControlPrepared(context.Context, string, *CredentialCoordinator, CredentialOwnerInitializer) (*CredentialControl, error) {
	return nil, ErrCredentialControlDisabled
}

func OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifier(context.Context, string, *CredentialCoordinator, CredentialOwnerInitializer, LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return nil, ErrCredentialControlDisabled
}
