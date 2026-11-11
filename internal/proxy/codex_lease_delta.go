package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const codexLeaseStorageVersion = 4
const codexLeaseHeadMaxBytes = 4096
const codexLeaseSegmentMaxBytes = 64 << 10

type codexLeaseStorageHead struct {
	Version    int    `json:"version"`
	Generation uint64 `json:"generation"`
	Checkpoint string `json:"checkpoint"`
	Tail       string `json:"tail,omitempty"`
	DeltaBytes uint64 `json:"delta_bytes"`
	MAC        string `json:"mac"`
	StateMAC   string `json:"state_mac"`
}

type codexLeaseDelta struct {
	Version       int                          `json:"version"`
	Before        uint64                       `json:"before"`
	Metadata      codexLeaseJournalEnvelopeV2  `json:"metadata"`
	Lanes         []CodexJournalLane           `json:"lanes,omitempty"`
	DeleteLanes   []string                     `json:"delete_lanes,omitempty"`
	Records       []CodexJournalRecordV2       `json:"records,omitempty"`
	DeleteRecords []CodexJournalRecordIdentity `json:"delete_records,omitempty"`
	MAC           string                       `json:"mac"`
}

type codexLeaseSegment struct {
	Version  int               `json:"version"`
	Previous string            `json:"previous,omitempty"`
	Deltas   []codexLeaseDelta `json:"deltas"`
	MAC      string            `json:"mac"`
}

func (store *CodexLeaseStore) signSegment(segment codexLeaseSegment) ([]byte, error) {
	segment.MAC = ""
	mac, err := store.storageMAC("segment-v1", segment)
	if err != nil {
		return nil, err
	}
	segment.MAC = mac
	return json.Marshal(segment)
}
func (store *CodexLeaseStore) decodeSegment(data []byte) (codexLeaseSegment, error) {
	var segment codexLeaseSegment
	if err := decodeCodexLeaseStorageJSON(data, &segment); err != nil {
		return segment, err
	}
	signed, err := store.signSegment(segment)
	if err != nil || !bytes.Equal(signed, data) || segment.Version != 1 || len(segment.Deltas) == 0 || (segment.Previous != "" && !validCodexStorageDigest(segment.Previous)) {
		return segment, errors.New("invalid Codex journal segment")
	}
	for index, delta := range segment.Deltas {
		encoded, err := json.Marshal(delta)
		if err != nil {
			return segment, err
		}
		if _, err := store.decodeDelta(encoded); err != nil {
			return segment, err
		}
		if index > 0 && delta.Before != segment.Deltas[index-1].Metadata.Generation {
			return segment, errors.New("non-contiguous Codex journal segment")
		}
	}
	return segment, nil
}

type codexLeaseStorageReference struct {
	data     []byte
	identity fsutil.SecureFileIdentity
}

type codexLeaseDeltaStorage struct {
	head      codexLeaseStorageHead
	headBytes []byte
	refs      map[string]codexLeaseStorageReference
}

func (store *CodexLeaseStore) storageMAC(domain string, unsigned any) (string, error) {
	data, err := json.Marshal(unsigned)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, store.key)
	_, _ = mac.Write([]byte("cq-codex-lease-" + domain + "\x00"))
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (store *CodexLeaseStore) signStorageHead(head codexLeaseStorageHead) ([]byte, error) {
	head.MAC = ""
	mac, err := store.storageMAC("head-v4", head)
	if err != nil {
		return nil, err
	}
	head.MAC = mac
	return json.Marshal(head)
}

func (store *CodexLeaseStore) signDelta(delta codexLeaseDelta) ([]byte, error) {
	delta.MAC = ""
	mac, err := store.storageMAC("delta-v1", delta)
	if err != nil {
		return nil, err
	}
	delta.MAC = mac
	return json.Marshal(delta)
}

