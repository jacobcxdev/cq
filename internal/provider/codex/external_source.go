package codex

import (
	"context"
	"errors"
	"time"
)

var (
	ErrExternalUnavailable = errors.New("external credential source unavailable")
	ErrExternalInvalid     = errors.New("external credential source invalid")
	ErrExternalUnsafePath  = errors.New("external credential path unsafe")
)

type ExternalCandidateRef struct {
	Source   string
	RecordID string
	Revision Revision
}

type ExternalCandidate struct {
	Ref             ExternalCandidateRef
	Identity        AccountIdentity
	AccessExpiresAt time.Time
	Routable        bool
}

type ExternalCredentialSource interface {
	Name() string
	List(context.Context) ([]ExternalCandidate, error)
	Resolve(context.Context, ExternalCandidateRef) (CredentialMaterial, error)
}
