package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type NormalCallerDomain string

const (
	NormalCallerLocal  NormalCallerDomain = "local_token_v1"
	NormalCallerClaude NormalCallerDomain = "claude_access_token_v1"
	NormalCallerCodex  NormalCallerDomain = "codex_chatgpt_bearer_v1"

	normalCallerAdmissionLifetime = 5 * time.Second
)

var (
	ErrNormalCallerAuthUnavailable   = errors.New("normal caller authentication unavailable")
	ErrNormalCallerAuthRequired      = errors.New("normal caller authentication required")
	ErrNormalCallerAuthScope         = errors.New("normal caller authentication scope violation")
	ErrNormalCallerAdmissionReplayed = errors.New("normal caller admission replayed")
)

type NormalCallerCredentialV1 struct {
	Domain     NormalCallerDomain
	Bearer     string
	SubjectID  string
	ValidFrom  time.Time
	ValidUntil time.Time

	identity string
}

type normalCallerIndexEntry struct {
	domain      NormalCallerDomain
	fingerprint [sha256.Size]byte
	subjectID   string
	validFrom   time.Time
	validUntil  time.Time
}

type NormalCallerIndexEntryV1 struct {
	Domain      NormalCallerDomain `json:"credential_domain"`
	Fingerprint string             `json:"bearer_fingerprint"`
	SubjectID   string             `json:"subject_id"`
	ValidFrom   time.Time          `json:"valid_from,omitempty"`
	ValidUntil  time.Time          `json:"valid_until,omitempty"`
}

type NormalCallerIndexV1 struct {
	SchemaVersion int                        `json:"schema_version"`
	IndexEpoch    uint64                     `json:"index_epoch"`
	Entries       []NormalCallerIndexEntryV1 `json:"entries"`
	MAC           string                     `json:"mac"`
}

type normalCallerAuthentication struct {
	domain      NormalCallerDomain
	fingerprint string
	subjectID   string
	indexEpoch  uint64
}

type ProviderBranchAdmissionConsumptionV1 struct {
	SchemaVersion     int                `json:"schema_version"`
	AdmissionID       string             `json:"admission_id"`
	SingleUseNonce    string             `json:"single_use_nonce"`
	Domain            NormalCallerDomain `json:"credential_domain"`
	SubjectID         string             `json:"subject_id"`
	RequestNonce      string             `json:"request_nonce"`
	Method            string             `json:"method"`
	RequestURI        string             `json:"request_uri"`
	IndexEpoch        uint64             `json:"index_epoch"`
	ConsumedAt        time.Time          `json:"consumed_at"`
	ValidUntil        time.Time          `json:"valid_until"`
	ConsumptionDigest string             `json:"consumption_digest"`
	MAC               string             `json:"mac"`
}

type RuntimeCallerAuthorityV1 struct {
	SchemaVersion     int                `json:"schema_version"`
	Kind              string             `json:"kind"`
	Domain            NormalCallerDomain `json:"credential_domain"`
	SubjectID         string             `json:"subject_id"`
	BearerFingerprint string             `json:"bearer_fingerprint"`
	IndexEpoch        uint64             `json:"index_epoch"`
	AdmissionID       string             `json:"admission_id"`
	SingleUseNonce    string             `json:"single_use_nonce"`
	RequestNonce      string             `json:"request_nonce"`
	Method            string             `json:"method"`
	RequestURI        string             `json:"request_uri"`
	ValidUntil        time.Time          `json:"valid_until"`
	ConsumptionDigest string             `json:"consumption_digest"`
	MAC               string             `json:"mac"`
}

type NormalCallerAdmissionConsumer interface {
	Consume(context.Context, ProviderBranchAdmissionConsumptionV1) error
}

type NormalCallerAuthority struct {
	mu        sync.RWMutex
	key       []byte
	epoch     uint64
	entries   []normalCallerIndexEntry
	published []normalCallerIndexEntry
	consumer  NormalCallerAdmissionConsumer
	now       func() time.Time
	random    io.Reader
}

// DeriveNormalCallerAuthorityKey derives the caller-index key from already
// initialised owner material. The caller must supply lifecycle-bound material;
// this function never creates or persists authority.
func DeriveNormalCallerAuthorityKey(root []byte) ([]byte, error) {
	if len(root) != sha256.Size {
		return nil, ErrNormalCallerAuthUnavailable
	}
	return DeriveAuthorityKey(root, "cq/normal-caller-authority/key/v1", sha256.Size)
}

