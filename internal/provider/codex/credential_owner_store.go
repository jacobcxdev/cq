package codex

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

type CredentialOwnerRecorder interface {
	PublishCommit(operationID, reservationDigest, capacityLeaseDigest string) (string, error)
	PublishReceipt(operationID, commitDigest string) (string, error)
}

type CredentialOwnerRefreshResult struct {
	Material   CredentialMaterial `json:"material"`
	ExpiresIn  int64              `json:"expires_in"`
	Error      string             `json:"error,omitempty"`
	Definitive bool               `json:"definitive,omitempty"`
}

type CredentialOwnerRefreshRecovery struct {
	CommitDigest string
	Attempted    bool
	Result       *CredentialOwnerRefreshResult
}

type CredentialOwnerRecoveryRecorder interface {
	PublishRefreshAttempt(operationID, commitDigest string) error
	PublishRefreshResult(operationID, commitDigest string, result CredentialOwnerRefreshResult) error
	RecoverRefresh(recovery RefreshMutationRecovery) (CredentialOwnerRefreshRecovery, error)
}

type CredentialAuthorityIdentity struct {
	Device uint64
	Inode  uint64
	Links  uint64
	Size   int64
	Digest string
}

// CredentialAuthorityBackend is implemented by the proxy authority adapter
// backed by a retained SecureDirectory, Task3 publisher/CAS capability, and a
// persistent mutation-lock description.
type CredentialAuthorityBackend interface {
	Acquire(context.Context) (func() error, error)
	PublishImmutable(context.Context, string, []byte) (CredentialAuthorityIdentity, error)
	ReplaceSelectorExactPrior(context.Context, string, CredentialAuthorityIdentity, []byte) (CredentialAuthorityIdentity, error)
	Read(context.Context, string, int64) ([]byte, CredentialAuthorityIdentity, error)
}

type CredentialAuthorityOccupancy struct {
	Files int
	Bytes int64
	Units int
}

type CredentialAuthorityOccupancyBackend interface {
	CredentialAuthorityOccupancy(context.Context) (CredentialAuthorityOccupancy, error)
}

type credentialAuthorityObject struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	OperationID    string `json:"operation_id"`
	Phase          string `json:"phase"`
	ValueDigest    string `json:"value_digest"`
	PreviousDigest string `json:"previous_digest,omitempty"`
	MAC            string `json:"mac,omitempty"`
}

type credentialAuthorityAnchor struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	OperationID   string `json:"operation_id"`
	Phase         string `json:"phase"`
	ObjectDigest  string `json:"object_digest"`
	MAC           string `json:"mac,omitempty"`
}

type credentialAuthorityChain struct {
	mu            sync.Mutex
	ctx           context.Context
	backend       CredentialAuthorityBackend
	kind          string
	key           [32]byte
	hook          func(string) error
	anchor        *credentialAuthorityAnchor
	anchorID      CredentialAuthorityIdentity
	selected      *credentialAuthorityObject
	receipt       *credentialAuthorityObject
	receiptDigest string
	release       func() error
}

func openCredentialAuthorityChain(ctx context.Context, backend CredentialAuthorityBackend, kind string, key []byte, hook func(string) error) (*credentialAuthorityChain, error) {
	if ctx == nil || backend == nil || kind == "" || len(key) != 32 {
		return nil, errors.New("credential authority durable capability unavailable")
	}
	chain := &credentialAuthorityChain{ctx: ctx, backend: backend, kind: kind, hook: hook}
	copy(chain.key[:], key)
	release, err := backend.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	chain.release = release
	if err := chain.reopenUnderGate(); err != nil {
		_ = chain.releaseGate()
		return nil, err
	}
	if chain.anchor == nil || chain.anchor.Phase == "terminal" {
		if err := chain.releaseGate(); err != nil {
			return nil, err
		}
	}
	return chain, nil
}

