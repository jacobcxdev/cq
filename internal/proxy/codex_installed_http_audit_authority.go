package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	codexInstalledProtectedFileMaxBytes = 16 << 20
	codexInstalledProtectedMaxFiles     = 256
	codexInstalledPrivacyFileMaxBytes   = 16 << 20
)

// codexInstalledHTTPClientOutcome is written only by the private installed
// client traffic exercise and consumed by the independent audit lease.
type codexInstalledHTTPClientOutcome struct {
	exactPong      atomic.Bool
	egressAttempts atomic.Uint64
}

type codexInstalledHTTPAuditAuthorityConfig struct {
	routes         *codexInstalledHTTPRouteAudit
	client         *codexInstalledHTTPClientOutcome
	protectedPaths []codexInstalledProtectedPath
	privacyRoot    string
	privacyNeedles [][]byte
}

type codexInstalledProtectedPath struct {
	path      string
	directory bool
}

type codexInstalledHTTPProductionAuditAuthority struct {
	mu     sync.Mutex
	active bool
	config codexInstalledHTTPAuditAuthorityConfig
}

func newCodexInstalledHTTPAuditAuthority(config codexInstalledHTTPAuditAuthorityConfig) *codexInstalledHTTPProductionAuditAuthority {
	config.protectedPaths = append([]codexInstalledProtectedPath(nil), config.protectedPaths...)
	config.privacyNeedles = cloneCodexInstalledPrivacyNeedles(config.privacyNeedles)
	return &codexInstalledHTTPProductionAuditAuthority{config: config}
}

func (authority *codexInstalledHTTPProductionAuditAuthority) Begin(
	ctx context.Context,
	tuple CodexReadinessTuple,
	binding codexInstalledListenerProcessBinding,
) (codexInstalledHTTPAuditLease, error) {
	if ctx == nil || ctx.Err() != nil || authority == nil || authority.config.routes == nil || authority.config.client == nil ||
		!filepath.IsAbs(authority.config.privacyRoot) || len(authority.config.protectedPaths) == 0 || len(authority.config.privacyNeedles) == 0 {
		return nil, errCodexInstalledListenerAcceptance
	}
	if authority.config.client.exactPong.Load() || authority.config.client.egressAttempts.Load() != 0 {
		return nil, errCodexInstalledListenerAcceptance
	}
	for _, protected := range authority.config.protectedPaths {
		if !filepath.IsAbs(protected.path) {
			return nil, errCodexInstalledListenerAcceptance
		}
	}
	authority.mu.Lock()
	if authority.active {
		authority.mu.Unlock()
		return nil, errCodexInstalledListenerAcceptance
	}
	authority.active = true
	authority.mu.Unlock()
	succeeded := false
	defer func() {
		if !succeeded {
			authority.release()
		}
	}()
	protected, err := captureCodexInstalledProtectedDigests(authority.config.protectedPaths)
	if err != nil {
		return nil, errCodexInstalledListenerAcceptance
	}
	models, unexpected := authority.config.routes.snapshot()
	lease := &codexInstalledHTTPProductionAuditLease{
		authority:        authority,
		tuple:            tuple,
		binding:          binding,
		protectedBefore:  protected,
		modelsBefore:     models,
		unexpectedBefore: unexpected,
	}
	succeeded = true
	return lease, nil
}

func (authority *codexInstalledHTTPProductionAuditAuthority) release() {
	if authority == nil {
		return
	}
	authority.mu.Lock()
	authority.active = false
	authority.mu.Unlock()
}

type codexInstalledHTTPProductionAuditLease struct {
	mu sync.Mutex

	authority        *codexInstalledHTTPProductionAuditAuthority
	tuple            CodexReadinessTuple
	binding          codexInstalledListenerProcessBinding
	protectedBefore  []codexInstalledProtectedDigest
	modelsBefore     uint64
	unexpectedBefore uint64
	completed        bool
	released         bool
}

