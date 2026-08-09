package proxy

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// CodexLeaseAuthorityHealth is deliberately closed over evidence that is safe
// to consume. Unhealthy stores return a zero value and an error instead.
type CodexLeaseAuthorityHealth string

const CodexLeaseAuthorityHealthy CodexLeaseAuthorityHealth = "healthy"

type CodexLeaseCutoverState string

const (
	CodexLeaseCutoverStateComplete         CodexLeaseCutoverState = CodexLeaseCutoverComplete
	CodexLeaseCutoverStateLegacyQuarantine CodexLeaseCutoverState = CodexLeaseCutoverLegacyQuarantine
)

// CodexLeaseAuthorityEvidence is a freshly revalidated, privacy-safe view of
// the installed lease authority. Slice fields are detached from store memory.
type CodexLeaseAuthorityEvidence struct {
	LeaseSchemaVersion                 int
	HashVersion                        int
	CompatibilityEpoch                 int
	SourceVersion                      int
	JournalGeneration                  uint64
	AuthoritativeModeEpochs            []uint64
	ShadowModeEpochs                   []uint64
	RepresentedAuthoritativeModeEpochs []uint64
	Health                             CodexLeaseAuthorityHealth
	CutoverState                       CodexLeaseCutoverState
	CutoverAt                          time.Time
	CutoverCompletedAt                 time.Time
	CutoverCompletionGeneration        uint64
	NoLegacyAuthority                  bool
	LegacyPinnedHorizon                time.Time
	LegacyV1ArchiveDigest              string
}

type CodexLeaseAuthority interface {
	AuthorityEvidence() (CodexLeaseAuthorityEvidence, error)
}

// CompleteLegacyCutover is the sole timer-driven global lease mutation. It
// leaves the signed migration horizon pinned and advances the journal once.
func (store *CodexLeaseStore) CompleteLegacyCutover(expectedJournalGeneration uint64, modes CodexModeAuthoritySnapshot) (uint64, error) {
	if store == nil {
		return 0, ErrCodexLeaseWriterUnavailable
	}
	modes = cloneCodexModeAuthoritySnapshot(modes)
	if modes.RecognisedAuthoritativeEpochs == nil || !validCodexLeaseEpochSet(modes.RecognisedAuthoritativeEpochs) {
		return 0, fmt.Errorf("%w: malformed authoritative mode snapshot", ErrCodexLegacyQuarantine)
	}
	operation, err := store.beginOperation()
	if err != nil {
		return 0, err
	}
	defer operation.Release()

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.validateCodexLeaseAuthorityStoreLocked(); err != nil {
		return 0, err
	}
	if expectedJournalGeneration != store.v2.Generation || expectedJournalGeneration == math.MaxUint64 {
		return 0, fmt.Errorf("%w: journal generation have %d expected %d", ErrCodexLeaseStaleMutation, store.v2.Generation, expectedJournalGeneration)
	}
	cutover := store.v2.Cutover
	if cutover.SourceVersion != 1 {
		if cutover.State == CodexLeaseCutoverComplete && cutover.NoLegacyAuthority {
			return store.v2.Generation, nil
		}
		return 0, fmt.Errorf("%w: legacy completion requires source version 1", ErrCodexLeaseTrustLost)
	}
	if err := validateCodexLeaseLegacyCutoverArchive(store, cutover, store.legacyArchiveBytes); err != nil {
		store.poisoned = err
		return 0, err
	}
	if err := validateCodexLeaseCompletionModes(cutover, modes); err != nil {
		return 0, err
	}
	if err := validateCodexLeaseRepresentedModes(representedCodexLeaseAuthoritativeEpochs(store.v2.Records), modes); err != nil {
		return 0, err
	}
	if cutover.State == CodexLeaseCutoverComplete {
		if err := validateCodexLeaseCompletionTuple(*store.v2); err != nil {
			store.poisoned = err
			return 0, err
		}
		store.modes = cloneCodexModeAuthoritySnapshot(modes)
		return store.v2.Generation, nil
	}
	if cutover.State != CodexLeaseCutoverLegacyQuarantine || cutover.NoLegacyAuthority {
		err := fmt.Errorf("%w: invalid legacy cutover state", ErrCodexLeaseTrustLost)
		store.poisoned = err
		return 0, err
	}
	now := store.policy.Now().UTC()
	if now.Before(cutover.LegacyQuarantineUntil) {
		return 0, ErrCodexLegacyQuarantine
	}
	if len(store.v2.Lanes) != 0 || len(store.v2.Records) != 0 {
		return 0, fmt.Errorf("%w: legacy cutover still contains routable state", ErrCodexLegacyQuarantine)
	}

	next := cloneCodexLeaseV2Envelope(*store.v2)
	next.Cutover.State = CodexLeaseCutoverComplete
	next.Cutover.CompletedAt = now
	next.Cutover.CompletionGeneration = expectedJournalGeneration + 1
	next.Cutover.NoLegacyAuthority = true
	if err := store.commitV2Locked(expectedJournalGeneration, next); err != nil {
		return 0, err
	}
	store.modes = cloneCodexModeAuthoritySnapshot(modes)
	return store.v2.Generation, nil
}

