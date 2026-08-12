package proxy

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/compat"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

const freshCodexLeaseStageSuffix = ".fresh-v2"

// InitialiseCodexContinuityAuthority creates or recovers the crash-safe fresh
// v2 key/journal pair. The caller must subsequently open the coordinator; this
// function never changes the mutation-free both-missing contract of Open.
func InitialiseCodexContinuityAuthority(options CodexContinuityOpenOptions, owner CodexLeaseWriterAuthority) error {
	if options.FS == nil || !freshCodexLeaseValidPath(options.KeyPath) || !freshCodexLeaseValidPath(options.JournalPath) || filepath.Dir(options.KeyPath) != filepath.Dir(options.JournalPath) || filepath.Base(options.KeyPath) == filepath.Base(options.JournalPath) {
		return errors.New("Codex lease journal and key require distinct names in one state directory")
	}
	if options.Policy.Retention <= 0 || options.Policy.Now == nil {
		return errors.New("Codex lease policy requires positive retention and a clock")
	}
	if !validCodexModeSnapshot(cloneCodexModeSnapshot(options.Modes)) {
		return fmt.Errorf("%w: missing or non-canonical mode authority snapshot", ErrCodexLeaseTrustLost)
	}
	if compat.CurrentEpoch != 4 {
		return fmt.Errorf("%w: unsupported compatibility floor %d", ErrCodexLeaseTrustLost, compat.CurrentEpoch)
	}
	keyName := filepath.Base(options.KeyPath)
	journalName := filepath.Base(options.JournalPath)
	stageKeyName := filepath.Base(freshCodexLeaseStagePath(options.KeyPath))
	stageJournalName := filepath.Base(freshCodexLeaseStagePath(options.JournalPath))
	if !freshCodexLeaseNamesDistinct(keyName, journalName, stageKeyName, stageJournalName) {
		return fmt.Errorf("%w: fresh Codex lease authority names collide", ErrCodexLeaseTrustLost)
	}

	operation, err := beginCodexLeaseOwnerOperation(owner)
	if err != nil {
		return err
	}
	defer operation.Release()

	inspector, ok := options.FS.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := options.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	directoryPath := filepath.Dir(options.JournalPath)
	if err := fsutil.EnsureSecureDirectory(options.FS, directoryPath); err != nil {
		return freshCodexLeaseTrustError("secure authority directory", err)
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return freshCodexLeaseTrustError("open authority directory", err)
	}
	defer directory.Close()
	directoryID, err := codexLeaseDirectoryIdentity(inspector, directory, directoryPath)
	if err != nil {
		return freshCodexLeaseTrustError("validate authority directory", err)
	}
	validateDirectory := func() error {
		identity, err := codexLeaseDirectoryIdentity(inspector, directory, directoryPath)
		if err != nil || !sameCodexLeaseObject(identity, directoryID) {
			return fmt.Errorf("%w: authority directory identity", ErrCodexLeaseTrustLost)
		}
		return nil
	}

	names := []string{keyName, journalName, stageKeyName, stageJournalName}
	var present [4]bool
	for index, name := range names {
		present[index], err = freshCodexLeaseEntryExists(directory, name)
		if err != nil {
			return freshCodexLeaseTrustError("inspect authority entry", err)
		}
	}
	canonicalKey, canonicalJournal, stagedKey, stagedJournal := present[0], present[1], present[2], present[3]
	if canonicalKey && canonicalJournal && !stagedKey && !stagedJournal {
		return validateFreshCodexLeaseInstalledPair(inspector, directory, keyName, journalName, validateDirectory)
	}
	if !freshCodexLeaseRecoverableShape(canonicalKey, canonicalJournal, stagedKey, stagedJournal) {
		return fmt.Errorf("%w: impossible fresh Codex lease authority shape", ErrCodexLeaseTrustLost)
	}

	if !canonicalKey && !stagedKey {
		key := make([]byte, codexLeaseHMACKeyBytes)
		if _, err := rand.Read(key); err != nil {
			clear(key)
			return errors.New("generate fresh Codex lease authority")
		}
		err := fsutil.SecureAtomicCreateInDirectoryChecked(inspector, directory, stageKeyName, key, validateDirectory)
		clear(key)
		if err != nil {
			return err
		}
		if err := validateDirectory(); err != nil {
			return freshCodexLeaseTrustError("revalidate authority directory after key stage", err)
		}
	}
	keySourceName := stageKeyName
	if canonicalKey {
		keySourceName = keyName
	}
	key, keyID, err := freshCodexLeaseReadKey(inspector, directory, keySourceName)
	if err != nil {
		return freshCodexLeaseTrustError("validate authority key", err)
	}
	defer clear(key)

	if !stagedJournal {
		journal, err := freshCodexLeaseJournal(key, options)
		if err != nil {
			return freshCodexLeaseTrustError("construct staged journal", err)
		}
		err = fsutil.SecureAtomicCreateInDirectoryChecked(inspector, directory, stageJournalName, journal, validateDirectory)
		clear(journal)
		if err != nil {
			return err
		}
		if err := validateDirectory(); err != nil {
			return freshCodexLeaseTrustError("revalidate authority directory after journal stage", err)
		}
	}
	journal, journalID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, stageJournalName, codexLeaseJournalMaxBytes)
	if err != nil {
		return freshCodexLeaseTrustError("read staged journal", err)
	}
	defer clear(journal)
	if err := validateFreshCodexLeaseJournal(key, journal); err != nil {
		return freshCodexLeaseTrustError("validate staged journal", err)
	}

	if !canonicalKey {
		if err := fsutil.SecurePromoteNoReplaceInDirectoryChecked(inspector, directory, stageKeyName, keyName, key, keyID, validateDirectory); err != nil {
			return err
		}
		if err := validateDirectory(); err != nil {
			return freshCodexLeaseTrustError("revalidate authority directory after key promotion", err)
		}
	}
	if err := fsutil.SecurePromoteNoReplaceInDirectoryChecked(inspector, directory, stageJournalName, journalName, journal, journalID, validateDirectory); err != nil {
		return err
	}
	if err := validateDirectory(); err != nil {
		return freshCodexLeaseTrustError("revalidate authority directory after journal promotion", err)
	}
	return nil
}

