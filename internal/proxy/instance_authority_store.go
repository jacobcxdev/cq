package proxy

import (
	"context"
	"errors"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

var instanceAuthorityExternalReferenceKinds = [...]string{
	"candidate_authority_terminal_v1",
	"candidate_removal_terminal_v1",
	"receipt_export_terminal_v1",
	"canonical_import_terminal_v1",
	"import_finalisation_terminal_v1",
	"promotion_terminal_v1",
	"release_history_terminal_v1",
	"runtime_stage_history_terminal_v1",
}

func InstanceAuthorityExternalReferenceKinds() []string {
	return append([]string(nil), instanceAuthorityExternalReferenceKinds[:]...)
}

func ValidInstanceAuthorityExternalReferenceKind(kind string) bool {
	for _, candidate := range instanceAuthorityExternalReferenceKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// InstanceAuthorityStore closes controller activation and staged release.
type InstanceAuthorityStore struct {
	coordinator *OperationCoordinatorStore
}

func OpenInstanceAuthorityStore(ctx context.Context, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, publisher DurableObjectPublisher, key []byte, hook func(string) error) (*InstanceAuthorityStore, error) {
	coordinator, err := OpenOperationCoordinatorStore(ctx, inspector, directory, publisher, key, hook)
	if err != nil {
		return nil, err
	}
	return &InstanceAuthorityStore{coordinator: coordinator}, nil
}

func (s *InstanceAuthorityStore) InitialiseController(instanceID string) error {
	if instanceID == "" {
		return errors.New("instance identity unavailable")
	}
	if _, exists := s.coordinator.SelectedPhase(); exists {
		return errors.New("instance controller already initialised")
	}
	return s.coordinator.PublishIntent(instanceID, "controller-initialised")
}

func (s *InstanceAuthorityStore) ActivateFeature(instanceID string) error {
	if phase, exists := s.coordinator.SelectedPhase(); !exists || phase != "intent" {
		return errors.New("instance controller is not initialised")
	}
	return s.coordinator.PublishAnchor(instanceID, "controller-initialised")
}

func (s *InstanceAuthorityStore) StageRelease(instanceID string) error {
	if phase, exists := s.coordinator.SelectedPhase(); !exists || phase != "anchor" {
		return errors.New("instance feature is not active")
	}
	return s.coordinator.PublishReceipt(instanceID, "release-staged")
}

func (s *InstanceAuthorityStore) Release(instanceID string) error {
	if phase, exists := s.coordinator.SelectedPhase(); !exists || phase != "receipt" {
		return errors.New("instance release is not staged")
	}
	return s.coordinator.PublishTerminal(instanceID, "released")
}
