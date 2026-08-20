package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type CandidateBrokerPrimitives interface {
	PublishSource(context.Context, CandidateValidationSourceV1) (StableObjectIdentity, error)
	AppendJournal(context.Context, CandidateBrokerRecordV1) (JournalPositionV1, error)
	PublishScanEvidence(context.Context, CandidateCredentialEchoScanEvidenceV1) (StableObjectIdentity, error)
	VerifySealedRun(context.Context, CandidateBrokerJournalSealV1) error
}

type CandidateBrokerCaps struct {
	Runs          int
	RecordsPerRun int
}

type CandidateBrokerRecordV1 struct {
	SchemaVersion    int    `json:"schema_version"`
	RunID            string `json:"run_id"`
	SourceDigest     string `json:"source_digest"`
	CapabilityDigest string `json:"capability_digest,omitempty"`
	Sequence         uint64 `json:"sequence"`
	Kind             string `json:"kind"`
	PayloadDigest    string `json:"payload_digest"`
	PreviousDigest   string `json:"previous_digest,omitempty"`
	MAC              string `json:"mac,omitempty"`
}

type JournalPositionV1 struct {
	RunID    string
	Sequence uint64
	Digest   string
}

type CandidateCredentialEchoScanEvidenceV1 struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	SourceDigest  string `json:"source_digest"`
	ScanDigest    string `json:"scan_digest"`
	EchoCount     uint64 `json:"echo_count"`
	MAC           string `json:"mac,omitempty"`
}

type CandidateBrokerJournalSealV1 struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	Count         uint64 `json:"count"`
	HeadDigest    string `json:"head_digest"`
}

type candidateStoredSource struct {
	source   CandidateValidationSourceV1
	identity StableObjectIdentity
	name     string
}

type candidateStoredRecord struct {
	record   CandidateBrokerRecordV1
	identity StableObjectIdentity
	name     string
}

type CandidateBrokerStore struct {
	mu        sync.Mutex
	ctx       context.Context
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	publisher DurableObjectPublisher
	key       [32]byte
	caps      CandidateBrokerCaps
	sources   map[string]candidateStoredSource
	journal   map[string][]candidateStoredRecord
	evidence  map[string][]string
}

func OpenCandidateBrokerStore(ctx context.Context, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, publisher DurableObjectPublisher, key []byte, caps CandidateBrokerCaps) (*CandidateBrokerStore, error) {
	if ctx == nil || inspector == nil || directory == nil || publisher == nil || len(key) != 32 || caps.Runs <= 0 || caps.RecordsPerRun <= 0 {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	store := &CandidateBrokerStore{
		ctx: ctx, inspector: inspector, directory: directory, publisher: publisher, caps: caps,
		sources: make(map[string]candidateStoredSource), journal: make(map[string][]candidateStoredRecord), evidence: make(map[string][]string),
	}
	copy(store.key[:], key)
	if err := store.reopen(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *CandidateBrokerStore) PublishSource(ctx context.Context, source CandidateValidationSourceV1) (StableObjectIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return StableObjectIdentity{}, err
	}
	source.MAC = ""
	if err := validateCandidateValidationSource(source, s.sources); err != nil {
		return StableObjectIdentity{}, err
	}
	if !s.runExists(source.RunID) && s.runCount() >= s.caps.Runs {
		return StableObjectIdentity{}, errors.New("candidate broker run cap exceeded")
	}
	source.MAC = candidateSourceMAC(source, s.key[:])
	body, err := json.Marshal(source)
	if err != nil {
		return StableObjectIdentity{}, err
	}
	digest := sha256.Sum256(body)
	name := "candidate-source-" + hex.EncodeToString(digest[:]) + ".json"
	identity, err := s.publisher.PublishImmutable(ctx, s.directory, name, body, fs.FileMode(0o600))
	if err != nil {
		return StableObjectIdentity{}, err
	}
	s.sources[identity.Digest] = candidateStoredSource{source: source, identity: identity, name: name}
	return identity, nil
}

func (s *CandidateBrokerStore) AppendJournal(ctx context.Context, record CandidateBrokerRecordV1) (JournalPositionV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return JournalPositionV1{}, err
	}
	source, exists := s.sources[record.SourceDigest]
	if !exists || source.source.RunID != record.RunID {
		return JournalPositionV1{}, errors.New("candidate journal source is not durable")
	}
	current := s.journal[record.RunID]
	if len(current) >= s.caps.RecordsPerRun {
		return JournalPositionV1{}, errors.New("candidate broker journal cap exceeded")
	}
	if record.SchemaVersion != 1 || record.RunID == "" || record.Kind == "" || !lowerHexDigest(record.PayloadDigest) {
		return JournalPositionV1{}, errors.New("invalid candidate broker record")
	}
	record.Sequence = uint64(len(current) + 1)
	record.PreviousDigest = ""
	if len(current) != 0 {
		record.PreviousDigest = current[len(current)-1].identity.Digest
	}
	record.MAC = candidateRecordMAC(record, s.key[:])
	body, err := json.Marshal(record)
	if err != nil {
		return JournalPositionV1{}, err
	}
	name := fmt.Sprintf("candidate-journal-%s-%06d.json", record.RunID, record.Sequence)
	if validateAuthorityEntryName(name) != nil {
		return JournalPositionV1{}, errors.New("invalid candidate run identity")
	}
	identity, err := s.publisher.PublishImmutable(ctx, s.directory, name, body, fs.FileMode(0o600))
	if err != nil {
		return JournalPositionV1{}, err
	}
	s.journal[record.RunID] = append(current, candidateStoredRecord{record: record, identity: identity, name: name})
	return JournalPositionV1{RunID: record.RunID, Sequence: record.Sequence, Digest: identity.Digest}, nil
}