func (c *credentialAuthorityChain) selectOperation(operationID, valueDigest string) (string, error) {
	if operationID == "" || valueDigest == "" {
		return "", errors.New("credential authority selection identity unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.release == nil {
		release, err := c.backend.Acquire(c.ctx)
		if err != nil {
			return "", err
		}
		c.release = release
		if err := c.reopenUnderGate(); err != nil {
			_ = c.releaseGate()
			return "", err
		}
	}
	if c.anchor != nil {
		if c.anchor.Phase == "selected" && c.anchor.OperationID == operationID && c.selected != nil && c.selected.ValueDigest == valueDigest {
			return c.anchor.ObjectDigest, nil
		}
		if c.anchor.Phase != "terminal" || c.anchor.OperationID == operationID {
			return "", errors.New("credential authority operation already selected")
		}
	}
	committed := false
	defer func() {
		if !committed {
			_ = c.releaseGate()
		}
	}()
	previousDigest := ""
	if c.anchor != nil {
		previousDigest = c.anchor.ObjectDigest
	}
	object, objectBody, objectDigest, err := c.sealObject(credentialAuthorityObject{SchemaVersion: 1, Kind: c.kind + "_object_v1", OperationID: operationID, Phase: "selected", ValueDigest: valueDigest, PreviousDigest: previousDigest})
	if err != nil {
		return "", err
	}
	if _, err := c.publishOrAdopt(c.objectName(objectDigest), objectBody); err != nil {
		return "", err
	}
	if err := c.callHook("selected_object_durable"); err != nil {
		return "", err
	}
	anchor, anchorBody, err := c.sealAnchor(credentialAuthorityAnchor{SchemaVersion: 1, Kind: c.kind + "_anchor_v1", OperationID: operationID, Phase: "selected", ObjectDigest: objectDigest})
	if err != nil {
		return "", err
	}
	var anchorIdentity CredentialAuthorityIdentity
	if c.anchor == nil {
		anchorIdentity, err = c.backend.PublishImmutable(c.ctx, c.anchorName(), anchorBody)
	} else {
		anchorIdentity, err = c.backend.ReplaceSelectorExactPrior(c.ctx, c.anchorName(), c.anchorID, anchorBody)
	}
	if err != nil {
		return "", err
	}
	c.anchor, c.anchorID, c.selected, c.receipt, c.receiptDigest = &anchor, anchorIdentity, &object, nil, ""
	if err := c.callHook("selected_anchor_durable"); err != nil {
		return "", err
	}
	committed = true
	return objectDigest, nil
}

func (c *credentialAuthorityChain) acquireAndReopenGate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.release != nil {
		return nil
	}
	release, err := c.backend.Acquire(c.ctx)
	if err != nil {
		return err
	}
	c.release = release
	if err := c.reopenUnderGate(); err != nil {
		_ = c.releaseGate()
		return err
	}
	return nil
}

func (c *credentialAuthorityChain) abandonGate() { // best-effort on a failed pre-effect path
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.releaseGate()
}