// AuthorityEvidence never returns stale evidence alongside an error.
func (store *CodexLeaseStore) AuthorityEvidence() (CodexLeaseAuthorityEvidence, error) {
	if store == nil {
		return CodexLeaseAuthorityEvidence{}, ErrCodexLeaseWriterUnavailable
	}
	operation, err := store.beginOperation()
	if err != nil {
		return CodexLeaseAuthorityEvidence{}, err
	}
	defer operation.Release()

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.validateCodexLeaseAuthorityStoreLocked(); err != nil {
		return CodexLeaseAuthorityEvidence{}, err
	}
	envelope := cloneCodexLeaseV2Envelope(*store.v2)
	if envelope.Cutover.SourceVersion == 1 {
		if err := validateCodexLeaseLegacyCutoverArchive(store, envelope.Cutover, store.legacyArchiveBytes); err != nil {
			store.poisoned = err
			return CodexLeaseAuthorityEvidence{}, err
		}
	}
	if envelope.Cutover.State == CodexLeaseCutoverComplete {
		if err := validateCodexLeaseCompletionTuple(envelope); err != nil {
			store.poisoned = err
			return CodexLeaseAuthorityEvidence{}, err
		}
	}
	represented := representedCodexLeaseAuthoritativeEpochs(envelope.Records)
	if envelope.Cutover.State == CodexLeaseCutoverComplete {
		if err := validateCodexLeaseRepresentedModes(represented, store.modes); err != nil {
			return CodexLeaseAuthorityEvidence{}, err
		}
		if err := validateCodexLeaseCompletionModes(envelope.Cutover, store.modes); err != nil {
			return CodexLeaseAuthorityEvidence{}, fmt.Errorf("%w: %v", ErrCodexLeaseAuthorityMismatch, err)
		}
	}
	return CodexLeaseAuthorityEvidence{
		LeaseSchemaVersion:                 envelope.Version,
		HashVersion:                        envelope.HashVersion,
		CompatibilityEpoch:                 envelope.Cutover.CompatibilityEpoch,
		SourceVersion:                      envelope.Cutover.SourceVersion,
		JournalGeneration:                  envelope.Generation,
		AuthoritativeModeEpochs:            append([]uint64(nil), envelope.Cutover.AuthoritativeModeEpochs...),
		ShadowModeEpochs:                   append([]uint64(nil), envelope.Cutover.ShadowModeEpochs...),
		RepresentedAuthoritativeModeEpochs: represented,
		Health:                             CodexLeaseAuthorityHealthy,
		CutoverState:                       CodexLeaseCutoverState(envelope.Cutover.State),
		CutoverAt:                          envelope.Cutover.At,
		CutoverCompletedAt:                 envelope.Cutover.CompletedAt,
		CutoverCompletionGeneration:        envelope.Cutover.CompletionGeneration,
		NoLegacyAuthority:                  envelope.Cutover.NoLegacyAuthority,
		LegacyPinnedHorizon:                envelope.Cutover.LegacyQuarantineUntil,
		LegacyV1ArchiveDigest:              envelope.Cutover.LegacyV1SHA256,
	}, nil
}

func (store *CodexLeaseStore) validateCodexLeaseAuthorityStoreLocked() error {
	if store.closed || store.v2 == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if err := store.revalidateV2InstalledLocked(); err != nil {
		store.poisoned = err
		return err
	}
	return nil
}