func decodeCodexLeaseStorageJSON(data []byte, destination any) error {
	if err := decodeCodexLeaseStrictJSON(data, destination); err != nil {
		return err
	}
	canonical, err := json.Marshal(destination)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, data) {
		return errors.New("non-canonical Codex journal storage")
	}
	return nil
}

func validCodexStorageDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == digest
}

func (store *CodexLeaseStore) storagePrefix() string {
	sum := sha256.Sum256([]byte(store.journalName))
	return ".cq-lease-" + hex.EncodeToString(sum[:]) + "."
}
func (store *CodexLeaseStore) storageName(kind, digest string) string {
	return store.storagePrefix() + kind + "-" + digest
}

func (store *CodexLeaseStore) readStorageReference(kind, digest string) (codexLeaseStorageReference, error) {
	if !validCodexStorageDigest(digest) {
		return codexLeaseStorageReference{}, fmt.Errorf("%w: invalid storage digest", ErrCodexLeaseTrustLost)
	}
	name := store.storageName(kind, digest)
	data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, name, codexLeaseJournalMaxBytes)
	if err != nil || codexLeaseSHA256(data) != digest {
		return codexLeaseStorageReference{}, fmt.Errorf("%w: invalid %s reference: %v", ErrCodexLeaseTrustLost, kind, err)
	}
	return codexLeaseStorageReference{data: data, identity: identity}, nil
}

func (store *CodexLeaseStore) decodeDelta(data []byte) (codexLeaseDelta, error) {
	var delta codexLeaseDelta
	if err := decodeCodexLeaseStorageJSON(data, &delta); err != nil {
		return delta, err
	}
	got := delta.MAC
	delta.MAC = ""
	want, err := store.storageMAC("delta-v1", delta)
	delta.MAC = got
	if err != nil || !hmac.Equal([]byte(want), []byte(got)) {
		return delta, errors.New("Codex journal delta MAC mismatch")
	}
	if delta.Version != 1 || delta.Before == math.MaxUint64 || delta.Metadata.Generation != delta.Before+1 || delta.Metadata.Version != codexLeaseJournalVersionV3 || delta.Metadata.HashVersion != codexLeaseHashVersion || delta.Metadata.Lanes != nil || delta.Metadata.Records != nil || delta.Metadata.MAC != "" {
		return delta, errors.New("invalid Codex journal delta metadata")
	}
	return delta, nil
}

