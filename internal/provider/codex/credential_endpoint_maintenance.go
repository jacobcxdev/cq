package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

const legacyCredentialEndpointSnapshotVersion = 1
const legacyCredentialEndpointProofMaxBytes = 16 << 10

var (
	ErrLegacyCredentialEndpointNotRefused            = errors.New("legacy credential endpoint is not an exact refused socket")
	ErrLegacyCredentialEndpointArtifacts             = errors.New("legacy credential endpoint has coordination artifacts")
	ErrCredentialEndpointMaintenancePending          = errors.New("credential endpoint maintenance is pending")
	ErrCredentialEndpointMaintenanceUnsupported      = errors.New("credential endpoint maintenance unsupported")
	ErrCredentialEndpointMaintenanceDrainRequired    = errors.New("credential endpoint stopped-and-drained authority required")
	ErrCredentialEndpointMaintenanceSnapshotChanged  = errors.New("legacy credential endpoint snapshot changed")
	ErrCredentialEndpointMaintenanceTicketMismatch   = errors.New("credential endpoint maintenance ticket mismatch")
	ErrCredentialEndpointMaintenanceConflict         = errors.New("credential endpoint maintenance conflict")
	ErrCredentialEndpointMaintenanceCommitDeprecated = errors.New("credential endpoint maintenance commit is unavailable; activate then finalise")
	ErrCredentialEndpointMaintenanceVerifierRequired = errors.New("credential endpoint maintenance finalise verifier required")
	ErrCredentialEndpointMaintenanceVerification     = errors.New("credential endpoint maintenance finalise verification failed")
)

type LegacyCredentialEndpointState string

const LegacyCredentialEndpointRefused LegacyCredentialEndpointState = "legacy_refused"

type LegacyCredentialEndpointIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint64 `json:"uid"`
	Links  uint64 `json:"links"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
}

type LegacyCredentialEndpointSnapshot struct {
	Version   int                              `json:"version"`
	Path      string                           `json:"path"`
	State     LegacyCredentialEndpointState    `json:"state"`
	Directory LegacyCredentialEndpointIdentity `json:"directory"`
	Socket    LegacyCredentialEndpointIdentity `json:"socket"`
}

type DrainAuthority interface {
	AssertStoppedAndDrained(context.Context, string) error
}

type DrainAuthorityFunc func(context.Context, string) error

func (assert DrainAuthorityFunc) AssertStoppedAndDrained(ctx context.Context, path string) error {
	if assert == nil {
		return ErrCredentialEndpointMaintenanceDrainRequired
	}
	return assert(ctx, path)
}

// LegacyMaintenanceFinaliseVerification binds a runtime verification to the
// exact activated receipt and current credential owner without exposing the
// opaque maintenance ticket to verifier logs or status surfaces.
type LegacyMaintenanceFinaliseVerification struct {
	TicketHash      string
	OwnerGeneration string
}

// LegacyMaintenanceFinaliseLease retains the runtime authority proved by a
// finalise verifier. Release must be safe to call more than once.
type LegacyMaintenanceFinaliseLease interface {
	Release()
}

// LegacyMaintenanceFinaliseVerifier acquires a lease over the current
// candidate/runtime health tuple from inside the exact live owner operation
// immediately before finalise becomes irreversible. Implementations remain
// outside the provider and may inspect runtime-specific readiness without
// coupling it here. A successful acquisition must already be linearised
// against runtime teardown; the provider retains that authority through the
// durable Finalising receipt and then releases it.
type LegacyMaintenanceFinaliseVerifier interface {
	AcquireLegacyMaintenanceFinalise(context.Context, LegacyMaintenanceFinaliseVerification) (LegacyMaintenanceFinaliseLease, error)
}

type LegacyMaintenanceFinaliseVerifierFunc func(context.Context, LegacyMaintenanceFinaliseVerification) (LegacyMaintenanceFinaliseLease, error)

func (verify LegacyMaintenanceFinaliseVerifierFunc) AcquireLegacyMaintenanceFinalise(ctx context.Context, proof LegacyMaintenanceFinaliseVerification) (LegacyMaintenanceFinaliseLease, error) {
	if verify == nil {
		return nil, ErrCredentialEndpointMaintenanceVerifierRequired
	}
	return verify(ctx, proof)
}

type CredentialEndpointMaintenanceState string

const (
	CredentialEndpointMaintenancePrepared    CredentialEndpointMaintenanceState = "prepared"
	CredentialEndpointMaintenanceQuarantined CredentialEndpointMaintenanceState = "quarantined"
	CredentialEndpointMaintenanceActivating  CredentialEndpointMaintenanceState = "activating"
	CredentialEndpointMaintenanceActivated   CredentialEndpointMaintenanceState = "activated"
	CredentialEndpointMaintenanceFinalising  CredentialEndpointMaintenanceState = "finalising"
	CredentialEndpointMaintenanceCommitting  CredentialEndpointMaintenanceState = "committing"
	CredentialEndpointMaintenanceRollingBack CredentialEndpointMaintenanceState = "rolling_back"
	CredentialEndpointMaintenanceRolledBack  CredentialEndpointMaintenanceState = "rolled_back"
	CredentialEndpointMaintenanceCommitted   CredentialEndpointMaintenanceState = "committed"
)

type LegacyCredentialEndpointTransitionTicket struct {
	Version        int                              `json:"version"`
	ID             string                           `json:"id"`
	Path           string                           `json:"path"`
	Directory      LegacyCredentialEndpointIdentity `json:"directory"`
	Socket         LegacyCredentialEndpointIdentity `json:"socket"`
	Lock           LegacyCredentialEndpointIdentity `json:"lock"`
	QuarantineName string                           `json:"quarantine_name"`
}

type LegacyCredentialEndpointTransitionStatus struct {
	State  CredentialEndpointMaintenanceState       `json:"state"`
	Ticket LegacyCredentialEndpointTransitionTicket `json:"ticket"`
}

type legacyCredentialEndpointTransitionImplementation interface {
	Ticket() LegacyCredentialEndpointTransitionTicket
	State() CredentialEndpointMaintenanceState
	Activate(context.Context) error
	Commit(context.Context) error
	Rollback(context.Context) error
	Close() error
}

type LegacyCredentialEndpointTransition struct {
	implementation legacyCredentialEndpointTransitionImplementation
}

func (transition *LegacyCredentialEndpointTransition) Ticket() LegacyCredentialEndpointTransitionTicket {
	if transition == nil || transition.implementation == nil {
		return LegacyCredentialEndpointTransitionTicket{}
	}
	return transition.implementation.Ticket()
}

func (transition *LegacyCredentialEndpointTransition) State() CredentialEndpointMaintenanceState {
	if transition == nil || transition.implementation == nil {
		return ""
	}
	return transition.implementation.State()
}

// Activate opens the reversible candidate smoke window while retaining the
// exact quarantined legacy endpoint for rollback.
func (transition *LegacyCredentialEndpointTransition) Activate(ctx context.Context) error {
	if transition == nil || transition.implementation == nil {
		return ErrCredentialEndpointMaintenanceUnsupported
	}
	return transition.implementation.Activate(ctx)
}

func (transition *LegacyCredentialEndpointTransition) Commit(ctx context.Context) error {
	if transition == nil || transition.implementation == nil {
		return ErrCredentialEndpointMaintenanceUnsupported
	}
	return transition.implementation.Commit(ctx)
}

func (transition *LegacyCredentialEndpointTransition) Rollback(ctx context.Context) error {
	if transition == nil || transition.implementation == nil {
		return ErrCredentialEndpointMaintenanceUnsupported
	}
	return transition.implementation.Rollback(ctx)
}

func (transition *LegacyCredentialEndpointTransition) Close() error {
	if transition == nil || transition.implementation == nil {
		return nil
	}
	return transition.implementation.Close()
}

func ParseLegacyCredentialEndpointSnapshot(data []byte) (LegacyCredentialEndpointSnapshot, error) {
	if len(data) == 0 || len(data) > legacyCredentialEndpointProofMaxBytes {
		return LegacyCredentialEndpointSnapshot{}, errors.New("invalid legacy credential endpoint snapshot size")
	}
	fields, err := decodeCredentialMaintenanceObject(data)
	if err != nil {
		return LegacyCredentialEndpointSnapshot{}, fmt.Errorf("parse legacy credential endpoint snapshot: %w", err)
	}
	if err := requireCredentialMaintenanceFields(fields, "version", "path", "state", "directory", "socket"); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	for _, name := range []string{"directory", "socket"} {
		identityFields, err := decodeCredentialMaintenanceObject(fields[name])
		if err != nil {
			return LegacyCredentialEndpointSnapshot{}, fmt.Errorf("parse legacy credential endpoint %s proof: %w", name, err)
		}
		if err := requireCredentialMaintenanceFields(identityFields, "device", "inode", "uid", "links", "type", "mode"); err != nil {
			return LegacyCredentialEndpointSnapshot{}, fmt.Errorf("parse legacy credential endpoint %s proof: %w", name, err)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot LegacyCredentialEndpointSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return LegacyCredentialEndpointSnapshot{}, fmt.Errorf("parse legacy credential endpoint snapshot: %w", err)
	}
	if err := requireCredentialMaintenanceEOF(decoder); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	if err := snapshot.validate(); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	return snapshot, nil
}

func (snapshot LegacyCredentialEndpointSnapshot) validate() error {
	if snapshot.Version != legacyCredentialEndpointSnapshotVersion || snapshot.State != LegacyCredentialEndpointRefused {
		return errors.New("invalid legacy credential endpoint snapshot version or state")
	}
	if snapshot.Path == "" || !filepath.IsAbs(snapshot.Path) || filepath.Clean(snapshot.Path) != snapshot.Path {
		return errors.New("invalid legacy credential endpoint snapshot path")
	}
	if filepath.Base(snapshot.Path) == "." || filepath.Base(snapshot.Path) == string(filepath.Separator) {
		return errors.New("invalid legacy credential endpoint snapshot name")
	}
	if err := snapshot.Directory.validate("directory", 0o700, false); err != nil {
		return err
	}
	return snapshot.Socket.validate("socket", 0o600, true)
}

func (identity LegacyCredentialEndpointIdentity) validate(kind string, mode uint32, singleLink bool) error {
	if identity.Inode == 0 || identity.Type != kind || identity.Mode != mode || identity.Links == 0 || !legacyCredentialEndpointIdentityOwnerIsCurrent(identity.UID) {
		return fmt.Errorf("invalid legacy credential endpoint %s identity", kind)
	}
	if singleLink && identity.Links != 1 {
		return fmt.Errorf("invalid legacy credential endpoint %s link count", kind)
	}
	return nil
}

func decodeCredentialMaintenanceObject(data []byte) (map[string]json.RawMessage, error) {
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
			return nil, fmt.Errorf("duplicate JSON field %q", name)
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
	if err := requireCredentialMaintenanceEOF(decoder); err != nil {
		return nil, err
	}
	return fields, nil
}

func requireCredentialMaintenanceFields(fields map[string]json.RawMessage, required ...string) error {
	want := make(map[string]struct{}, len(required))
	for _, name := range required {
		want[name] = struct{}{}
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing JSON field %q", name)
		}
	}
	for name := range fields {
		if _, ok := want[name]; !ok {
			return fmt.Errorf("unknown JSON field %q", name)
		}
	}
	return nil
}

func requireCredentialMaintenanceEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON data")
		}
		return err
	}
	return nil
}