// NormalCallerSubjectID returns a safe, key-bound identifier for a worker-side
// credential. Raw credential and account identity material never crosses IPC.
func NormalCallerSubjectID(key []byte, domain NormalCallerDomain, identity string) (string, error) {
	if len(key) != sha256.Size || !validNormalCallerDomain(domain) || identity == "" {
		return "", ErrNormalCallerAuthUnavailable
	}
	return FramedHMACSHA256Hex(key, "cq/normal-caller-subject/v1\x00", []byte(domain), []byte(identity))
}

func NewNormalCallerAuthority(key []byte, epoch uint64, credentials []NormalCallerCredentialV1, consumer NormalCallerAdmissionConsumer, now func() time.Time, random io.Reader) (*NormalCallerAuthority, error) {
	if len(key) != sha256.Size || epoch == 0 || consumer == nil || now == nil || random == nil {
		return nil, ErrNormalCallerAuthUnavailable
	}
	index, err := BuildNormalCallerIndexV1(key, epoch, credentials)
	if err != nil {
		return nil, err
	}
	entries, err := normalCallerEntriesFromIndex(index)
	if err != nil {
		return nil, err
	}
	authority := &NormalCallerAuthority{key: append([]byte(nil), key...), epoch: epoch, entries: entries, consumer: consumer, now: now, random: random}
	authority.published = append([]normalCallerIndexEntry(nil), authority.entries...)
	return authority, nil
}

func BuildNormalCallerIndexV1(key []byte, epoch uint64, credentials []NormalCallerCredentialV1) (NormalCallerIndexV1, error) {
	if len(key) != sha256.Size || epoch == 0 {
		return NormalCallerIndexV1{}, ErrNormalCallerAuthUnavailable
	}
	index := NormalCallerIndexV1{SchemaVersion: 1, IndexEpoch: epoch}
	for _, credential := range credentials {
		if !validNormalCallerDomain(credential.Domain) || !validNormalBearer(credential.Bearer) || credential.SubjectID == "" || (!credential.ValidUntil.IsZero() && !credential.ValidFrom.IsZero() && !credential.ValidFrom.Before(credential.ValidUntil)) {
			return NormalCallerIndexV1{}, ErrNormalCallerAuthUnavailable
		}
		fingerprint := normalCallerFingerprint(key, credential.Bearer)
		entry := NormalCallerIndexEntryV1{Domain: credential.Domain, Fingerprint: hex.EncodeToString(fingerprint[:]), SubjectID: credential.SubjectID, ValidFrom: credential.ValidFrom, ValidUntil: credential.ValidUntil}
		duplicate := false
		for _, existing := range index.Entries {
			if existing == entry {
				duplicate = true
				break
			}
		}
		if !duplicate {
			index.Entries = append(index.Entries, entry)
		}
	}
	sort.Slice(index.Entries, func(i, j int) bool {
		left, right := index.Entries[i], index.Entries[j]
		if left.Domain != right.Domain {
			return left.Domain < right.Domain
		}
		if left.Fingerprint != right.Fingerprint {
			return left.Fingerprint < right.Fingerprint
		}
		if left.SubjectID != right.SubjectID {
			return left.SubjectID < right.SubjectID
		}
		if !left.ValidFrom.Equal(right.ValidFrom) {
			return left.ValidFrom.Before(right.ValidFrom)
		}
		return left.ValidUntil.Before(right.ValidUntil)
	})
	index.MAC = normalCallerIndexMAC(key, index)
	return index, nil
}

func NewNormalCallerAuthorityFromIndex(key []byte, index NormalCallerIndexV1, consumer NormalCallerAdmissionConsumer, now func() time.Time, random io.Reader) (*NormalCallerAuthority, error) {
	if !VerifyNormalCallerIndexV1(key, index) || consumer == nil || now == nil || random == nil {
		return nil, ErrNormalCallerAuthUnavailable
	}
	authority := &NormalCallerAuthority{key: append([]byte(nil), key...), epoch: index.IndexEpoch, consumer: consumer, now: now, random: random}
	entries, err := normalCallerEntriesFromIndex(index)
	if err != nil {
		return nil, err
	}
	authority.entries = entries
	authority.published = append([]normalCallerIndexEntry(nil), entries...)
	return authority, nil
}