// The small canonical head is the sole publication/acknowledgement point.
// Synced but unreferenced candidates are never replayed after a crash.
func (store *CodexLeaseStore) loadDeltaStorageLocked(data []byte) ([]byte, error) {
	var head codexLeaseStorageHead
	if len(data) > codexLeaseHeadMaxBytes {
		return nil, fmt.Errorf("%w: oversized journal head", ErrCodexLeaseTrustLost)
	}
	if err := decodeCodexLeaseStorageJSON(data, &head); err != nil {
		return nil, fmt.Errorf("%w: journal head: %v", ErrCodexLeaseTrustLost, err)
	}
	signed, err := store.signStorageHead(head)
	if err != nil || !bytes.Equal(signed, data) || head.Version != codexLeaseStorageVersion || head.DeltaBytes > codexLeaseJournalMaxBytes {
		return nil, fmt.Errorf("%w: invalid journal head", ErrCodexLeaseTrustLost)
	}
	checkpoint, err := store.readStorageReference("checkpoint", head.Checkpoint)
	if err != nil {
		return nil, err
	}
	var envelope codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(checkpoint.data, &envelope); err != nil || envelope.Version != codexLeaseJournalVersionV3 || !store.validV2EnvelopeMAC(envelope) {
		return nil, fmt.Errorf("%w: invalid journal checkpoint: %v", ErrCodexLeaseTrustLost, err)
	}
	if err := store.validateV2Envelope(envelope); err != nil {
		return nil, err
	}
	refs := map[string]codexLeaseStorageReference{store.storageName("checkpoint", head.Checkpoint): checkpoint}
	var reverse []codexLeaseDelta
	var total uint64
	digest := head.Tail
	generation := head.Generation

	for digest != "" {
		name := store.storageName("segment", digest)
		if _, duplicate := refs[name]; duplicate {
			return nil, fmt.Errorf("%w: cyclic journal chain", ErrCodexLeaseTrustLost)
		}
		reference, err := store.readStorageReference("segment", digest)
		if err != nil {
			return nil, err
		}
		segment, err := store.decodeSegment(reference.data)
		if err != nil {
			return nil, fmt.Errorf("%w: journal segment: %v", ErrCodexLeaseTrustLost, err)
		}
		for index := len(segment.Deltas) - 1; index >= 0; index-- {
			delta := segment.Deltas[index]
			encoded, err := json.Marshal(delta)
			if err != nil {
				return nil, err
			}
			total += uint64(len(encoded))
			if total > head.DeltaBytes || delta.Metadata.Generation != generation {
				return nil, fmt.Errorf("%w: journal generation/budget chain", ErrCodexLeaseTrustLost)
			}
			reverse = append(reverse, delta)
			generation = delta.Before
		}
		refs[name] = reference
		digest = segment.Previous
	}

	if total != head.DeltaBytes || generation != envelope.Generation {
		return nil, fmt.Errorf("%w: checkpoint generation mismatch", ErrCodexLeaseTrustLost)
	}
	lanes := make(map[string]CodexJournalLane, len(envelope.Lanes))
	records := make(map[CodexJournalRecordIdentity]CodexJournalRecordV2, len(envelope.Records))
	for _, lane := range envelope.Lanes {
		lanes[codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)] = lane
	}
	for _, record := range envelope.Records {
		records[record.Identity()] = record
	}
	for index := len(reverse) - 1; index >= 0; index-- {
		delta := reverse[index]
		seenLanes := map[string]bool{}
		seenRecords := map[CodexJournalRecordIdentity]bool{}
		for _, digest := range delta.DeleteLanes {
			if !validCodexLeaseDigest(digest) || seenLanes[digest] {
				return nil, fmt.Errorf("%w: duplicate/invalid delta lane", ErrCodexLeaseTrustLost)
			}
			if _, exists := lanes[digest]; !exists {
				return nil, fmt.Errorf("%w: absent deleted lane", ErrCodexLeaseTrustLost)
			}
			delete(lanes, digest)
			seenLanes[digest] = true
		}
		for _, lane := range delta.Lanes {
			digest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
			if seenLanes[digest] {
				return nil, fmt.Errorf("%w: duplicate delta lane", ErrCodexLeaseTrustLost)
			}
			lanes[digest] = lane
			seenLanes[digest] = true
		}
		for _, identity := range delta.DeleteRecords {
			if identity.IsZero() || seenRecords[identity] {
				return nil, fmt.Errorf("%w: duplicate/invalid delta record", ErrCodexLeaseTrustLost)
			}
			if _, exists := records[identity]; !exists {
				return nil, fmt.Errorf("%w: absent deleted record", ErrCodexLeaseTrustLost)
			}
			delete(records, identity)
			seenRecords[identity] = true
		}
		for _, record := range delta.Records {
			identity := record.Identity()
			if identity.IsZero() || seenRecords[identity] {
				return nil, fmt.Errorf("%w: duplicate delta record", ErrCodexLeaseTrustLost)
			}
			records[identity] = record
			seenRecords[identity] = true
		}
		envelope = delta.Metadata
	}
	if len(reverse) > 0 {
		envelope.Lanes = make([]CodexJournalLane, 0, len(lanes))
		for _, lane := range lanes {
			envelope.Lanes = append(envelope.Lanes, lane)
		}
		envelope.Records = make([]CodexJournalRecordV2, 0, len(records))
		for _, record := range records {
			envelope.Records = append(envelope.Records, record)
		}
		canonicaliseCodexLeaseV2(&envelope)
	}
	envelope.MAC = head.StateMAC
	logical, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	if len(logical) > codexLeaseJournalMaxBytes || envelope.Generation != head.Generation || !store.validV2EnvelopeMAC(envelope) {
		return nil, fmt.Errorf("%w: reconstructed journal MAC mismatch", ErrCodexLeaseTrustLost)
	}
	store.delta = &codexLeaseDeltaStorage{head: head, headBytes: bytes.Clone(data), refs: refs}
	return logical, nil
}

