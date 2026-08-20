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

var (
	ErrRealClientValidationInvalid     = errors.New("real-client validation authority invalid")
	ErrRealClientValidationReplay      = errors.New("real-client validation dispatch replayed")
	ErrRealClientValidationQuarantined = errors.New("real-client validation quarantined")
)

type RealClientValidationOutcome string

const (
	RealClientValidationPassed        RealClientValidationOutcome = "passed"
	RealClientValidationFailed        RealClientValidationOutcome = "failed"
	RealClientValidationIndeterminate RealClientValidationOutcome = "indeterminate"
)

type RealClientValidationPreparationV1 struct {
	SchemaVersion          int    `json:"schema_version"`
	OperationID            string `json:"operation_id"`
	ValidationRunID        string `json:"validation_run_id"`
	FinalRouteChoiceDigest string `json:"final_route_choice_digest"`
	RequestDigest          string `json:"request_digest"`
	MAC                    string `json:"mac"`
}

type RealClientValidationDispatchGrantV1 struct {
	SchemaVersion     int       `json:"schema_version"`
	OperationID       string    `json:"operation_id"`
	PreparationDigest string    `json:"preparation_digest"`
	GrantID           string    `json:"grant_id"`
	IssuedAt          time.Time `json:"issued_at"`
	ValidUntil        time.Time `json:"valid_until"`
	MAC               string    `json:"mac"`
	Digest            string    `json:"digest"`
}

type RealClientValidationConsumptionV1 struct {
	SchemaVersion            int       `json:"schema_version"`
	OperationID              string    `json:"operation_id"`
	GrantDigest              string    `json:"grant_digest"`
	JournalTransactionDigest string    `json:"journal_transaction_digest"`
	DispatchCommittedAt      time.Time `json:"dispatch_committed_at"`
	MAC                      string    `json:"mac"`
}

type RealClientValidationObservationV1 struct {
	Outcome        RealClientValidationOutcome `json:"outcome"`
	ResponseDigest string                      `json:"response_digest,omitempty"`
}

type RealClientValidationReceiptV1 struct {
	SchemaVersion     int                         `json:"schema_version"`
	OperationID       string                      `json:"operation_id"`
	GrantDigest       string                      `json:"grant_digest"`
	ConsumptionDigest string                      `json:"consumption_digest"`
	Outcome           RealClientValidationOutcome `json:"outcome"`
	ResponseDigest    string                      `json:"response_digest,omitempty"`
	CompletedAt       time.Time                   `json:"completed_at"`
	MAC               string                      `json:"mac"`
	Digest            string                      `json:"digest"`
}

type realClientValidationQuarantineV1 struct {
	SchemaVersion int                         `json:"schema_version"`
	OperationID   string                      `json:"operation_id"`
	GrantDigest   string                      `json:"grant_digest"`
	Outcome       RealClientValidationOutcome `json:"outcome"`
	SelectedAt    time.Time                   `json:"selected_at"`
	MAC           string                      `json:"mac"`
}

type RealClientValidationStore struct {
	mu        sync.Mutex
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	key       [sha256.Size]byte
	now       func() time.Time
	random    io.Reader
	closed    bool
}

func OpenRealClientValidationStore(fsys fsutil.FileSystem, path string, key []byte, now func() time.Time, random io.Reader) (*RealClientValidationStore, error) {
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if fsys == nil || !inspectorOK || !openerOK || path == "" || len(key) != sha256.Size || now == nil || random == nil {
		return nil, ErrRealClientValidationInvalid
	}
	if err := fsutil.EnsureSecureDirectory(fsys, path); err != nil {
		return nil, err
	}
	directory, err := opener.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	store := &RealClientValidationStore{inspector: inspector, directory: directory, now: now, random: random}
	copy(store.key[:], key)
	return store, nil
}

func (s *RealClientValidationStore) Prepare(ctx context.Context, preparation RealClientValidationPreparationV1) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	if preparation.OperationID == "" || preparation.ValidationRunID == "" || !lowerHexDigest(preparation.FinalRouteChoiceDigest) || !lowerHexDigest(preparation.RequestDigest) {
		return ErrRealClientValidationInvalid
	}
	preparation.SchemaVersion = 1
	preparation.MAC = s.mac("cq/real-client-validation-preparation/v1\x00", preparation)
	return s.create(rcvName("preparation", preparation.OperationID), preparation)
}