func (lease *codexInstalledHTTPProductionAuditLease) Complete(ctx context.Context) (codexInstalledHTTPSealedAuditProof, error) {
	if ctx == nil || ctx.Err() != nil || lease == nil {
		return codexInstalledHTTPSealedAuditProof{}, errCodexInstalledListenerAcceptance
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.completed || lease.released || lease.authority == nil {
		return codexInstalledHTTPSealedAuditProof{}, errCodexInstalledListenerAcceptance
	}
	lease.completed = true
	config := lease.authority.config
	protectedAfter, err := captureCodexInstalledProtectedDigests(config.protectedPaths)
	if err != nil {
		return codexInstalledHTTPSealedAuditProof{}, errCodexInstalledListenerAcceptance
	}
	authWrites := countCodexInstalledProtectedDigestChanges(lease.protectedBefore, protectedAfter)
	rawLeaks, err := countCodexInstalledPrivacyLeaks(config.privacyRoot, config.privacyNeedles)
	if err != nil {
		return codexInstalledHTTPSealedAuditProof{}, errCodexInstalledListenerAcceptance
	}
	modelsAfter, unexpectedAfter := config.routes.snapshot()
	if modelsAfter < lease.modelsBefore || unexpectedAfter < lease.unexpectedBefore {
		return codexInstalledHTTPSealedAuditProof{}, errCodexInstalledListenerAcceptance
	}
	proof := codexInstalledHTTPSealedAuditProof{
		tuple:               lease.tuple,
		binding:             lease.binding,
		rawIdentifierLeaks:  rawLeaks,
		automaticAuthWrites: authWrites,
		egressAttempts:      config.client.egressAttempts.Load(),
		modelRequests:       modelsAfter - lease.modelsBefore,
		unexpectedRoutes:    unexpectedAfter - lease.unexpectedBefore,
		exactClientPong:     config.client.exactPong.Load(),
	}
	proof.seal = &codexInstalledHTTPAuditProofSeal{
		tuple:               proof.tuple,
		binding:             proof.binding,
		rawIdentifierLeaks:  proof.rawIdentifierLeaks,
		automaticAuthWrites: proof.automaticAuthWrites,
		egressAttempts:      proof.egressAttempts,
		modelRequests:       proof.modelRequests,
		unexpectedRoutes:    proof.unexpectedRoutes,
		exactClientPong:     proof.exactClientPong,
	}
	return proof, nil
}

func (lease *codexInstalledHTTPProductionAuditLease) Release() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	if lease.released {
		lease.mu.Unlock()
		return
	}
	lease.released = true
	authority := lease.authority
	lease.authority = nil
	lease.protectedBefore = nil
	lease.mu.Unlock()
	if authority != nil {
		authority.release()
	}
}

type codexInstalledProtectedDigest struct {
	path      string
	absent    bool
	directory bool
	digest    [sha256.Size]byte
}

func captureCodexInstalledProtectedDigests(paths []codexInstalledProtectedPath) ([]codexInstalledProtectedDigest, error) {
	result := make([]codexInstalledProtectedDigest, 0, len(paths))
	for _, protected := range paths {
		digest, err := captureCodexInstalledProtectedDigest(protected.path)
		if err != nil {
			return nil, err
		}
		if !digest.absent && digest.directory != protected.directory {
			return nil, fsutil.ErrUnsafeSecurePath
		}
		result = append(result, digest)
	}
	return result, nil
}

func captureCodexInstalledProtectedDigest(path string) (codexInstalledProtectedDigest, error) {
	result := codexInstalledProtectedDigest{path: path}
	inspector := fsutil.OSFileSystem{}
	pathInfo, err := inspector.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		result.absent = true
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if pathInfo.IsDir() {
		result.directory = true
		result.digest, err = captureCodexInstalledProtectedDirectoryDigest(inspector, path)
		return result, err
	}
	data, identity, err := readCodexInstalledProtectedFile(inspector, path)
	if err != nil {
		return result, err
	}
	hash := sha256.New()
	writeCodexInstalledProtectedIdentity(hash, identity)
	_, _ = hash.Write(data)
	copy(result.digest[:], hash.Sum(nil))
	clearBytes(data)
	return result, nil
}

func readCodexInstalledProtectedFile(inspector fsutil.SecurePathInspector, path string) ([]byte, fsutil.SecureFileIdentity, error) {
	directoryPath := filepath.Dir(path)
	directory, err := (fsutil.OSFileSystem{}).OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, fsutil.SecureFileIdentity{}, err
	}
	defer directory.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(inspector, directory, directoryPath); err != nil {
		return nil, fsutil.SecureFileIdentity{}, err
	}
	data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		inspector, directory, filepath.Base(path), codexInstalledProtectedFileMaxBytes,
	)
	if err != nil {
		return nil, fsutil.SecureFileIdentity{}, err
	}
	if err := fsutil.ValidateSecureDirectoryHandle(inspector, directory, directoryPath); err != nil {
		clearBytes(data)
		return nil, fsutil.SecureFileIdentity{}, err
	}
	return data, identity, nil
}