func (c *credentialAuthorityChain) terminalise(operationID, valueDigest string) (string, string, error) {
	if operationID == "" || valueDigest == "" {
		return "", "", errors.New("credential authority terminal identity unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.anchor == nil || c.selected == nil || c.anchor.OperationID != operationID {
		return "", "", errors.New("credential authority predecessor unavailable")
	}
	if c.anchor.Phase == "terminal" {
		if c.receipt != nil && c.receipt.ValueDigest == valueDigest {
			return c.receiptDigest, c.anchor.ObjectDigest, nil
		}
		return "", "", errors.New("credential authority terminal collision")
	}
	if c.anchor.Phase != "selected" || c.release == nil {
		return "", "", errors.New("credential authority mutation lock unavailable")
	}
	receipt, receiptBody, receiptDigest, err := c.sealObject(credentialAuthorityObject{SchemaVersion: 1, Kind: c.kind + "_receipt_v1", OperationID: operationID, Phase: "receipt", ValueDigest: valueDigest, PreviousDigest: c.anchor.ObjectDigest})
	if err != nil {
		return "", "", err
	}
	if _, err := c.publishOrAdopt(c.receiptName(operationID), receiptBody); err != nil {
		return "", "", err
	}
	c.receipt, c.receiptDigest = &receipt, receiptDigest
	if err := c.callHook("receipt_durable"); err != nil {
		return "", "", err
	}
	terminal, terminalBody, terminalDigest, err := c.sealObject(credentialAuthorityObject{SchemaVersion: 1, Kind: c.kind + "_object_v1", OperationID: operationID, Phase: "terminal", ValueDigest: valueDigest, PreviousDigest: c.anchor.ObjectDigest})
	if err != nil {
		return "", "", err
	}
	if _, err := c.publishOrAdopt(c.objectName(terminalDigest), terminalBody); err != nil {
		return "", "", err
	}
	if err := c.callHook("terminal_object_durable"); err != nil {
		return "", "", err
	}
	anchor, anchorBody, err := c.sealAnchor(credentialAuthorityAnchor{SchemaVersion: 1, Kind: c.kind + "_anchor_v1", OperationID: operationID, Phase: "terminal", ObjectDigest: terminalDigest})
	if err != nil {
		return "", "", err
	}
	anchorIdentity, err := c.backend.ReplaceSelectorExactPrior(c.ctx, c.anchorName(), c.anchorID, anchorBody)
	if err != nil {
		return "", "", err
	}
	c.anchor, c.anchorID, c.selected = &anchor, anchorIdentity, &terminal
	if err := c.releaseGate(); err != nil {
		return "", "", err
	}
	return receiptDigest, terminalDigest, nil
}

func (c *credentialAuthorityChain) reopenUnderGate() error {
	c.anchor, c.selected, c.receipt, c.receiptDigest = nil, nil, nil, ""
	anchorBody, anchorIdentity, err := c.backend.Read(c.ctx, c.anchorName(), 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	anchor, err := c.openAnchor(anchorBody)
	if err != nil {
		return err
	}
	objectBody, _, err := c.backend.Read(c.ctx, c.objectName(anchor.ObjectDigest), 512<<10)
	if err != nil {
		return err
	}
	object, objectDigest, err := c.openObject(objectBody)
	if err != nil || objectDigest != anchor.ObjectDigest || object.OperationID != anchor.OperationID || object.Phase != anchor.Phase {
		return errors.New("credential authority anchor/object mismatch")
	}
	c.anchor, c.anchorID, c.selected = &anchor, anchorIdentity, &object
	receiptBody, _, receiptErr := c.backend.Read(c.ctx, c.receiptName(anchor.OperationID), 64<<10)
	if receiptErr == nil {
		receipt, receiptDigest, openErr := c.openObject(receiptBody)
		expectedPrevious := anchor.ObjectDigest
		if object.Phase == "terminal" {
			expectedPrevious = object.PreviousDigest
		}
		if openErr != nil || receipt.Phase != "receipt" || receipt.OperationID != anchor.OperationID || receipt.PreviousDigest != expectedPrevious {
			return errors.New("credential authority receipt mismatch")
		}
		c.receipt, c.receiptDigest = &receipt, receiptDigest
	} else if !errors.Is(receiptErr, os.ErrNotExist) {
		return receiptErr
	}
	return nil
}

func (c *credentialAuthorityChain) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releaseGate()
}

func (c *credentialAuthorityChain) releaseGate() error {
	if c.release == nil {
		return nil
	}
	release := c.release
	c.release = nil
	return release()
}

func (c *credentialAuthorityChain) sealObject(object credentialAuthorityObject) (credentialAuthorityObject, []byte, string, error) {
	unsigned, err := json.Marshal(object)
	if err != nil {
		return object, nil, "", err
	}
	object.MAC = credentialAuthorityHMACHex(c.key[:], "cq/credential-owner/"+c.kind+"/object/mac/v1\x00", unsigned)
	body, err := json.Marshal(object)
	if err != nil {
		return object, nil, "", err
	}
	return object, body, framedSHA256("cq/credential-owner/"+c.kind+"/object/v1\x00", body), nil
}

func (c *credentialAuthorityChain) openObject(body []byte) (credentialAuthorityObject, string, error) {
	var object credentialAuthorityObject
	if err := json.Unmarshal(body, &object); err != nil || object.SchemaVersion != 1 || object.OperationID == "" || object.ValueDigest == "" || object.Kind == "" {
		return object, "", errors.New("invalid credential authority object")
	}
	wantMAC := object.MAC
	object.MAC = ""
	unsigned, _ := json.Marshal(object)
	if !hmac.Equal([]byte(wantMAC), []byte(credentialAuthorityHMACHex(c.key[:], "cq/credential-owner/"+c.kind+"/object/mac/v1\x00", unsigned))) {
		return object, "", errors.New("invalid credential authority object MAC")
	}
	object.MAC = wantMAC
	return object, framedSHA256("cq/credential-owner/"+c.kind+"/object/v1\x00", body), nil
}

func (c *credentialAuthorityChain) sealAnchor(anchor credentialAuthorityAnchor) (credentialAuthorityAnchor, []byte, error) {
	unsigned, err := json.Marshal(anchor)
	if err != nil {
		return anchor, nil, err
	}
	anchor.MAC = credentialAuthorityHMACHex(c.key[:], "cq/credential-owner/"+c.kind+"/anchor/mac/v1\x00", unsigned)
	body, err := json.Marshal(anchor)
	return anchor, body, err
}

func (c *credentialAuthorityChain) openAnchor(body []byte) (credentialAuthorityAnchor, error) {
	var anchor credentialAuthorityAnchor
	if err := json.Unmarshal(body, &anchor); err != nil || anchor.SchemaVersion != 1 || anchor.OperationID == "" || anchor.ObjectDigest == "" || anchor.Kind == "" {
		return anchor, errors.New("invalid credential authority anchor")
	}
	wantMAC := anchor.MAC
	anchor.MAC = ""
	unsigned, _ := json.Marshal(anchor)
	if !hmac.Equal([]byte(wantMAC), []byte(credentialAuthorityHMACHex(c.key[:], "cq/credential-owner/"+c.kind+"/anchor/mac/v1\x00", unsigned))) {
		return anchor, errors.New("invalid credential authority anchor MAC")
	}
	anchor.MAC = wantMAC
	return anchor, nil
}

func credentialAuthorityHMACHex(key []byte, domain string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	_, _ = mac.Write(size[:])
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *credentialAuthorityChain) publishOrAdopt(name string, body []byte) (CredentialAuthorityIdentity, error) {
	identity, err := c.backend.PublishImmutable(c.ctx, name, body)
	if err == nil {
		return identity, nil
	}
	existing, existingIdentity, readErr := c.backend.Read(c.ctx, name, int64(len(body))+1)
	if readErr != nil || !hmac.Equal(existing, body) {
		return CredentialAuthorityIdentity{}, err
	}
	return existingIdentity, nil
}

func (c *credentialAuthorityChain) callHook(phase string) error {
	if c.hook == nil {
		return nil
	}
	if err := c.hook(phase); err != nil {
		return fmt.Errorf("credential authority %s: %w", phase, err)
	}
	return nil
}

func (c *credentialAuthorityChain) objectName(digest string) string {
	return c.kind + "-object-" + digest + ".json"
}
func (c *credentialAuthorityChain) anchorName() string { return c.kind + "-anchor" }
func (c *credentialAuthorityChain) receiptName(operationID string) string {
	return c.kind + "-receipt-" + operationID + ".json"
}

type credentialOwnerContinuationV1 struct {
	SchemaVersion       int          `json:"schema_version"`
	OperationID         string       `json:"operation_id"`
	Ref                 CandidateRef `json:"ref"`
	Revision            Revision     `json:"revision"`
	ReservationDigest   string       `json:"reservation_digest"`
	CapacityLeaseDigest string       `json:"capacity_lease_digest"`
	CommitValueDigest   string       `json:"commit_value_digest"`
	MAC                 string       `json:"mac,omitempty"`
}

type credentialOwnerEncryptedResultV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	OperationID   string `json:"operation_id"`
	CommitDigest  string `json:"commit_digest"`
	Nonce         string `json:"nonce"`
	Ciphertext    string `json:"ciphertext"`
}