func validateCodexLeaseLegacyCutoverArchive(store *CodexLeaseStore, cutover CodexLeaseCutover, archive []byte) error {
	if store == nil || cutover.SourceVersion != 1 || cutover.JournalGeneration == 0 || len(archive) == 0 || codexLeaseSHA256(archive) != cutover.LegacyV1SHA256 {
		return fmt.Errorf("%w: invalid legacy archive identity", ErrCodexLeaseTrustLost)
	}
	if err := store.validateLegacyArchive(archive, cutover.JournalGeneration-1); err != nil {
		return fmt.Errorf("%w: invalid legacy archive MAC or generation", ErrCodexLeaseTrustLost)
	}
	var legacy codexLeaseJournalEnvelope
	if err := decodeCodexLeaseStrictJSON(archive, &legacy); err != nil {
		return fmt.Errorf("%w: decode legacy archive: %v", ErrCodexLeaseTrustLost, err)
	}
	authoritative, shadow := codexLeaseV1Epochs(legacy.Records)
	if !validCodexLeaseEpochSet(cutover.AuthoritativeModeEpochs) || !validCodexLeaseEpochSet(cutover.ShadowModeEpochs) || !equalCodexEpochs(authoritative, cutover.AuthoritativeModeEpochs) || !equalCodexEpochs(shadow, cutover.ShadowModeEpochs) {
		return fmt.Errorf("%w: legacy archive mode epochs differ", ErrCodexLeaseTrustLost)
	}
	return nil
}

func validateCodexLeaseCompletionModes(cutover CodexLeaseCutover, modes CodexModeAuthoritySnapshot) error {
	recognised := modes.RecognisedAuthoritativeEpochs
	if recognised == nil || !validCodexLeaseEpochSet(recognised) {
		return fmt.Errorf("%w: malformed authoritative mode snapshot", ErrCodexLegacyQuarantine)
	}
	for _, epoch := range cutover.AuthoritativeModeEpochs {
		if !containsCodexLeaseEpoch(recognised, epoch) {
			return fmt.Errorf("%w: authoritative mode epoch %d is unrecognised", ErrCodexLegacyQuarantine, epoch)
		}
	}
	return nil
}

func validateCodexLeaseCompletionTuple(envelope codexLeaseJournalEnvelopeV2) error {
	cutover := envelope.Cutover
	if cutover.State != CodexLeaseCutoverComplete || !cutover.NoLegacyAuthority || cutover.CompletedAt.IsZero() || cutover.CompletionGeneration == 0 || cutover.CompletionGeneration > envelope.Generation {
		return fmt.Errorf("%w: incomplete lease cutover tuple", ErrCodexLeaseTrustLost)
	}
	if cutover.SourceVersion == 1 {
		if cutover.CompletedAt.Before(cutover.LegacyQuarantineUntil) || cutover.JournalGeneration == math.MaxUint64 || cutover.CompletionGeneration != cutover.JournalGeneration+1 {
			return fmt.Errorf("%w: invalid legacy completion fence", ErrCodexLeaseTrustLost)
		}
	}
	return nil
}

func validateCodexLeaseRepresentedModes(represented []uint64, modes CodexModeAuthoritySnapshot) error {
	if !validCodexLeaseEpochSet(modes.RecognisedAuthoritativeEpochs) {
		return fmt.Errorf("%w: malformed represented mode authority", ErrCodexLeaseAuthorityMismatch)
	}
	for _, epoch := range represented {
		if !containsCodexLeaseEpoch(modes.RecognisedAuthoritativeEpochs, epoch) {
			return fmt.Errorf("%w: represented authoritative epoch %d is not retained", ErrCodexLeaseAuthorityMismatch, epoch)
		}
	}
	return nil
}

func representedCodexLeaseAuthoritativeEpochs(records []CodexJournalRecordV2) []uint64 {
	set := make(map[uint64]struct{})
	for _, record := range records {
		if record.Authoritative {
			set[record.ModeEpoch] = struct{}{}
		}
	}
	return sortedCodexEpochSet(set)
}

func validCodexLeaseEpochSet(epochs []uint64) bool {
	for index, epoch := range epochs {
		if epoch == 0 || (index != 0 && epochs[index-1] >= epoch) {
			return false
		}
	}
	return true
}

func containsCodexLeaseEpoch(epochs []uint64, epoch uint64) bool {
	index := sort.Search(len(epochs), func(index int) bool { return epochs[index] >= epoch })
	return index < len(epochs) && epochs[index] == epoch
}

func cloneCodexModeAuthoritySnapshot(snapshot CodexModeAuthoritySnapshot) CodexModeAuthoritySnapshot {
	return CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: append([]uint64(nil), snapshot.RecognisedAuthoritativeEpochs...)}
}
