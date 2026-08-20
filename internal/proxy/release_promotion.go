package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

var ErrReleasePromotionInvalid = errors.New("candidate release promotion invalid")

type CandidateReleasePromotionInputV1 struct {
	SchemaVersion                        int       `json:"schema_version"`
	FloorSourceCommit                    string    `json:"floor_source_commit"`
	TargetSourceCommit                   string    `json:"target_source_commit"`
	SourceAncestry                       []string  `json:"source_ancestry"`
	TargetReleaseBundleDigest            string    `json:"target_release_bundle_digest"`
	RollbackFloorAcceptanceReceiptDigest string    `json:"rollback_floor_acceptance_receipt_digest"`
	ClientBarrierReceiptDigest           string    `json:"client_barrier_receipt_digest"`
	ClientStopProofDigest                string    `json:"client_stop_proof_digest"`
	RealClientValidationReceiptDigest    string    `json:"real_client_validation_receipt_digest"`
	CandidateBrokerSealDigest            string    `json:"candidate_broker_seal_digest"`
	CandidateConfinementReceiptDigest    string    `json:"candidate_confinement_receipt_digest"`
	CandidateStageReceiptDigest          string    `json:"candidate_stage_receipt_digest"`
	CompletedAt                          time.Time `json:"completed_at"`
	Nonce                                string    `json:"nonce"`
}

type CandidateReleasePromotionReceiptV1 struct {
	CandidateReleasePromotionInputV1
	MAC    string `json:"mac"`
	Digest string `json:"digest"`
}

func BuildCandidateReleasePromotion(input CandidateReleasePromotionInputV1, key []byte) (CandidateReleasePromotionReceiptV1, error) {
	if len(key) != sha256.Size || !validCandidateReleasePromotionInput(input) {
		return CandidateReleasePromotionReceiptV1{}, ErrReleasePromotionInvalid
	}
	input.SourceAncestry = append([]string(nil), input.SourceAncestry...)
	receipt := CandidateReleasePromotionReceiptV1{CandidateReleasePromotionInputV1: input}
	receipt.MAC = releasePromotionMAC(key, receipt)
	receipt.Digest = releasePromotionReceiptDigest(receipt)
	return receipt, nil
}

func validCandidateReleasePromotionInput(input CandidateReleasePromotionInputV1) bool {
	if input.SchemaVersion != 1 || !lowerHexCommit(input.FloorSourceCommit) || !lowerHexCommit(input.TargetSourceCommit) || input.FloorSourceCommit == input.TargetSourceCommit || len(input.SourceAncestry) < 2 || input.SourceAncestry[0] != input.FloorSourceCommit || input.SourceAncestry[len(input.SourceAncestry)-1] != input.TargetSourceCommit || !input.CompletedAt.Equal(input.CompletedAt.UTC()) || input.CompletedAt.IsZero() || len(input.Nonce) != 32 {
		return false
	}
	seen := make(map[string]struct{}, len(input.SourceAncestry))
	for _, commit := range input.SourceAncestry {
		if !lowerHexCommit(commit) {
			return false
		}
		if _, duplicate := seen[commit]; duplicate {
			return false
		}
		seen[commit] = struct{}{}
	}
	for _, digest := range []string{input.TargetReleaseBundleDigest, input.RollbackFloorAcceptanceReceiptDigest, input.ClientBarrierReceiptDigest, input.ClientStopProofDigest, input.RealClientValidationReceiptDigest, input.CandidateBrokerSealDigest, input.CandidateConfinementReceiptDigest, input.CandidateStageReceiptDigest} {
		if !lowerHexDigest(digest) {
			return false
		}
	}
	_, err := hex.DecodeString(input.Nonce)
	return err == nil
}

func lowerHexCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 20 && value == hex.EncodeToString(decoded)
}

func releasePromotionMAC(key []byte, receipt CandidateReleasePromotionReceiptV1) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/candidate-release-promotion-receipt/v1\x00"))
	_, _ = mac.Write(releasePromotionCanonical(receipt))
	return hex.EncodeToString(mac.Sum(nil))
}

func releasePromotionReceiptDigest(receipt CandidateReleasePromotionReceiptV1) string {
	receipt.Digest = ""
	body, _ := json.Marshal(receipt)
	return releasePromotionDigest("cq/candidate-release-promotion-receipt-digest/v1\x00", body)
}

func releasePromotionCanonical(receipt CandidateReleasePromotionReceiptV1) []byte {
	receipt.MAC = ""
	receipt.Digest = ""
	body, _ := json.Marshal(receipt)
	return body
}