type credentialOwnerRefreshAttemptV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	OperationID   string `json:"operation_id"`
	CommitDigest  string `json:"commit_digest"`
	MAC           string `json:"mac,omitempty"`
}

const credentialOwnerRefreshResultEnvelopeMaxBytes = 1 << 20

type CredentialOwnerStore struct {
	chain     *credentialAuthorityChain
	resultKey [32]byte
	random    io.Reader
}

func OpenCredentialOwnerStore(ctx context.Context, backend CredentialAuthorityBackend, key []byte, hook func(string) error) (*CredentialOwnerStore, error) {
	chain, err := openCredentialAuthorityChain(ctx, backend, "commit", key, hook)
	if err != nil {
		return nil, err
	}
	resultKey, err := hkdf.Key(sha256.New, key, nil, "cq/credential-owner/refresh-result/key/v1", 32)
	if err != nil {
		_ = chain.Close()
		return nil, err
	}
	store := &CredentialOwnerStore{chain: chain, random: rand.Reader}
	copy(store.resultKey[:], resultKey)
	clear(resultKey)
	return store, nil
}

func (s *CredentialOwnerStore) PublishCommit(operationID, reservationDigest, capacityLeaseDigest string) (string, error) {
	if reservationDigest == "" || capacityLeaseDigest == "" {
		return "", errors.New("credential owner refresh binding unavailable")
	}
	reservationBody, _, err := s.chain.backend.Read(s.chain.ctx, "refresh-reservation-"+reservationDigest+".json", 128<<10)
	if err != nil || framedSHA256("cq/credential-owner/refresh-mutation/reservation/v1\x00", reservationBody) != reservationDigest {
		return "", errors.New("credential owner refresh reservation unavailable")
	}
	var reservation refreshReservationV1
	if err := json.Unmarshal(reservationBody, &reservation); err != nil || reservation.SchemaVersion != 1 || reservation.OperationID != operationID || reservation.CapacityLeaseDigest != capacityLeaseDigest {
		return "", errors.New("credential owner refresh reservation mismatch")
	}
	leaseBody, _, err := s.chain.backend.Read(s.chain.ctx, "refresh-capacity-lease-"+capacityLeaseDigest+".json", 128<<10)
	if err != nil || framedSHA256("cq/credential-owner/refresh-mutation/capacity-lease/v1\x00", leaseBody) != capacityLeaseDigest {
		return "", errors.New("credential owner refresh capacity lease unavailable")
	}
	var lease refreshCapacityLeaseV1
	if err := json.Unmarshal(leaseBody, &lease); err != nil || lease.OperationID != operationID || lease.validate() != nil {
		return "", errors.New("credential owner refresh capacity lease mismatch")
	}
	binding, err := json.Marshal(struct {
		ReservationDigest   string `json:"reservation_digest"`
		CapacityLeaseDigest string `json:"capacity_lease_digest"`
	}{reservationDigest, capacityLeaseDigest})
	if err != nil {
		return "", err
	}
	commitValueDigest := framedSHA256("cq/credential-owner/refresh-binding/v1\x00", binding)
	continuation, err := s.sealContinuation(credentialOwnerContinuationV1{
		SchemaVersion: 1, OperationID: operationID, Ref: reservation.Ref, Revision: reservation.Revision,
		ReservationDigest: reservationDigest, CapacityLeaseDigest: capacityLeaseDigest, CommitValueDigest: commitValueDigest,
	})
	if err != nil {
		return "", err
	}
	if _, err := s.chain.publishOrAdopt(s.continuationName(operationID), continuation); err != nil {
		return "", err
	}
	if err := s.chain.callHook("continuation_durable"); err != nil {
		return "", err
	}
	return s.chain.selectOperation(operationID, commitValueDigest)
}