// UpdateFromIndex atomically replaces only with an authenticated newer index.
// Replaying byte-identical current state is harmless; stale or conflicting
// current-generation state fails closed.
func (authority *NormalCallerAuthority) UpdateFromIndex(index NormalCallerIndexV1) error {
	if authority == nil {
		return ErrNormalCallerAuthUnavailable
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if !VerifyNormalCallerIndexV1(authority.key, index) {
		return ErrNormalCallerAuthUnavailable
	}
	entries, err := normalCallerEntriesFromIndex(index)
	if err != nil || index.IndexEpoch < authority.epoch {
		return ErrNormalCallerAuthUnavailable
	}
	if index.IndexEpoch == authority.epoch {
		if !normalCallerEntriesEqual(authority.published, entries) {
			return ErrNormalCallerAuthUnavailable
		}
		return nil
	}
	accepted := append([]normalCallerIndexEntry(nil), entries...)
	now := authority.now()
	for _, existing := range authority.entries {
		if existing.validUntil.IsZero() || !now.Before(existing.validUntil) || normalCallerFingerprintExists(accepted, existing) {
			continue
		}
		accepted = append(accepted, existing)
	}
	authority.epoch = index.IndexEpoch
	authority.entries = accepted
	authority.published = append([]normalCallerIndexEntry(nil), entries...)
	return nil
}

func normalCallerFingerprintExists(entries []normalCallerIndexEntry, candidate normalCallerIndexEntry) bool {
	for _, entry := range entries {
		if entry.domain == candidate.domain && entry.fingerprint == candidate.fingerprint {
			return true
		}
	}
	return false
}

func normalCallerEntriesFromIndex(index NormalCallerIndexV1) ([]normalCallerIndexEntry, error) {
	entries := make([]normalCallerIndexEntry, 0, len(index.Entries))
	for _, published := range index.Entries {
		fingerprint, err := hex.DecodeString(published.Fingerprint)
		if err != nil || len(fingerprint) != sha256.Size || published.Fingerprint != strings.ToLower(published.Fingerprint) || !validNormalCallerDomain(published.Domain) || published.SubjectID == "" || (!published.ValidUntil.IsZero() && !published.ValidFrom.IsZero() && !published.ValidFrom.Before(published.ValidUntil)) {
			return nil, ErrNormalCallerAuthUnavailable
		}
		var exact [sha256.Size]byte
		copy(exact[:], fingerprint)
		entries = append(entries, normalCallerIndexEntry{domain: published.Domain, fingerprint: exact, subjectID: published.SubjectID, validFrom: published.ValidFrom, validUntil: published.ValidUntil})
	}
	return entries, nil
}

func normalCallerEntriesEqual(left, right []normalCallerIndexEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func VerifyNormalCallerIndexV1(key []byte, index NormalCallerIndexV1) bool {
	if len(key) != sha256.Size || index.SchemaVersion != 1 || index.IndexEpoch == 0 || len(index.MAC) != sha256.Size*2 || index.MAC != strings.ToLower(index.MAC) {
		return false
	}
	provided, err := hex.DecodeString(index.MAC)
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(normalCallerIndexMAC(key, index))
	return err == nil && hmac.Equal(provided, expected)
}

func normalCallerIndexMAC(key []byte, index NormalCallerIndexV1) string {
	index.MAC = ""
	encoded, _ := json.Marshal(index)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/normal-caller-index/v1\x00"))
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

type NormalCallerAdmissionStore struct {
	mu        sync.Mutex
	directory fsutil.SecureDirectory
	inspector fsutil.SecurePathInspector
	now       func() time.Time
	lastPrune time.Time
	closed    bool
}

func NormalCallerAdmissionPath(stateDir string) string {
	return filepath.Join(stateDir, "normal-caller-admissions-v1")
}

func DefaultNormalCallerAdmissionPath() (string, error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return "", err
	}
	return NormalCallerAdmissionPath(paths.StateDir), nil
}

func OpenNormalCallerAdmissionStore(fsys fsutil.FileSystem, path string) (*NormalCallerAdmissionStore, error) {
	if fsys == nil || path == "" {
		return nil, ErrNormalCallerAuthUnavailable
	}
	if err := fsutil.EnsureSecureDirectory(fsys, path); err != nil {
		return nil, errors.Join(ErrNormalCallerAuthUnavailable, err)
	}
	opener, ok := fsys.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, ErrNormalCallerAuthUnavailable
	}
	directory, err := opener.OpenSecureDirectory(path)
	if err != nil {
		return nil, errors.Join(ErrNormalCallerAuthUnavailable, err)
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		_ = directory.Close()
		return nil, ErrNormalCallerAuthUnavailable
	}
	store := &NormalCallerAdmissionStore{directory: directory, inspector: inspector, now: time.Now}
	store.pruneLocked(store.now())
	return store, nil
}

func (store *NormalCallerAdmissionStore) Consume(_ context.Context, consumption ProviderBranchAdmissionConsumptionV1) error {
	if store == nil || len(consumption.AdmissionID) != 32 || consumption.SchemaVersion != 1 || consumption.ConsumptionDigest == "" || consumption.MAC == "" {
		return ErrNormalCallerAuthUnavailable
	}
	if _, err := hex.DecodeString(consumption.AdmissionID); err != nil || consumption.AdmissionID != strings.ToLower(consumption.AdmissionID) {
		return ErrNormalCallerAuthUnavailable
	}
	encoded, err := json.Marshal(consumption)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.directory == nil {
		return ErrNormalCallerAuthUnavailable
	}
	store.pruneLocked(store.now())
	name := "consumed-" + consumption.AdmissionID + ".json"
	file, err := store.directory.CreateExclusive(name, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrNormalCallerAdmissionReplayed
		}
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = store.directory.Remove(name)
			_ = store.directory.Sync()
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := store.directory.Sync(); err != nil {
		return err
	}
	committed = true
	proxyProcessEphemeralState.recordCreate(ephemeralReceiptAdmission)
	return nil
}

func (store *NormalCallerAdmissionStore) pruneLocked(now time.Time) {
	if store == nil || store.directory == nil || (!store.lastPrune.IsZero() && now.Before(store.lastPrune.Add(ephemeralReceiptPruneInterval))) {
		return
	}
	store.lastPrune = now
	remaining, pruned, err := pruneEphemeralReceipts(store.inspector, store.directory, "consumed-", now)
	proxyProcessEphemeralState.recordScan(ephemeralReceiptAdmission, remaining, pruned, err)
}

func (store *NormalCallerAdmissionStore) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	if store.directory == nil {
		return nil
	}
	err := store.directory.Close()
	store.directory = nil
	return err
}

