package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	installedHTTPValidationRequestVersion = 1
	installedHTTPValidationRequestTTL     = 2 * time.Minute
	installedHTTPValidationRequestMaxSize = 4 << 10
	installedHTTPValidationClaimName      = "request.claimed"
	installedHTTPValidationLockName       = "request.lock"
	candidateProxyAgentLabel              = "dev.jacobcx.cq.proxy.candidate"
)

type installedHTTPValidationFileSystem interface {
	fsutil.FileSystem
	fsutil.SecurePathInspector
	fsutil.SecureDirectoryOpener
}

type installedHTTPValidationServiceBinding struct {
	label            string
	executableSHA256 string
	serviceSHA256    string
	port             int
}

type installedHTTPValidationRequestStore struct {
	fs             installedHTTPValidationFileSystem
	path           string
	now            func() time.Time
	random         io.Reader
	resolveService func(string) (installedHTTPValidationServiceBinding, error)
}

type installedHTTPValidationRequest struct {
	Version              int       `json:"version"`
	Nonce                string    `json:"nonce"`
	IssuedAt             time.Time `json:"issued_at"`
	ExpiresAt            time.Time `json:"expires_at"`
	CQBuild              string    `json:"cq_build"`
	ServiceLabel         string    `json:"service_label"`
	CQExecutableSHA256   string    `json:"cq_executable_sha256"`
	ServiceBindingSHA256 string    `json:"service_binding_sha256"`
}

type installedHTTPValidationRequestReceipt struct {
	payloadSHA256 [sha256.Size]byte
	identity      fsutil.SecureFileIdentity
	usedName      string
	cancelledName string
}

type installedHTTPValidationConsumedRequest struct {
	store   installedHTTPValidationRequestStore
	receipt installedHTTPValidationRequestReceipt
}

func createInstalledHTTPValidationRequest(store installedHTTPValidationRequestStore, build string) error {
	_, err := createInstalledHTTPValidationRequestWithReceipt(store, build)
	return err
}

func createInstalledHTTPValidationRequestWithReceipt(store installedHTTPValidationRequestStore, build string) (installedHTTPValidationRequestReceipt, error) {
	if err := validateInstalledHTTPValidationRequestStore(store, true); err != nil {
		return installedHTTPValidationRequestReceipt{}, err
	}
	if build == "" {
		return installedHTTPValidationRequestReceipt{}, errors.New("installed HTTP validation request requires a CQ build")
	}
	binding, err := store.resolveService("")
	if err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("resolve installed proxy service: %w", err)
	}
	if err := binding.validate(); err != nil {
		return installedHTTPValidationRequestReceipt{}, err
	}
	var nonce [32]byte
	if _, err := io.ReadFull(store.random, nonce[:]); err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("generate installed HTTP validation nonce: %w", err)
	}
	now := store.now().UTC()
	request := installedHTTPValidationRequest{
		Version:              installedHTTPValidationRequestVersion,
		Nonce:                base64.RawURLEncoding.EncodeToString(nonce[:]),
		IssuedAt:             now,
		ExpiresAt:            now.Add(installedHTTPValidationRequestTTL),
		CQBuild:              build,
		ServiceLabel:         binding.label,
		CQExecutableSHA256:   binding.executableSHA256,
		ServiceBindingSHA256: binding.serviceSHA256,
	}
	data, err := json.Marshal(request)
	if err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("marshal installed HTTP validation request: %w", err)
	}
	data = append(data, '\n')
	if err := fsutil.EnsureSecureDirectory(store.fs, filepath.Dir(store.path)); err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("secure installed HTTP validation request directory: %w", err)
	}
	directory, err := store.fs.OpenSecureDirectory(filepath.Dir(store.path))
	if err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("open installed HTTP validation request directory: %w", err)
	}
	defer directory.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, filepath.Dir(store.path)); err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("fence installed HTTP validation request directory: %w", err)
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(store.fs, directory, installedHTTPValidationLockName)
	if err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("lock installed HTTP validation request store: %w", err)
	}
	defer lock.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, filepath.Dir(store.path)); err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("refence installed HTTP validation request directory: %w", err)
	}
	if err := fsutil.SecureAtomicCreateInDirectory(store.fs, directory, filepath.Base(store.path), data); err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("create installed HTTP validation request: %w", err)
	}
	created, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.fs, directory, filepath.Base(store.path), installedHTTPValidationRequestMaxSize)
	if err != nil || !bytes.Equal(created, data) {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("verify created installed HTTP validation request: %w", errors.Join(err, fsutil.ErrUnsafeSecurePath))
	}
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, filepath.Dir(store.path)); err != nil {
		return installedHTTPValidationRequestReceipt{}, fmt.Errorf("refence created installed HTTP validation request directory: %w", err)
	}
	return newInstalledHTTPValidationRequestReceipt(data, identity, nonce[:]), nil
}