func (s *CredentialOwnerStore) PublishRefreshAttempt(operationID, commitDigest string) error {
	if err := s.validateSelectedCommit(operationID, commitDigest); err != nil {
		return err
	}
	name := s.resultName(operationID)
	if existing, _, err := s.chain.backend.Read(s.chain.ctx, name, credentialOwnerRefreshResultEnvelopeMaxBytes); err == nil {
		if _, openErr := s.openRefreshAttempt(existing, operationID, commitDigest); openErr == nil {
			return nil
		}
		if _, openErr := s.openRefreshResult(existing, operationID, commitDigest); openErr == nil {
			return nil
		}
		return errors.New("credential owner refresh attempt collision")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := s.sealRefreshAttempt(credentialOwnerRefreshAttemptV1{SchemaVersion: 1, Kind: "refresh_attempt", OperationID: operationID, CommitDigest: commitDigest})
	if err != nil {
		return err
	}
	if _, err := s.chain.publishOrAdopt(name, body); err != nil {
		return err
	}
	return s.chain.callHook("refresh_attempt_durable")
}

func (s *CredentialOwnerStore) PublishRefreshResult(operationID, commitDigest string, result CredentialOwnerRefreshResult) error {
	if err := s.validateSelectedCommit(operationID, commitDigest); err != nil {
		return err
	}
	name := s.resultName(operationID)
	existing, existingIdentity, err := s.chain.backend.Read(s.chain.ctx, name, credentialOwnerRefreshResultEnvelopeMaxBytes)
	if err != nil {
		return err
	}
	if reopened, openErr := s.openRefreshResult(existing, operationID, commitDigest); openErr == nil {
		if reopened == result {
			return nil
		}
		return errors.New("credential owner refresh result collision")
	}
	if _, err := s.openRefreshAttempt(existing, operationID, commitDigest); err != nil {
		return errors.New("credential owner refresh attempt unavailable")
	}
	if err := validateCredentialOwnerRefreshResultInputBound(result); err != nil {
		return err
	}
	plaintext, err := json.Marshal(result)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	block, err := aes.NewCipher(s.resultKey[:])
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	envelopeSize, err := credentialOwnerRefreshResultEnvelopeSize(operationID, commitDigest, len(plaintext), gcm.NonceSize(), gcm.Overhead())
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return err
	}
	aad := []byte(operationID + "\x00" + commitDigest)
	envelope := credentialOwnerEncryptedResultV1{
		SchemaVersion: 1, Kind: "refresh_result", OperationID: operationID, CommitDigest: commitDigest,
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, aad)),
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if len(body) != envelopeSize {
		return errors.New("credential owner refresh result envelope size mismatch")
	}
	if _, err := s.chain.backend.ReplaceSelectorExactPrior(s.chain.ctx, name, existingIdentity, body); err != nil {
		return err
	}
	return s.chain.callHook("refresh_result_durable")
}

