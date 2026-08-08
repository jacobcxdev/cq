package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

type AccountKey string
type CandidateID string
type Revision string

// CandidateRef is opaque outside this package. It identifies exact credential
// source saved or selected by an explicit command.
type CandidateRef struct {
	AccountKey  AccountKey
	CandidateID CandidateID
	path        string
}

type SystemSnapshot struct {
	Present    bool
	AccountKey AccountKey
	Revision   Revision
}

type ActivationResult struct {
	SystemCommitted bool
	ProjectionError error
}

type DeactivationResult struct {
	SystemRemoved   bool
	ProjectionError error
}

type SystemActivator interface {
	Active(context.Context) (SystemSnapshot, error)
	Activate(context.Context, CandidateRef, Revision) (ActivationResult, error)
	Deactivate(context.Context, AccountKey, Revision) (DeactivationResult, error)
}

type FileSystemActivator struct {
	FS       fsutil.FileSystem
	Home     string
	Registry Registry
}

func NewFileSystemActivator(fs fsutil.FileSystem) (*FileSystemActivator, error) {
	home, err := fs.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	return &FileSystemActivator{
		FS:       fs,
		Home:     home,
		Registry: Registry{FS: fs, Home: home},
	}, nil
}

func credentialRevision(data []byte) Revision {
	sum := sha256.Sum256(data)
	return Revision(hex.EncodeToString(sum[:]))
}

func candidateRefFromFS(fs fsutil.FileSystem, acct CodexAccount) (CandidateRef, Revision, error) {
	if acct.RecordKey == "" || acct.FilePath == "" {
		return CandidateRef{}, "", errors.New("credential candidate lacks stable identity")
	}
	data, err := fs.ReadFile(acct.FilePath)
	if err != nil {
		return CandidateRef{}, "", fmt.Errorf("read credential candidate: %w", err)
	}
	source := "managed:"
	if acct.IsActive {
		source = "system:"
	}
	return CandidateRef{
		AccountKey:  AccountKey(acct.RecordKey),
		CandidateID: CandidateID(source + acct.RecordKey),
		path:        acct.FilePath,
	}, credentialRevision(data), nil
}

func (a *FileSystemActivator) Active(context.Context) (SystemSnapshot, error) {
	path := filepath.Join(a.Home, ".codex", "auth.json")
	data, err := a.FS.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return SystemSnapshot{}, nil
	}
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("read system auth: %w", err)
	}
	acct, ok := parseAccountData(data, path)
	if !ok || acct.RecordKey == "" {
		return SystemSnapshot{}, errors.New("system auth lacks stable identity")
	}
	return SystemSnapshot{Present: true, AccountKey: AccountKey(acct.RecordKey), Revision: credentialRevision(data)}, nil
}

func (a *FileSystemActivator) Activate(_ context.Context, ref CandidateRef, expected Revision) (ActivationResult, error) {
	if ref.AccountKey == "" || ref.CandidateID == "" || ref.path == "" || expected == "" {
		return ActivationResult{}, errors.New("invalid activation candidate")
	}
	candidateData, err := a.FS.ReadFile(ref.path)
	if err != nil {
		return ActivationResult{}, fmt.Errorf("read activation candidate: %w", err)
	}
	if credentialRevision(candidateData) != expected {
		return ActivationResult{}, errors.New("activation candidate revision changed")
	}
	candidate, ok := parseAccountData(candidateData, ref.path)
	if !ok || AccountKey(candidate.RecordKey) != ref.AccountKey {
		return ActivationResult{}, errors.New("activation candidate identity changed")
	}

	systemPath := filepath.Join(a.Home, ".codex", "auth.json")
	existing, readErr := a.FS.ReadFile(systemPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return ActivationResult{}, fmt.Errorf("read system auth: %w", readErr)
	}
	if readErr == nil {
		if err := a.adoptOutgoing(existing); err != nil {
			return ActivationResult{}, err
		}
	}
	merged, err := mergeSystemCredential(existing, candidateData)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := atomicWrite(a.FS, systemPath, merged); err != nil {
		return ActivationResult{}, fmt.Errorf("write system auth: %w", err)
	}

	result := ActivationResult{SystemCommitted: true}
	result.ProjectionError = a.Registry.ProjectActive(string(ref.AccountKey))
	return result, nil
}

func (a *FileSystemActivator) adoptOutgoing(data []byte) error {
	acct, ok := parseAccountData(data, filepath.Join(a.Home, ".codex", "auth.json"))
	if !ok || acct.RecordKey == "" {
		return errors.New("outgoing system auth lacks stable identity")
	}
	path := filepath.Join(a.Home, ".codex", "accounts", acct.RecordKey+".auth.json")
	if _, err := a.FS.ReadFile(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read outgoing managed credential: %w", err)
	}
	if err := a.FS.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create accounts directory: %w", err)
	}
	if err := atomicWrite(a.FS, path, data); err != nil {
		return fmt.Errorf("adopt outgoing system auth: %w", err)
	}
	return nil
}

