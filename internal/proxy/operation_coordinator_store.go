package proxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type CoordinatorRequiredLock struct {
	Kind string
	Mode string
}

type CoordinatorChildSelectionRow struct {
	Row                  int
	Domain               string
	ChildKind            string
	ProofKind            string
	StoreLocator         string
	PreTemporaryGrammars []string
	LaterTempGrammars    []string
	Locks                []CoordinatorRequiredLock
}

var coordinatorChildSelectionRows = []CoordinatorChildSelectionRow{
	{1, "feature_activation", "feature_activation", "feature_activation_anchor_absent", "R/feature-activation", []string{"objects/.object-<32>.tmp", "receipts/.imported-receipt-<32>.tmp"}, []string{".anchor-<32>.tmp"}, []CoordinatorRequiredLock{{"parent_lifecycle", "shared"}, {"authority", "exclusive"}}},
	{2, "authority_transition", "authority_journal", "authority_journal_unselected", "R", []string{"authority-objects/.object-<32>.tmp"}, []string{".authority-anchor-<32>.tmp"}, []CoordinatorRequiredLock{{"parent_lifecycle", "shared"}, {"authority", "exclusive"}}},
	{3, "lifecycle_action", "lifecycle_action", "lifecycle_action_anchor_absent", "R/lifecycle-actions", []string{"objects/.object-<32>.tmp"}, []string{".anchor-<32>.tmp"}, []CoordinatorRequiredLock{{"parent_lifecycle", "shared"}, {"authority", "exclusive"}}},
	{4, "staged_release", "staged_release", "staged_release_anchor_absent", "R/release-staging/operations", []string{"objects/.tmp.<32>"}, []string{".tmp.<32>"}, []CoordinatorRequiredLock{{"parent_lifecycle", "shared"}, {"authority", "exclusive"}}},
	{5, "import_finalisation", "import_finalisation", "import_finalisation_anchor_absent", "R/import-finalisation", []string{"objects/.object-<32>.tmp", "manifests/.manifest-<32>.tmp"}, []string{".anchor-<32>.tmp", "receipts/primary-rehearsal/.receipt-<32>.tmp", "receipts/evidence/.receipt-<32>.tmp", "receipts/completion/.receipt-<32>.tmp"}, []CoordinatorRequiredLock{{"parent_lifecycle", "shared"}, {"authority", "exclusive"}}},
	{6, "candidate_removal", "quarantine_candidate_remove", "quarantine_candidate_remove_anchor_absent", "P/.cq-instance-<basename>.quarantine", []string{"objects/.object-<32>.tmp", "manifests/.manifest-<32>.tmp"}, []string{".anchor-<32>.tmp", "tombstones/.tombstone-<32>.tmp"}, []CoordinatorRequiredLock{{"parent_lifecycle", "exclusive"}}},
	{7, "authority_reset", "quarantine_authority_reset", "quarantine_authority_reset_anchor_absent", "P/.cq-instance-<basename>.quarantine", []string{"objects/.object-<32>.tmp", "manifests/.manifest-<32>.tmp"}, []string{".anchor-<32>.tmp", "tombstones/.tombstone-<32>.tmp"}, []CoordinatorRequiredLock{{"parent_lifecycle", "exclusive"}}},
}

func CoordinatorChildSelectionMapping(domain, childKind string) (CoordinatorChildSelectionRow, error) {
	for _, row := range coordinatorChildSelectionRows {
		if row.Domain == domain && row.ChildKind == childKind {
			return row, nil
		}
	}
	return CoordinatorChildSelectionRow{}, errors.New("unknown coordinator child selection mapping")
}

type OperationCoordinatorKeyBootstrap struct {
	Verifier    [ed25519.PublicKeySize]byte
	Fingerprint string
}

func BootstrapOperationCoordinatorKey(seed []byte) (OperationCoordinatorKeyBootstrap, error) {
	if len(seed) != ed25519.SeedSize {
		return OperationCoordinatorKeyBootstrap{}, errors.New("operation coordinator seed must be exactly 32 bytes")
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	var verifier [ed25519.PublicKeySize]byte
	copy(verifier[:], publicKey)
	prefix := []byte("cq-operation-coordinator-verification-key-fingerprint-v1\x00")
	input := make([]byte, 0, len(prefix)+4+len(publicKey))
	input = append(input, prefix...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(publicKey)))
	input = append(input, length[:]...)
	input = append(input, publicKey...)
	digest := sha256.Sum256(input)
	return OperationCoordinatorKeyBootstrap{Verifier: verifier, Fingerprint: hex.EncodeToString(digest[:])}, nil
}