func (s *CredentialOwnerStore) RecoverRefresh(recovery RefreshMutationRecovery) (CredentialOwnerRefreshRecovery, error) {
	if err := s.chain.acquireAndReopenGate(); err != nil {
		return CredentialOwnerRefreshRecovery{}, err
	}
	defer func() {
		if s.chain.anchor == nil || s.chain.anchor.Phase == "terminal" {
			s.chain.abandonGate()
		}
	}()
	continuationBody, _, err := s.chain.backend.Read(s.chain.ctx, s.continuationName(recovery.OperationID), 64<<10)
	if errors.Is(err, os.ErrNotExist) && s.chain.anchor == nil {
		return CredentialOwnerRefreshRecovery{}, nil
	}
	if err != nil {
		return CredentialOwnerRefreshRecovery{}, err
	}
	continuation, err := s.openContinuation(continuationBody)
	if err != nil || continuation.OperationID != recovery.OperationID || continuation.Ref != recovery.Ref || continuation.Revision != recovery.Revision || continuation.ReservationDigest != recovery.Selection.ReservationDigest || continuation.CapacityLeaseDigest != recovery.Selection.CapacityLeaseDigest {
		return CredentialOwnerRefreshRecovery{}, errors.New("credential owner continuation mismatch")
	}
	if s.chain.anchor == nil {
		if _, err := s.chain.selectOperation(recovery.OperationID, continuation.CommitValueDigest); err != nil {
			return CredentialOwnerRefreshRecovery{}, err
		}
	}
	if s.chain.selected == nil || s.chain.anchor.OperationID != recovery.OperationID {
		return CredentialOwnerRefreshRecovery{}, errors.New("credential owner selected continuation unavailable")
	}
	commitDigest := s.chain.anchor.ObjectDigest
	if s.chain.anchor.Phase == "selected" {
		if s.chain.selected.ValueDigest != continuation.CommitValueDigest {
			return CredentialOwnerRefreshRecovery{}, errors.New("credential owner selected continuation mismatch")
		}
	} else if s.chain.anchor.Phase == "terminal" {
		commitDigest = s.chain.selected.PreviousDigest
		commitBody, _, readErr := s.chain.backend.Read(s.chain.ctx, s.chain.objectName(commitDigest), 512<<10)
		commit, openedDigest, openErr := s.chain.openObject(commitBody)
		if readErr != nil || openErr != nil || openedDigest != commitDigest || commit.OperationID != recovery.OperationID || commit.Phase != "selected" || commit.ValueDigest != continuation.CommitValueDigest {
			return CredentialOwnerRefreshRecovery{}, errors.New("credential owner terminal continuation mismatch")
		}
	} else {
		return CredentialOwnerRefreshRecovery{}, errors.New("credential owner continuation phase unavailable")
	}
	resultBody, _, resultErr := s.chain.backend.Read(s.chain.ctx, s.resultName(recovery.OperationID), credentialOwnerRefreshResultEnvelopeMaxBytes)
	if errors.Is(resultErr, os.ErrNotExist) {
		return CredentialOwnerRefreshRecovery{CommitDigest: commitDigest}, nil
	}
	if resultErr != nil {
		return CredentialOwnerRefreshRecovery{}, resultErr
	}
	if _, err := s.openRefreshAttempt(resultBody, recovery.OperationID, commitDigest); err == nil {
		return CredentialOwnerRefreshRecovery{CommitDigest: commitDigest, Attempted: true}, nil
	}
	result, err := s.openRefreshResult(resultBody, recovery.OperationID, commitDigest)
	if err != nil {
		return CredentialOwnerRefreshRecovery{}, err
	}
	return CredentialOwnerRefreshRecovery{CommitDigest: commitDigest, Attempted: true, Result: &result}, nil
}