func makeCodexLeaseDelta(before, after codexLeaseJournalEnvelopeV2) codexLeaseDelta {
	metadata := after
	metadata.Lanes = nil
	metadata.Records = nil
	metadata.MAC = ""
	delta := codexLeaseDelta{Version: 1, Before: before.Generation, Metadata: metadata}
	// Both snapshots are canonical. Merge their ordered identities instead of
	// allocating full-history maps and boxing large record copies for equality.
	for old, next := 0, 0; old < len(before.Lanes) || next < len(after.Lanes); {
		switch {
		case next == len(after.Lanes) || (old < len(before.Lanes) && codexJournalLaneLess(before.Lanes[old], after.Lanes[next])):
			lane := &before.Lanes[old]
			delta.DeleteLanes = append(delta.DeleteLanes, codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash))
			old++
		case old == len(before.Lanes) || codexJournalLaneLess(after.Lanes[next], before.Lanes[old]):
			delta.Lanes = append(delta.Lanes, after.Lanes[next])
			next++
		default:
			if !equalCodexDeltaLane(&before.Lanes[old], &after.Lanes[next]) {
				delta.Lanes = append(delta.Lanes, after.Lanes[next])
			}
			old++
			next++
		}
	}
	for old, next := 0, 0; old < len(before.Records) || next < len(after.Records); {
		switch {
		case next == len(after.Records) || (old < len(before.Records) && codexJournalRecordLess(before.Records[old], after.Records[next])):
			delta.DeleteRecords = append(delta.DeleteRecords, before.Records[old].Identity())
			old++
		case old == len(before.Records) || codexJournalRecordLess(after.Records[next], before.Records[old]):
			delta.Records = append(delta.Records, after.Records[next])
			next++
		default:
			if !equalCodexDeltaRecord(&before.Records[old], &after.Records[next]) {
				delta.Records = append(delta.Records, after.Records[next])
			}
			old++
			next++
		}
	}
	return delta
}

func (store *CodexLeaseStore) writeStorageReference(kind string, data []byte, candidates map[string][]byte) (string, codexLeaseStorageReference, error) {
	digest := codexLeaseSHA256(data)
	name := store.storageName(kind, digest)
	if store.delta != nil {
		if ref, found := store.delta.refs[name]; found {
			return digest, ref, nil
		}
	}
	candidates[name] = data
	existing, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, name, codexLeaseJournalMaxBytes)
	if err == nil && !bytes.Equal(existing, data) {
		return "", codexLeaseStorageReference{}, fmt.Errorf("%w: storage candidate replaced", ErrCodexLeaseTrustLost)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", codexLeaseStorageReference{}, err
	}
	if err == nil {
		// A prior interrupted publication may have left an unreferenced candidate.
		// Reseal its bytes durably before using it as authority.
		err = fsutil.SecureAtomicWriteInDirectory(store.inspector, store.directory, name, data)
	} else {
		err = fsutil.SecureAtomicCreateInDirectory(store.inspector, store.directory, name, data)
	}
	if err != nil {
		return "", codexLeaseStorageReference{}, err
	}
	reference, err := store.readStorageReference(kind, digest)
	return digest, reference, err
}