func (s *RealClientValidationStore) IssueGrant(ctx context.Context, operationID string) (RealClientValidationDispatchGrantV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return RealClientValidationDispatchGrantV1{}, err
	}
	var preparation RealClientValidationPreparationV1
	if err := s.read(rcvName("preparation", operationID), &preparation); err != nil || !s.validMAC("cq/real-client-validation-preparation/v1\x00", preparation, preparation.MAC) {
		return RealClientValidationDispatchGrantV1{}, ErrRealClientValidationInvalid
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return RealClientValidationDispatchGrantV1{}, err
	}
	issuedAt := s.now().UTC()
	grant := RealClientValidationDispatchGrantV1{SchemaVersion: 1, OperationID: operationID, PreparationDigest: rcvDigest("cq/real-client-validation-preparation-digest/v1\x00", preparation), GrantID: hex.EncodeToString(nonce), IssuedAt: issuedAt, ValidUntil: issuedAt.Add(5 * time.Second)}
	grant.MAC = s.mac("cq/real-client-validation-grant/v1\x00", grant)
	grant.Digest = rcvDigest("cq/real-client-validation-grant-digest/v1\x00", grant)
	if err := s.create(rcvName("grant", operationID), grant); err != nil {
		return RealClientValidationDispatchGrantV1{}, err
	}
	return grant, nil
}

func (s *RealClientValidationStore) ConsumeDispatch(ctx context.Context, grant RealClientValidationDispatchGrantV1, transactionDigest string) (RealClientValidationConsumptionV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return RealClientValidationConsumptionV1{}, err
	}
	if !lowerHexDigest(transactionDigest) || !s.validGrant(grant) {
		return RealClientValidationConsumptionV1{}, ErrRealClientValidationInvalid
	}
	var durableGrant RealClientValidationDispatchGrantV1
	if err := s.read(rcvName("grant", grant.OperationID), &durableGrant); err != nil || durableGrant.Digest != grant.Digest || !s.validGrant(durableGrant) {
		return RealClientValidationConsumptionV1{}, ErrRealClientValidationInvalid
	}
	committedAt := s.now().UTC()
	if committedAt.Before(grant.IssuedAt) || !committedAt.Before(grant.ValidUntil) {
		return RealClientValidationConsumptionV1{}, ErrRealClientValidationInvalid
	}
	consumption := RealClientValidationConsumptionV1{SchemaVersion: 1, OperationID: grant.OperationID, GrantDigest: grant.Digest, JournalTransactionDigest: transactionDigest, DispatchCommittedAt: committedAt}
	consumption.MAC = s.mac("cq/real-client-validation-consumption/v1\x00", consumption)
	if err := s.create(rcvName("consumption", grant.OperationID), consumption); err != nil {
		if errors.Is(err, os.ErrExist) {
			return RealClientValidationConsumptionV1{}, ErrRealClientValidationReplay
		}
		return RealClientValidationConsumptionV1{}, err
	}
	return consumption, nil
}

func (s *RealClientValidationStore) Complete(ctx context.Context, grant RealClientValidationDispatchGrantV1, observation RealClientValidationObservationV1) (RealClientValidationReceiptV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return RealClientValidationReceiptV1{}, err
	}
	if !s.validGrant(grant) || !validRCVObservation(observation) {
		return RealClientValidationReceiptV1{}, ErrRealClientValidationInvalid
	}
	var consumption RealClientValidationConsumptionV1
	if err := s.read(rcvName("consumption", grant.OperationID), &consumption); err != nil || consumption.GrantDigest != grant.Digest || !s.validMAC("cq/real-client-validation-consumption/v1\x00", consumption, consumption.MAC) {
		return RealClientValidationReceiptV1{}, ErrRealClientValidationInvalid
	}
	completedAt := s.now().UTC()
	if observation.Outcome != RealClientValidationPassed {
		quarantine := realClientValidationQuarantineV1{SchemaVersion: 1, OperationID: grant.OperationID, GrantDigest: grant.Digest, Outcome: observation.Outcome, SelectedAt: completedAt}
		quarantine.MAC = s.mac("cq/real-client-validation-quarantine/v1\x00", quarantine)
		if err := s.create(rcvName("quarantine", grant.OperationID), quarantine); err != nil && !errors.Is(err, os.ErrExist) {
			return RealClientValidationReceiptV1{}, err
		}
	}
	receipt := RealClientValidationReceiptV1{SchemaVersion: 1, OperationID: grant.OperationID, GrantDigest: grant.Digest, ConsumptionDigest: rcvDigest("cq/real-client-validation-consumption-digest/v1\x00", consumption), Outcome: observation.Outcome, ResponseDigest: observation.ResponseDigest, CompletedAt: completedAt}
	receipt.MAC = s.mac("cq/real-client-validation-receipt/v1\x00", receipt)
	receipt.Digest = rcvDigest("cq/real-client-validation-receipt-digest/v1\x00", receipt)
	if err := s.create(rcvName("receipt", grant.OperationID), receipt); err != nil {
		return RealClientValidationReceiptV1{}, err
	}
	return receipt, nil
}