func (s *CredentialOwnerStore) validateSelectedCommit(operationID, commitDigest string) error {
	if operationID == "" || commitDigest == "" || s.chain.anchor == nil || s.chain.anchor.OperationID != operationID || s.chain.anchor.Phase != "selected" || s.chain.anchor.ObjectDigest != commitDigest {
		return errors.New("credential owner selected commit unavailable")
	}
	return nil
}

func validateCredentialOwnerRefreshResultInputBound(result CredentialOwnerRefreshResult) error {
	remaining := credentialOwnerRefreshResultEnvelopeMaxBytes
	for _, value := range []string{
		result.Material.AccessToken,
		result.Material.RefreshToken,
		result.Material.IDToken,
		result.Material.AccountID,
		result.Error,
	} {
		if len(value) > remaining {
			return errors.New("credential owner refresh result exceeds bounded envelope")
		}
		remaining -= len(value)
	}
	return nil
}

func credentialOwnerRefreshResultEnvelopeSize(operationID, commitDigest string, plaintextBytes, nonceBytes, overheadBytes int) (int, error) {
	if plaintextBytes < 0 || nonceBytes < 0 || overheadBytes < 0 || plaintextBytes > credentialOwnerRefreshResultEnvelopeMaxBytes {
		return 0, errors.New("credential owner refresh result exceeds bounded envelope")
	}
	emptyEnvelope, err := json.Marshal(credentialOwnerEncryptedResultV1{
		SchemaVersion: 1,
		Kind:          "refresh_result",
		OperationID:   operationID,
		CommitDigest:  commitDigest,
	})
	if err != nil {
		return 0, err
	}
	size := len(emptyEnvelope) + base64.RawURLEncoding.EncodedLen(nonceBytes) + base64.RawURLEncoding.EncodedLen(plaintextBytes+overheadBytes)
	if size > credentialOwnerRefreshResultEnvelopeMaxBytes {
		return 0, errors.New("credential owner refresh result exceeds bounded envelope")
	}
	return size, nil
}

func (s *CredentialOwnerStore) sealRefreshAttempt(attempt credentialOwnerRefreshAttemptV1) ([]byte, error) {
	unsigned, err := json.Marshal(attempt)
	if err != nil {
		return nil, err
	}
	attempt.MAC = credentialAuthorityHMACHex(s.chain.key[:], "cq/credential-owner/refresh-attempt/mac/v1\x00", unsigned)
	return json.Marshal(attempt)
}