func (store *CodexLeaseStore) revalidateDeltaReferencesLocked(storage *codexLeaseDeltaStorage) error {
	for name, reference := range storage.refs {
		if err := fsutil.MatchSecureFileInDirectoryWithIdentity(store.inspector, store.directory, name, reference.data, reference.identity); err != nil {
			return fmt.Errorf("%w: journal storage reference replaced", ErrCodexLeaseTrustLost)
		}
	}
	return nil
}

// Checkpoint only after accumulated encoded deltas pay for its bytes. Thus
// checkpoint cost is amortised over changed data, never a fixed commit count.
func (store *CodexLeaseStore) persistPreparedV2Locked(envelope codexLeaseJournalEnvelopeV2, data []byte, beforeReplace func() error) (result error) {
	if len(data) > codexLeaseJournalMaxBytes {
		return fsutil.ErrSecureFileTooLarge
	}
	if envelope.Cutover.State != CodexLeaseCutoverComplete || !envelope.Cutover.NoLegacyAuthority {
		return store.persistLegacyV2Locked(envelope, data, beforeReplace)
	}
	candidates := map[string][]byte{}
	defer func() {
		if result != nil && fsutil.AtomicWriteOutcome(result) == fsutil.CommitNotCommitted {
			store.cleanUnpublishedStorageCandidatesLocked(candidates)
		}
	}()
	head := codexLeaseStorageHead{Version: codexLeaseStorageVersion, Generation: envelope.Generation, StateMAC: envelope.MAC}
	refs := map[string]codexLeaseStorageReference{}
	if store.delta != nil {
		head.Checkpoint = store.delta.head.Checkpoint
		head.DeltaBytes = store.delta.head.DeltaBytes
		for name, reference := range store.delta.refs {
			refs[name] = reference
		}
	}
	delta := makeCodexLeaseDelta(*store.v2, envelope)
	deltaData, err := store.signDelta(delta)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(deltaData, &delta); err != nil {
		return err
	}
	checkpoint := head.DeltaBytes+uint64(len(deltaData)) >= uint64(len(data))
	if checkpoint {
		digest, reference, err := store.writeStorageReference("checkpoint", data, candidates)
		if err != nil {
			return store.storageCandidateError(err)
		}
		head.Checkpoint = digest
		head.DeltaBytes = 0
		refs = map[string]codexLeaseStorageReference{store.storageName("checkpoint", digest): reference}
	} else {
		if head.Checkpoint == "" {
			digest, reference, err := store.writeStorageReference("checkpoint", store.journalBytes, candidates)
			if err != nil {
				return store.storageCandidateError(err)
			}
			head.Checkpoint = digest
			refs[store.storageName("checkpoint", digest)] = reference
		}
		segment := codexLeaseSegment{Version: 1, Deltas: []codexLeaseDelta{delta}}
		if store.delta != nil && store.delta.head.Tail != "" {
			tail := store.delta.head.Tail
			name := store.storageName("segment", tail)
			prior, err := store.decodeSegment(store.delta.refs[name].data)
			if err != nil {
				return store.storageCandidateError(fmt.Errorf("%w: %v", ErrCodexLeaseTrustLost, err))
			}
			joined := prior
			joined.Deltas = append(append([]codexLeaseDelta(nil), prior.Deltas...), delta)
			encoded, err := store.signSegment(joined)
			if err != nil {
				return err
			}
			if len(encoded) <= codexLeaseSegmentMaxBytes {
				segment = joined
				delete(refs, name)
			} else {
				segment.Previous = tail
			}
		}
		segmentData, err := store.signSegment(segment)
		if err != nil {
			return err
		}
		digest, reference, err := store.writeStorageReference("segment", segmentData, candidates)
		if err != nil {
			return store.storageCandidateError(err)
		}
		head.Tail = digest
		head.DeltaBytes += uint64(len(deltaData))
		refs[store.storageName("segment", digest)] = reference
	}
	headData, err := store.signStorageHead(head)
	if err != nil {
		return err
	}
	storage := &codexLeaseDeltaStorage{head: head, headBytes: headData, refs: refs}
	if err := json.Unmarshal(headData, &storage.head); err != nil {
		return err
	}
	check := func() error {
		if beforeReplace != nil {
			if err := beforeReplace(); err != nil {
				return err
			}
		}
		// The precondition has just checked every old reference. Check only new
		// candidate identities/bytes here; unchanged shared references already have
		// their exact proof from that same publication barrier.
		added := &codexLeaseDeltaStorage{refs: map[string]codexLeaseStorageReference{}}
		for name, reference := range storage.refs {
			if store.delta != nil {
				if prior, found := store.delta.refs[name]; found && prior.identity == reference.identity && bytes.Equal(prior.data, reference.data) {
					continue
				}
			}
			added.refs[name] = reference
		}
		return store.revalidateDeltaReferencesLocked(added)
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, store.journalName, headData, check); err != nil {
		if fsutil.AtomicWriteOutcome(err) == fsutil.CommitIndeterminate || errors.Is(err, ErrCodexLeaseTrustLost) {
			store.poisoned = err
		}
		return err
	}
	installed, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseHeadMaxBytes)
	if err != nil || !bytes.Equal(installed, headData) {
		return store.poisonStorageCommit("verify committed Codex journal head", err)
	}
	if err := store.revalidateDeltaReferencesLocked(storage); err != nil {
		return store.poisonStorageCommit("verify committed Codex journal references", err)
	}
	old := store.delta
	store.delta = storage
	store.v2 = &envelope
	store.generation = envelope.Generation
	store.journalBytes = data
	store.journalID = identity
	if old != nil {
		if err := store.pruneDeltaReferencesLocked(old, storage); err != nil {
			return store.poisonStorageCommit("fence Codex journal cleanup", err)
		}
	}
	return nil
}

