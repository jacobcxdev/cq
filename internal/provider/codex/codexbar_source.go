package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/auth"
)

const (
	codexBarSourceName          = "codexbar"
	codexBarManifestVersion     = 3
	codexBarMaximumDeclaredSize = int64(1 << 20)
)

type CodexBarSource struct {
	root     string
	fs       codexBarReadFileSystem
	ownsFile func(os.FileInfo) bool
}

type codexBarReadFileSystem interface {
	Lstat(string) (os.FileInfo, error)
	OpenNoFollow(string, string) (codexBarReadFile, error)
	EvalSymlinks(string) (string, error)
}

type codexBarReadFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type osCodexBarReadFileSystem struct{}

func (osCodexBarReadFileSystem) Lstat(path string) (os.FileInfo, error) { return os.Lstat(path) }
func (osCodexBarReadFileSystem) OpenNoFollow(root, path string) (codexBarReadFile, error) {
	return openExternalFileNoFollow(root, path)
}
func (osCodexBarReadFileSystem) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

type externalFileGeneration struct {
	device   uint64
	inode    uint64
	owner    uint64
	size     int64
	mode     os.FileMode
	modified int64
}

type validatedExternalRead struct {
	data       []byte
	generation externalFileGeneration
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
	return &CodexBarSource{
		root: filepath.Clean(root), fs: osCodexBarReadFileSystem{},
		ownsFile: ownedByCurrentUser,
	}
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
	candidates := make([]ExternalCandidate, 0, len(manifest.Accounts))
	for _, record := range manifest.Accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		account, revision, err := s.readRecord(record, "")
		if err != nil {
			return nil, err
		}
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
		account, _, err := s.readRecord(record, ref.Revision)
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
	if err := validateExternalFile(s.fs, s.ownsFile, s.root, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codexBarManifest{}, ErrExternalUnavailable
		}
		return codexBarManifest{}, err
	}
	path := filepath.Join(s.root, "managed-codex-accounts.json")
	read, err := s.readValidatedFile(path, ErrExternalUnavailable, ErrExternalUnsafePath)
	if err != nil {
		return codexBarManifest{}, err
	}
	if err := validateExternalFile(s.fs, s.ownsFile, s.root, true); err != nil {
		if errors.Is(err, ErrExternalUnsafePath) {
			return codexBarManifest{}, err
		}
		return codexBarManifest{}, fmt.Errorf("%w: root generation", ErrExternalUnsafePath)
	}
	if err := s.confirmExternalFileGeneration(path, read.generation, ErrExternalUnsafePath); err != nil {
		return codexBarManifest{}, err
	}
	var manifest codexBarManifest
	if json.Unmarshal(read.data, &manifest) != nil || manifest.Version != codexBarManifestVersion || manifest.Accounts == nil {
		return codexBarManifest{}, fmt.Errorf("%w: manifest", ErrExternalInvalid)
	}
	seen := make(map[string]bool, len(manifest.Accounts))
	for _, record := range manifest.Accounts {
		if record.ID == "" || seen[record.ID] {
			return codexBarManifest{}, fmt.Errorf("%w: duplicate or empty record", ErrExternalInvalid)
		}
		seen[record.ID] = true
	}
	return manifest, nil
}

