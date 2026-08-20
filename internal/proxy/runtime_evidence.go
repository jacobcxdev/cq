package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type RuntimeModePhase string

const (
	RuntimeModePhaseIntent    RuntimeModePhase = "intent"
	RuntimeModePhaseEffective RuntimeModePhase = "effective"
)

type RuntimeModeEvidenceV1 struct {
	SchemaVersion int              `json:"schema_version"`
	Generation    uint64           `json:"generation"`
	DesiredMode   TrafficMode      `json:"desired_mode"`
	EffectiveMode TrafficMode      `json:"effective_mode"`
	Phase         RuntimeModePhase `json:"phase"`
}

func (evidence RuntimeModeEvidenceV1) validate() error {
	if evidence.SchemaVersion != 1 || evidence.Generation == 0 {
		return errors.New("runtime mode evidence invalid")
	}
	switch evidence.Phase {
	case RuntimeModePhaseIntent:
		if (evidence.DesiredMode != TrafficModeRescue || evidence.EffectiveMode != TrafficModeRescueDraining) &&
			(evidence.DesiredMode != TrafficModeNormal || evidence.EffectiveMode != TrafficModeRescueExitDraining) {
			return errors.New("runtime mode intent invalid")
		}
	case RuntimeModePhaseEffective:
		if evidence.DesiredMode != evidence.EffectiveMode ||
			(evidence.EffectiveMode != TrafficModeNormal && evidence.EffectiveMode != TrafficModeRescue) {
			return errors.New("runtime mode receipt invalid")
		}
	default:
		return errors.New("runtime mode phase invalid")
	}
	return nil
}

type RuntimeModeEvidenceStore interface {
	Load(context.Context) (RuntimeModeEvidenceV1, bool, error)
	Commit(context.Context, RuntimeModeEvidenceV1) error
}

type runtimeModeEvidenceEnvelopeV1 struct {
	Evidence RuntimeModeEvidenceV1 `json:"evidence"`
	MAC      string                `json:"mac"`
}

type DurableRuntimeModeEvidenceStore struct {
	ctx       context.Context
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	publisher DurableObjectPublisher
	key       [sha256.Size]byte
	current   *RuntimeModeEvidenceV1
	identity  *StableObjectIdentity
}

func OpenRuntimeModeEvidenceStore(ctx context.Context, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, publisher DurableObjectPublisher, key []byte) (*DurableRuntimeModeEvidenceStore, error) {
	if ctx == nil || inspector == nil || directory == nil || publisher == nil || len(key) != sha256.Size {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	store := &DurableRuntimeModeEvidenceStore{ctx: ctx, inspector: inspector, directory: directory, publisher: publisher}
	copy(store.key[:], key)
	body, fileIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, "runtime-mode", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	evidence, err := store.open(body)
	if err != nil {
		return nil, err
	}
	stableIdentity, err := stableAuthorityIdentityFromParts(fileIdentity, int64(len(body)), body)
	if err != nil {
		return nil, err
	}
	store.current, store.identity = &evidence, &stableIdentity
	return store, nil
}

func (store *DurableRuntimeModeEvidenceStore) Load(ctx context.Context) (RuntimeModeEvidenceV1, bool, error) {
	if store == nil || ctx == nil {
		return RuntimeModeEvidenceV1{}, false, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return RuntimeModeEvidenceV1{}, false, err
	}
	if store.current == nil {
		return RuntimeModeEvidenceV1{}, false, nil
	}
	return *store.current, true, nil
}

func (store *DurableRuntimeModeEvidenceStore) Commit(ctx context.Context, evidence RuntimeModeEvidenceV1) error {
	if store == nil || ctx == nil {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	if err := evidence.validate(); err != nil {
		return err
	}
	if store.current != nil && (evidence.Generation < store.current.Generation ||
		(evidence.Generation == store.current.Generation && (store.current.Phase != RuntimeModePhaseIntent || evidence.Phase != RuntimeModePhaseEffective))) {
		return errors.New("runtime mode evidence order invalid")
	}
	body, err := store.seal(evidence)
	if err != nil {
		return err
	}
	identity, err := store.publisher.ReplaceSelectorExactPrior(ctx, store.directory, "runtime-mode", store.identity, body)
	if err != nil {
		return err
	}
	store.current, store.identity = &evidence, &identity
	return nil
}

func (store *DurableRuntimeModeEvidenceStore) seal(evidence RuntimeModeEvidenceV1) ([]byte, error) {
	canonical, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, store.key[:])
	_, _ = mac.Write([]byte("cq/runtime-mode-evidence/v1\x00"))
	_, _ = mac.Write(canonical)
	return json.Marshal(runtimeModeEvidenceEnvelopeV1{Evidence: evidence, MAC: base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
}

func (store *DurableRuntimeModeEvidenceStore) open(body []byte) (RuntimeModeEvidenceV1, error) {
	var envelope runtimeModeEvidenceEnvelopeV1
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeModeEvidenceV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RuntimeModeEvidenceV1{}, errors.New("runtime mode evidence trailing data")
	}
	if err := envelope.Evidence.validate(); err != nil {
		return RuntimeModeEvidenceV1{}, err
	}
	canonical, err := json.Marshal(envelope.Evidence)
	if err != nil {
		return RuntimeModeEvidenceV1{}, err
	}
	provided, err := base64.RawURLEncoding.Strict().DecodeString(envelope.MAC)
	if err != nil {
		return RuntimeModeEvidenceV1{}, err
	}
	mac := hmac.New(sha256.New, store.key[:])
	_, _ = mac.Write([]byte("cq/runtime-mode-evidence/v1\x00"))
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return RuntimeModeEvidenceV1{}, errors.New("runtime mode evidence authentication failed")
	}
	return envelope.Evidence, nil
}