func (s *RealClientValidationStore) RequirePromotionReceipt(operationID, receiptDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrRealClientValidationInvalid
	}
	var receipt RealClientValidationReceiptV1
	if err := s.read(rcvName("receipt", operationID), &receipt); err != nil || receipt.Digest != receiptDigest || receipt.Outcome != RealClientValidationPassed || !s.validMAC("cq/real-client-validation-receipt/v1\x00", receipt, receipt.MAC) || receipt.Digest != rcvDigest("cq/real-client-validation-receipt-digest/v1\x00", receipt) {
		return ErrRealClientValidationQuarantined
	}
	var quarantine realClientValidationQuarantineV1
	if err := s.read(rcvName("quarantine", operationID), &quarantine); err == nil || !errors.Is(err, os.ErrNotExist) {
		return ErrRealClientValidationQuarantined
	}
	return nil
}

func (s *RealClientValidationStore) create(name string, value any) error {
	body, err := json.Marshal(value)
	if err != nil || len(body) > 64<<10 {
		return ErrRealClientValidationInvalid
	}
	return fsutil.SecureAtomicCreateInDirectory(s.inspector, s.directory, name, body)
}

func (s *RealClientValidationStore) read(name string, target any) error {
	body, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(s.inspector, s.directory, name, 64<<10)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrRealClientValidationInvalid
	}
	return nil
}

func (s *RealClientValidationStore) ready(ctx context.Context) error {
	if s == nil || s.closed || s.directory == nil || ctx == nil {
		return ErrRealClientValidationInvalid
	}
	return ctx.Err()
}

func (s *RealClientValidationStore) validGrant(grant RealClientValidationDispatchGrantV1) bool {
	return grant.SchemaVersion == 1 && grant.OperationID != "" && lowerHexDigest(grant.PreparationDigest) && lowerHexDigest(grant.Digest) && grant.GrantID != "" && grant.IssuedAt.Equal(grant.IssuedAt.UTC()) && grant.ValidUntil.Equal(grant.ValidUntil.UTC()) && grant.IssuedAt.Before(grant.ValidUntil) && grant.ValidUntil.Sub(grant.IssuedAt) <= 5*time.Second && s.validMAC("cq/real-client-validation-grant/v1\x00", grant, grant.MAC) && grant.Digest == rcvDigest("cq/real-client-validation-grant-digest/v1\x00", grant)
}

func validRCVObservation(observation RealClientValidationObservationV1) bool {
	switch observation.Outcome {
	case RealClientValidationPassed:
		return lowerHexDigest(observation.ResponseDigest)
	case RealClientValidationFailed, RealClientValidationIndeterminate:
		return observation.ResponseDigest == "" || lowerHexDigest(observation.ResponseDigest)
	default:
		return false
	}
}

func (s *RealClientValidationStore) mac(domain string, value any) string {
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write(rcvCanonical(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *RealClientValidationStore) validMAC(domain string, value any, got string) bool {
	return lowerHexDigest(got) && hmac.Equal([]byte(s.mac(domain, value)), []byte(got))
}

func rcvCanonical(value any) []byte {
	switch typed := value.(type) {
	case RealClientValidationPreparationV1:
		typed.MAC = ""
		value = typed
	case RealClientValidationDispatchGrantV1:
		typed.MAC = ""
		typed.Digest = ""
		value = typed
	case RealClientValidationConsumptionV1:
		typed.MAC = ""
		value = typed
	case RealClientValidationReceiptV1:
		typed.MAC = ""
		typed.Digest = ""
		value = typed
	case realClientValidationQuarantineV1:
		typed.MAC = ""
		value = typed
	}
	body, _ := json.Marshal(value)
	return body
}

func rcvDigest(domain string, value any) string {
	digest := sha256.Sum256(append([]byte(domain), rcvCanonical(value)...))
	return hex.EncodeToString(digest[:])
}

func rcvName(kind, operationID string) string {
	digest := sha256.Sum256([]byte(operationID))
	return kind + "-" + hex.EncodeToString(digest[:]) + ".json"
}

func (s *RealClientValidationStore) Close() error {
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