func (s *CodexBarSource) readRecord(record codexBarManifestRecord, expected Revision) (CodexAccount, Revision, error) {
	missingErr := error(ErrExternalInvalid)
	changedErr := error(ErrExternalUnsafePath)
	if expected != "" {
		missingErr = ErrStaleRevision
		changedErr = ErrStaleRevision
	}
	authPath, err := s.authPath(record.ManagedHomePath, missingErr)
	if err != nil {
		return CodexAccount{}, "", err
	}
	read, err := s.readValidatedFile(authPath, missingErr, changedErr)
	if err != nil {
		return CodexAccount{}, "", err
	}
	confirmedPath, err := s.authPath(record.ManagedHomePath, changedErr)
	if err != nil {
		return CodexAccount{}, "", err
	}
	if confirmedPath != authPath {
		return CodexAccount{}, "", changedErr
	}
	if err := s.confirmExternalFileGeneration(authPath, read.generation, changedErr); err != nil {
		return CodexAccount{}, "", err
	}
	revision := externalCredentialRevision(read.data, authPath, read.generation)
	if expected != "" && revision != expected {
		return CodexAccount{}, "", ErrStaleRevision
	}
	if err := validateCodexBarFingerprint(record.AuthFingerprint, read.data); err != nil {
		return CodexAccount{}, "", err
	}
	account, ok := parseAccountData(read.data, "")
	if !ok || account.AccountID == "" || account.RecordKey == "" {
		return CodexAccount{}, "", fmt.Errorf("%w: credential identity", ErrExternalInvalid)
	}
	claims := auth.DecodeCodexClaims(account.IDToken)
	claimRecordKey := claims.RecordKey()
	if claims.AccountID == "" || claims.UserID == "" || claimRecordKey == "" {
		return CodexAccount{}, "", fmt.Errorf("%w: credential claims", ErrExternalInvalid)
	}
	if account.AccountID != claims.AccountID || account.UserID != claims.UserID || account.RecordKey != claimRecordKey {
		return CodexAccount{}, "", ErrExternalIdentityMismatch
	}
	if record.ProviderAccountID == "" || record.WorkspaceAccountID == "" {
		return CodexAccount{}, "", fmt.Errorf("%w: manifest identity", ErrExternalInvalid)
	}
	if record.ProviderAccountID != claims.AccountID || record.WorkspaceAccountID != claims.UserID {
		return CodexAccount{}, "", ErrExternalIdentityMismatch
	}
	return account, revision, nil
}

func (s *CodexBarSource) authPath(managedHome string, missingErr error) (string, error) {
	if managedHome == "" || !filepath.IsAbs(managedHome) {
		return "", fmt.Errorf("%w: managed home", ErrExternalUnsafePath)
	}
	root, err := s.fs.EvalSymlinks(s.root)
	if err != nil {
		return "", fmt.Errorf("%w: root", ErrExternalUnsafePath)
	}
	home := filepath.Clean(managedHome)
	if !pathContained(s.root, home) {
		return "", fmt.Errorf("%w: managed home", ErrExternalUnsafePath)
	}
	if err := validateNoSymlinkBelow(s.fs, s.root, home); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: managed home unavailable", missingErr)
		}
		return "", fmt.Errorf("%w: managed home", ErrExternalUnsafePath)
	}
	evaluatedHome, err := s.fs.EvalSymlinks(home)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: managed home unavailable", missingErr)
	}
	if err != nil || !pathContained(root, evaluatedHome) {
		return "", fmt.Errorf("%w: managed home", ErrExternalUnsafePath)
	}
	if err := validateExternalFile(s.fs, s.ownsFile, home, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: managed home unavailable", missingErr)
		}
		return "", err
	}
	authPath := filepath.Join(home, "auth.json")
	if err := validateNoSymlinkBelow(s.fs, s.root, authPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: auth unavailable", missingErr)
		}
		return "", fmt.Errorf("%w: auth", ErrExternalUnsafePath)
	}
	if err := validateExternalFile(s.fs, s.ownsFile, authPath, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: auth unavailable", missingErr)
		}
		return "", err
	}
	evaluatedAuth, err := s.fs.EvalSymlinks(authPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: auth unavailable", missingErr)
	}
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

func validateExternalFile(fs codexBarReadFileSystem, ownsFile func(os.FileInfo) bool, path string, directory bool) error {
	info, err := fs.Lstat(path)
	if err != nil {
		return err
	}
	return validateExternalFileInfo(ownsFile, info, directory)
}

func validateExternalFileInfo(ownsFile func(os.FileInfo) bool, info os.FileInfo, directory bool) error {
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("%w: file type", ErrExternalUnsafePath)
	}
	unsafePermissions := info.Mode().Perm()&0o077 != 0
	if directory {
		unsafePermissions = info.Mode().Perm()&0o022 != 0
	}
	if unsafePermissions || ownsFile == nil || !ownsFile(info) {
		return fmt.Errorf("%w: file authority", ErrExternalUnsafePath)
	}
	return nil
}

