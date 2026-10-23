package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	operatorControlDirectoryName = "operator-control"
	operatorControlKeyName       = "key"
	candidateReceiptDirectory    = "receipt-export"
	candidateReceiptKeyName      = "key"
)

type OperationCoordinatorInspectionV1 struct {
	Found       bool   `json:"-"`
	OperationID string `json:"operation_id,omitempty"`
	Phase       string `json:"phase,omitempty"`
	ValueDigest string `json:"value_digest,omitempty"`
	Receipt     bool   `json:"receipt"`
}

func InspectOperationCoordinatorState(ctx context.Context, fsys fsutil.FileSystem, root, operationID string) (OperationCoordinatorInspectionV1, error) {
	if ctx == nil || fsys == nil || invalidInspectionRoot(root) || (operationID != "" && !lowerHexBytes(operationID, 16)) {
		return OperationCoordinatorInspectionV1{}, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return OperationCoordinatorInspectionV1{}, err
	}
	directoryPath := filepath.Join(root, operatorControlDirectoryName)
	if err := fsutil.ValidateSecureDirectory(fsys, directoryPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OperationCoordinatorInspectionV1{}, nil
		}
		return OperationCoordinatorInspectionV1{}, err
	}
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	if !openerOK || !inspectorOK {
		return OperationCoordinatorInspectionV1{}, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return OperationCoordinatorInspectionV1{}, err
	}
	defer directory.Close()
	key, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, operatorControlKeyName, sha256.Size+1)
	if errors.Is(err, os.ErrNotExist) {
		return OperationCoordinatorInspectionV1{}, nil
	}
	if err != nil {
		return OperationCoordinatorInspectionV1{}, err
	}
	defer zeroRuntimeBytes(key)
	if len(key) != sha256.Size {
		return OperationCoordinatorInspectionV1{}, errors.New("operator control key invalid")
	}
	store := &OperationCoordinatorStore{ctx: ctx, inspector: inspector, directory: directory}
	copy(store.key[:], key)
	if err := store.reopen(); err != nil {
		return OperationCoordinatorInspectionV1{}, err
	}
	return store.inspect(operationID)
}

func (s *OperationCoordinatorStore) inspect(operationID string) (OperationCoordinatorInspectionV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selected != nil && (operationID == "" || s.selected.OperationID == operationID) {
		return OperationCoordinatorInspectionV1{
			Found: true, OperationID: s.selected.OperationID, Phase: s.selected.Phase,
			ValueDigest: s.selected.ValueDigest, Receipt: s.receipt != nil || s.selected.Phase == "terminal",
		}, nil
	}
	if operationID == "" {
		return OperationCoordinatorInspectionV1{}, nil
	}
	body, _, err := s.readStable("receipt-"+operationID+".json", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return OperationCoordinatorInspectionV1{}, nil
	}
	if err != nil {
		return OperationCoordinatorInspectionV1{}, err
	}
	receipt, _, err := s.openObject(body)
	if err != nil || receipt.OperationID != operationID || receipt.Phase != "receipt" {
		return OperationCoordinatorInspectionV1{}, errors.New("operation coordinator receipt mismatch")
	}
	return OperationCoordinatorInspectionV1{Found: true, OperationID: operationID, Phase: "terminal", ValueDigest: receipt.ValueDigest, Receipt: true}, nil
}

type CandidateReceiptInspectionV1 struct {
	Found           bool   `json:"-"`
	AttemptID       string `json:"attempt_id,omitempty"`
	Outcome         string `json:"terminal_outcome,omitempty"`
	ReceiptDigest   string `json:"compact_terminal_receipt_digest,omitempty"`
	PromotionDigest string `json:"promotion_receipt_digest,omitempty"`
}

type candidateReceiptStoredV1 struct {
	SchemaVersion   int    `json:"schema_version"`
	Kind            string `json:"kind"`
	AttemptID       string `json:"attempt_id"`
	Outcome         string `json:"terminal_outcome"`
	ReceiptDigest   string `json:"compact_terminal_receipt_digest"`
	PromotionDigest string `json:"promotion_receipt_digest"`
	MAC             string `json:"mac"`
}

