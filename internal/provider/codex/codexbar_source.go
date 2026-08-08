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
	"strings"
)

const codexBarSourceName = "codexbar"
const codexBarManifestVersion = 3

type CodexBarSource struct {
	root string
	fs   codexBarReadFileSystem
}

type codexBarReadFileSystem interface {
	Lstat(string) (os.FileInfo, error)
	ReadFile(string) ([]byte, error)
	EvalSymlinks(string) (string, error)
}

type osCodexBarReadFileSystem struct{}

func (osCodexBarReadFileSystem) Lstat(path string) (os.FileInfo, error) { return os.Lstat(path) }
func (osCodexBarReadFileSystem) ReadFile(path string) ([]byte, error)   { return os.ReadFile(path) }
func (osCodexBarReadFileSystem) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

type codexBarManifest struct {
	Version  int                      `json:"version"`
	Accounts []codexBarManifestRecord `json:"accounts"`
}

type codexBarManifestRecord struct {
	ID                 string `json:"id"`
	ManagedHomePath    string `json:"managedHomePath"`
	ProviderAccountID  string `json:"providerAccountID"`
	WorkspaceAccountID string `json:"workspaceAccountID"`
	AuthFingerprint    string `json:"authFingerprint"`
}

func NewCodexBarSource(root string) *CodexBarSource {
	return &CodexBarSource{root: filepath.Clean(root), fs: osCodexBarReadFileSystem{}}
}

func DefaultCodexBarRoot(home string) string {
	return filepath.Join(home, "Library", "Application Support", "CodexBar")
}

func (s *CodexBarSource) Name() string { return codexBarSourceName }

func (s *CodexBarSource) List(ctx context.Context) ([]ExternalCandidate, error) {
	manifest, err := s.loadManifest()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(manifest.Accounts))
	candidates := make([]ExternalCandidate, 0, len(manifest.Accounts))
	for _, record := range manifest.Accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.ID == "" || seen[record.ID] {
			return nil, fmt.Errorf("%w: duplicate or empty record", ErrExternalInvalid)
		}
		seen[record.ID] = true
		account, data, err := s.readRecord(record)
		if err != nil {
			return nil, err
		}
		revision := credentialRevision(data)
		candidates = append(candidates, ExternalCandidate{
			Ref: ExternalCandidateRef{
				Source: codexBarSourceName, RecordID: record.ID, Revision: revision,
			},
			Identity:        identityFromAccount(account),
			AccessExpiresAt: unixMilliTime(account.ExpiresAt),
			Routable:        account.AccountID != "" && account.AccessToken != "",
		})
	}
	return candidates, nil
}

func (s *CodexBarSource) Resolve(ctx context.Context, ref ExternalCandidateRef) (CredentialMaterial, error) {
	if err := ctx.Err(); err != nil {
		return CredentialMaterial{}, err
	}
	if ref.Source != codexBarSourceName || ref.RecordID == "" || ref.Revision == "" {
		return CredentialMaterial{}, ErrStaleRevision
	}
	manifest, err := s.loadManifest()
	if err != nil {
		return CredentialMaterial{}, err
	}
	for _, record := range manifest.Accounts {
		if record.ID != ref.RecordID {
			continue
		}
		authPath, err := s.authPath(record.ManagedHomePath)
		if err != nil {
			return CredentialMaterial{}, err
		}
		data, err := s.fs.ReadFile(authPath)
		if err != nil {
			return CredentialMaterial{}, fmt.Errorf("%w: read auth", ErrExternalInvalid)
		}
		if credentialRevision(data) != ref.Revision {
			return CredentialMaterial{}, ErrStaleRevision
		}
		account, _, err := s.readRecord(record)
		if err != nil {
			return CredentialMaterial{}, err
		}
		return CredentialMaterial{
			AccessToken: account.AccessToken, RefreshToken: account.RefreshToken,
			IDToken: account.IDToken, AccountID: account.AccountID,
		}, nil
	}
	return CredentialMaterial{}, ErrStaleRevision
}