func NewProductionNormalCallerAuthority(key []byte, epoch uint64, credentials []NormalCallerCredentialV1, store *NormalCallerAdmissionStore) (*NormalCallerAuthority, error) {
	if store == nil {
		return nil, ErrNormalCallerAuthUnavailable
	}
	authority, err := NewNormalCallerAuthority(key, epoch, credentials, store, time.Now, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("normal caller authority: %w", err)
	}
	return authority, nil
}

func validNormalCallerDomain(domain NormalCallerDomain) bool {
	return domain == NormalCallerLocal || domain == NormalCallerClaude || domain == NormalCallerCodex
}

func validNormalBearer(value string) bool {
	if value == "" || len(value) > 16<<10 {
		return false
	}
	for index, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("-._~+/", rune(character)) || (character == '=' && index > 0) {
			continue
		}
		return false
	}
	return true
}

func (authority *NormalCallerAuthority) fingerprint(bearer string) [sha256.Size]byte {
	return normalCallerFingerprint(authority.key, bearer)
}

func normalCallerFingerprint(key []byte, bearer string) [sha256.Size]byte {
	return normalCallerFingerprintBytes(key, []byte(bearer))
}

func normalCallerFingerprintBytes(key, bearer []byte) [sha256.Size]byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/normal-caller-fingerprint/v1\x00"))
	_, _ = mac.Write(bearer)
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

// DeniesBearer reports whether bearer belongs to any registered normal caller.
// Rescue uses this to prevent CQ-owned credentials from crossing its relay.
func (authority *NormalCallerAuthority) DeniesBearer(bearer []byte) bool {
	if authority == nil || len(bearer) == 0 {
		return false
	}
	fingerprint := normalCallerFingerprintBytes(authority.key, bearer)
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	for _, entry := range authority.entries {
		if subtle.ConstantTimeCompare(fingerprint[:], entry.fingerprint[:]) == 1 {
			return true
		}
	}
	return false
}

