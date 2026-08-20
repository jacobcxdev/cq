package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func DefaultRuntimeLifecyclePath() string {
	return filepath.Join(configDir(), ".cq-instance-cq.lifecycle.lock")
}

const (
	RuntimeListenerFD   = 3
	RuntimeLifecycleFD  = 4
	RuntimeControlFD    = 5
	RuntimeSecretFD     = 6
	RuntimeNoListenerFD = -1

	RuntimeSecretSize        = 32
	RuntimeControlFrameLimit = 64 << 10
	RuntimeHTTPBodyLimit     = 32 << 10
)

var (
	ErrRuntimeRoleManifest        = errors.New("invalid runtime role manifest")
	ErrRuntimeRoleUnavailable     = errors.New("runtime role unavailable")
	ErrRuntimeControlFrame        = errors.New("invalid runtime control frame")
	ErrRuntimeControlBackpressure = errors.New("runtime control backpressure")
	ErrRuntimeSecretDestroyed     = errors.New("runtime secret destroyed")
)

type RuntimeRole string

const (
	RuntimeRoleSupervisor RuntimeRole = "supervisor"
	RuntimeRoleWorker     RuntimeRole = "worker"
)

type RuntimeRoleManifestV1 struct {
	SchemaVersion                 int               `json:"schema_version"`
	Role                          RuntimeRole       `json:"role"`
	ManifestDigest                [sha256.Size]byte `json:"manifest_digest"`
	ProxyInstanceID               string            `json:"proxy_instance_id"`
	RuntimeInstanceID             string            `json:"runtime_instance_id"`
	ListenerFD                    int               `json:"listener_fd"`
	LifecycleFD                   int               `json:"lifecycle_fd"`
	ControlFD                     int               `json:"control_fd"`
	SecretFD                      int               `json:"secret_fd"`
	LifecycleHolderIdentityDigest [sha256.Size]byte `json:"lifecycle_holder_identity_digest"`
}

func (manifest RuntimeRoleManifestV1) validate() error {
	if manifest.SchemaVersion != 1 ||
		(manifest.Role != RuntimeRoleSupervisor && manifest.Role != RuntimeRoleWorker) ||
		manifest.ProxyInstanceID == "" || manifest.RuntimeInstanceID == "" ||
		manifest.ManifestDigest == ([sha256.Size]byte{}) ||
		manifest.LifecycleHolderIdentityDigest == ([sha256.Size]byte{}) ||
		((manifest.Role == RuntimeRoleSupervisor && manifest.ListenerFD != RuntimeListenerFD) ||
			(manifest.Role == RuntimeRoleWorker && manifest.ListenerFD != RuntimeNoListenerFD)) ||
		manifest.LifecycleFD != RuntimeLifecycleFD ||
		manifest.ControlFD != RuntimeControlFD || manifest.ListenerFD == manifest.LifecycleFD ||
		manifest.ListenerFD == manifest.ControlFD || manifest.LifecycleFD == manifest.ControlFD ||
		manifest.SecretFD != RuntimeSecretFD || manifest.SecretFD == manifest.ListenerFD ||
		manifest.SecretFD == manifest.LifecycleFD || manifest.SecretFD == manifest.ControlFD {
		return ErrRuntimeRoleManifest
	}
	return nil
}

func RuntimeRoleArguments(manifest RuntimeRoleManifestV1) []string {
	if manifest.validate() != nil {
		return nil
	}
	arguments := []string{
		"--runtime-role", string(manifest.Role),
		"--runtime-schema", strconv.Itoa(manifest.SchemaVersion),
		"--runtime-manifest-digest", hex.EncodeToString(manifest.ManifestDigest[:]),
		"--proxy-instance", manifest.ProxyInstanceID,
		"--runtime-instance", manifest.RuntimeInstanceID,
	}
	if manifest.Role == RuntimeRoleSupervisor {
		arguments = append(arguments, "--listener-fd", strconv.Itoa(manifest.ListenerFD))
	}
	return append(arguments,
		"--lifecycle-fd", strconv.Itoa(manifest.LifecycleFD),
		"--control-fd", strconv.Itoa(manifest.ControlFD),
		"--lifecycle-holder-digest", hex.EncodeToString(manifest.LifecycleHolderIdentityDigest[:]),
		"--secret-fd", strconv.Itoa(manifest.SecretFD),
	)
}

