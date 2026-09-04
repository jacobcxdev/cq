package proxy

import (
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
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

var (
	ErrCallerDispatchPermitInvalid  = errors.New("caller dispatch permit invalid")
	ErrCallerDispatchPermitReplayed = errors.New("caller dispatch permit replayed")
)

type CallerDispatchPermitRequestV2 struct {
	CallerAdmissionDigest string
	CallerDomain          NormalCallerDomain
	CallerSubjectID       string
	SessionDigest         string
	PoolID                PoolID
	RoutingGeneration     uint64
	AllowedAccounts       []codex.AccountKey
	SelectedAccount       codex.AccountKey
}

type CallerDispatchPermitV2 struct {
	SchemaVersion         int                `json:"schema_version"`
	PermitID              string             `json:"permit_id"`
	CallerAdmissionDigest string             `json:"caller_admission_digest"`
	CallerDomain          NormalCallerDomain `json:"caller_domain"`
	CallerSubjectID       string             `json:"caller_subject_id"`
	SessionDigest         string             `json:"session_digest"`
	PoolID                PoolID             `json:"pool_id"`
	RoutingGeneration     uint64             `json:"routing_generation"`
	AllowedAccounts       []codex.AccountKey `json:"allowed_accounts"`
	SelectedAccount       codex.AccountKey   `json:"selected_account"`
	IssuedAt              time.Time          `json:"issued_at"`
	ValidUntil            time.Time          `json:"valid_until"`
	Digest                string             `json:"digest"`
	MAC                   string             `json:"mac"`
}

type CallerDispatchPermitAuthority interface {
	IssueAndConsume(context.Context, CallerDispatchPermitRequestV2) (CallerDispatchPermitV2, error)
}

type CallerDispatchPermitStore struct {
	mu        sync.Mutex
	directory fsutil.SecureDirectory
	inspector fsutil.SecurePathInspector
	key       [sha256.Size]byte
	now       func() time.Time
	random    io.Reader
	lastPrune time.Time
	closed    bool
}

func OpenCallerDispatchPermitStore(fsys fsutil.FileSystem, path string, key []byte, now func() time.Time, random io.Reader) (*CallerDispatchPermitStore, error) {
	if fsys == nil || path == "" || len(key) != sha256.Size || now == nil || random == nil {
		return nil, ErrCallerDispatchPermitInvalid
	}
	if err := fsutil.EnsureSecureDirectory(fsys, path); err != nil {
		return nil, err
	}
	opener, ok := fsys.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, ErrCallerDispatchPermitInvalid
	}
	directory, err := opener.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		_ = directory.Close()
		return nil, ErrCallerDispatchPermitInvalid
	}
	store := &CallerDispatchPermitStore{directory: directory, inspector: inspector, now: now, random: random}
	copy(store.key[:], key)
	store.pruneLocked(now())
	return store, nil
}

func (store *CallerDispatchPermitStore) IssueAndConsume(ctx context.Context, request CallerDispatchPermitRequestV2) (CallerDispatchPermitV2, error) {
	if store == nil || ctx == nil {
		return CallerDispatchPermitV2{}, ErrCallerDispatchPermitInvalid
	}
	if err := ctx.Err(); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	request.AllowedAccounts = sortedAccountKeys(request.AllowedAccounts)
	if err := validateCallerDispatchPermitRequest(request); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(store.random, nonce); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	issuedAt := store.now().UTC()
	permit := CallerDispatchPermitV2{
		SchemaVersion: 2, PermitID: hex.EncodeToString(nonce),
		CallerAdmissionDigest: request.CallerAdmissionDigest, CallerDomain: request.CallerDomain, CallerSubjectID: request.CallerSubjectID,
		SessionDigest: request.SessionDigest, PoolID: request.PoolID, RoutingGeneration: request.RoutingGeneration,
		AllowedAccounts: append([]codex.AccountKey(nil), request.AllowedAccounts...), SelectedAccount: request.SelectedAccount,
		IssuedAt: issuedAt, ValidUntil: issuedAt.Add(5 * time.Second),
	}
	permit.MAC = callerDispatchPermitMAC(store.key[:], permit)
	permit.Digest = callerDispatchPermitDigest(permit)
	if err := validateCallerDispatchPermit(permit); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	body, err := json.Marshal(permit)
	if err != nil {
		return CallerDispatchPermitV2{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.directory == nil {
		return CallerDispatchPermitV2{}, ErrCallerDispatchPermitInvalid
	}
	store.pruneLocked(issuedAt)
	file, err := store.directory.CreateExclusive("dispatch-permit-"+permit.PermitID+".json", 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return CallerDispatchPermitV2{}, ErrCallerDispatchPermitReplayed
		}
		return CallerDispatchPermitV2{}, err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = store.directory.Remove("dispatch-permit-" + permit.PermitID + ".json")
			_ = store.directory.Sync()
		}
	}()
	if _, err := file.Write(body); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	if err := file.Sync(); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	if err := file.Close(); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	if err := store.directory.Sync(); err != nil {
		return CallerDispatchPermitV2{}, err
	}
	committed = true
	proxyProcessEphemeralState.recordCreate(ephemeralReceiptDispatch)
	return permit, nil
}