func exactBearer(request *http.Request) (string, error) {
	if request == nil {
		return "", ErrNormalCallerAuthRequired
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || strings.Count(values[0], " ") != 1 {
		return "", ErrNormalCallerAuthRequired
	}
	bearer := strings.TrimPrefix(values[0], "Bearer ")
	if !validNormalBearer(bearer) {
		return "", ErrNormalCallerAuthRequired
	}
	return bearer, nil
}

func (authority *NormalCallerAuthority) authenticate(request *http.Request, policy normalCallerRoutePolicy) (normalCallerAuthentication, error) {
	if authority == nil {
		return normalCallerAuthentication{}, ErrNormalCallerAuthUnavailable
	}
	bearer, err := exactBearer(request)
	if err != nil {
		return normalCallerAuthentication{}, err
	}
	fingerprint := authority.fingerprint(bearer)
	now := authority.now()
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	matches := make([]normalCallerIndexEntry, 0, 1)
	for _, entry := range authority.entries {
		if subtle.ConstantTimeCompare(fingerprint[:], entry.fingerprint[:]) != 1 || (!entry.validFrom.IsZero() && now.Before(entry.validFrom)) || (!entry.validUntil.IsZero() && !now.Before(entry.validUntil)) {
			continue
		}
		matches = append(matches, entry)
	}
	if len(matches) == 0 {
		return normalCallerAuthentication{}, ErrNormalCallerAuthRequired
	}
	if len(matches) != 1 || !policyAllowsCaller(policy, matches[0].domain) {
		return normalCallerAuthentication{}, ErrNormalCallerAuthScope
	}
	return normalCallerAuthentication{domain: matches[0].domain, fingerprint: hex.EncodeToString(fingerprint[:]), subjectID: matches[0].subjectID, indexEpoch: authority.epoch}, nil
}

func policyAllowsCaller(policy normalCallerRoutePolicy, domain NormalCallerDomain) bool {
	switch policy {
	case normalCallerRouteLocal:
		return domain == NormalCallerLocal
	case normalCallerRouteCodex:
		return domain == NormalCallerLocal || domain == NormalCallerCodex
	case normalCallerRouteLocalOrClaude:
		return domain == NormalCallerLocal || domain == NormalCallerClaude
	case normalCallerRouteClassified:
		return validNormalCallerDomain(domain)
	default:
		return false
	}
}

func (authority *NormalCallerAuthority) consume(ctx context.Context, authentication normalCallerAuthentication, request *http.Request) (RuntimeCallerAuthorityV1, error) {
	issuedAt := authority.now().UTC()
	validUntil := issuedAt.Add(normalCallerAdmissionLifetime)
	admissionID, err := readNormalCallerNonce(authority.random)
	if err != nil {
		return RuntimeCallerAuthorityV1{}, err
	}
	singleUseNonce, err := readNormalCallerNonce(authority.random)
	if err != nil {
		return RuntimeCallerAuthorityV1{}, err
	}
	requestNonce, err := readNormalCallerNonce(authority.random)
	if err != nil {
		return RuntimeCallerAuthorityV1{}, err
	}
	consumption := ProviderBranchAdmissionConsumptionV1{SchemaVersion: 1, AdmissionID: admissionID, SingleUseNonce: singleUseNonce, Domain: authentication.domain, SubjectID: authentication.subjectID, RequestNonce: requestNonce, Method: request.Method, RequestURI: request.URL.RequestURI(), IndexEpoch: authentication.indexEpoch, ConsumedAt: issuedAt, ValidUntil: validUntil}
	consumption.MAC = authority.mac("cq/normal-caller-admission-consumption-mac/v1\x00", consumption)
	consumption.ConsumptionDigest = authority.digest("cq/normal-caller-admission-consumption/v1\x00", consumption)
	if err := authority.consumer.Consume(ctx, consumption); err != nil {
		return RuntimeCallerAuthorityV1{}, err
	}
	runtime := RuntimeCallerAuthorityV1{SchemaVersion: 1, Kind: "provider_branch_admission_consumed_v1", Domain: authentication.domain, SubjectID: authentication.subjectID, BearerFingerprint: authentication.fingerprint, IndexEpoch: authentication.indexEpoch, AdmissionID: admissionID, SingleUseNonce: singleUseNonce, RequestNonce: requestNonce, Method: request.Method, RequestURI: request.URL.RequestURI(), ValidUntil: validUntil, ConsumptionDigest: consumption.ConsumptionDigest}
	runtime.MAC = authority.mac("cq/runtime-caller-authority-mac/v1\x00", runtime)
	return runtime, nil
}

func readNormalCallerNonce(random io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (authority *NormalCallerAuthority) mac(domain string, value any) string {
	encoded, _ := json.Marshal(value)
	mac := hmac.New(sha256.New, authority.key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

func validateRuntimeCallerAuthorityMAC(key []byte, caller RuntimeCallerAuthorityV1) bool {
	if len(key) != sha256.Size || len(caller.MAC) != sha256.Size*2 || caller.MAC != strings.ToLower(caller.MAC) {
		return false
	}
	provided, err := hex.DecodeString(caller.MAC)
	if err != nil {
		return false
	}
	caller.MAC = ""
	encoded, _ := json.Marshal(caller)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/runtime-caller-authority-mac/v1\x00"))
	_, _ = mac.Write(encoded)
	return hmac.Equal(provided, mac.Sum(nil))
}

func (authority *NormalCallerAuthority) digest(domain string, value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(append([]byte(domain), encoded...))
	return hex.EncodeToString(digest[:])
}
