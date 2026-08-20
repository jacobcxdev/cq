package proxy

import (
	"errors"
	"path/filepath"
)

const CandidateControllerIPC = "controller_ipc"

type CandidateInheritedDescriptor struct {
	FD      int    `json:"fd"`
	Purpose string `json:"purpose"`
}

// CandidateLaunchSpec is inspected in the trusted controller before spawn.
// Sensitive fields are rejection sentinels and are never serialised to a grant.
type CandidateLaunchSpec struct {
	CandidateRoot  string
	Inherited      []CandidateInheritedDescriptor
	DirectNetwork  bool
	ExternalPaths  []string
	Executable     string
	ProviderBearer []byte
	ProviderOrigin string
	AuthorityKey   []byte
}

func ValidateCandidateConfinement(spec CandidateLaunchSpec) error {
	if spec.CandidateRoot == "" || !filepath.IsAbs(spec.CandidateRoot) {
		return errors.New("candidate root unavailable")
	}
	if len(spec.ProviderBearer) != 0 || spec.ProviderOrigin != "" || len(spec.AuthorityKey) != 0 {
		return errors.New("candidate credential or provider authority denied")
	}
	if spec.DirectNetwork || len(spec.ExternalPaths) != 0 || spec.Executable != "" {
		return errors.New("candidate external authority denied")
	}
	if len(spec.Inherited) != 1 || spec.Inherited[0].FD < 3 || spec.Inherited[0].Purpose != CandidateControllerIPC {
		return errors.New("candidate inherited descriptor denied")
	}
	if !candidatePlatformConfinementAvailable() {
		return errors.New("candidate platform confinement unavailable")
	}
	return nil
}
