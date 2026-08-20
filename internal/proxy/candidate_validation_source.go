package proxy

import (
	"errors"
	"sort"
)

var errCandidateSourceDependencyUnavailable = errors.New("candidate source dependency is not durable")

type CandidateValidationSourceKind string

const (
	CandidateSourceIngress       CandidateValidationSourceKind = "synthetic_ingress"
	CandidateSourceMaterialised  CandidateValidationSourceKind = "materialised_request"
	CandidateSourceExpectedReply CandidateValidationSourceKind = "expected_reply"
)

type CandidateValidationSourceV1 struct {
	SchemaVersion   int                           `json:"schema_version"`
	RunID           string                        `json:"run_id"`
	Kind            CandidateValidationSourceKind `json:"kind"`
	Dependencies    []string                      `json:"dependencies,omitempty"`
	CatalogueDigest string                        `json:"catalogue_digest"`
	SyntheticDigest string                        `json:"synthetic_digest,omitempty"`
	MAC             string                        `json:"mac,omitempty"`
}

func validateCandidateValidationSource(source CandidateValidationSourceV1, available map[string]candidateStoredSource) error {
	if source.SchemaVersion != 1 || source.RunID == "" || validateAuthorityEntryName("run-"+source.RunID) != nil || !lowerHexDigest(source.CatalogueDigest) {
		return errors.New("invalid candidate validation source")
	}
	switch source.Kind {
	case CandidateSourceIngress:
		if len(source.Dependencies) != 0 {
			return errors.New("synthetic ingress cannot have dependencies")
		}
	case CandidateSourceMaterialised, CandidateSourceExpectedReply:
		if len(source.Dependencies) == 0 {
			return errors.New("candidate source dependency unavailable")
		}
	default:
		return errors.New("unknown candidate validation source kind")
	}
	dependencies := append([]string(nil), source.Dependencies...)
	sort.Strings(dependencies)
	for index, dependency := range dependencies {
		if !lowerHexDigest(dependency) {
			return errors.New("invalid candidate source dependency")
		}
		if index > 0 && dependency == dependencies[index-1] {
			return errors.New("duplicate candidate source dependency")
		}
		stored, exists := available[dependency]
		if !exists || stored.source.RunID != source.RunID {
			return errCandidateSourceDependencyUnavailable
		}
	}
	return nil
}