func (a *FileSystemActivator) Deactivate(ctx context.Context, expectedKey AccountKey, expectedRevision Revision) (DeactivationResult, error) {
	active, err := a.Active(ctx)
	if err != nil {
		return DeactivationResult{}, err
	}
	if !active.Present || active.AccountKey != expectedKey || active.Revision != expectedRevision {
		return DeactivationResult{}, errors.New("active system credential changed")
	}
	path := filepath.Join(a.Home, ".codex", "auth.json")
	if err := a.FS.Remove(path); err != nil {
		return DeactivationResult{}, fmt.Errorf("remove system auth: %w", err)
	}
	result := DeactivationResult{SystemRemoved: true}
	result.ProjectionError = a.Registry.ProjectActive("")
	return result, nil
}

func parseAccountData(data []byte, path string) (CodexAccount, bool) {
	var af codexAuthFile
	if json.Unmarshal(data, &af) != nil || af.Tokens.AccessToken == "" {
		return CodexAccount{}, false
	}
	claims := auth.DecodeCodexClaims(af.Tokens.IDToken)
	accountID := af.Tokens.AccountID
	if accountID == "" {
		accountID = claims.AccountID
	}
	return CodexAccount{
		AccessToken: af.Tokens.AccessToken, RefreshToken: af.Tokens.RefreshToken,
		IDToken: af.Tokens.IDToken, AccountID: accountID, UserID: claims.UserID,
		Email: claims.Email, PlanType: claims.PlanType, RecordKey: claims.RecordKey(),
		FilePath: path,
	}, true
}

func mergeSystemCredential(existing, candidate []byte) ([]byte, error) {
	destination := make(map[string]any)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &destination); err != nil || destination == nil {
			return nil, errors.New("parse system auth: invalid JSON")
		}
	}
	var source map[string]any
	if err := json.Unmarshal(candidate, &source); err != nil || source == nil {
		return nil, errors.New("parse activation candidate: invalid JSON")
	}
	for _, key := range []string{"auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh"} {
		if value, ok := source[key]; ok {
			destination[key] = value
		} else {
			delete(destination, key)
		}
	}
	delete(destination, "_cq")
	data, err := json.Marshal(destination)
	if err != nil {
		return nil, fmt.Errorf("marshal system auth: %w", err)
	}
	return data, nil
}

type LoginCredential struct {
	Tokens    auth.CodexTokenResponse
	Claims    auth.CodexClaims
	CreatedAt time.Time
}

// SaveLogin stores exact managed candidate and updates catalogue metadata. It
// never changes system auth or active-account projection.
func SaveLogin(fs fsutil.FileSystem, home string, credential LoginCredential) (CandidateRef, Revision, error) {
	recordKey := credential.Claims.RecordKey()
	if recordKey == "" {
		return CandidateRef{}, "", errors.New("login credential lacks stable identity")
	}
	path := filepath.Join(home, ".codex", "accounts", recordKey+".auth.json")
	doc := make(map[string]any)
	if existing, err := fs.ReadFile(path); err == nil {
		if err := json.Unmarshal(existing, &doc); err != nil || doc == nil {
			return CandidateRef{}, "", errors.New("parse managed credential: invalid JSON")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return CandidateRef{}, "", fmt.Errorf("read managed credential: %w", err)
	}
	doc["auth_mode"] = "chatgpt"
	doc["OPENAI_API_KEY"] = nil
	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		tokens = make(map[string]any)
	}
	tokens["id_token"] = credential.Tokens.IDToken
	tokens["access_token"] = credential.Tokens.AccessToken
	tokens["refresh_token"] = credential.Tokens.RefreshToken
	tokens["account_id"] = credential.Claims.AccountID
	doc["tokens"] = tokens
	if credential.CreatedAt.IsZero() {
		credential.CreatedAt = time.Now().UTC()
	}
	doc["last_refresh"] = credential.CreatedAt.UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(doc)
	if err != nil {
		return CandidateRef{}, "", fmt.Errorf("marshal managed credential: %w", err)
	}
	if err := fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return CandidateRef{}, "", fmt.Errorf("create accounts directory: %w", err)
	}
	if err := atomicWrite(fs, path, data); err != nil {
		return CandidateRef{}, "", fmt.Errorf("write managed credential: %w", err)
	}
	registry := Registry{FS: fs, Home: home}
	if err := registry.UpsertAccount(RegistryAccount{
		AccountKey: recordKey, AccountID: credential.Claims.AccountID,
		UserID: credential.Claims.UserID, Email: credential.Claims.Email,
		Plan: credential.Claims.PlanType, AuthMode: "chatgpt",
		CreatedAt: credential.CreatedAt.Unix(),
	}); err != nil {
		return CandidateRef{}, "", err
	}
	return CandidateRef{
		AccountKey: AccountKey(recordKey), CandidateID: CandidateID("managed:" + recordKey), path: path,
	}, credentialRevision(data), nil
}