func ParseRuntimeRoleArguments(arguments []string) (RuntimeRoleManifestV1, error) {
	var manifest RuntimeRoleManifestV1
	if len(arguments) < 2 {
		return manifest, ErrRuntimeRoleManifest
	}
	manifest.Role = RuntimeRole(arguments[1])
	wantLength := 18
	listenerOffset := 0
	manifest.ListenerFD = RuntimeNoListenerFD
	if manifest.Role == RuntimeRoleSupervisor {
		wantLength = 20
		listenerOffset = 2
	} else if manifest.Role != RuntimeRoleWorker {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	if len(arguments) != wantLength || arguments[0] != "--runtime-role" || arguments[2] != "--runtime-schema" ||
		arguments[4] != "--runtime-manifest-digest" || arguments[6] != "--proxy-instance" || arguments[8] != "--runtime-instance" ||
		(listenerOffset != 0 && arguments[10] != "--listener-fd") || arguments[10+listenerOffset] != "--lifecycle-fd" ||
		arguments[12+listenerOffset] != "--control-fd" || arguments[14+listenerOffset] != "--lifecycle-holder-digest" ||
		arguments[16+listenerOffset] != "--secret-fd" {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	manifest.ProxyInstanceID = arguments[7]
	manifest.RuntimeInstanceID = arguments[9]
	var err error
	if manifest.SchemaVersion, err = parseCanonicalRuntimeDecimal(arguments[3]); err != nil {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	if arguments[5] != strings.ToLower(arguments[5]) {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	manifestDigest, err := hex.DecodeString(arguments[5])
	if err != nil || len(manifestDigest) != sha256.Size {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	copy(manifest.ManifestDigest[:], manifestDigest)
	if listenerOffset != 0 {
		if manifest.ListenerFD, err = parseCanonicalRuntimeDecimal(arguments[11]); err != nil {
			return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
		}
	}
	if manifest.LifecycleFD, err = parseCanonicalRuntimeDecimal(arguments[11+listenerOffset]); err != nil {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	if manifest.ControlFD, err = parseCanonicalRuntimeDecimal(arguments[13+listenerOffset]); err != nil {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	if arguments[15+listenerOffset] != strings.ToLower(arguments[15+listenerOffset]) {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	holderDigest, err := hex.DecodeString(arguments[15+listenerOffset])
	if err != nil || len(holderDigest) != sha256.Size {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	copy(manifest.LifecycleHolderIdentityDigest[:], holderDigest)
	if manifest.SecretFD, err = parseCanonicalRuntimeDecimal(arguments[17+listenerOffset]); err != nil {
		return RuntimeRoleManifestV1{}, ErrRuntimeRoleManifest
	}
	if err := manifest.validate(); err != nil {
		return RuntimeRoleManifestV1{}, err
	}
	return manifest, nil
}

func parseCanonicalRuntimeDecimal(value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || strconv.Itoa(parsed) != value {
		return 0, ErrRuntimeRoleManifest
	}
	return parsed, nil
}

func ReadRuntimeSecret(reader io.ReadCloser) (*RuntimeSecret, error) {
	if reader == nil {
		return nil, ErrRuntimeControlFrame
	}
	defer reader.Close()
	material := make([]byte, RuntimeSecretSize)
	defer zeroRuntimeBytes(material)
	if _, err := io.ReadFull(reader, material); err != nil {
		return nil, ErrRuntimeControlFrame
	}
	var trailing [1]byte
	if count, err := reader.Read(trailing[:]); count != 0 || !errors.Is(err, io.EOF) {
		return nil, ErrRuntimeControlFrame
	}
	return NewRuntimeSecret(material)
}

func NewRuntimePrivateTransport() (net.Conn, net.Conn, error) {
	firstFile, secondFile, err := newRuntimePrivateSocketFiles()
	if err != nil {
		return nil, nil, err
	}
	first, firstErr := net.FileConn(firstFile)
	second, secondErr := net.FileConn(secondFile)
	closeErr := errors.Join(firstFile.Close(), secondFile.Close())
	if firstErr != nil || secondErr != nil || closeErr != nil {
		if first != nil {
			_ = first.Close()
		}
		if second != nil {
			_ = second.Close()
		}
		return nil, nil, errors.Join(firstErr, secondErr, closeErr)
	}
	return first, second, nil
}

func newRuntimePrivateSocketFiles() (*os.File, *os.File, error) {
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(descriptors[0])
	unix.CloseOnExec(descriptors[1])
	firstFile := os.NewFile(uintptr(descriptors[0]), "runtime-supervisor-control")
	secondFile := os.NewFile(uintptr(descriptors[1]), "runtime-worker-control")
	return firstFile, secondFile, nil
}

func WriteRuntimeControlMessage(writer io.Writer, secret *RuntimeSecret, frame RuntimeControlFrameV1) error {
	encoded, err := SealRuntimeControlFrame(secret, frame)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(encoded)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err = writer.Write(encoded)
	return err
}

func ReadRuntimeControlMessage(reader io.Reader, receiver *RuntimeControlReceiver) (RuntimeControlFrameV1, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return RuntimeControlFrameV1{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > RuntimeControlFrameLimit {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	encoded := make([]byte, size)
	if _, err := io.ReadFull(reader, encoded); err != nil {
		return RuntimeControlFrameV1{}, err
	}
	return receiver.Receive(encoded)
}

func RuntimeDescriptorIdentityDigest(file *os.File) ([sha256.Size]byte, error) {
	if file == nil {
		return [sha256.Size]byte{}, ErrRuntimeRoleManifest
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return [sha256.Size]byte{}, err
	}
	canonical := "cq/runtime-descriptor-identity/v1\x00" + strconv.FormatUint(uint64(stat.Dev), 10) + "\x00" + strconv.FormatUint(uint64(stat.Ino), 10) + "\x00" + strconv.FormatUint(uint64(stat.Nlink), 10)
	return sha256.Sum256([]byte(canonical)), nil
}

func RuntimeLifecycleHolder(file *os.File, descriptionID string) (LifecycleHolderProof, error) {
	if file == nil || descriptionID == "" {
		return LifecycleHolderProof{}, ErrLifecycleHolderConflict
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return LifecycleHolderProof{}, err
	}
	if stat.Nlink != 1 {
		return LifecycleHolderProof{}, ErrLifecycleHolderConflict
	}
	return LifecycleHolderProof{
		LockIdentity:  fsutil.SecureFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Links: uint64(stat.Nlink)},
		DescriptionID: descriptionID,
		Mode:          LifecycleShared,
	}, nil
}

type TrafficMode string

const (
	TrafficModeNormal TrafficMode = "normal"
	TrafficModeDrain  TrafficMode = "drain"
)

type RuntimeQuiescenceAckV1 struct {
	SchemaVersion int  `json:"schema_version"`
	Quiescent     bool `json:"quiescent"`
}

type RuntimeHTTPRequestV1 struct {
	Method     string                   `json:"method"`
	RequestURI string                   `json:"request_uri"`
	Header     http.Header              `json:"header,omitempty"`
	Body       []byte                   `json:"body,omitempty"`
	Caller     RuntimeCallerAuthorityV1 `json:"caller"`
}

type RuntimeHTTPResponseV1 struct {
	StatusCode int         `json:"status_code"`
	Header     http.Header `json:"header,omitempty"`
	Body       []byte      `json:"body,omitempty"`
}

type WorkerManifestV1 struct {
	SchemaVersion        int    `json:"schema_version"`
	WorkerArtifactDigest string `json:"worker_artifact_digest"`
}

type RuntimeBootAckV1 struct {
	SchemaVersion int                  `json:"schema_version"`
	Kind          string               `json:"kind"`
	Holder        LifecycleHolderProof `json:"holder"`
	CallerIndex   NormalCallerIndexV1  `json:"caller_index"`
	// CallerAuthorityKey is derived independently by each endpoint from the
	// private channel secret. It is never serialised into a control frame.
	CallerAuthorityKey []byte `json:"-"`
}

type RuntimeControl interface {
	BeginDrain(context.Context, TrafficMode, uint64) error
	AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error)
	ReplaceWorker(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error)
}

type RuntimeControlFrameV1 struct {
	SchemaVersion int             `json:"schema_version"`
	Sequence      uint64          `json:"sequence"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
}

type runtimeControlEnvelopeV1 struct {
	Frame RuntimeControlFrameV1 `json:"frame"`
	MAC   string                `json:"mac"`
}

type RuntimeSecret struct {
	mu        sync.Mutex
	value     []byte
	destroyed bool
}

func NewRuntimeSecret(material []byte) (*RuntimeSecret, error) {
	if len(material) != RuntimeSecretSize {
		return nil, ErrRuntimeControlFrame
	}
	return &RuntimeSecret{value: append([]byte(nil), material...)}, nil
}

func (secret *RuntimeSecret) key() ([]byte, error) {
	if secret == nil {
		return nil, ErrRuntimeSecretDestroyed
	}
	secret.mu.Lock()
	defer secret.mu.Unlock()
	if secret.destroyed || len(secret.value) != RuntimeSecretSize {
		return nil, ErrRuntimeSecretDestroyed
	}
	return append([]byte(nil), secret.value...), nil
}

func (secret *RuntimeSecret) Destroy() {
	if secret == nil {
		return
	}
	secret.mu.Lock()
	defer secret.mu.Unlock()
	for i := range secret.value {
		secret.value[i] = 0
	}
	secret.value = nil
	secret.destroyed = true
}

func (secret *RuntimeSecret) Destroyed() bool {
	if secret == nil {
		return true
	}
	secret.mu.Lock()
	defer secret.mu.Unlock()
	return secret.destroyed
}

func SealRuntimeControlFrame(secret *RuntimeSecret, frame RuntimeControlFrameV1) ([]byte, error) {
	if frame.SchemaVersion != 1 || frame.Sequence == 0 || frame.Kind == "" || len(frame.Payload) > RuntimeControlFrameLimit {
		if len(frame.Payload) > RuntimeControlFrameLimit {
			return nil, ErrRuntimeControlBackpressure
		}
		return nil, ErrRuntimeControlFrame
	}
	key, err := secret.key()
	if err != nil {
		return nil, err
	}
	defer zeroRuntimeBytes(key)
	canonical, err := json.Marshal(frame)
	if err != nil || len(canonical) > RuntimeControlFrameLimit {
		return nil, ErrRuntimeControlBackpressure
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(canonical)
	envelope, err := json.Marshal(runtimeControlEnvelopeV1{Frame: frame, MAC: base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
	if err != nil || len(envelope) > RuntimeControlFrameLimit {
		return nil, ErrRuntimeControlBackpressure
	}
	return envelope, nil
}

func OpenRuntimeControlFrame(secret *RuntimeSecret, encoded []byte) (RuntimeControlFrameV1, error) {
	key, err := secret.key()
	if err != nil {
		return RuntimeControlFrameV1{}, err
	}
	defer zeroRuntimeBytes(key)
	if len(encoded) == 0 || len(encoded) > RuntimeControlFrameLimit {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	var envelope runtimeControlEnvelopeV1
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	if envelope.Frame.SchemaVersion != 1 || envelope.Frame.Sequence == 0 || envelope.Frame.Kind == "" || len(envelope.Frame.Payload) > RuntimeControlFrameLimit {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	provided, err := base64.RawURLEncoding.DecodeString(envelope.MAC)
	if err != nil || len(provided) != sha256.Size {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	canonical, err := json.Marshal(envelope.Frame)
	if err != nil {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	return envelope.Frame, nil
}

type RuntimeControlReceiver struct {
	mu         sync.Mutex
	secret     *RuntimeSecret
	next       uint64
	terminated bool
}

func NewRuntimeControlReceiver(secret *RuntimeSecret) *RuntimeControlReceiver {
	return &RuntimeControlReceiver{secret: secret, next: 1}
}

func (receiver *RuntimeControlReceiver) Receive(encoded []byte) (RuntimeControlFrameV1, error) {
	if receiver == nil {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if receiver.terminated {
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	frame, err := OpenRuntimeControlFrame(receiver.secret, encoded)
	if err != nil || frame.Sequence != receiver.next {
		receiver.terminated = true
		if err != nil {
			return RuntimeControlFrameV1{}, err
		}
		return RuntimeControlFrameV1{}, ErrRuntimeControlFrame
	}
	receiver.next++
	return frame, nil
}

func (receiver *RuntimeControlReceiver) Close() {
	if receiver == nil {
		return
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	receiver.terminated = true
	if receiver.secret != nil {
		receiver.secret.Destroy()
	}
}

func zeroRuntimeBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

type RuntimeControlQueue struct {
	frames chan []byte
}

func NewRuntimeControlQueue(capacity int) *RuntimeControlQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &RuntimeControlQueue{frames: make(chan []byte, capacity)}
}

func (queue *RuntimeControlQueue) Enqueue(ctx context.Context, frame []byte) error {
	if queue == nil || len(frame) > RuntimeControlFrameLimit {
		return ErrRuntimeControlBackpressure
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case queue.frames <- append([]byte(nil), frame...):
		return nil
	}
}

func (queue *RuntimeControlQueue) TryEnqueue(frame []byte) error {
	if queue == nil || len(frame) > RuntimeControlFrameLimit {
		return ErrRuntimeControlBackpressure
	}
	select {
	case queue.frames <- append([]byte(nil), frame...):
		return nil
	default:
		return ErrRuntimeControlBackpressure
	}
}