func captureCodexInstalledProtectedDirectoryDigest(
	inspector fsutil.SecurePathInspector,
	path string,
) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	directory, err := (fsutil.OSFileSystem{}).OpenSecureDirectory(path)
	if err != nil {
		return result, err
	}
	defer directory.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(inspector, directory, path); err != nil {
		return result, err
	}
	directoryInfo, err := directory.Stat()
	if err != nil {
		return result, err
	}
	directoryIdentity, ok := inspector.FileIdentity(directoryInfo)
	if !ok {
		return result, fsutil.ErrUnsafeSecurePath
	}
	reader, ok := directory.(fsutil.SecureDirectoryReader)
	if !ok {
		return result, fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return result, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if filepath.Base(entry.Name()) == entry.Name() && len(entry.Name()) >= len(".auth.json") && strings.HasSuffix(entry.Name(), ".auth.json") {
			names = append(names, entry.Name())
		}
	}
	if len(names) > codexInstalledProtectedMaxFiles {
		return result, fsutil.ErrSecureFileTooLarge
	}
	sort.Strings(names)
	hash := sha256.New()
	writeCodexInstalledProtectedIdentity(hash, directoryIdentity)
	var total int64
	for _, name := range names {
		data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, name, codexInstalledProtectedFileMaxBytes)
		if err != nil {
			return result, err
		}
		total += int64(len(data))
		if total > codexInstalledProtectedFileMaxBytes {
			clearBytes(data)
			return result, fsutil.ErrSecureFileTooLarge
		}
		finalData, finalIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, name, codexInstalledProtectedFileMaxBytes)
		if err != nil || finalIdentity != identity || !bytes.Equal(finalData, data) {
			clearBytes(data)
			clearBytes(finalData)
			return result, errors.Join(err, fsutil.ErrUnsafeSecurePath)
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		writeCodexInstalledProtectedIdentity(hash, identity)
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
		clearBytes(data)
		clearBytes(finalData)
	}
	finalEntries, err := reader.ReadDir()
	if err != nil {
		return result, err
	}
	finalNames := make([]string, 0, len(names))
	for _, entry := range finalEntries {
		if filepath.Base(entry.Name()) == entry.Name() && len(entry.Name()) >= len(".auth.json") && strings.HasSuffix(entry.Name(), ".auth.json") {
			finalNames = append(finalNames, entry.Name())
		}
	}
	sort.Strings(finalNames)
	if len(names) != len(finalNames) {
		return result, fsutil.ErrUnsafeSecurePath
	}
	for index := range names {
		if names[index] != finalNames[index] {
			return result, fsutil.ErrUnsafeSecurePath
		}
	}
	if err := fsutil.ValidateSecureDirectoryHandle(inspector, directory, path); err != nil {
		return result, err
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func writeCodexInstalledProtectedIdentity(writer io.Writer, identity fsutil.SecureFileIdentity) {
	var encoded [3 * 8]byte
	binary.BigEndian.PutUint64(encoded[0:8], identity.Device)
	binary.BigEndian.PutUint64(encoded[8:16], identity.Inode)
	binary.BigEndian.PutUint64(encoded[16:24], identity.Links)
	_, _ = writer.Write(encoded[:])
}

func countCodexInstalledProtectedDigestChanges(before, after []codexInstalledProtectedDigest) uint64 {
	if len(before) != len(after) {
		return 1
	}
	var changed uint64
	for index := range before {
		if before[index] != after[index] {
			changed++
		}
	}
	return changed
}

func cloneCodexInstalledPrivacyNeedles(needles [][]byte) [][]byte {
	cloned := make([][]byte, len(needles))
	for index := range needles {
		cloned[index] = append([]byte(nil), needles[index]...)
	}
	return cloned
}

func countCodexInstalledPrivacyLeaks(root string, needles [][]byte) (uint64, error) {
	var leaks uint64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fsutil.ErrUnsafeSecurePath
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > codexInstalledPrivacyFileMaxBytes {
			return fsutil.ErrUnsafeSecurePath
		}
		file, err := (fsutil.OSFileSystem{}).OpenNoFollow(path)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, codexInstalledPrivacyFileMaxBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > codexInstalledPrivacyFileMaxBytes {
			clearBytes(data)
			return errors.Join(readErr, closeErr, fmt.Errorf("invalid installed validation privacy file"))
		}
		for _, needle := range needles {
			if len(needle) > 0 && bytes.Contains(data, needle) {
				leaks++
				break
			}
		}
		clearBytes(data)
		return nil
	})
	return leaks, err
}
