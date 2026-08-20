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

type CallerDispatchPermitRequestV1 struct {
	CallerAdmissionDigest string
	CallerDomain          NormalCallerDomain
	CallerSubjectID       string
	SessionDigest         string
	Pool                  string
	RoutingGeneration     uint64
	AllowedAccounts       []codex.AccountKey
	SelectedAccount       codex.AccountKey
}

type CallerDispatchPermitV1 struct {
	SchemaVersion         int                `json:"schema_version"`
	PermitID              string             `json:"permit_id"`
	CallerAdmissionDigest string             `json:"caller_admission_digest"`
	CallerDomain          NormalCallerDomain `json:"caller_domain"`
	CallerSubjectID       string             `json:"caller_subject_id"`
	SessionDigest         string             `json:"session_digest"`
	Pool                  string             `json:"pool"`
	RoutingGeneration     uint64             `json:"routing_generation"`
	AllowedAccounts       []codex.AccountKey `json:"allowed_accounts"`
	SelectedAccount       codex.AccountKey   `json:"selected_account"`
	IssuedAt              time.Time          `json:"issued_at"`
	ValidUntil            time.Time          `json:"valid_until"`
	Digest                string             `json:"digest"`
	MAC                   string             `json:"mac"`
}

type CallerDispatchPermitAuthority interface {
	IssueAndConsume(context.Context, CallerDispatchPermitRequestV1) (CallerDispatchPermitV1, error)
}

type CallerDispatchPermitStore struct {
	mu        sync.Mutex
	directory fsutil.SecureDirectory
	key       [sha256.Size]byte
	now       func() time.Time
	random    io.Reader
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
	store := &CallerDispatchPermitStore{directory: directory, now: now, random: random}
	copy(store.key[:], key)
	return store, nil
}

func (store *CallerDispatchPermitStore) IssueAndConsume(ctx context.Context, request CallerDispatchPermitRequestV1) (CallerDispatchPermitV1, error) {
	if store == nil || ctx == nil {
		return CallerDispatchPermitV1{}, ErrCallerDispatchPermitInvalid
	}
	if err := ctx.Err(); err != nil {
		return CallerDispatchPermitV1{}, err
	}
	request.AllowedAccounts = sortedAccountKeys(request.AllowedAccounts)
	if err := validateCallerDispatchPermitRequest(request); err != nil {
		return CallerDispatchPermitV1{}, err
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(store.random, nonce); err != nil {
		return CallerDispatchPermitV1{}, err
	}
	issuedAt := store.now().UTC()
	permit := CallerDispatchPermitV1{
		SchemaVersion: 1, PermitID: hex.EncodeToString(nonce),
		CallerAdmissionDigest: request.CallerAdmissionDigest, CallerDomain: request.CallerDomain, CallerSubjectID: request.CallerSubjectID,
		SessionDigest: request.SessionDigest, Pool: request.Pool, RoutingGeneration: request.RoutingGeneration,
		AllowedAccounts: append([]codex.AccountKey(nil), request.AllowedAccounts...), SelectedAccount: request.SelectedAccount,
		IssuedAt: issuedAt, ValidUntil: issuedAt.Add(5 * time.Second),
	}
	permit.MAC = callerDispatchPermitMAC(store.key[:], permit)
	permit.Digest = callerDispatchPermitDigest(permit)
	body, err := json.Marshal(permit)
	if err != nil {
		return CallerDispatchPermitV1{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.directory == nil {
		return CallerDispatchPermitV1{}, ErrCallerDispatchPermitInvalid
	}
	file, err := store.directory.CreateExclusive("dispatch-permit-"+permit.PermitID+".json", 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return CallerDispatchPermitV1{}, ErrCallerDispatchPermitReplayed
		}
		return CallerDispatchPermitV1{}, err
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
		return CallerDispatchPermitV1{}, err
	}
	if err := file.Sync(); err != nil {
		return CallerDispatchPermitV1{}, err
	}
	if err := file.Close(); err != nil {
		return CallerDispatchPermitV1{}, err
	}
	if err := store.directory.Sync(); err != nil {
		return CallerDispatchPermitV1{}, err
	}
	committed = true
	return permit, nil
}

func validateCallerDispatchPermitRequest(request CallerDispatchPermitRequestV1) error {
	if !lowerHexDigest(request.CallerAdmissionDigest) || !validNormalCallerDomain(request.CallerDomain) || request.CallerSubjectID == "" || !lowerHexDigest(request.SessionDigest) || !poolNamePattern.MatchString(request.Pool) || request.RoutingGeneration == 0 || len(request.AllowedAccounts) == 0 || request.SelectedAccount == "" {
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

func callerDispatchPermitMAC(key []byte, permit CallerDispatchPermitV1) string {
	permit.MAC = ""
	permit.Digest = ""
	body, _ := json.Marshal(permit)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/caller-dispatch-permit/v1\x00"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func callerDispatchPermitDigest(permit CallerDispatchPermitV1) string {
	permit.Digest = ""
	body, _ := json.Marshal(permit)
	digest := sha256.Sum256(append([]byte("cq/caller-dispatch-permit-digest/v1\x00"), body...))
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
