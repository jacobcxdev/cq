//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package codex

import "context"

func legacyCredentialEndpointIdentityOwnerIsCurrent(uint64) bool { return true }

func InspectLegacyCredentialEndpoint(context.Context, string) (LegacyCredentialEndpointSnapshot, error) {
	return LegacyCredentialEndpointSnapshot{}, ErrCredentialEndpointMaintenanceUnsupported
}

func InspectLegacyCredentialEndpointTransition(context.Context, string) (LegacyCredentialEndpointTransitionStatus, error) {
	return LegacyCredentialEndpointTransitionStatus{}, ErrCredentialEndpointMaintenanceUnsupported
}

func PrepareLegacyCredentialEndpointTransition(context.Context, string, LegacyCredentialEndpointSnapshot, DrainAuthority) (*LegacyCredentialEndpointTransition, error) {
	return nil, ErrCredentialEndpointMaintenanceUnsupported
}

func ResumeLegacyCredentialEndpointTransition(context.Context, string, LegacyCredentialEndpointTransitionTicket, DrainAuthority) (*LegacyCredentialEndpointTransition, error) {
	return nil, ErrCredentialEndpointMaintenanceUnsupported
}