func canonicalReleasePromotion(receipt CandidateReleasePromotionReceiptV1) ([]byte, error) {
	if !validCandidateReleasePromotionInput(receipt.CandidateReleasePromotionInputV1) || !lowerHexDigest(receipt.MAC) || receipt.Digest != releasePromotionReceiptDigest(receipt) {
		return nil, ErrReleasePromotionInvalid
	}
	return json.Marshal(receipt)
}

type CanonicalCandidateReleaseImportedObjectIdentityV1 struct {
	ObjectKind    string `json:"object_kind"`
	Device        uint64 `json:"device"`
	Inode         uint64 `json:"inode"`
	LinkCount     uint64 `json:"link_count"`
	ByteCount     int64  `json:"byte_count"`
	ContentDigest string `json:"content_digest"`
}

type canonicalCandidateReleaseImportSelectorV1 struct {
	SchemaVersion                          int                                               `json:"schema_version"`
	RollbackFloorAcceptanceReceiptDigest   string                                            `json:"rollback_floor_acceptance_receipt_digest"`
	CandidateReleasePromotionReceiptDigest string                                            `json:"candidate_release_promotion_receipt_digest"`
	RollbackFloorIdentity                  CanonicalCandidateReleaseImportedObjectIdentityV1 `json:"rollback_floor_identity"`
	PromotionIdentity                      CanonicalCandidateReleaseImportedObjectIdentityV1 `json:"promotion_identity"`
	SelectedAt                             time.Time                                         `json:"selected_at"`
	MAC                                    string                                            `json:"mac"`
	Digest                                 string                                            `json:"digest"`
}

type CanonicalCandidateReleaseImportReceiptV1 struct {
	SchemaVersion                          int       `json:"schema_version"`
	RollbackFloorAcceptanceReceiptDigest   string    `json:"rollback_floor_acceptance_receipt_digest"`
	CandidateReleasePromotionReceiptDigest string    `json:"candidate_release_promotion_receipt_digest"`
	SelectorDigest                         string    `json:"selector_digest"`
	CompletedAt                            time.Time `json:"completed_at"`
	MAC                                    string    `json:"mac"`
	Digest                                 string    `json:"digest"`
}

type ReleaseImportStore struct {
	mu        sync.Mutex
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	key       [sha256.Size]byte
	hook      func(string) error
	closed    bool
}

func OpenReleaseImportStore(fsys fsutil.FileSystem, path string, key []byte, hook func(string) error) (*ReleaseImportStore, error) {
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if fsys == nil || !inspectorOK || !openerOK || path == "" || len(key) != sha256.Size {
		return nil, ErrReleasePromotionInvalid
	}
	if err := fsutil.EnsureSecureDirectory(fsys, path); err != nil {
		return nil, err
	}
	directory, err := opener.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	store := &ReleaseImportStore{inspector: inspector, directory: directory, hook: hook}
	copy(store.key[:], key)
	return store, nil
}