func newInstalledHTTPValidationRequestReceipt(data []byte, identity fsutil.SecureFileIdentity, nonce []byte) installedHTTPValidationRequestReceipt {
	nonceDigest := sha256.Sum256(nonce)
	nonceHex := hex.EncodeToString(nonceDigest[:])
	return installedHTTPValidationRequestReceipt{
		payloadSHA256: sha256.Sum256(data),
		identity:      identity,
		usedName:      "used-" + nonceHex + ".json",
		cancelledName: "cancelled-" + nonceHex + ".json",
	}
}

func consumeInstalledHTTPValidationRequest(store installedHTTPValidationRequestStore, build string) (bool, error) {
	intent, err := consumeInstalledHTTPValidationRequestWithIntent(store, build)
	return intent != nil, err
}

func consumeInstalledHTTPValidationRequestWithIntent(store installedHTTPValidationRequestStore, build string) (*installedHTTPValidationConsumedRequest, error) {
	if err := validateInstalledHTTPValidationRequestStore(store, false); err != nil {
		return nil, err
	}
	if build == "" {
		return nil, errors.New("installed HTTP validation request requires a CQ build")
	}
	directoryPath := filepath.Dir(store.path)
	requestName := filepath.Base(store.path)
	if _, err := store.fs.Lstat(directoryPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect installed HTTP validation request directory: %w", err)
	}
	_, requestErr := store.fs.Lstat(store.path)
	_, claimErr := store.fs.Lstat(filepath.Join(directoryPath, installedHTTPValidationClaimName))
	if errors.Is(requestErr, os.ErrNotExist) && errors.Is(claimErr, os.ErrNotExist) {
		return nil, nil
	}
	if requestErr != nil && !errors.Is(requestErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect installed HTTP validation request: %w", requestErr)
	}
	if claimErr != nil && !errors.Is(claimErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect claimed installed HTTP validation request: %w", claimErr)
	}
	if err := fsutil.ValidateSecureDirectory(store.fs, directoryPath); err != nil {
		return nil, fmt.Errorf("validate installed HTTP validation request directory: %w", err)
	}
	directory, err := store.fs.OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, fmt.Errorf("open installed HTTP validation request directory: %w", err)
	}
	defer directory.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("fence installed HTTP validation request directory: %w", err)
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(store.fs, directory, installedHTTPValidationLockName)
	if err != nil {
		return nil, fmt.Errorf("lock installed HTTP validation request store: %w", err)
	}
	defer lock.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("refence installed HTTP validation request directory: %w", err)
	}
	requestPresent, err := secureInstalledHTTPValidationEntryExists(directory, requestName)
	if err != nil {
		return nil, fmt.Errorf("inspect installed HTTP validation request: %w", err)
	}
	if !requestPresent {
		claimPresent, claimErr := secureInstalledHTTPValidationEntryExists(directory, installedHTTPValidationClaimName)
		if claimErr != nil {
			return nil, fmt.Errorf("inspect interrupted installed HTTP validation request: %w", claimErr)
		}
		if !claimPresent {
			return nil, nil
		}
		if err := directory.Remove(installedHTTPValidationClaimName); err != nil {
			return nil, fmt.Errorf("discard interrupted installed HTTP validation request: %w", err)
		}
		if err := directory.Sync(); err != nil {
			return nil, fmt.Errorf("sync interrupted installed HTTP validation request cleanup: %w", err)
		}
		return nil, errors.New("discarded interrupted installed HTTP validation request")
	}
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("fence installed HTTP validation request claim: %w", err)
	}
	if err := directory.RenameNoReplace(requestName, installedHTTPValidationClaimName); err != nil {
		_ = directory.Remove(requestName)
		_ = directory.Sync()
		return nil, fmt.Errorf("claim installed HTTP validation request: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Remove(installedHTTPValidationClaimName)
		_ = directory.Sync()
		return nil, fmt.Errorf("sync installed HTTP validation request claim: %w", err)
	}
	claimed := true
	defer func() {
		if claimed {
			_ = directory.Remove(installedHTTPValidationClaimName)
			_ = directory.Sync()
		}
	}()
	data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.fs, directory, installedHTTPValidationClaimName, installedHTTPValidationRequestMaxSize)
	if err != nil {
		return nil, fmt.Errorf("read installed HTTP validation request: %w", err)
	}
	request, nonce, err := decodeInstalledHTTPValidationRequest(data, store.now().UTC(), build)
	if err != nil {
		return nil, err
	}
	binding, err := store.resolveService(request.ServiceLabel)
	if err != nil {
		return nil, fmt.Errorf("resolve installed proxy service: %w", err)
	}
	if err := binding.validate(); err != nil {
		return nil, err
	}
	if !constantTimeStringEqual(binding.label, request.ServiceLabel) ||
		!constantTimeStringEqual(binding.executableSHA256, request.CQExecutableSHA256) ||
		!constantTimeStringEqual(binding.serviceSHA256, request.ServiceBindingSHA256) {
		return nil, errors.New("installed proxy service binding changed")
	}
	receipt := newInstalledHTTPValidationRequestReceipt(data, identity, nonce)
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("fence installed HTTP validation request promotion: %w", err)
	}
	if err := fsutil.SecurePromoteNoReplaceInDirectory(store.fs, directory, installedHTTPValidationClaimName, receipt.usedName, data, identity); err != nil {
		return nil, fmt.Errorf("reject replayed installed HTTP validation request: %w", err)
	}
	claimed = false
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("refence installed HTTP validation request promotion: %w", err)
	}
	return &installedHTTPValidationConsumedRequest{store: store, receipt: receipt}, nil
}