func (s *CredentialOwnerStore) openRefreshAttempt(body []byte, operationID, commitDigest string) (credentialOwnerRefreshAttemptV1, error) {
	var attempt credentialOwnerRefreshAttemptV1
	if err := json.Unmarshal(body, &attempt); err != nil || attempt.SchemaVersion != 1 || attempt.Kind != "refresh_attempt" || attempt.OperationID != operationID || attempt.CommitDigest != commitDigest || attempt.MAC == "" {
		return attempt, errors.New("invalid credential owner refresh attempt")
	}
	wantMAC := attempt.MAC
	attempt.MAC = ""
	unsigned, _ := json.Marshal(attempt)
	if !hmac.Equal([]byte(wantMAC), []byte(credentialAuthorityHMACHex(s.chain.key[:], "cq/credential-owner/refresh-attempt/mac/v1\x00", unsigned))) {
		return attempt, errors.New("invalid credential owner refresh attempt MAC")
	}
	attempt.MAC = wantMAC
	return attempt, nil
}

func (s *CredentialOwnerStore) sealContinuation(continuation credentialOwnerContinuationV1) ([]byte, error) {
	unsigned, err := json.Marshal(continuation)
	if err != nil {
		return nil, err
	}
	continuation.MAC = credentialAuthorityHMACHex(s.chain.key[:], "cq/credential-owner/continuation/mac/v1\x00", unsigned)
	return json.Marshal(continuation)
}

func (s *CredentialOwnerStore) openContinuation(body []byte) (credentialOwnerContinuationV1, error) {
	var continuation credentialOwnerContinuationV1
	if err := json.Unmarshal(body, &continuation); err != nil || continuation.SchemaVersion != 1 || continuation.OperationID == "" || continuation.Ref.CandidateID == "" || continuation.Revision == "" || continuation.ReservationDigest == "" || continuation.CapacityLeaseDigest == "" || continuation.CommitValueDigest == "" {
		return continuation, errors.New("invalid credential owner continuation")
	}
	wantMAC := continuation.MAC
	continuation.MAC = ""
	unsigned, _ := json.Marshal(continuation)
	if !hmac.Equal([]byte(wantMAC), []byte(credentialAuthorityHMACHex(s.chain.key[:], "cq/credential-owner/continuation/mac/v1\x00", unsigned))) {
		return continuation, errors.New("invalid credential owner continuation MAC")
	}
	continuation.MAC = wantMAC
	return continuation, nil
}

func (s *CredentialOwnerStore) openRefreshResult(body []byte, operationID, commitDigest string) (CredentialOwnerRefreshResult, error) {
	var envelope credentialOwnerEncryptedResultV1
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.SchemaVersion != 1 || envelope.Kind != "refresh_result" || envelope.OperationID != operationID || envelope.CommitDigest != commitDigest {
		return CredentialOwnerRefreshResult{}, errors.New("invalid credential owner refresh result")
	}
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	ciphertext, ciphertextErr := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	block, blockErr := aes.NewCipher(s.resultKey[:])
	if nonceErr != nil || ciphertextErr != nil || blockErr != nil {
		return CredentialOwnerRefreshResult{}, errors.New("invalid credential owner refresh result encryption")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return CredentialOwnerRefreshResult{}, errors.New("invalid credential owner refresh result nonce")
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(operationID+"\x00"+commitDigest))
	if err != nil {
		return CredentialOwnerRefreshResult{}, errors.New("invalid credential owner refresh result authentication")
	}
	defer clear(plaintext)
	var result CredentialOwnerRefreshResult
	if err := json.Unmarshal(plaintext, &result); err != nil {
		return CredentialOwnerRefreshResult{}, errors.New("invalid credential owner refresh result payload")
	}
	return result, nil
}

func (s *CredentialOwnerStore) continuationName(operationID string) string {
	return "credential-owner-continuation-" + framedSHA256("cq/credential-owner/continuation/name/v1\x00", []byte(operationID)) + ".json"
}

func (s *CredentialOwnerStore) resultName(operationID string) string {
	return "credential-owner-result-" + framedSHA256("cq/credential-owner/refresh-result/name/v1\x00", []byte(operationID)) + ".json"
}

func (s *CredentialOwnerStore) PublishReceipt(operationID, commitDigest string) (string, error) {
	receiptDigest, _, err := s.chain.terminalise(operationID, commitDigest)
	return receiptDigest, err
}

func (s *CredentialOwnerStore) Close() error { return s.chain.Close() }