func freshCodexLeaseStagePath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+freshCodexLeaseStageSuffix)
}

func freshCodexLeaseValidPath(path string) bool {
	return path != "" && filepath.Clean(path) == path && filepath.Join(filepath.Dir(path), filepath.Base(path)) == path
}

func freshCodexLeaseNamesDistinct(names ...string) bool {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := seen[name]; exists {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func freshCodexLeaseEntryExists(directory fsutil.SecureDirectory, name string) (bool, error) {
	file, err := directory.OpenNoFollow(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := file.Stat(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func freshCodexLeaseRecoverableShape(canonicalKey, canonicalJournal, stagedKey, stagedJournal bool) bool {
	return (!canonicalKey && !canonicalJournal && !stagedKey && !stagedJournal) ||
		(!canonicalKey && !canonicalJournal && stagedKey && !stagedJournal) ||
		(!canonicalKey && !canonicalJournal && stagedKey && stagedJournal) ||
		(canonicalKey && !canonicalJournal && !stagedKey && stagedJournal)
}

func freshCodexLeaseReadKey(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string) ([]byte, fsutil.SecureFileIdentity, error) {
	key, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, name, codexLeaseHMACKeyBytes+1)
	if err != nil {
		return nil, fsutil.SecureFileIdentity{}, err
	}
	if len(key) != codexLeaseHMACKeyBytes {
		clear(key)
		return nil, fsutil.SecureFileIdentity{}, errors.New("invalid fresh Codex lease key length")
	}
	return key, identity, nil
}

func validateFreshCodexLeaseInstalledPair(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, keyName, journalName string, validateDirectory func() error) error {
	key, keyID, err := freshCodexLeaseReadKey(inspector, directory, keyName)
	if err != nil {
		return freshCodexLeaseTrustError("validate installed key", err)
	}
	defer clear(key)
	journal, journalID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, journalName, codexLeaseJournalMaxBytes)
	if err != nil {
		return freshCodexLeaseTrustError("validate installed journal", err)
	}
	defer clear(journal)
	if err := validateDirectory(); err != nil {
		return freshCodexLeaseTrustError("validate installed authority directory", err)
	}
	if err := directory.Sync(); err != nil {
		return freshCodexLeaseTrustError("sync installed authority", fmt.Errorf("%w: %v", fsutil.ErrCommitIndeterminate, err))
	}
	installedKey, installedKeyID, err := freshCodexLeaseReadKey(inspector, directory, keyName)
	if err != nil || installedKeyID != keyID || !bytes.Equal(installedKey, key) {
		clear(installedKey)
		return freshCodexLeaseTrustError("revalidate installed key", err)
	}
	clear(installedKey)
	installedJournal, installedJournalID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, journalName, codexLeaseJournalMaxBytes)
	if err != nil || installedJournalID != journalID || !bytes.Equal(installedJournal, journal) {
		clear(installedJournal)
		return freshCodexLeaseTrustError("revalidate installed journal", err)
	}
	clear(installedJournal)
	if err := validateDirectory(); err != nil {
		return freshCodexLeaseTrustError("revalidate installed authority directory", err)
	}
	return nil
}

func freshCodexLeaseJournal(key []byte, options CodexContinuityOpenOptions) ([]byte, error) {
	now := options.Policy.Now().UTC()
	envelope := codexLeaseJournalEnvelopeV2{
		Version:     codexLeaseJournalVersionV3,
		HashVersion: codexLeaseHashVersion,
		Generation:  1,
		Cutover: CodexLeaseCutover{
			SourceVersion:        0,
			CompatibilityEpoch:   compat.CurrentEpoch,
			State:                CodexLeaseCutoverComplete,
			At:                   now,
			JournalGeneration:    1,
			CompletedAt:          now,
			CompletionGeneration: 1,
			NoLegacyAuthority:    true,
		},
		Lanes:   []CodexJournalLane{},
		Records: []CodexJournalRecordV2{},
	}
	store := &CodexLeaseStore{key: append([]byte(nil), key...)}
	defer clear(store.key)
	journal, err := store.marshalV2Envelope(envelope)
	if err != nil {
		return nil, err
	}
	if err := validateFreshCodexLeaseJournal(key, journal); err != nil {
		clear(journal)
		return nil, err
	}
	return journal, nil
}

func validateFreshCodexLeaseJournal(key, journal []byte) error {
	store := &CodexLeaseStore{key: append([]byte(nil), key...)}
	defer clear(store.key)
	var envelope codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(journal, &envelope); err != nil {
		return err
	}
	if err := store.validateV2Envelope(envelope); err != nil {
		return err
	}
	cutover := envelope.Cutover
	if envelope.Version != codexLeaseJournalVersionV3 || envelope.HashVersion != codexLeaseHashVersion || envelope.Generation != 1 ||
		cutover.SourceVersion != 0 || cutover.CompatibilityEpoch != compat.CurrentEpoch || cutover.State != CodexLeaseCutoverComplete || cutover.At.IsZero() || cutover.JournalGeneration != 1 ||
		len(cutover.AuthoritativeModeEpochs) != 0 || len(cutover.ShadowModeEpochs) != 0 || !cutover.LegacyQuarantineUntil.IsZero() || cutover.LegacyV1SHA256 != "" || cutover.CompletedAt != cutover.At || cutover.CompletionGeneration != 1 || !cutover.NoLegacyAuthority ||
		len(envelope.Lanes) != 0 || len(envelope.Records) != 0 {
		return errors.New("invalid fresh Codex lease genesis")
	}
	canonical, err := store.marshalV2Envelope(envelope)
	if err != nil {
		return err
	}
	defer clear(canonical)
	if !bytes.Equal(journal, canonical) {
		return errors.New("non-canonical fresh Codex lease genesis")
	}
	return nil
}

func freshCodexLeaseTrustError(operation string, err error) error {
	if err == nil {
		err = errors.New("authority validation failed")
	}
	return fmt.Errorf("%w: %s: %w", ErrCodexLeaseTrustLost, operation, err)
}