func (store *CodexLeaseStore) storageCandidateError(err error) error {
	// Candidate publication is not the journal commit point. Old authority is
	// intact, even if a candidate's own rename/sync outcome was uncertain.
	if errors.Is(err, ErrCodexLeaseTrustLost) {
		store.poisoned = err
	}
	return &fsutil.CommitError{Outcome: fsutil.CommitNotCommitted, Op: "prepare Codex journal storage", Err: err}
}
func (store *CodexLeaseStore) poisonStorageCommit(op string, err error) error {
	if err == nil {
		err = errors.New("installed bytes differ from committed bytes")
	}
	poison := &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: op, Err: err}
	store.poisoned = poison
	return fmt.Errorf("%w: %w", ErrCodexLeaseStorePoisoned, poison)
}
func (store *CodexLeaseStore) persistLegacyV2Locked(envelope codexLeaseJournalEnvelopeV2, data []byte, beforeReplace func() error) error {
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, store.journalName, data, beforeReplace); err != nil {
		if fsutil.AtomicWriteOutcome(err) == fsutil.CommitIndeterminate || errors.Is(err, ErrCodexLeaseTrustLost) {
			store.poisoned = err
		}
		return err
	}
	installed, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseJournalMaxBytes)
	if err != nil || !bytes.Equal(installed, data) {
		return store.poisonStorageCommit("verify committed Codex lease journal", err)
	}
	store.v2 = &envelope
	store.generation = envelope.Generation
	store.journalBytes = installed
	store.journalID = identity
	return nil
}

