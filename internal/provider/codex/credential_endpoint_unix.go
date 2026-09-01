//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

const credentialEndpointSidecarVersion = 1
const credentialEndpointSidecarMaxBytes = 16 << 10
const credentialEndpointDialTimeout = 250 * time.Millisecond

type credentialEndpointPublicationState string

const (
	credentialEndpointPrepared  credentialEndpointPublicationState = "prepared"
	credentialEndpointPublished credentialEndpointPublicationState = "published"
	credentialEndpointClosing   credentialEndpointPublicationState = "closing"
)

type credentialEndpointIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint64 `json:"uid"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
}

type credentialEndpointPrevious struct {
	Generation    string `json:"generation"`
	TemporaryName string `json:"temporary_name"`
	credentialEndpointIdentity
}

type credentialEndpointSidecar struct {
	Version         int                                `json:"version"`
	ProtocolVersion int                                `json:"protocol_version"`
	Generation      string                             `json:"generation"`
	State           credentialEndpointPublicationState `json:"state"`
	TemporaryName   string                             `json:"temporary_name"`
	FinalName       string                             `json:"final_name"`
	credentialEndpointIdentity
	LockDevice uint64                      `json:"lock_device"`
	LockInode  uint64                      `json:"lock_inode"`
	LockLinks  uint64                      `json:"lock_links"`
	Previous   *credentialEndpointPrevious `json:"previous,omitempty"`
}

var (
	ErrCredentialEndpointOccupied               = errors.New("credential coordinator endpoint already exists")
	ErrCredentialEndpointPublicationUnsupported = errors.New("credential coordinator endpoint publication unsupported")
	ErrCredentialEndpointIncompatible           = errors.New("credential coordinator endpoint protocol incompatible")
	ErrCredentialEndpointIdentityChanged        = errors.New("credential coordinator endpoint identity changed")
	ErrCredentialEndpointLockHeld               = errors.New("credential coordinator endpoint owner lock is held")
	ErrCredentialEndpointDurability             = errors.New("credential coordinator endpoint durability failure")
)

type credentialEndpointPhase string
type credentialEndpointPhaseHook func(credentialEndpointPhase)

type credentialEndpointSidecarCAS struct {
	identity fsutil.SecureFileIdentity
	digest   string
}

const (
	credentialEndpointPhaseNamespacePinned                       credentialEndpointPhase = "namespace_pinned"
	credentialEndpointPhaseMaintenanceAdmitted                   credentialEndpointPhase = "maintenance_admitted"
	credentialEndpointPhaseMaintenanceRollbackCandidateValidated credentialEndpointPhase = "maintenance_rollback_candidate_validated"
	credentialEndpointPhaseLifetimeLockAcquired                  credentialEndpointPhase = "lifetime_lock_acquired"
	credentialEndpointPhasePrepared                              credentialEndpointPhase = "prepared_sidecar_durable"
	credentialEndpointPhaseLinked                                credentialEndpointPhase = "final_link_created"
	credentialEndpointPhaseTemporaryRemoved                      credentialEndpointPhase = "temporary_link_removed"
	credentialEndpointPhasePublished                             credentialEndpointPhase = "published_sidecar_durable"
	credentialEndpointPhaseClosing                               credentialEndpointPhase = "closing_sidecar_durable"
	credentialEndpointPhaseFinalRemoved                          credentialEndpointPhase = "closing_final_removed"
	credentialEndpointPhaseSidecarRemoved                        credentialEndpointPhase = "closing_sidecar_removed"
)

type credentialOwnerProtocol uint8

const (
	credentialOwnerUnavailable credentialOwnerProtocol = iota
	credentialOwnerRefused
	credentialOwnerVersioned
)

type credentialEndpoint struct {
	fs                     fsutil.OSFileSystem
	listener               *net.UnixListener
	lock                   fsutil.ExclusiveLock
	secureDirectory        fsutil.SecureDirectory
	directoryFD            int
	directory              string
	directoryID            fsutil.SecureFileIdentity
	path                   string
	finalName              string
	generation             string
	identity               credentialEndpointIdentity
	lockIdentity           fsutil.SecureFileIdentity
	sidecar                credentialEndpointSidecar
	sidecarCAS             *credentialEndpointSidecarCAS
	hook                   credentialEndpointPhaseHook
	writeHook              func(credentialEndpointSidecar) error
	maintenanceMu          sync.Mutex
	maintenanceGate        credentialEndpointMaintenanceOpenGate
	recoveryRecorder       CredentialEndpointRecoveryRecorder
	recoveryRecordRequired bool
	closeOnce              sync.Once
	closeErr               error
	releaseOnce            sync.Once
}

func openCredentialEndpoint(path string, allowRecovery bool, hook credentialEndpointPhaseHook) (*credentialEndpoint, *rpc.Client, error) {
	return openCredentialEndpointWithContext(context.Background(), path, allowRecovery, hook)
}

func openCredentialEndpointWithContext(ctx context.Context, path string, allowRecovery bool, hook credentialEndpointPhaseHook) (*credentialEndpoint, *rpc.Client, error) {
	return openCredentialEndpointWithRecoveryObservation(ctx, path, allowRecovery, hook, nil, false)
}

func openCredentialEndpointWithRecoveryRecorder(path string, allowRecovery bool, hook credentialEndpointPhaseHook, recorder CredentialEndpointRecoveryRecorder) (*credentialEndpoint, *rpc.Client, error) {
	return openCredentialEndpointWithRecoveryRecorderContext(context.Background(), path, allowRecovery, hook, recorder)
}

func openCredentialEndpointWithRecoveryRecorderContext(ctx context.Context, path string, allowRecovery bool, hook credentialEndpointPhaseHook, recorder CredentialEndpointRecoveryRecorder) (*credentialEndpoint, *rpc.Client, error) {
	return openCredentialEndpointWithRecoveryObservation(ctx, path, allowRecovery, hook, recorder, true)
}

func openCredentialEndpointWithRecoveryObservation(ctx context.Context, path string, allowRecovery bool, hook credentialEndpointPhaseHook, recorder CredentialEndpointRecoveryRecorder, recoveryRecordRequired bool) (*credentialEndpoint, *rpc.Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	directory, finalName, err := validateCredentialEndpointPath(path)
	if err != nil {
		return nil, nil, err
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.EnsureSecureDirectory(fsys, directory); err != nil {
		return nil, nil, err
	}

	secureDirectory, err := fsys.OpenSecureDirectory(directory)
	if err != nil {
		return nil, nil, err
	}
	directoryInfo, err := secureDirectory.Stat()
	if err != nil {
		_ = secureDirectory.Close()
		return nil, nil, err
	}
	directoryID, ok := fsys.FileIdentity(directoryInfo)
	if !ok {
		_ = secureDirectory.Close()
		return nil, nil, fmt.Errorf("%w: endpoint directory identity", fsutil.ErrUnsafeSecurePath)
	}
	directoryFD, err := openCredentialEndpointDirectory(directory)
	if err != nil {
		_ = secureDirectory.Close()
		return nil, nil, err
	}
	endpoint := &credentialEndpoint{
		fs: fsys, secureDirectory: secureDirectory, directoryFD: directoryFD,
		directory: directory, directoryID: directoryID,
		path: path, finalName: finalName, hook: hook,
		recoveryRecorder: recorder, recoveryRecordRequired: recoveryRecordRequired,
	}
	if err := endpoint.validateDirectoryNamespace(); err != nil {
		endpoint.release()
		return nil, nil, err
	}
	endpoint.invokePhase(credentialEndpointPhaseNamespacePinned)
	client, err := endpoint.openLocked(ctx, allowRecovery)
	if err != nil {
		endpoint.release()
		return nil, nil, err
	}
	if client != nil {
		endpoint.release()
		return nil, client, nil
	}
	return endpoint, nil, nil
}

func (e *credentialEndpoint) validateDirectoryNamespace() error {
	if e == nil || e.secureDirectory == nil || e.directoryFD < 0 {
		return fmt.Errorf("%w: endpoint directory unavailable", fsutil.ErrUnsafeSecurePath)
	}
	pathInfo, err := e.fs.Lstat(e.directory)
	if err != nil {
		return err
	}
	if !pathInfo.IsDir() || pathInfo.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || pathInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: endpoint directory metadata", fsutil.ErrUnsafeSecurePath)
	}
	owner, ok := e.fs.FileOwnerUID(pathInfo)
	if !ok || owner != e.fs.EffectiveUID() {
		return fmt.Errorf("%w: endpoint directory owner", fsutil.ErrUnsafeSecurePath)
	}
	pathID, ok := e.fs.FileIdentity(pathInfo)
	if !ok || !sameCredentialDirectory(pathID, e.directoryID) {
		return fmt.Errorf("%w: endpoint directory path identity", fsutil.ErrUnsafeSecurePath)
	}
	descriptorInfo, err := e.secureDirectory.Stat()
	if err != nil {
		return err
	}
	descriptorID, ok := e.fs.FileIdentity(descriptorInfo)
	if !ok || !sameCredentialDirectory(descriptorID, e.directoryID) {
		return fmt.Errorf("%w: retained endpoint directory identity", fsutil.ErrUnsafeSecurePath)
	}
	var rawStat unix.Stat_t
	if err := unix.Fstat(e.directoryFD, &rawStat); err != nil {
		return err
	}
	if rawStat.Mode&unix.S_IFMT != unix.S_IFDIR || rawStat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || uint32(rawStat.Mode)&0o777 != 0o700 || uint64(rawStat.Uid) != e.fs.EffectiveUID() || uint64(rawStat.Dev) != e.directoryID.Device || uint64(rawStat.Ino) != e.directoryID.Inode {
		return fmt.Errorf("%w: raw endpoint directory identity", fsutil.ErrUnsafeSecurePath)
	}
	return nil
}

func sameCredentialDirectory(left, right fsutil.SecureFileIdentity) bool {
	return left.Device == right.Device && left.Inode == right.Inode
}

func (e *credentialEndpoint) Close() error {
	if e == nil {
		return nil
	}
	var listenerErr error
	if e.listener != nil {
		listenerErr = e.listener.Close()
	}
	return errors.Join(listenerErr, e.CloseAfterListener())
}

func (e *credentialEndpoint) CloseAfterListener() error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		e.closeErr = e.closePublished()
		e.release()
	})
	return e.closeErr
}

func validateCredentialEndpointPath(path string) (string, string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", fmt.Errorf("%w: invalid endpoint path", fsutil.ErrUnsafeSecurePath)
	}
	finalName := filepath.Base(path)
	if finalName == "." || finalName == string(filepath.Separator) {
		return "", "", fmt.Errorf("%w: invalid endpoint name", fsutil.ErrUnsafeSecurePath)
	}
	return filepath.Dir(path), finalName, nil
}

func openCredentialEndpointDirectory(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()
	var descriptorStat unix.Stat_t
	if err := unix.Fstat(fd, &descriptorStat); err != nil {
		return -1, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return -1, err
	}
	pathStat, ok := pathInfo.Sys().(*syscall.Stat_t)
	if !ok || descriptorStat.Mode&unix.S_IFMT != unix.S_IFDIR || descriptorStat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || uint32(descriptorStat.Mode)&0o777 != 0o700 || uint64(descriptorStat.Uid) != uint64(os.Geteuid()) || uint64(descriptorStat.Dev) != uint64(pathStat.Dev) || uint64(descriptorStat.Ino) != uint64(pathStat.Ino) {
		return -1, fmt.Errorf("%w: endpoint directory identity", fsutil.ErrUnsafeSecurePath)
	}
	closeFD = false
	return fd, nil
}

type credentialEndpointDialer func(network, address string, timeout time.Duration) (net.Conn, error)

func probeCredentialOwnerWithDial(path string, timeout time.Duration, dial credentialEndpointDialer) (*rpc.Client, credentialOwnerProtocol, string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	lastProtocol := credentialOwnerUnavailable
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, lastProtocol, "", lastErr
		}
		client, protocol, generation, err := probeCredentialOwnerAttempt(path, remaining, dial)
		if client != nil {
			return client, protocol, generation, nil
		}
		lastErr = err
		lastProtocol = protocol
		if wait := min(2*time.Millisecond, time.Until(deadline)); wait > 0 {
			time.Sleep(wait)
		}
	}
}

func probeCredentialOwnerAttempt(path string, timeout time.Duration, dial credentialEndpointDialer) (*rpc.Client, credentialOwnerProtocol, string, error) {
	conn, err := dial("unix", path, timeout)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, credentialOwnerRefused, "", err
		}
		return nil, credentialOwnerUnavailable, "", err
	}
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)
	client := rpc.NewClient(conn)
	var reply CredentialEndpointPingReply
	pingErr := client.Call("CredentialEndpoint.Ping", CredentialEndpointPingArgs{ProtocolVersion: credentialEndpointProtocolVersion}, &reply)
	if pingErr == nil {
		if reply.ProtocolVersion != credentialEndpointProtocolVersion || reply.Generation == "" {
			_ = client.Close()
			return nil, credentialOwnerUnavailable, "", ErrCredentialEndpointIncompatible
		}
		_ = conn.SetDeadline(time.Time{})
		return client, credentialOwnerVersioned, reply.Generation, nil
	}
	_ = client.Close()
	return nil, credentialOwnerUnavailable, "", errors.Join(ErrCredentialEndpointIncompatible, pingErr)
}

func generateCredentialEndpointGeneration() (string, error) {
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", generation[:]), nil
}

func credentialEndpointLockPath(path string) string    { return path + ".lock" }
func credentialEndpointSidecarPath(path string) string { return path + ".owner.json" }

func decodeCredentialEndpointSidecar(data []byte, finalPath string) (credentialEndpointSidecar, error) {
	if len(data) == 0 || len(data) > credentialEndpointSidecarMaxBytes {
		return credentialEndpointSidecar{}, fmt.Errorf("%w: sidecar size", ErrCredentialOwnerStale)
	}
	if err := validateCredentialEndpointSidecarFields(data); err != nil {
		return credentialEndpointSidecar{}, err
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(data), credentialEndpointSidecarMaxBytes+1))
	decoder.DisallowUnknownFields()
	var sidecar credentialEndpointSidecar
	if err := decoder.Decode(&sidecar); err != nil {
		return credentialEndpointSidecar{}, fmt.Errorf("%w: malformed sidecar", ErrCredentialOwnerStale)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return credentialEndpointSidecar{}, fmt.Errorf("%w: trailing sidecar data", ErrCredentialOwnerStale)
	}
	if err := sidecar.validate(finalPath); err != nil {
		return credentialEndpointSidecar{}, err
	}
	return sidecar, nil
}

func validateCredentialEndpointSidecarFields(data []byte) error {
	fields, err := decodeCredentialEndpointObject(data)
	if err != nil {
		return fmt.Errorf("%w: malformed sidecar", ErrCredentialOwnerStale)
	}
	required := []string{"version", "protocol_version", "generation", "state", "temporary_name", "final_name", "device", "inode", "uid", "type", "mode", "lock_device", "lock_inode", "lock_links"}
	allowed := make(map[string]struct{}, len(required)+1)
	for _, field := range required {
		allowed[field] = struct{}{}
	}
	allowed["previous"] = struct{}{}
	for field := range fields {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("%w: unknown sidecar field %s", ErrCredentialOwnerStale, field)
		}
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("%w: missing sidecar field %s", ErrCredentialOwnerStale, field)
		}
	}
	previousData, hasPrevious := fields["previous"]
	if !hasPrevious || bytes.Equal(bytes.TrimSpace(previousData), []byte("null")) {
		return nil
	}
	previousFields, err := decodeCredentialEndpointObject(previousData)
	if err != nil {
		return fmt.Errorf("%w: malformed previous proof", ErrCredentialOwnerStale)
	}
	previousRequired := []string{"generation", "temporary_name", "device", "inode", "uid", "type", "mode"}
	previousAllowed := make(map[string]struct{}, len(previousRequired))
	for _, field := range previousRequired {
		previousAllowed[field] = struct{}{}
	}
	for field := range previousFields {
		if _, ok := previousAllowed[field]; !ok {
			return fmt.Errorf("%w: unknown previous field %s", ErrCredentialOwnerStale, field)
		}
	}
	for _, field := range previousRequired {
		if _, ok := previousFields[field]; !ok {
			return fmt.Errorf("%w: missing previous field %s", ErrCredentialOwnerStale, field)
		}
	}
	return nil
}

func decodeCredentialEndpointObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, errors.New("expected JSON object field")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("duplicate JSON object field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("unterminated JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return fields, nil
}

func (s credentialEndpointSidecar) validate(finalPath string) error {
	validState := s.State == credentialEndpointPrepared || s.State == credentialEndpointPublished || s.State == credentialEndpointClosing
	validNames := s.FinalName == filepath.Base(finalPath) && s.TemporaryName != "" && s.TemporaryName != s.FinalName && filepath.Base(s.TemporaryName) == s.TemporaryName
	validIdentity := s.Generation != "" && s.Type == "socket" && s.Mode == 0o600 && s.UID == uint64(os.Geteuid())
	validLockIdentity := s.LockInode != 0 && s.LockLinks == 1
	if s.Version != credentialEndpointSidecarVersion || s.ProtocolVersion != credentialEndpointProtocolVersion || !validState || !validNames || !validIdentity || !validLockIdentity {
		return fmt.Errorf("%w: invalid sidecar proof", ErrCredentialOwnerStale)
	}
	if s.State != credentialEndpointPrepared && s.Previous != nil {
		return fmt.Errorf("%w: invalid sidecar transition", ErrCredentialOwnerStale)
	}
	if s.Previous != nil {
		validPreviousName := s.Previous.TemporaryName != "" && s.Previous.TemporaryName != s.FinalName && filepath.Base(s.Previous.TemporaryName) == s.Previous.TemporaryName
		if s.Previous.Generation == "" || s.Previous.Generation == s.Generation || !validPreviousName || s.Previous.TemporaryName == s.TemporaryName || s.Previous.Type != "socket" || s.Previous.Mode != 0o600 || s.Previous.UID != uint64(os.Geteuid()) || s.Previous.credentialEndpointIdentity == s.credentialEndpointIdentity {
			return fmt.Errorf("%w: invalid previous endpoint proof", ErrCredentialOwnerStale)
		}
	}
	return nil
}

func (s credentialEndpointSidecar) lockFileIdentity() fsutil.SecureFileIdentity {
	return fsutil.SecureFileIdentity{Device: s.LockDevice, Inode: s.LockInode, Links: s.LockLinks}
}

func credentialEndpointIdentityFromStat(stat *unix.Stat_t, requireSecureMode bool) (credentialEndpointIdentity, error) {
	identity := credentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid),
		Type: "socket", Mode: uint32(stat.Mode) & 0o777,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || identity.UID != uint64(os.Geteuid()) {
		return credentialEndpointIdentity{}, fmt.Errorf("%w: unsafe socket identity", ErrCredentialOwnerStale)
	}
	if requireSecureMode && identity.Mode != 0o600 {
		return credentialEndpointIdentity{}, fmt.Errorf("%w: unsafe socket mode", ErrCredentialOwnerStale)
	}
	return identity, nil
}

func statCredentialEndpointSocketAt(directoryFD int, name string, requireSecureMode bool) (credentialEndpointIdentity, bool, error) {
	if name == "" || filepath.Base(name) != name {
		return credentialEndpointIdentity{}, false, fmt.Errorf("%w: invalid socket name", ErrCredentialOwnerStale)
	}
	var stat unix.Stat_t
	err := unix.Fstatat(directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return credentialEndpointIdentity{}, false, nil
	}
	if err != nil {
		return credentialEndpointIdentity{}, false, err
	}
	identity, err := credentialEndpointIdentityFromStat(&stat, requireSecureMode)
	if err != nil {
		return credentialEndpointIdentity{}, true, err
	}
	return identity, true, nil
}

func (e *credentialEndpoint) readSidecar() (credentialEndpointSidecar, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := e.validateDirectoryNamespace(); err != nil {
			return credentialEndpointSidecar{}, false, errors.Join(ErrCredentialOwnerStale, err)
		}
		data, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(e.fs, e.secureDirectory, filepath.Base(credentialEndpointSidecarPath(e.path)), credentialEndpointSidecarMaxBytes)
		if errors.Is(err, os.ErrNotExist) {
			return credentialEndpointSidecar{}, false, nil
		}
		if err != nil {
			lastErr = err
			if errors.Is(err, fsutil.ErrUnsafeSecurePath) && attempt < 2 && e.validateDirectoryNamespace() == nil {
				time.Sleep(time.Millisecond)
				continue
			}
			return credentialEndpointSidecar{}, false, errors.Join(ErrCredentialOwnerStale, err)
		}
		sidecar, err := decodeCredentialEndpointSidecar(data, e.path)
		if err != nil {
			return credentialEndpointSidecar{}, true, err
		}
		if err := e.validateDirectoryNamespace(); err != nil {
			return credentialEndpointSidecar{}, true, errors.Join(ErrCredentialOwnerStale, err)
		}
		return sidecar, true, nil
	}
	return credentialEndpointSidecar{}, false, errors.Join(ErrCredentialOwnerStale, lastErr)
}

func (e *credentialEndpoint) writeSidecar(sidecar credentialEndpointSidecar) error {
	data, err := json.Marshal(sidecar)
	if err != nil {
		return err
	}
	if err := e.validateDirectoryNamespace(); err != nil {
		return errors.Join(ErrCredentialEndpointDurability, &fsutil.CommitError{Outcome: fsutil.CommitNotCommitted, Op: "validate endpoint namespace", Err: err})
	}
	beforeReplace := e.validateDirectoryNamespace
	var expectedCAS credentialEndpointSidecarCAS
	if e.sidecarCAS != nil {
		expectedCAS = *e.sidecarCAS
		beforeReplace = func() error {
			if err := e.validateDirectoryNamespace(); err != nil {
				return err
			}
			return e.validateSidecarCAS(expectedCAS)
		}
	}
	err = fsutil.SecureAtomicWriteInDirectoryChecked(
		e.fs, e.secureDirectory, filepath.Base(credentialEndpointSidecarPath(e.path)), data, beforeReplace,
	)
	if err == nil && e.sidecarCAS != nil {
		identity, digest, proofErr := e.inspectSidecarCAS()
		if proofErr != nil || digest != digestMaintenanceBytes(data) {
			err = errors.Join(ErrCredentialEndpointIdentityChanged, proofErr)
		} else {
			e.sidecarCAS = &credentialEndpointSidecarCAS{identity: identity, digest: digest}
		}
	}
	if err == nil && e.writeHook != nil {
		err = e.writeHook(sidecar)
	}
	if err != nil {
		return errors.Join(ErrCredentialEndpointDurability, err)
	}
	if err := e.validateDirectoryNamespace(); err != nil {
		return errors.Join(ErrCredentialEndpointDurability, &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: "revalidate endpoint namespace", Err: err})
	}
	return nil
}

func (e *credentialEndpoint) inspectSidecarCAS() (fsutil.SecureFileIdentity, string, error) {
	data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		e.fs, e.secureDirectory, filepath.Base(credentialEndpointSidecarPath(e.path)), credentialEndpointSidecarMaxBytes,
	)
	if err != nil {
		return fsutil.SecureFileIdentity{}, "", err
	}
	return identity, digestMaintenanceBytes(data), nil
}

func (e *credentialEndpoint) validateSidecarCAS(expected credentialEndpointSidecarCAS) error {
	identity, digest, err := e.inspectSidecarCAS()
	if err != nil || identity != expected.identity || digest != expected.digest {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return nil
}

func (e *credentialEndpoint) invokePhase(phase credentialEndpointPhase) {
	if e.hook != nil {
		e.hook(phase)
	}
}

func (e *credentialEndpoint) syncDirectory() error {
	if err := e.validateDirectoryNamespace(); err != nil {
		return errors.Join(ErrCredentialEndpointDurability, err)
	}
	if err := unix.Fsync(e.directoryFD); err != nil {
		return errors.Join(ErrCredentialEndpointDurability, err)
	}
	if err := e.validateDirectoryNamespace(); err != nil {
		return errors.Join(ErrCredentialEndpointDurability, err)
	}
	return nil
}

func (e *credentialEndpoint) removeExactSocket(name string, expected credentialEndpointIdentity) (bool, error) {
	if err := e.validateDirectoryNamespace(); err != nil {
		return false, err
	}
	identity, exists, err := statCredentialEndpointSocketAt(e.directoryFD, name, false)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if identity != expected {
		return false, ErrCredentialEndpointIdentityChanged
	}
	identity, exists, err = statCredentialEndpointSocketAt(e.directoryFD, name, false)
	if err != nil {
		return false, err
	}
	if !exists || identity != expected {
		return false, ErrCredentialEndpointIdentityChanged
	}
	if err := unix.Unlinkat(e.directoryFD, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, ErrCredentialEndpointIdentityChanged
		}
		return false, err
	}
	if err := e.validateDirectoryNamespace(); err != nil {
		return true, err
	}
	return true, nil
}

func (e *credentialEndpoint) removeExpectedSidecar(expected credentialEndpointSidecar, allowMissing bool) error {
	if e.sidecarCAS != nil {
		if err := e.validateSidecarCAS(*e.sidecarCAS); err != nil {
			return err
		}
	}
	actual, exists, err := e.readSidecar()
	if err != nil {
		return err
	}
	if !exists {
		if allowMissing {
			return nil
		}
		return ErrCredentialEndpointIdentityChanged
	}
	if !credentialEndpointSidecarsEqual(actual, expected) {
		return ErrCredentialEndpointIdentityChanged
	}
	actual, exists, err = e.readSidecar()
	if err != nil || !exists || !credentialEndpointSidecarsEqual(actual, expected) {
		return errors.Join(err, ErrCredentialEndpointIdentityChanged)
	}
	if err := e.validateDirectoryNamespace(); err != nil {
		return err
	}
	if e.sidecarCAS != nil {
		if err := e.validateSidecarCAS(*e.sidecarCAS); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(e.directoryFD, filepath.Base(credentialEndpointSidecarPath(e.path)), 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrCredentialEndpointIdentityChanged
		}
		return err
	}
	return e.validateDirectoryNamespace()
}

func credentialEndpointSidecarsEqual(left, right credentialEndpointSidecar) bool {
	if left.Version != right.Version || left.ProtocolVersion != right.ProtocolVersion || left.Generation != right.Generation || left.State != right.State || left.TemporaryName != right.TemporaryName || left.FinalName != right.FinalName || left.credentialEndpointIdentity != right.credentialEndpointIdentity || left.lockFileIdentity() != right.lockFileIdentity() {
		return false
	}
	if left.Previous == nil || right.Previous == nil {
		return left.Previous == nil && right.Previous == nil
	}
	return *left.Previous == *right.Previous
}

func (e *credentialEndpoint) release() {
	e.releaseOnce.Do(func() {
		if e.directoryFD >= 0 {
			_ = unix.Close(e.directoryFD)
			e.directoryFD = -1
		}
		if e.lock != nil {
			_ = e.lock.Close()
			e.lock = nil
		}
		if e.secureDirectory != nil {
			_ = e.secureDirectory.Close()
			e.secureDirectory = nil
		}
	})
}

func (e *credentialEndpoint) createTemporaryListener() (*net.UnixListener, string, string, credentialEndpointIdentity, error) {
	if err := e.validateDirectoryNamespace(); err != nil {
		return nil, "", "", credentialEndpointIdentity{}, err
	}
	generation, err := generateCredentialEndpointGeneration()
	if err != nil {
		return nil, "", "", credentialEndpointIdentity{}, err
	}
	temporaryName := ".cq-credential-" + generation + ".sock"
	temporaryPath := filepath.Join(e.directory, temporaryName)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: temporaryPath, Net: "unix"})
	if err != nil {
		return nil, "", "", credentialEndpointIdentity{}, err
	}
	listener.SetUnlinkOnClose(false)
	if err := e.validateDirectoryNamespace(); err != nil {
		_ = listener.Close()
		return nil, "", "", credentialEndpointIdentity{}, err
	}
	rawIdentity, exists, statErr := statCredentialEndpointSocketAt(e.directoryFD, temporaryName, false)
	if statErr != nil || !exists {
		_ = listener.Close()
		return nil, "", "", credentialEndpointIdentity{}, errors.Join(statErr, ErrCredentialEndpointIdentityChanged)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		_ = listener.Close()
		_, cleanupErr := e.removeExactSocket(temporaryName, rawIdentity)
		return nil, "", "", credentialEndpointIdentity{}, errors.Join(err, cleanupErr)
	}
	identity, exists, err := statCredentialEndpointSocketAt(e.directoryFD, temporaryName, true)
	if err != nil || !exists || !credentialEndpointSameObject(rawIdentity, identity) {
		_ = listener.Close()
		_, cleanupErr := e.removeExactSocket(temporaryName, rawIdentity)
		return nil, "", "", credentialEndpointIdentity{}, errors.Join(err, cleanupErr, ErrCredentialEndpointIdentityChanged)
	}
	return listener, generation, temporaryName, identity, nil
}

func credentialEndpointSameObject(left, right credentialEndpointIdentity) bool {
	return left.Device == right.Device && left.Inode == right.Inode && left.UID == right.UID && left.Type == right.Type
}

func (e *credentialEndpoint) publish(previous *credentialEndpointPrevious) error {
	listener, generation, temporaryName, identity, err := e.createTemporaryListener()
	if err != nil {
		return err
	}
	prepared := credentialEndpointSidecar{
		Version: credentialEndpointSidecarVersion, ProtocolVersion: credentialEndpointProtocolVersion,
		Generation: generation, State: credentialEndpointPrepared,
		TemporaryName: temporaryName, FinalName: e.finalName,
		credentialEndpointIdentity: identity,
		LockDevice:                 e.lockIdentity.Device, LockInode: e.lockIdentity.Inode, LockLinks: e.lockIdentity.Links,
		Previous: previous,
	}
	preparedCommitted := false
	abort := func(cause error) error {
		return errors.Join(cause, e.abortPublication(listener, prepared, preparedCommitted))
	}
	if err := e.writeSidecar(prepared); err != nil {
		return abort(err)
	}
	preparedCommitted = true
	e.invokePhase(credentialEndpointPhasePrepared)

	if previous != nil {
		removed, err := e.removeExactSocket(e.finalName, previous.credentialEndpointIdentity)
		if err != nil || !removed {
			return abort(errors.Join(err, ErrCredentialEndpointIdentityChanged))
		}
		if err := e.syncDirectory(); err != nil {
			return abort(err)
		}
	} else if _, exists, err := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true); err != nil {
		return abort(err)
	} else if exists {
		return abort(ErrCredentialEndpointOccupied)
	}

	if err := publishCredentialSocketNoReplaceAt(e.directoryFD, temporaryName, e.finalName); err != nil {
		return abort(err)
	}
	e.invokePhase(credentialEndpointPhaseLinked)
	if err := e.syncDirectory(); err != nil {
		return abort(err)
	}
	finalIdentity, exists, err := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true)
	if err != nil || !exists || finalIdentity != identity {
		return abort(errors.Join(err, ErrCredentialEndpointIdentityChanged))
	}
	if err := e.verifyCredentialSocketAlias(); err != nil {
		return abort(err)
	}
	removed, err := e.removeExactSocket(temporaryName, identity)
	if err != nil || !removed {
		return abort(errors.Join(err, ErrCredentialEndpointIdentityChanged))
	}
	e.invokePhase(credentialEndpointPhaseTemporaryRemoved)
	if err := e.syncDirectory(); err != nil {
		return abort(err)
	}
	published := prepared
	published.State = credentialEndpointPublished
	published.Previous = nil
	if err := e.writeSidecar(published); err != nil {
		if fsutil.AtomicWriteOutcome(err) == fsutil.CommitIndeterminate {
			// The final socket and either prepared or published proof are both
			// exact recoverable crash states. Do not guess which sidecar rename
			// reached durable storage by destructively aborting either state.
			return errors.Join(err, listener.Close())
		}
		return abort(err)
	}
	e.invokePhase(credentialEndpointPhasePublished)

	e.listener = listener
	e.generation = generation
	e.identity = identity
	e.sidecar = published
	return nil
}

func (e *credentialEndpoint) verifyCredentialSocketAlias() error {
	if err := e.validateDirectoryNamespace(); err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", e.path, credentialEndpointDialTimeout)
	if err != nil {
		return errors.Join(ErrCredentialEndpointPublicationUnsupported, err)
	}
	if err := conn.Close(); err != nil {
		return errors.Join(ErrCredentialEndpointPublicationUnsupported, err)
	}
	return e.validateDirectoryNamespace()
}

func (e *credentialEndpoint) abortPublication(listener *net.UnixListener, prepared credentialEndpointSidecar, preparedCommitted bool) error {
	var cleanupErr error
	if listener != nil {
		cleanupErr = errors.Join(cleanupErr, listener.Close())
	}
	finalIdentity, finalExists, err := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, false)
	cleanupErr = errors.Join(cleanupErr, err)
	previousStillPublished := false
	if err == nil && finalExists {
		switch {
		case finalIdentity == prepared.credentialEndpointIdentity:
			_, removeErr := e.removeExactSocket(e.finalName, prepared.credentialEndpointIdentity)
			cleanupErr = errors.Join(cleanupErr, removeErr)
		case prepared.Previous != nil && finalIdentity == prepared.Previous.credentialEndpointIdentity:
			previousStillPublished = true
		default:
			cleanupErr = errors.Join(cleanupErr, ErrCredentialEndpointIdentityChanged)
		}
	}
	temporaryIdentity, temporaryExists, err := statCredentialEndpointSocketAt(e.directoryFD, prepared.TemporaryName, false)
	cleanupErr = errors.Join(cleanupErr, err)
	if err == nil && temporaryExists {
		if temporaryIdentity != prepared.credentialEndpointIdentity {
			cleanupErr = errors.Join(cleanupErr, ErrCredentialEndpointIdentityChanged)
		} else {
			_, removeErr := e.removeExactSocket(prepared.TemporaryName, prepared.credentialEndpointIdentity)
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
	}
	cleanupErr = errors.Join(cleanupErr, e.syncDirectory())
	if previousStillPublished {
		previous := prepared.Previous
		restored := credentialEndpointSidecar{
			Version: credentialEndpointSidecarVersion, ProtocolVersion: credentialEndpointProtocolVersion,
			Generation: previous.Generation, State: credentialEndpointPublished,
			TemporaryName: previous.TemporaryName, FinalName: e.finalName,
			credentialEndpointIdentity: previous.credentialEndpointIdentity,
			LockDevice:                 prepared.LockDevice, LockInode: prepared.LockInode, LockLinks: prepared.LockLinks,
		}
		cleanupErr = errors.Join(cleanupErr, e.writeSidecar(restored))
	} else if preparedCommitted {
		cleanupErr = errors.Join(cleanupErr, e.removeExpectedSidecar(prepared, false))
		cleanupErr = errors.Join(cleanupErr, e.syncDirectory())
	} else {
		actual, exists, readErr := e.readSidecar()
		cleanupErr = errors.Join(cleanupErr, readErr)
		if readErr == nil && exists && credentialEndpointSidecarsEqual(actual, prepared) {
			cleanupErr = errors.Join(cleanupErr, e.removeExpectedSidecar(prepared, false), e.syncDirectory())
		}
	}
	return cleanupErr
}

func (e *credentialEndpoint) recover(sidecar credentialEndpointSidecar, finalIdentity credentialEndpointIdentity, finalExists bool) error {
	switch sidecar.State {
	case credentialEndpointPublished:
		if !finalExists || finalIdentity != sidecar.credentialEndpointIdentity {
			return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
		}
		previous := &credentialEndpointPrevious{
			Generation: sidecar.Generation, TemporaryName: sidecar.TemporaryName,
			credentialEndpointIdentity: sidecar.credentialEndpointIdentity,
		}
		return e.publish(previous)
	case credentialEndpointPrepared, credentialEndpointClosing:
		if err := e.cleanInterruptedPublication(sidecar, finalIdentity, finalExists); err != nil {
			return err
		}
		return e.publish(nil)
	default:
		return ErrCredentialOwnerStale
	}
}

func (e *credentialEndpoint) cleanInterruptedPublication(sidecar credentialEndpointSidecar, finalIdentity credentialEndpointIdentity, finalExists bool) error {
	if e.sidecarCAS != nil {
		if err := e.validateSidecarCAS(*e.sidecarCAS); err != nil {
			return err
		}
	}
	if finalExists {
		allowed := finalIdentity == sidecar.credentialEndpointIdentity
		if sidecar.State == credentialEndpointPrepared && sidecar.Previous != nil {
			allowed = allowed || finalIdentity == sidecar.Previous.credentialEndpointIdentity
		}
		if !allowed {
			return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
		}
		removed, err := e.removeExactSocket(e.finalName, finalIdentity)
		if err != nil || !removed {
			return errors.Join(err, ErrCredentialEndpointIdentityChanged)
		}
	}
	temporaryIdentity, temporaryExists, err := statCredentialEndpointSocketAt(e.directoryFD, sidecar.TemporaryName, false)
	if err != nil {
		return err
	}
	if temporaryExists {
		if temporaryIdentity != sidecar.credentialEndpointIdentity {
			return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
		}
		removed, err := e.removeExactSocket(sidecar.TemporaryName, sidecar.credentialEndpointIdentity)
		if err != nil || !removed {
			return errors.Join(err, ErrCredentialEndpointIdentityChanged)
		}
	}
	if err := e.syncDirectory(); err != nil {
		return err
	}
	if err := e.removeExpectedSidecar(sidecar, false); err != nil {
		return err
	}
	return e.syncDirectory()
}

func (e *credentialEndpoint) openLocked(ctx context.Context, allowRecovery bool) (*rpc.Client, error) {
	if err := e.rejectMaintenanceJournal(); err != nil {
		return nil, err
	}
	finalIdentity, finalExists, err := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true)
	if err != nil {
		return nil, errors.Join(ErrCredentialOwnerStale, err)
	}
	sidecar, sidecarExists, err := e.readSidecar()
	if err != nil {
		if e.hasUnboundActivatedMaintenanceGate() {
			if pendingErr := e.rejectMaintenanceJournal(); pendingErr != nil {
				return nil, pendingErr
			}
			if e.hasUnboundActivatedMaintenanceGate() {
				return nil, errors.Join(ErrCredentialEndpointMaintenancePending, err)
			}
		}
		return nil, err
	}
	if sidecarExists && e.hasUnboundActivatedMaintenanceGate() {
		if pendingErr := e.rejectMaintenanceJournal(); pendingErr != nil {
			return nil, pendingErr
		}
		if e.hasUnboundActivatedMaintenanceGate() {
			return nil, ErrCredentialEndpointMaintenancePending
		}
	}
	if !sidecarExists {
		if finalExists && !e.hasUnboundActivatedMaintenanceGate() {
			return nil, ErrCredentialOwnerStale
		}
		if err := e.acquireLifetimeLock(nil); errors.Is(err, fsutil.ErrExclusiveLockHeld) {
			if pendingErr := e.rejectMaintenanceJournal(); pendingErr != nil {
				return nil, pendingErr
			}
			return e.waitForLiveOwner(ctx, credentialEndpointDialTimeout)
		} else if err != nil {
			return nil, err
		}
		_, exists, err := e.readSidecar()
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
		}
		if err := e.rejectMaintenanceJournal(); err != nil {
			return nil, err
		}
		if _, exists, err := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true); err != nil {
			return nil, err
		} else if exists {
			return nil, ErrCredentialEndpointOccupied
		}
		if err := e.publish(nil); err != nil {
			return nil, err
		}
		if err := e.bindActivatedMaintenanceOwner(); err != nil {
			return nil, e.preserveUnboundMaintenancePublication(err)
		}
		return nil, nil
	}

	effectiveSidecar := sidecar
	deviceRollover := false
	if crashErr := e.validateCrashState(sidecar, finalIdentity, finalExists); crashErr != nil {
		if errors.Is(crashErr, ErrCredentialEndpointIdentityChanged) {
			heldErr := fsutil.ValidateExclusiveLockHeldInDirectory(e.fs, e.secureDirectory, filepath.Base(credentialEndpointLockPath(e.path)), sidecar.lockFileIdentity())
			if heldErr == nil {
				client, waitErr := e.waitForLiveOwner(ctx, credentialEndpointDialTimeout)
				if client != nil || !errors.Is(waitErr, fsutil.ErrExclusiveLockNotHeld) {
					return client, waitErr
				}
			}
		}
		if !allowRecovery || !e.canRecoverDarwinDeviceRollover(sidecar, finalIdentity, finalExists) {
			return nil, crashErr
		}
		if e.hasBoundActivatedMaintenanceGate() {
			return nil, ErrCredentialEndpointMaintenancePending
		}
		identity, digest, inspectErr := e.inspectSidecarCAS()
		if inspectErr != nil {
			return nil, errors.Join(ErrCredentialOwnerStale, inspectErr)
		}
		e.sidecarCAS = &credentialEndpointSidecarCAS{identity: identity, digest: digest}
		if err := e.acquireLifetimeLock(nil); err != nil {
			if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
				return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointLockHeld)
			}
			return nil, err
		}
		var rebaseErr error
		effectiveSidecar, rebaseErr = e.rebaseDarwinDeviceRollover(sidecar, finalIdentity, finalExists, e.lockIdentity)
		if rebaseErr != nil {
			return nil, rebaseErr
		}
		deviceRollover = true
	}
	expectedLock := sidecar.lockFileIdentity()
	if !deviceRollover {
		heldErr := fsutil.ValidateExclusiveLockHeldInDirectory(e.fs, e.secureDirectory, filepath.Base(credentialEndpointLockPath(e.path)), expectedLock)
		switch {
		case heldErr == nil:
			client, waitErr := e.waitForLiveOwner(ctx, credentialEndpointDialTimeout)
			if client != nil || !errors.Is(waitErr, fsutil.ErrExclusiveLockNotHeld) {
				return client, waitErr
			}
		case errors.Is(heldErr, fsutil.ErrExclusiveLockNotHeld):
			// A proved but unlocked endpoint can only be changed by supervised
			// recovery below.
		default:
			return nil, errors.Join(ErrCredentialOwnerStale, heldErr)
		}
		if e.hasBoundActivatedMaintenanceGate() {
			return nil, ErrCredentialEndpointMaintenancePending
		}
		if !allowRecovery {
			return nil, ErrCredentialOwnerStale
		}
		if err := e.acquireLifetimeLock(&expectedLock); err != nil {
			if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
				if pendingErr := e.rejectMaintenanceJournal(); pendingErr != nil {
					return nil, pendingErr
				}
				return e.waitForLiveOwner(ctx, credentialEndpointDialTimeout)
			}
			return nil, err
		}
	}
	current, exists, err := e.readSidecar()
	if err != nil || !exists || !credentialEndpointSidecarsEqual(current, sidecar) {
		return nil, errors.Join(err, ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
	}
	finalIdentity, finalExists, err = statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true)
	if err != nil {
		return nil, errors.Join(ErrCredentialOwnerStale, err)
	}
	if deviceRollover {
		effectiveSidecar, err = e.rebaseDarwinDeviceRollover(sidecar, finalIdentity, finalExists, e.lockIdentity)
		if err != nil {
			return nil, err
		}
		if err := e.validateSidecarCAS(*e.sidecarCAS); err != nil {
			return nil, errors.Join(ErrCredentialOwnerStale, err)
		}
	}
	if err := e.validateCrashState(effectiveSidecar, finalIdentity, finalExists); err != nil {
		return nil, err
	}
	if finalExists {
		client, protocol, _, probeErr := probeCredentialOwnerAttempt(e.path, credentialEndpointDialTimeout, net.DialTimeout)
		if client != nil {
			_ = client.Close()
			return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointLockHeld)
		}
		if protocol != credentialOwnerRefused {
			return nil, errors.Join(ErrCredentialOwnerStale, probeErr)
		}
	}
	if err := e.rejectMaintenanceJournal(); err != nil {
		return nil, err
	}
	if err := e.recordEndpointRecovery(); err != nil {
		return nil, err
	}
	if err := e.recover(effectiveSidecar, finalIdentity, finalExists); err != nil {
		return nil, err
	}
	if err := e.bindActivatedMaintenanceOwner(); err != nil {
		return nil, e.preserveUnboundMaintenancePublication(err)
	}
	return nil, nil
}

func (e *credentialEndpoint) canRecoverDarwinDeviceRollover(sidecar credentialEndpointSidecar, final credentialEndpointIdentity, finalExists bool) bool {
	if runtime.GOOS != "darwin" || sidecar.State != credentialEndpointPublished || !finalExists {
		return false
	}
	if sidecar.Device == 0 || sidecar.Device != sidecar.LockDevice || sidecar.Device == final.Device {
		return false
	}
	expected := sidecar.credentialEndpointIdentity
	expected.Device = final.Device
	return final.Device == e.directoryID.Device && final == expected
}

func (e *credentialEndpoint) rebaseDarwinDeviceRollover(sidecar credentialEndpointSidecar, final credentialEndpointIdentity, finalExists bool, lock fsutil.SecureFileIdentity) (credentialEndpointSidecar, error) {
	if !e.canRecoverDarwinDeviceRollover(sidecar, final, finalExists) ||
		lock.Device != final.Device || lock.Inode != sidecar.LockInode || lock.Links != sidecar.LockLinks {
		return credentialEndpointSidecar{}, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
	}
	rebased := sidecar
	rebased.Device = final.Device
	rebased.LockDevice = lock.Device
	return rebased, nil
}

func (e *credentialEndpoint) recordEndpointRecovery() (result error) {
	if !e.recoveryRecordRequired {
		return nil
	}
	if e.recoveryRecorder == nil {
		return ErrCredentialEndpointRecoveryUnrecorded
	}
	defer func() {
		if recover() != nil {
			result = ErrCredentialEndpointRecoveryUnrecorded
		}
	}()
	if err := e.recoveryRecorder.RecordCredentialEndpointRecovery(); err != nil {
		return ErrCredentialEndpointRecoveryUnrecorded
	}
	return nil
}

func (e *credentialEndpoint) preserveUnboundMaintenancePublication(cause error) error {
	var listenerErr error
	if e != nil && e.listener != nil {
		listenerErr = e.listener.Close()
		e.listener = nil
	}
	return errors.Join(ErrCredentialEndpointMaintenancePending, cause, listenerErr)
}

func (e *credentialEndpoint) acquireLifetimeLock(expected *fsutil.SecureFileIdentity) error {
	if err := e.validateDirectoryNamespace(); err != nil {
		return err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(e.fs, e.secureDirectory, filepath.Base(credentialEndpointLockPath(e.path)))
	if err != nil {
		return err
	}
	info, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return err
	}
	identity, ok := e.fs.FileIdentity(info)
	if !ok || (expected != nil && identity != *expected) {
		_ = lock.Close()
		return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
	}
	if err := e.validateDirectoryNamespace(); err != nil {
		_ = lock.Close()
		return err
	}
	e.lock = lock
	e.lockIdentity = identity
	e.invokePhase(credentialEndpointPhaseLifetimeLockAcquired)
	return nil
}

func (e *credentialEndpoint) validateCrashState(sidecar credentialEndpointSidecar, finalIdentity credentialEndpointIdentity, finalExists bool) error {
	temporaryIdentity, temporaryExists, err := statCredentialEndpointSocketAt(e.directoryFD, sidecar.TemporaryName, false)
	if err != nil {
		return errors.Join(ErrCredentialOwnerStale, err)
	}
	return validateCredentialEndpointCrashState(sidecar, finalIdentity, finalExists, temporaryIdentity, temporaryExists)
}

func (e *credentialEndpoint) waitForLiveOwner(ctx context.Context, timeout time.Duration) (*rpc.Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	contextDeadline, hasContextDeadline := ctx.Deadline()
	if hasContextDeadline {
		deadline = contextDeadline
	}
	deadlineError := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hasContextDeadline {
			return context.DeadlineExceeded
		}
		return nil
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointLockHeld, err, lastErr)
		}
		if err := e.rejectMaintenanceJournal(); err != nil {
			return nil, err
		}
		if err := e.validateDirectoryNamespace(); err != nil {
			return nil, errors.Join(ErrCredentialOwnerStale, err)
		}
		finalIdentity, finalExists, err := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true)
		if err != nil {
			return nil, errors.Join(ErrCredentialOwnerStale, err)
		}
		sidecar, sidecarExists, err := e.readSidecar()
		if err != nil {
			return nil, err
		}
		if sidecarExists {
			heldErr := fsutil.ValidateExclusiveLockHeldInDirectory(e.fs, e.secureDirectory, filepath.Base(credentialEndpointLockPath(e.path)), sidecar.lockFileIdentity())
			if errors.Is(heldErr, fsutil.ErrExclusiveLockNotHeld) {
				return nil, heldErr
			}
			if heldErr != nil {
				return nil, errors.Join(ErrCredentialOwnerStale, heldErr)
			}
			crashErr := e.validateCrashState(sidecar, finalIdentity, finalExists)
			if crashErr != nil && !errors.Is(crashErr, ErrCredentialEndpointIdentityChanged) {
				return nil, crashErr
			}
			if crashErr != nil {
				lastErr = crashErr
			} else if sidecar.State == credentialEndpointPublished && finalExists && finalIdentity == sidecar.credentialEndpointIdentity {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointLockHeld, deadlineError(), lastErr)
				}
				client, protocol, generation, probeErr := probeCredentialOwnerAttempt(e.path, min(remaining, 25*time.Millisecond), net.DialTimeout)
				lastErr = probeErr
				if client != nil {
					if protocol != credentialOwnerVersioned {
						_ = client.Close()
						return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIncompatible)
					}
					if generation != sidecar.Generation {
						_ = client.Close()
						lastErr = ErrCredentialEndpointIdentityChanged
						continue
					}
					current, exists, readErr := e.readSidecar()
					currentIdentity, currentExists, statErr := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true)
					heldErr = fsutil.ValidateExclusiveLockHeldInDirectory(e.fs, e.secureDirectory, filepath.Base(credentialEndpointLockPath(e.path)), sidecar.lockFileIdentity())
					var crashErr error
					if readErr == nil && statErr == nil && exists {
						crashErr = e.validateCrashState(current, currentIdentity, currentExists)
					}
					if readErr != nil || statErr != nil || heldErr != nil || crashErr != nil {
						_ = client.Close()
						return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged, readErr, statErr, heldErr, crashErr)
					}
					if !exists || !currentExists || !credentialEndpointSidecarsEqual(current, sidecar) || currentIdentity != sidecar.credentialEndpointIdentity {
						_ = client.Close()
						lastErr = ErrCredentialEndpointIdentityChanged
						continue
					}
					if err := e.validateMaintenanceDelegation(current, generation); err != nil {
						_ = client.Close()
						lastErr = err
						continue
					}
					return client, nil
				}
				if protocol != credentialOwnerRefused && protocol != credentialOwnerUnavailable {
					return nil, errors.Join(ErrCredentialOwnerStale, probeErr)
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointLockHeld, deadlineError(), lastErr)
		}
		wait := min(2*time.Millisecond, time.Until(deadline))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointLockHeld, ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func (e *credentialEndpoint) rejectMaintenanceJournal() error {
	return e.validateMaintenanceOpenGate()
}

func validateCredentialEndpointCrashState(sidecar credentialEndpointSidecar, finalIdentity credentialEndpointIdentity, finalExists bool, temporaryIdentity credentialEndpointIdentity, temporaryExists bool) error {
	switch sidecar.State {
	case credentialEndpointPublished:
		if !finalExists || finalIdentity != sidecar.credentialEndpointIdentity || temporaryExists {
			return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
		}
	case credentialEndpointClosing:
		if (finalExists && finalIdentity != sidecar.credentialEndpointIdentity) || temporaryExists {
			return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
		}
	case credentialEndpointPrepared:
		if temporaryExists && temporaryIdentity != sidecar.credentialEndpointIdentity {
			return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
		}
		if finalExists {
			matchesCurrent := finalIdentity == sidecar.credentialEndpointIdentity
			matchesPrevious := sidecar.Previous != nil && finalIdentity == sidecar.Previous.credentialEndpointIdentity
			if !matchesCurrent && !matchesPrevious {
				return errors.Join(ErrCredentialOwnerStale, ErrCredentialEndpointIdentityChanged)
			}
		}
	default:
		return ErrCredentialOwnerStale
	}
	return nil
}

func (e *credentialEndpoint) closePublished() error {
	finalIdentity, finalExists, err := statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true)
	if err != nil || !finalExists || finalIdentity != e.identity {
		return errors.Join(err, ErrCredentialEndpointIdentityChanged)
	}
	current, exists, err := e.readSidecar()
	if err != nil || !exists || !credentialEndpointSidecarsEqual(current, e.sidecar) {
		return errors.Join(err, ErrCredentialEndpointIdentityChanged)
	}
	closing := e.sidecar
	closing.State = credentialEndpointClosing
	if err := e.writeSidecar(closing); err != nil {
		return err
	}
	e.invokePhase(credentialEndpointPhaseClosing)
	finalIdentity, finalExists, err = statCredentialEndpointSocketAt(e.directoryFD, e.finalName, true)
	if err != nil || !finalExists || finalIdentity != e.identity {
		return errors.Join(err, ErrCredentialEndpointIdentityChanged)
	}
	removed, err := e.removeExactSocket(e.finalName, e.identity)
	if err != nil || !removed {
		return errors.Join(err, ErrCredentialEndpointIdentityChanged)
	}
	e.invokePhase(credentialEndpointPhaseFinalRemoved)
	if err := e.syncDirectory(); err != nil {
		return err
	}
	if err := e.removeExpectedSidecar(closing, false); err != nil {
		return err
	}
	e.invokePhase(credentialEndpointPhaseSidecarRemoved)
	return e.syncDirectory()
}

func publishCredentialSocketNoReplace(temporaryPath, finalPath string) error {
	temporaryDir := filepath.Clean(filepath.Dir(temporaryPath))
	finalDir := filepath.Clean(filepath.Dir(finalPath))
	if temporaryDir != finalDir {
		return fmt.Errorf("%w: paths require one directory", ErrCredentialEndpointPublicationUnsupported)
	}
	directoryFD, err := unix.Open(finalDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(directoryFD)
	return publishCredentialSocketNoReplaceAt(directoryFD, filepath.Base(temporaryPath), filepath.Base(finalPath))
}

func publishCredentialSocketNoReplaceAt(directoryFD int, temporaryName, finalName string) error {
	if temporaryName == "" || finalName == "" || filepath.Base(temporaryName) != temporaryName || filepath.Base(finalName) != finalName {
		return fmt.Errorf("%w: invalid publication name", ErrCredentialEndpointPublicationUnsupported)
	}
	err := unix.Linkat(directoryFD, temporaryName, directoryFD, finalName, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return ErrCredentialEndpointOccupied
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %v", ErrCredentialEndpointPublicationUnsupported, err)
	default:
		return err
	}
}