func (s *CandidateBrokerStore) PublishScanEvidence(ctx context.Context, evidence CandidateCredentialEchoScanEvidenceV1) (StableObjectIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return StableObjectIdentity{}, err
	}
	source, exists := s.sources[evidence.SourceDigest]
	if !exists || source.source.RunID != evidence.RunID || evidence.SchemaVersion != 1 || !lowerHexDigest(evidence.ScanDigest) {
		return StableObjectIdentity{}, errors.New("invalid candidate scan evidence")
	}
	evidence.MAC = candidateEvidenceMAC(evidence, s.key[:])
	body, err := json.Marshal(evidence)
	if err != nil {
		return StableObjectIdentity{}, err
	}
	digest := sha256.Sum256(body)
	name := "candidate-evidence-" + hex.EncodeToString(digest[:]) + ".json"
	identity, err := s.publisher.PublishImmutable(ctx, s.directory, name, body, fs.FileMode(0o600))
	if err != nil {
		return StableObjectIdentity{}, err
	}
	s.evidence[evidence.RunID] = append(s.evidence[evidence.RunID], name)
	return identity, nil
}

func (s *CandidateBrokerStore) VerifySealedRun(ctx context.Context, seal CandidateBrokerJournalSealV1) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	records := s.journal[seal.RunID]
	if seal.SchemaVersion != 1 || seal.Count != uint64(len(records)) || len(records) == 0 || seal.HeadDigest != records[len(records)-1].identity.Digest {
		return errors.New("candidate broker seal mismatch")
	}
	return nil
}

// JournalRecords returns an authenticated in-memory projection reconstructed
// from the durable journal. The returned slice does not alias store state.
func (s *CandidateBrokerStore) JournalRecords(runID string) []CandidateBrokerRecordV1 {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.journal[runID]
	result := make([]CandidateBrokerRecordV1, len(records))
	for index := range records {
		result[index] = records[index].record
	}
	return result
}

// RetireRun removes dependants before their source objects.
func (s *CandidateBrokerStore) RetireRun(ctx context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, record := range s.journal[runID] {
		if err := s.directory.Remove(record.name); err != nil {
			return err
		}
	}
	delete(s.journal, runID)
	for _, name := range s.evidence[runID] {
		if err := s.directory.Remove(name); err != nil {
			return err
		}
	}
	delete(s.evidence, runID)
	for digest, source := range s.sources {
		if source.source.RunID == runID {
			if err := s.directory.Remove(source.name); err != nil {
				return err
			}
			delete(s.sources, digest)
		}
	}
	return s.directory.Sync()
}

func (s *CandidateBrokerStore) reopen() error {
	reader, ok := s.directory.(fsutil.SecureDirectoryReader)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return err
	}
	pendingSources := make(map[string]struct{})
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "candidate-source-") {
			pendingSources[entry.Name()] = struct{}{}
		}
	}
	for len(pendingSources) != 0 {
		progress := false
		names := make([]string, 0, len(pendingSources))
		for name := range pendingSources {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			err := s.reopenSource(name)
			if errors.Is(err, errCandidateSourceDependencyUnavailable) {
				continue
			}
			if err != nil {
				return err
			}
			delete(pendingSources, name)
			progress = true
		}
		if !progress {
			return errors.New("candidate source DAG is incomplete or cyclic")
		}
	}
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.Name(), "candidate-journal-"):
			if err := s.reopenRecord(entry.Name()); err != nil {
				return err
			}
		case strings.HasPrefix(entry.Name(), "candidate-evidence-"):
			if err := s.reopenEvidence(entry.Name()); err != nil {
				return err
			}
		}
	}
	for runID := range s.journal {
		sort.Slice(s.journal[runID], func(i, j int) bool { return s.journal[runID][i].record.Sequence < s.journal[runID][j].record.Sequence })
		for index, stored := range s.journal[runID] {
			if stored.record.Sequence != uint64(index+1) || (index > 0 && stored.record.PreviousDigest != s.journal[runID][index-1].identity.Digest) || (index == 0 && stored.record.PreviousDigest != "") {
				return errors.New("candidate broker journal chain invalid")
			}
		}
	}
	if s.runCount() > s.caps.Runs {
		return errors.New("candidate broker run cap exceeded")
	}
	return nil
}