func InspectCandidateReceiptState(ctx context.Context, fsys fsutil.FileSystem, root, attemptID string) (CandidateReceiptInspectionV1, error) {
	if ctx == nil || fsys == nil || invalidInspectionRoot(root) || !lowerHexBytes(attemptID, 16) {
		return CandidateReceiptInspectionV1{}, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return CandidateReceiptInspectionV1{}, err
	}
	directoryPath := filepath.Join(root, candidateReceiptDirectory)
	if err := fsutil.ValidateSecureDirectory(fsys, directoryPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CandidateReceiptInspectionV1{}, nil
		}
		return CandidateReceiptInspectionV1{}, err
	}
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	if !openerOK || !inspectorOK {
		return CandidateReceiptInspectionV1{}, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return CandidateReceiptInspectionV1{}, err
	}
	defer directory.Close()
	key, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, candidateReceiptKeyName, sha256.Size+1)
	if errors.Is(err, os.ErrNotExist) {
		return CandidateReceiptInspectionV1{}, nil
	}
	if err != nil {
		return CandidateReceiptInspectionV1{}, err
	}
	defer zeroRuntimeBytes(key)
	if len(key) != sha256.Size {
		return CandidateReceiptInspectionV1{}, errors.New("candidate receipt key invalid")
	}
	body, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, attemptID+".json", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return CandidateReceiptInspectionV1{}, nil
	}
	if err != nil {
		return CandidateReceiptInspectionV1{}, err
	}
	var stored candidateReceiptStoredV1
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return CandidateReceiptInspectionV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CandidateReceiptInspectionV1{}, errors.New("candidate receipt has trailing data")
	}
	wantMAC := stored.MAC
	stored.MAC = ""
	unsigned, _ := CanonicalJSONV1(stored)
	if stored.SchemaVersion != 1 || stored.Kind != "candidate_receipt_export_terminal_v1" || stored.AttemptID != attemptID || (stored.Outcome != "published" && stored.Outcome != "conflicted") || !lowerHexBytes(stored.ReceiptDigest, 32) || !lowerHexBytes(stored.PromotionDigest, 32) || !hmac.Equal([]byte(wantMAC), []byte(hmacHex(key, "cq-candidate-receipt-export-v1\x00", unsigned))) {
		return CandidateReceiptInspectionV1{}, errors.New("candidate receipt authentication failed")
	}
	stored.MAC = wantMAC
	canonical, _ := CanonicalJSONV1(stored)
	if !bytes.Equal(canonical, body) {
		return CandidateReceiptInspectionV1{}, errors.New("candidate receipt is not canonical")
	}
	return CandidateReceiptInspectionV1{Found: true, AttemptID: attemptID, Outcome: stored.Outcome, ReceiptDigest: stored.ReceiptDigest, PromotionDigest: stored.PromotionDigest}, nil
}

func invalidInspectionRoot(root string) bool {
	clean := filepath.Clean(root)
	return root == "" || !filepath.IsAbs(root) || clean != root || clean == string(filepath.Separator)
}

func lowerHexBytes(value string, byteCount int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == byteCount && value == hex.EncodeToString(decoded)
}

func CandidateReceiptStoredBytesV1(result CandidateReceiptInspectionV1, key []byte) ([]byte, error) {
	if !result.Found || len(key) != sha256.Size || !lowerHexBytes(result.AttemptID, 16) || (result.Outcome != "published" && result.Outcome != "conflicted") || !lowerHexBytes(result.ReceiptDigest, 32) || !lowerHexBytes(result.PromotionDigest, 32) {
		return nil, fmt.Errorf("candidate receipt invalid")
	}
	stored := candidateReceiptStoredV1{SchemaVersion: 1, Kind: "candidate_receipt_export_terminal_v1", AttemptID: result.AttemptID, Outcome: result.Outcome, ReceiptDigest: result.ReceiptDigest, PromotionDigest: result.PromotionDigest}
	unsigned, _ := CanonicalJSONV1(stored)
	stored.MAC = hmacHex(key, "cq-candidate-receipt-export-v1\x00", unsigned)
	return CanonicalJSONV1(stored)
}