func secureInstalledHTTPValidationEntryExists(directory fsutil.SecureDirectory, name string) (bool, error) {
	file, err := directory.OpenNoFollow(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

func (intent *installedHTTPValidationConsumedRequest) Acquire() (func(), error) {
	if intent == nil {
		return nil, errors.New("missing installed HTTP validation intent")
	}
	store := intent.store
	if err := validateInstalledHTTPValidationRequestStore(store, false); err != nil {
		return nil, err
	}
	directoryPath := filepath.Dir(store.path)
	if err := fsutil.ValidateSecureDirectory(store.fs, directoryPath); err != nil {
		return nil, fmt.Errorf("validate installed HTTP validation guard directory: %w", err)
	}
	directory, err := store.fs.OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, fmt.Errorf("open installed HTTP validation guard directory: %w", err)
	}
	closeDirectory := true
	defer func() {
		if closeDirectory {
			_ = directory.Close()
		}
	}()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("fence installed HTTP validation guard directory: %w", err)
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(store.fs, directory, installedHTTPValidationLockName)
	if err != nil {
		return nil, fmt.Errorf("lock installed HTTP validation guard: %w", err)
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lock.Close()
		}
	}()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("refence installed HTTP validation guard directory: %w", err)
	}
	if err := validateInstalledHTTPValidationIntentEntry(store, directory, intent.receipt.cancelledName, intent.receipt, false); err == nil {
		return nil, errors.New("installed HTTP validation request was cancelled")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := validateInstalledHTTPValidationIntentEntry(store, directory, intent.receipt.usedName, intent.receipt, true); err != nil {
		return nil, err
	}
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return nil, fmt.Errorf("final fence installed HTTP validation guard directory: %w", err)
	}
	closeLock = false
	closeDirectory = false
	return func() {
		if err := errors.Join(lock.Close(), directory.Close()); err != nil {
			panic("release installed HTTP validation guard")
		}
	}, nil
}