type operationCoordinatorObject struct {
	SchemaVersion  int    `json:"schema_version"`
	OperationID    string `json:"operation_id"`
	Phase          string `json:"phase"`
	ValueDigest    string `json:"value_digest"`
	PreviousDigest string `json:"previous_digest,omitempty"`
	MAC            string `json:"mac,omitempty"`
}

type operationCoordinatorAnchor struct {
	SchemaVersion int    `json:"schema_version"`
	OperationID   string `json:"operation_id"`
	Phase         string `json:"phase"`
	ObjectDigest  string `json:"object_digest"`
	MAC           string `json:"mac,omitempty"`
}

// OperationCoordinatorStore selects authenticated immutable objects through
// one durable anchor. Unselected objects remain inert across reopen.
type OperationCoordinatorStore struct {
	mu        sync.Mutex
	ctx       context.Context
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	publisher DurableObjectPublisher
	key       [32]byte
	hook      func(string) error
	anchor    *operationCoordinatorAnchor
	anchorID  *StableObjectIdentity
	selected  *operationCoordinatorObject
	receipt   *operationCoordinatorObject
}

func OpenOperationCoordinatorStore(ctx context.Context, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, publisher DurableObjectPublisher, key []byte, hook func(string) error) (*OperationCoordinatorStore, error) {
	if ctx == nil || inspector == nil || directory == nil || publisher == nil || len(key) != 32 {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	store := &OperationCoordinatorStore{ctx: ctx, inspector: inspector, directory: directory, publisher: publisher, hook: hook}
	copy(store.key[:], key)
	if err := store.reopen(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *OperationCoordinatorStore) PublishIntent(operationID, digest string) error {
	if operationID == "" || digest == "" || validateAuthorityEntryName("receipt-"+operationID+".json") != nil {
		return errors.New("operation intent identity unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor != nil {
		return errors.New("operation intent already selected")
	}
	object, body, objectDigest, err := s.sealObject(operationCoordinatorObject{SchemaVersion: 1, OperationID: operationID, Phase: "intent", ValueDigest: digest})
	if err != nil {
		return err
	}
	if _, err := s.publishOrAdopt("object-"+objectDigest+".json", body); err != nil {
		return err
	}
	if err := s.callHook("intent_object_durable"); err != nil {
		return err
	}
	anchor, anchorBody, err := s.sealAnchor(operationCoordinatorAnchor{SchemaVersion: 1, OperationID: operationID, Phase: "intent", ObjectDigest: objectDigest})
	if err != nil {
		return err
	}
	anchorIdentity, err := s.publisher.PublishImmutable(s.ctx, s.directory, "anchor", anchorBody, fs.FileMode(0o600))
	if err != nil {
		return err
	}
	s.anchor, s.anchorID, s.selected = &anchor, &anchorIdentity, &object
	return nil
}

func (s *OperationCoordinatorStore) PublishAnchor(operationID, intentDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor == nil || s.anchorID == nil || s.selected == nil || s.anchor.OperationID != operationID || s.anchor.Phase != "intent" || s.selected.ValueDigest != intentDigest {
		return errors.New("operation anchor predecessor unavailable")
	}
	object, body, objectDigest, err := s.sealObject(operationCoordinatorObject{SchemaVersion: 1, OperationID: operationID, Phase: "anchor", ValueDigest: intentDigest, PreviousDigest: s.anchor.ObjectDigest})
	if err != nil {
		return err
	}
	if _, err := s.publishOrAdopt("object-"+objectDigest+".json", body); err != nil {
		return err
	}
	if err := s.callHook("anchor_object_durable"); err != nil {
		return err
	}
	anchor, anchorBody, err := s.sealAnchor(operationCoordinatorAnchor{SchemaVersion: 1, OperationID: operationID, Phase: "anchor", ObjectDigest: objectDigest})
	if err != nil {
		return err
	}
	nextIdentity, err := s.publisher.ReplaceSelectorExactPrior(s.ctx, s.directory, "anchor", s.anchorID, anchorBody)
	if err != nil {
		return err
	}
	s.anchor, s.anchorID, s.selected = &anchor, &nextIdentity, &object
	return nil
}

func (s *OperationCoordinatorStore) PublishReceipt(operationID, digest string) error {
	if digest == "" {
		return errors.New("operation receipt digest unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor == nil || s.selected == nil || s.anchor.OperationID != operationID || s.anchor.Phase != "anchor" || s.receipt != nil {
		return errors.New("operation receipt predecessor unavailable")
	}
	receipt, body, _, err := s.sealObject(operationCoordinatorObject{SchemaVersion: 1, OperationID: operationID, Phase: "receipt", ValueDigest: digest, PreviousDigest: s.anchor.ObjectDigest})
	if err != nil {
		return err
	}
	if _, err := s.publishOrAdopt("receipt-"+operationID+".json", body); err != nil {
		return err
	}
	if err := s.callHook("receipt_durable"); err != nil {
		return err
	}
	s.receipt = &receipt
	return nil
}

func (s *OperationCoordinatorStore) PublishTerminal(operationID, digest string) error {
	if digest == "" {
		return errors.New("operation terminal digest unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor == nil || s.anchorID == nil || s.selected == nil || s.receipt == nil || s.anchor.OperationID != operationID || s.anchor.Phase != "anchor" {
		return errors.New("operation terminal predecessor unavailable")
	}
	object, body, objectDigest, err := s.sealObject(operationCoordinatorObject{SchemaVersion: 1, OperationID: operationID, Phase: "terminal", ValueDigest: digest, PreviousDigest: s.anchor.ObjectDigest})
	if err != nil {
		return err
	}
	if _, err := s.publishOrAdopt("object-"+objectDigest+".json", body); err != nil {
		return err
	}
	if err := s.callHook("terminal_object_durable"); err != nil {
		return err
	}
	anchor, anchorBody, err := s.sealAnchor(operationCoordinatorAnchor{SchemaVersion: 1, OperationID: operationID, Phase: "terminal", ObjectDigest: objectDigest})
	if err != nil {
		return err
	}
	nextIdentity, err := s.publisher.ReplaceSelectorExactPrior(s.ctx, s.directory, "anchor", s.anchorID, anchorBody)
	if err != nil {
		return err
	}
	s.anchor, s.anchorID, s.selected = &anchor, &nextIdentity, &object
	return nil
}

func (s *OperationCoordinatorStore) SelectedPhase() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selected == nil {
		return "", false
	}
	if s.selected.Phase == "anchor" && s.receipt != nil {
		return "receipt", true
	}
	return s.selected.Phase, true
}

func (s *OperationCoordinatorStore) reopen() error {
	anchorBody, anchorIdentity, err := s.readStable("anchor", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	anchor, err := s.openAnchor(anchorBody)
	if err != nil {
		return err
	}
	objectBody, _, err := s.readStable("object-"+anchor.ObjectDigest+".json", 512<<10)
	if err != nil {
		return err
	}
	object, objectDigest, err := s.openObject(objectBody)
	if err != nil || objectDigest != anchor.ObjectDigest || object.OperationID != anchor.OperationID || object.Phase != anchor.Phase {
		return errors.New("operation coordinator anchor/object mismatch")
	}
	s.anchor, s.anchorID, s.selected = &anchor, &anchorIdentity, &object
	receiptName := "receipt-" + anchor.OperationID + ".json"
	receiptBody, _, receiptErr := s.readStable(receiptName, 64<<10)
	if receiptErr == nil {
		receipt, _, openErr := s.openObject(receiptBody)
		expectedPrevious := anchor.ObjectDigest
		if object.Phase == "terminal" {
			expectedPrevious = object.PreviousDigest
		}
		if openErr != nil || receipt.Phase != "receipt" || receipt.OperationID != anchor.OperationID || receipt.PreviousDigest != expectedPrevious {
			return errors.New("operation coordinator receipt mismatch")
		}
		s.receipt = &receipt
	} else if !errors.Is(receiptErr, os.ErrNotExist) {
		return receiptErr
	}
	return nil
}

func (s *OperationCoordinatorStore) sealObject(object operationCoordinatorObject) (operationCoordinatorObject, []byte, string, error) {
	unsigned, err := json.Marshal(object)
	if err != nil {
		return object, nil, "", err
	}
	object.MAC = hmacHex(s.key[:], "cq-operation-coordinator-object-mac-v1\x00", unsigned)
	body, err := json.Marshal(object)
	if err != nil {
		return object, nil, "", err
	}
	digest, err := FramedSHA256Hex("cq-operation-coordinator-object-v1\x00", body)
	return object, body, digest, err
}

func (s *OperationCoordinatorStore) openObject(body []byte) (operationCoordinatorObject, string, error) {
	var object operationCoordinatorObject
	if err := json.Unmarshal(body, &object); err != nil || object.SchemaVersion != 1 || object.OperationID == "" || object.ValueDigest == "" {
		return object, "", errors.New("invalid operation coordinator object")
	}
	wantMAC := object.MAC
	object.MAC = ""
	unsigned, _ := json.Marshal(object)
	if !hmac.Equal([]byte(wantMAC), []byte(hmacHex(s.key[:], "cq-operation-coordinator-object-mac-v1\x00", unsigned))) {
		return object, "", errors.New("invalid operation coordinator object MAC")
	}
	object.MAC = wantMAC
	digest, err := FramedSHA256Hex("cq-operation-coordinator-object-v1\x00", body)
	return object, digest, err
}

func (s *OperationCoordinatorStore) sealAnchor(anchor operationCoordinatorAnchor) (operationCoordinatorAnchor, []byte, error) {
	unsigned, err := json.Marshal(anchor)
	if err != nil {
		return anchor, nil, err
	}
	anchor.MAC = hmacHex(s.key[:], "cq-operation-coordinator-anchor-mac-v1\x00", unsigned)
	body, err := json.Marshal(anchor)
	return anchor, body, err
}

func (s *OperationCoordinatorStore) openAnchor(body []byte) (operationCoordinatorAnchor, error) {
	var anchor operationCoordinatorAnchor
	if err := json.Unmarshal(body, &anchor); err != nil || anchor.SchemaVersion != 1 || anchor.OperationID == "" || anchor.ObjectDigest == "" {
		return anchor, errors.New("invalid operation coordinator anchor")
	}
	wantMAC := anchor.MAC
	anchor.MAC = ""
	unsigned, _ := json.Marshal(anchor)
	if !hmac.Equal([]byte(wantMAC), []byte(hmacHex(s.key[:], "cq-operation-coordinator-anchor-mac-v1\x00", unsigned))) {
		return anchor, errors.New("invalid operation coordinator anchor MAC")
	}
	anchor.MAC = wantMAC
	return anchor, nil
}

func hmacHex(key []byte, domain string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(body)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// CredentialAuthorityFSBackend adapts the Task3 retained-directory publisher
// and two holder-bound lock capabilities to the provider executor contract.
// mutationLock is retained across an effect; selectorLock linearises each
// anchor CAS without borrowing the mutation description.
type CredentialAuthorityFSBackend struct {
	ctxLock   *SelectorCASLock
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	publisher *AuthorityObjectPublisher
}

func NewCredentialAuthorityFSBackend(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, random io.Reader, mutationLock, selectorLock *SelectorCASLock) (*CredentialAuthorityFSBackend, error) {
	if inspector == nil || directory == nil || random == nil || mutationLock == nil || selectorLock == nil || mutationLock.sharesDescription(selectorLock) {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := mutationLock.validateSelectorCAS(directory); err != nil {
		return nil, err
	}
	if err := selectorLock.validateSelectorCAS(directory); err != nil {
		return nil, err
	}
	return &CredentialAuthorityFSBackend{
		ctxLock: mutationLock, inspector: inspector, directory: directory,
		publisher: NewAuthorityObjectPublisher(inspector, random, selectorLock),
	}, nil
}

func (b *CredentialAuthorityFSBackend) Acquire(ctx context.Context) (func() error, error) {
	return b.ctxLock.AcquireSelectorCAS(ctx, b.inspector, b.directory)
}

func (b *CredentialAuthorityFSBackend) PublishImmutable(ctx context.Context, name string, body []byte) (providerCodex.CredentialAuthorityIdentity, error) {
	identity, err := b.publisher.PublishImmutable(ctx, b.directory, name, body, 0o600)
	return codexAuthorityIdentity(identity), err
}

func (b *CredentialAuthorityFSBackend) ReplaceSelectorExactPrior(ctx context.Context, name string, prior providerCodex.CredentialAuthorityIdentity, body []byte) (providerCodex.CredentialAuthorityIdentity, error) {
	stablePrior := stableCodexAuthorityIdentity(prior)
	identity, err := b.publisher.ReplaceSelectorExactPrior(ctx, b.directory, name, &stablePrior, body)
	return codexAuthorityIdentity(identity), err
}

func (b *CredentialAuthorityFSBackend) Read(_ context.Context, name string, maxBytes int64) ([]byte, providerCodex.CredentialAuthorityIdentity, error) {
	body, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(b.inspector, b.directory, name, maxBytes)
	if err != nil {
		return nil, providerCodex.CredentialAuthorityIdentity{}, err
	}
	stable, err := stableAuthorityIdentityFromParts(identity, int64(len(body)), body)
	return body, codexAuthorityIdentity(stable), err
}

func (b *CredentialAuthorityFSBackend) CredentialAuthorityOccupancy(ctx context.Context) (providerCodex.CredentialAuthorityOccupancy, error) {
	if err := ctx.Err(); err != nil {
		return providerCodex.CredentialAuthorityOccupancy{}, err
	}
	reader, ok := b.directory.(fsutil.SecureDirectoryReader)
	if !ok {
		return providerCodex.CredentialAuthorityOccupancy{}, fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return providerCodex.CredentialAuthorityOccupancy{}, err
	}
	var occupancy providerCodex.CredentialAuthorityOccupancy
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		file, err := b.directory.OpenNoFollow(entry.Name())
		if err != nil {
			return providerCodex.CredentialAuthorityOccupancy{}, err
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			return providerCodex.CredentialAuthorityOccupancy{}, statErr
		}
		if closeErr != nil {
			return providerCodex.CredentialAuthorityOccupancy{}, closeErr
		}
		identity, identityOK := b.inspector.FileIdentity(info)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || fsutil.ValidateSecureOwner(b.inspector, info) != nil || !identityOK || identity.Links != 1 || info.Size() < 0 || occupancy.Bytes > int64(^uint64(0)>>1)-info.Size() {
			return providerCodex.CredentialAuthorityOccupancy{}, fsutil.ErrUnsafeSecurePath
		}
		occupancy.Files++
		occupancy.Units++
		occupancy.Bytes += info.Size()
	}
	return occupancy, nil
}

func codexAuthorityIdentity(identity StableObjectIdentity) providerCodex.CredentialAuthorityIdentity {
	return providerCodex.CredentialAuthorityIdentity{Device: identity.File.Device, Inode: identity.File.Inode, Links: identity.File.Links, FileID: identity.File.FileID, Size: identity.Size, Digest: identity.Digest}
}

func stableCodexAuthorityIdentity(identity providerCodex.CredentialAuthorityIdentity) StableObjectIdentity {
	return StableObjectIdentity{File: fsutil.SecureFileIdentity{Device: identity.Device, Inode: identity.Inode, Links: identity.Links, FileID: identity.FileID}, Size: identity.Size, Digest: identity.Digest}
}

func (s *OperationCoordinatorStore) publishOrAdopt(name string, body []byte) (StableObjectIdentity, error) {
	identity, err := s.publisher.PublishImmutable(s.ctx, s.directory, name, body, 0o600)
	if err == nil {
		return identity, nil
	}
	existing, existingIdentity, readErr := s.readStable(name, int64(len(body))+1)
	if readErr != nil || !bytes.Equal(existing, body) {
		return StableObjectIdentity{}, err
	}
	return existingIdentity, nil
}

func (s *OperationCoordinatorStore) readStable(name string, maxBytes int64) ([]byte, StableObjectIdentity, error) {
	body, fileIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(s.inspector, s.directory, name, maxBytes)
	if err != nil {
		return nil, StableObjectIdentity{}, err
	}
	identity, err := stableAuthorityIdentityFromParts(fileIdentity, int64(len(body)), body)
	return body, identity, err
}

func (s *OperationCoordinatorStore) callHook(phase string) error {
	if s.hook == nil {
		return nil
	}
	if err := s.hook(phase); err != nil {
		return fmt.Errorf("operation coordinator %s: %w", phase, err)
	}
	return nil
}