// Cleanup never follows cached references across an installed-authority change.
// Callers have checked the complete referenced state before entering cleanup.
// Repeat the small head/key/directory fence before each identity-bound removal.
func (store *CodexLeaseStore) pruneDeltaReferencesLocked(old, current *codexLeaseDeltaStorage) error {
	if err := store.revalidateV2InstalledHeadLocked(); err != nil {
		store.poisoned = err
		return err
	}
	remover, ok := store.directory.(fsutil.IdentityBoundRemover)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	var failure error
	removed := false
	for name, reference := range old.refs {
		if _, keep := current.refs[name]; keep {
			continue
		}
		if err := store.revalidateV2InstalledHeadLocked(); err != nil {
			store.poisoned = err
			return err
		}
		if err := remover.RemoveChecked(name, reference.identity); err != nil && !errors.Is(err, os.ErrNotExist) {
			failure = errors.Join(failure, err)
		} else {
			removed = true
		}
	}
	if removed {
		failure = errors.Join(failure, store.directory.Sync())
	}
	if failure != nil {
		fmt.Fprintf(os.Stderr, "cq: Codex journal cleanup failed; obsolete files retained: %v\n", failure)
	}
	return nil
}
func (store *CodexLeaseStore) cleanUnpublishedStorageCandidatesLocked(candidates map[string][]byte) {
	if len(candidates) == 0 {
		return
	}
	if err := store.revalidateV2InstalledLocked(); err != nil {
		store.poisoned = err
		fmt.Fprintf(os.Stderr, "cq: Codex journal candidate cleanup fenced: %v\n", err)
		return
	}
	obsolete := &codexLeaseDeltaStorage{refs: map[string]codexLeaseStorageReference{}}
	current := store.delta
	if current == nil {
		current = &codexLeaseDeltaStorage{refs: map[string]codexLeaseStorageReference{}}
	}
	for name, want := range candidates {
		if _, keep := current.refs[name]; keep {
			continue
		}
		data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, name, codexLeaseJournalMaxBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !bytes.Equal(data, want) {
			fmt.Fprintln(os.Stderr, "cq: unverified Codex journal candidate retained")
			continue
		}
		obsolete.refs[name] = codexLeaseStorageReference{data: data, identity: identity}
	}
	if len(obsolete.refs) > 0 {
		if err := store.pruneDeltaReferencesLocked(obsolete, current); err != nil {
			fmt.Fprintf(os.Stderr, "cq: Codex journal candidate cleanup fenced: %v\n", err)
		}
	}
}

// Production directory visitors stream bounded batches. One candidate is read,
// authenticated and discarded at a time; unknown entries remain intact.
func (store *CodexLeaseStore) pruneDeltaOrphansLocked() error {
	if err := store.revalidateV2InstalledLocked(); err != nil {
		return err
	}
	current := store.delta
	if current == nil {
		current = &codexLeaseDeltaStorage{refs: map[string]codexLeaseStorageReference{}}
	}
	visit := func(entry os.DirEntry) error {
		name := entry.Name()
		if _, keep := current.refs[name]; keep || !strings.HasPrefix(name, store.storagePrefix()) {
			return nil
		}
		suffix := strings.TrimPrefix(name, store.storagePrefix())
		kind, digest, ok := strings.Cut(suffix, "-")
		if !ok || (kind != "checkpoint" && kind != "segment") || !validCodexStorageDigest(digest) {
			return nil
		}
		reference, err := store.readStorageReference(kind, digest)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cq: unverified Codex journal candidate retained")
			return nil
		}
		valid := false
		if kind == "segment" {
			_, err := store.decodeSegment(reference.data)
			valid = err == nil
		} else {
			var envelope codexLeaseJournalEnvelopeV2
			valid = decodeCodexLeaseV2StrictJSON(reference.data, &envelope) == nil && envelope.Version == codexLeaseJournalVersionV3 && store.validV2EnvelopeMAC(envelope)
		}
		if !valid {
			fmt.Fprintln(os.Stderr, "cq: unauthenticated Codex journal candidate retained")
			return nil
		}
		return store.pruneDeltaReferencesLocked(&codexLeaseDeltaStorage{refs: map[string]codexLeaseStorageReference{name: reference}}, current)
	}
	if visitor, ok := store.directory.(fsutil.SecureDirectoryVisitor); ok {
		return visitor.VisitEntries(32, visit)
	}
	if reader, ok := store.directory.(fsutil.SecureDirectoryReader); ok {
		entries, err := reader.ReadDir()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
	}
	return nil
}