func (s *CodexBarSource) readValidatedFile(path string, missingErr, changedErr error) (validatedExternalRead, error) {
	pathBefore, err := s.fs.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return validatedExternalRead{}, missingErr
		}
		return validatedExternalRead{}, fmt.Errorf("%w: file access", ErrExternalUnsafePath)
	}
	if err := validateExternalFileInfo(s.ownsFile, pathBefore, false); err != nil {
		return validatedExternalRead{}, err
	}
	pathGeneration, ok := generationForExternalFile(pathBefore)
	if !ok {
		return validatedExternalRead{}, fmt.Errorf("%w: file identity", ErrExternalUnsafePath)
	}

	file, err := s.fs.OpenNoFollow(s.root, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return validatedExternalRead{}, changedErr
		}
		return validatedExternalRead{}, fmt.Errorf("%w: file open", ErrExternalUnsafePath)
	}
	defer file.Close()

	openedBefore, err := file.Stat()
	if err != nil {
		return validatedExternalRead{}, changedErr
	}
	if err := validateExternalFileInfo(s.ownsFile, openedBefore, false); err != nil {
		return validatedExternalRead{}, err
	}
	openedGeneration, ok := generationForExternalFile(openedBefore)
	if !ok {
		return validatedExternalRead{}, fmt.Errorf("%w: file identity", ErrExternalUnsafePath)
	}
	if pathGeneration != openedGeneration {
		return validatedExternalRead{}, changedErr
	}

	data, readErr := readCodexBarFileBounded(file, codexBarMaximumDeclaredSize)
	if readErr != nil && !errors.Is(readErr, ErrExternalInvalid) {
		return validatedExternalRead{}, changedErr
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return validatedExternalRead{}, changedErr
	}
	pathAfter, err := s.fs.Lstat(path)
	if err != nil {
		return validatedExternalRead{}, changedErr
	}
	if err := validateExternalFileInfo(s.ownsFile, openedAfter, false); err != nil {
		return validatedExternalRead{}, err
	}
	if err := validateExternalFileInfo(s.ownsFile, pathAfter, false); err != nil {
		return validatedExternalRead{}, err
	}
	openedAfterGeneration, openedOK := generationForExternalFile(openedAfter)
	pathAfterGeneration, pathOK := generationForExternalFile(pathAfter)
	if !openedOK || !pathOK {
		return validatedExternalRead{}, fmt.Errorf("%w: file identity", ErrExternalUnsafePath)
	}
	if openedGeneration != openedAfterGeneration || openedGeneration != pathAfterGeneration {
		return validatedExternalRead{}, changedErr
	}
	if readErr != nil {
		return validatedExternalRead{}, readErr
	}
	return validatedExternalRead{data: data, generation: openedGeneration}, nil
}

func readCodexBarFileBounded(reader io.Reader, maximumSize int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximumSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximumSize {
		return nil, fmt.Errorf("%w: oversized file", ErrExternalInvalid)
	}
	return data, nil
}

func (s *CodexBarSource) confirmExternalFileGeneration(path string, expected externalFileGeneration, changedErr error) error {
	info, err := s.fs.Lstat(path)
	if err != nil {
		return changedErr
	}
	if err := validateExternalFileInfo(s.ownsFile, info, false); err != nil {
		return err
	}
	generation, ok := generationForExternalFile(info)
	if !ok {
		return fmt.Errorf("%w: file identity", ErrExternalUnsafePath)
	}
	if generation != expected {
		return changedErr
	}
	return nil
}

func generationForExternalFile(info os.FileInfo) (externalFileGeneration, bool) {
	device, inode, owner, ok := externalFileIdentity(info)
	if !ok {
		return externalFileGeneration{}, false
	}
	return externalFileGeneration{
		device: device, inode: inode, owner: owner, size: info.Size(),
		mode: info.Mode(), modified: info.ModTime().UnixNano(),
	}, true
}

func externalCredentialRevision(data []byte, path string, generation externalFileGeneration) Revision {
	hash := sha256.New()
	hash.Write([]byte("cq-external-credential-v1\x00"))
	hash.Write([]byte(filepath.Clean(path)))
	hash.Write([]byte{0})
	var metadata [6 * 8]byte
	binary.LittleEndian.PutUint64(metadata[0:8], generation.device)
	binary.LittleEndian.PutUint64(metadata[8:16], generation.inode)
	binary.LittleEndian.PutUint64(metadata[16:24], generation.owner)
	binary.LittleEndian.PutUint64(metadata[24:32], uint64(generation.size))
	binary.LittleEndian.PutUint64(metadata[32:40], uint64(generation.mode))
	binary.LittleEndian.PutUint64(metadata[40:48], uint64(generation.modified))
	hash.Write(metadata[:])
	hash.Write(data)
	return Revision(hex.EncodeToString(hash.Sum(nil)))
}

func pathContained(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validateCodexBarFingerprint(value string, data []byte) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%w: fingerprint format", ErrExternalInvalid)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%w: fingerprint format", ErrExternalInvalid)
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(decoded, sum[:]) {
		return ErrExternalFingerprintMismatch
	}
	return nil
}