func (store *CallerDispatchPermitStore) pruneLocked(now time.Time) {
	if store == nil || store.directory == nil || (!store.lastPrune.IsZero() && now.Before(store.lastPrune.Add(ephemeralReceiptPruneInterval))) {
		return
	}
	store.lastPrune = now
	remaining, pruned, err := pruneEphemeralReceipts(store.inspector, store.directory, "dispatch-permit-", now)
	proxyProcessEphemeralState.recordScan(ephemeralReceiptDispatch, remaining, pruned, err)
}

func validateCallerDispatchPermitRequest(request CallerDispatchPermitRequestV2) error {
	if !lowerHexDigest(request.CallerAdmissionDigest) || !validNormalCallerDomain(request.CallerDomain) || request.CallerSubjectID == "" || !lowerHexDigest(request.SessionDigest) || !validPoolID(request.PoolID) || request.RoutingGeneration == 0 || len(request.AllowedAccounts) == 0 || request.SelectedAccount == "" {
		return ErrCallerDispatchPermitInvalid
	}
	selected := false
	for index, account := range request.AllowedAccounts {
		if account == "" || (index > 0 && request.AllowedAccounts[index-1] >= account) {
			return ErrCallerDispatchPermitInvalid
		}
		selected = selected || account == request.SelectedAccount
	}
	if !selected {
		return ErrCallerDispatchPermitInvalid
	}
	return nil
}

func validateCallerDispatchPermit(permit CallerDispatchPermitV2) error {
	request := CallerDispatchPermitRequestV2{
		CallerAdmissionDigest: permit.CallerAdmissionDigest, CallerDomain: permit.CallerDomain, CallerSubjectID: permit.CallerSubjectID,
		SessionDigest: permit.SessionDigest, PoolID: permit.PoolID, RoutingGeneration: permit.RoutingGeneration,
		AllowedAccounts: append([]codex.AccountKey(nil), permit.AllowedAccounts...), SelectedAccount: permit.SelectedAccount,
	}
	if permit.SchemaVersion != 2 || !lowerHexBytes(permit.PermitID, 16) || permit.IssuedAt.IsZero() || !permit.ValidUntil.Equal(permit.IssuedAt.Add(5*time.Second)) || !lowerHexDigest(permit.MAC) || !lowerHexDigest(permit.Digest) {
		return ErrCallerDispatchPermitInvalid
	}
	return validateCallerDispatchPermitRequest(request)
}

func callerDispatchPermitMAC(key []byte, permit CallerDispatchPermitV2) string {
	permit.MAC = ""
	permit.Digest = ""
	body, _ := json.Marshal(permit)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/caller-dispatch-permit/v2\x00"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func callerDispatchPermitDigest(permit CallerDispatchPermitV2) string {
	permit.Digest = ""
	body, _ := json.Marshal(permit)
	digest := sha256.Sum256(append([]byte("cq/caller-dispatch-permit-digest/v2\x00"), body...))
	return hex.EncodeToString(digest[:])
}

func (store *CallerDispatchPermitStore) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	for index := range store.key {
		store.key[index] = 0
	}
	if store.directory == nil {
		return nil
	}
	err := store.directory.Close()
	store.directory = nil
	return err
}