func (s *CodexBarSource) loadManifest() (codexBarManifest, error) {
	if s == nil || s.root == "" || !filepath.IsAbs(s.root) {
		return codexBarManifest{}, fmt.Errorf("%w: root", ErrExternalUnsafePath)
	}
	path := filepath.Join(s.root, "managed-codex-accounts.json")
	if err := validateExternalFile(s.fs, path, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codexBarManifest{}, ErrExternalUnavailable
		}
		return codexBarManifest{}, err
	}
	data, err := s.fs.ReadFile(path)
	if err != nil {
		return codexBarManifest{}, fmt.Errorf("%w: read manifest", ErrExternalInvalid)
	}
	var manifest codexBarManifest
	if json.Unmarshal(data, &manifest) != nil || manifest.Version != codexBarManifestVersion {
		return codexBarManifest{}, fmt.Errorf("%w: manifest", ErrExternalInvalid)
	}
	return manifest, nil
}

func (s *CodexBarSource) readRecord(record codexBarManifestRecord) (CodexAccount, []byte, error) {
	authPath, err := s.authPath(record.ManagedHomePath)
	if err != nil {
		return CodexAccount{}, nil, err
	}
	data, err := s.fs.ReadFile(authPath)
	if err != nil {
		return CodexAccount{}, nil, fmt.Errorf("%w: read auth", ErrExternalInvalid)
	}
	if !validCodexBarFingerprint(record.AuthFingerprint, data) {
		return CodexAccount{}, nil, fmt.Errorf("%w: fingerprint", ErrExternalInvalid)
	}
	account, ok := parseAccountData(data, "")
	if !ok || account.AccountID == "" || account.RecordKey == "" {
		return CodexAccount{}, nil, fmt.Errorf("%w: credential identity", ErrExternalInvalid)
	}
	if record.ProviderAccountID == "" || record.ProviderAccountID != account.AccountID {
		return CodexAccount{}, nil, fmt.Errorf("%w: provider identity", ErrExternalInvalid)
	}
	return account, data, nil
}

func (s *CodexBarSource) authPath(managedHome string) (string, error) {
	if managedHome == "" || !filepath.IsAbs(managedHome) {
		return "", fmt.Errorf("%w: managed home", ErrExternalUnsafePath)
	}
	root, err := s.fs.EvalSymlinks(s.root)
	if err != nil {
		return "", fmt.Errorf("%w: root", ErrExternalUnsafePath)
	}
	home := filepath.Clean(managedHome)
	if !pathContained(s.root, home) || validateNoSymlinkBelow(s.fs, s.root, home) != nil {
		return "", fmt.Errorf("%w: managed home", ErrExternalUnsafePath)
	}
	evaluatedHome, err := s.fs.EvalSymlinks(home)
	if err != nil || !pathContained(root, evaluatedHome) {
		return "", fmt.Errorf("%w: managed home", ErrExternalUnsafePath)
	}
	if err := validateExternalFile(s.fs, home, true); err != nil {
		return "", err
	}
	authPath := filepath.Join(home, "auth.json")
	if validateNoSymlinkBelow(s.fs, s.root, authPath) != nil {
		return "", fmt.Errorf("%w: auth", ErrExternalUnsafePath)
	}
	if err := validateExternalFile(s.fs, authPath, false); err != nil {
		return "", err
	}
	evaluatedAuth, err := s.fs.EvalSymlinks(authPath)
	if err != nil || !pathContained(root, evaluatedAuth) {
		return "", fmt.Errorf("%w: auth", ErrExternalUnsafePath)
	}
	return authPath, nil
}

func validateNoSymlinkBelow(fs codexBarReadFileSystem, root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrExternalUnsafePath
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := fs.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrExternalUnsafePath
		}
	}
	return nil
}

func validateExternalFile(fs codexBarReadFileSystem, path string, directory bool) error {
	info, err := fs.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("%w: file type", ErrExternalUnsafePath)
	}
	unsafePermissions := info.Mode().Perm()&0o077 != 0
	if directory {
		unsafePermissions = info.Mode().Perm()&0o022 != 0
	}
	if unsafePermissions || !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: file authority", ErrExternalUnsafePath)
	}
	return nil
}

func pathContained(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validCodexBarFingerprint(value string, data []byte) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	sum := sha256.Sum256(data)
	return string(decoded) == string(sum[:])
}