func (s *CandidateBrokerStore) reopenSource(name string) error {
	body, identity, err := s.readStable(name)
	if err != nil {
		return err
	}
	var source CandidateValidationSourceV1
	if err := json.Unmarshal(body, &source); err != nil || !hmac.Equal([]byte(source.MAC), []byte(candidateSourceMAC(source, s.key[:]))) {
		return errors.New("candidate source authentication failed")
	}
	if err := validateCandidateValidationSource(source, s.sources); err != nil {
		return err
	}
	s.sources[identity.Digest] = candidateStoredSource{source: source, identity: identity, name: name}
	return nil
}

func (s *CandidateBrokerStore) reopenRecord(name string) error {
	body, identity, err := s.readStable(name)
	if err != nil {
		return err
	}
	var record CandidateBrokerRecordV1
	if err := json.Unmarshal(body, &record); err != nil || !hmac.Equal([]byte(record.MAC), []byte(candidateRecordMAC(record, s.key[:]))) {
		return errors.New("candidate broker record authentication failed")
	}
	if source, exists := s.sources[record.SourceDigest]; !exists || source.source.RunID != record.RunID || record.Sequence == 0 || !lowerHexDigest(record.PayloadDigest) {
		return errors.New("candidate broker record source invalid")
	}
	s.journal[record.RunID] = append(s.journal[record.RunID], candidateStoredRecord{record: record, identity: identity, name: name})
	if len(s.journal[record.RunID]) > s.caps.RecordsPerRun {
		return errors.New("candidate broker journal cap exceeded")
	}
	return nil
}

func (s *CandidateBrokerStore) reopenEvidence(name string) error {
	body, _, err := s.readStable(name)
	if err != nil {
		return err
	}
	var evidence CandidateCredentialEchoScanEvidenceV1
	if err := json.Unmarshal(body, &evidence); err != nil || !hmac.Equal([]byte(evidence.MAC), []byte(candidateEvidenceMAC(evidence, s.key[:]))) {
		return errors.New("candidate scan evidence authentication failed")
	}
	if source, exists := s.sources[evidence.SourceDigest]; !exists || source.source.RunID != evidence.RunID || !lowerHexDigest(evidence.ScanDigest) {
		return errors.New("candidate scan evidence source invalid")
	}
	s.evidence[evidence.RunID] = append(s.evidence[evidence.RunID], name)
	return nil
}

func (s *CandidateBrokerStore) readStable(name string) ([]byte, StableObjectIdentity, error) {
	body, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(s.inspector, s.directory, name, routingPolicyMaxBytes)
	if err != nil {
		return nil, StableObjectIdentity{}, err
	}
	stable, err := stableAuthorityIdentityFromParts(identity, int64(len(body)), body)
	return body, stable, err
}

func (s *CandidateBrokerStore) runExists(runID string) bool {
	for _, source := range s.sources {
		if source.source.RunID == runID {
			return true
		}
	}
	return false
}

func (s *CandidateBrokerStore) runCount() int {
	runs := make(map[string]struct{})
	for _, source := range s.sources {
		runs[source.source.RunID] = struct{}{}
	}
	return len(runs)
}

func candidateSourceMAC(source CandidateValidationSourceV1, key []byte) string {
	source.MAC = ""
	body, _ := json.Marshal(source)
	return candidateMAC("cq/candidate-source/v1\x00", body, key)
}

func candidateRecordMAC(record CandidateBrokerRecordV1, key []byte) string {
	record.MAC = ""
	body, _ := json.Marshal(record)
	return candidateMAC("cq/candidate-journal/v1\x00", body, key)
}

func candidateEvidenceMAC(evidence CandidateCredentialEchoScanEvidenceV1, key []byte) string {
	evidence.MAC = ""
	body, _ := json.Marshal(evidence)
	return candidateMAC("cq/candidate-evidence/v1\x00", body, key)
}

func candidateMAC(domain string, body, key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