func validateInstalledHTTPValidationIntentEntry(
	store installedHTTPValidationRequestStore,
	directory fsutil.SecureDirectory,
	name string,
	receipt installedHTTPValidationRequestReceipt,
	required bool,
) error {
	data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.fs, directory, name, installedHTTPValidationRequestMaxSize)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("read installed HTTP validation intent entry: %w", err)
	}
	if sha256.Sum256(data) != receipt.payloadSHA256 || identity != receipt.identity {
		return fmt.Errorf("installed HTTP validation intent identity: %w", fsutil.ErrUnsafeSecurePath)
	}
	return nil
}

func decodeInstalledHTTPValidationRequest(data []byte, now time.Time, build string) (installedHTTPValidationRequest, []byte, error) {
	if len(data) == 0 || len(data) > installedHTTPValidationRequestMaxSize {
		return installedHTTPValidationRequest{}, nil, errors.New("invalid installed HTTP validation request size")
	}
	var request installedHTTPValidationRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return installedHTTPValidationRequest{}, nil, fmt.Errorf("parse installed HTTP validation request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return installedHTTPValidationRequest{}, nil, errors.New("installed HTTP validation request has trailing data")
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return installedHTTPValidationRequest{}, nil, fmt.Errorf("canonicalise installed HTTP validation request: %w", err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return installedHTTPValidationRequest{}, nil, errors.New("installed HTTP validation request is not canonical")
	}
	if request.Version != installedHTTPValidationRequestVersion || request.IssuedAt.Location() != time.UTC || request.ExpiresAt.Location() != time.UTC || request.ExpiresAt.Sub(request.IssuedAt) != installedHTTPValidationRequestTTL {
		return installedHTTPValidationRequest{}, nil, errors.New("invalid installed HTTP validation request envelope")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(request.Nonce)
	if err != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != request.Nonce {
		return installedHTTPValidationRequest{}, nil, errors.New("invalid installed HTTP validation request nonce")
	}
	if now.Before(request.IssuedAt) || !now.Before(request.ExpiresAt) {
		return installedHTTPValidationRequest{}, nil, errors.New("expired installed HTTP validation request")
	}
	if !constantTimeStringEqual(request.CQBuild, build) {
		return installedHTTPValidationRequest{}, nil, errors.New("installed HTTP validation CQ build mismatch")
	}
	if request.ServiceLabel == "" || !isLowerHexSHA256(request.CQExecutableSHA256) || !isLowerHexSHA256(request.ServiceBindingSHA256) {
		return installedHTTPValidationRequest{}, nil, errors.New("invalid installed HTTP validation service binding")
	}
	return request, nonce, nil
}

func constantTimeStringEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func validateInstalledHTTPValidationRequestStore(store installedHTTPValidationRequestStore, requireRandom bool) error {
	if store.fs == nil || store.path == "" || !filepath.IsAbs(store.path) || store.now == nil || store.resolveService == nil || (requireRandom && store.random == nil) {
		return errors.New("incomplete installed HTTP validation request store")
	}
	return nil
}

func (binding installedHTTPValidationServiceBinding) validate() error {
	if binding.label == "" || !isLowerHexSHA256(binding.executableSHA256) || !isLowerHexSHA256(binding.serviceSHA256) {
		return errors.New("invalid installed proxy service binding")
	}
	if binding.label == candidateProxyAgentLabel {
		if binding.port <= 0 || binding.port > 65535 || binding.port == proxy.DefaultPort {
			return errors.New("invalid installed candidate proxy port")
		}
	} else if binding.port != 0 {
		return errors.New("unexpected installed proxy service port override")
	}
	return nil
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func defaultInstalledHTTPValidationRequestStore() (installedHTTPValidationRequestStore, error) {
	paths, err := proxy.ResolveDefaultPaths()
	if err != nil {
		return installedHTTPValidationRequestStore{}, err
	}
	fsys, ok := any(fsutil.OSFileSystem{}).(installedHTTPValidationFileSystem)
	if !ok {
		return installedHTTPValidationRequestStore{}, fmt.Errorf("installed HTTP validation request store: %w", fsutil.ErrSecureCapabilityUnavailable)
	}
	return installedHTTPValidationRequestStore{
		fs:             fsys,
		path:           filepath.Join(paths.RuntimeDir, "installed-http-validation", "request.json"),
		now:            time.Now,
		random:         rand.Reader,
		resolveService: resolveInstalledHTTPValidationService,
	}, nil
}