func (s *ReleaseImportStore) Import(ctx context.Context, floor, promotion []byte) (CanonicalCandidateReleaseImportReceiptV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil || s.closed || s.directory == nil || ctx == nil || ctx.Err() != nil || len(floor) == 0 || len(floor) > 64<<10 || len(promotion) == 0 || len(promotion) > 64<<10 {
		return CanonicalCandidateReleaseImportReceiptV1{}, ErrReleasePromotionInvalid
	}
	var promotionReceipt CandidateReleasePromotionReceiptV1
	if err := strictReleasePromotionDecode(promotion, &promotionReceipt); err != nil || !hmac.Equal([]byte(promotionReceipt.MAC), []byte(releasePromotionMAC(s.key[:], promotionReceipt))) || promotionReceipt.Digest != releasePromotionReceiptDigest(promotionReceipt) {
		return CanonicalCandidateReleaseImportReceiptV1{}, ErrReleasePromotionInvalid
	}
	floorDigest := releasePromotionDigest("cq/release-import-floor/v1\x00", floor)
	if promotionReceipt.RollbackFloorAcceptanceReceiptDigest != floorDigest {
		return CanonicalCandidateReleaseImportReceiptV1{}, ErrReleasePromotionInvalid
	}
	floorIdentity, err := s.ensureObject("floor-"+floorDigest+".json", "rollback_floor_validation_receipt", floor)
	if err != nil {
		return CanonicalCandidateReleaseImportReceiptV1{}, err
	}
	if err := s.callHook("floor_durable"); err != nil {
		return CanonicalCandidateReleaseImportReceiptV1{}, err
	}
	promotionIdentity, err := s.ensureObject("promotion-"+promotionReceipt.Digest+".json", "candidate_release_promotion_receipt", promotion)
	if err != nil {
		return CanonicalCandidateReleaseImportReceiptV1{}, err
	}
	if err := s.callHook("promotion_durable"); err != nil {
		return CanonicalCandidateReleaseImportReceiptV1{}, err
	}
	selector := canonicalCandidateReleaseImportSelectorV1{SchemaVersion: 1, RollbackFloorAcceptanceReceiptDigest: floorDigest, CandidateReleasePromotionReceiptDigest: promotionReceipt.Digest, RollbackFloorIdentity: floorIdentity, PromotionIdentity: promotionIdentity, SelectedAt: promotionReceipt.CompletedAt}
	selector.MAC = s.objectMAC("cq/canonical-candidate-release-import-selector/v1\x00", selector)
	selector.Digest = releasePromotionDigest("cq/canonical-candidate-release-import-selector-digest/v1\x00", mustReleasePromotionJSON(selector))
	selectorBody := mustReleasePromotionJSON(selector)
	if _, err := s.ensureObject("selector-"+selector.Digest+".json", "canonical_import_selector", selectorBody); err != nil {
		return CanonicalCandidateReleaseImportReceiptV1{}, err
	}
	if err := s.callHook("selector_durable"); err != nil {
		return CanonicalCandidateReleaseImportReceiptV1{}, err
	}
	receipt := CanonicalCandidateReleaseImportReceiptV1{SchemaVersion: 1, RollbackFloorAcceptanceReceiptDigest: floorDigest, CandidateReleasePromotionReceiptDigest: promotionReceipt.Digest, SelectorDigest: selector.Digest, CompletedAt: promotionReceipt.CompletedAt}
	receipt.MAC = s.objectMAC("cq/canonical-candidate-release-import-receipt/v1\x00", receipt)
	receipt.Digest = releasePromotionDigest("cq/canonical-candidate-release-import-receipt-digest/v1\x00", mustReleasePromotionJSON(receipt))
	receiptBody := mustReleasePromotionJSON(receipt)
	if _, err := s.ensureObject("receipt-"+receipt.Digest+".json", "canonical_import_receipt", receiptBody); err != nil {
		return CanonicalCandidateReleaseImportReceiptV1{}, err
	}
	return receipt, nil
}

func (s *ReleaseImportStore) ensureObject(name, kind string, body []byte) (CanonicalCandidateReleaseImportedObjectIdentityV1, error) {
	err := fsutil.SecureAtomicCreateInDirectory(s.inspector, s.directory, name, body)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return CanonicalCandidateReleaseImportedObjectIdentityV1{}, err
	}
	reopened, identity, readErr := fsutil.ReadSecureFileInDirectoryWithIdentity(s.inspector, s.directory, name, int64(len(body))+1)
	if readErr != nil || !bytes.Equal(reopened, body) {
		return CanonicalCandidateReleaseImportedObjectIdentityV1{}, ErrReleasePromotionInvalid
	}
	return CanonicalCandidateReleaseImportedObjectIdentityV1{ObjectKind: kind, Device: identity.Device, Inode: identity.Inode, LinkCount: identity.Links, ByteCount: int64(len(body)), ContentDigest: releasePromotionDigest("cq/canonical-release-import-object/v1\x00", body)}, nil
}

func (s *ReleaseImportStore) objectMAC(domain string, value any) string {
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write(mustReleasePromotionJSONWithoutAuthority(value))
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *ReleaseImportStore) callHook(phase string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(phase)
}

func strictReleasePromotionDecode(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrReleasePromotionInvalid
	}
	return nil
}

func mustReleasePromotionJSON(value any) []byte { body, _ := json.Marshal(value); return body }
func mustReleasePromotionJSONWithoutAuthority(value any) []byte {
	switch typed := value.(type) {
	case canonicalCandidateReleaseImportSelectorV1:
		typed.MAC = ""
		typed.Digest = ""
		value = typed
	case CanonicalCandidateReleaseImportReceiptV1:
		typed.MAC = ""
		typed.Digest = ""
		value = typed
	}
	return mustReleasePromotionJSON(value)
}
func releasePromotionDigest(domain string, body []byte) string {
	digest := sha256.Sum256(append([]byte(domain), body...))
	return hex.EncodeToString(digest[:])
}

func (s *ReleaseImportStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for index := range s.key {
		s.key[index] = 0
	}
	if s.directory == nil {
		return nil
	}
	err := s.directory.Close()
	s.directory = nil
	return err
}
