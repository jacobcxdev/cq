//go:build !darwin

package main

import "context"

// The Linux owned exercise is synchronous and creates its own controller; it
// does not attest an already installed asynchronous candidate service.
func validateCanonicalHTTPValidationCandidate(context.Context, int) (installedHTTPValidationCandidateAuthority, error) {
	return installedHTTPValidationCandidateAuthority{}, errValidationCandidateUnavailable
}
func resolveCanonicalHTTPValidationService(context.Context, string) (installedHTTPValidationServiceBinding, error) {
	return installedHTTPValidationServiceBinding{}, errValidationCandidateUnavailable
}
func restartCanonicalHTTPValidationCandidate(context.Context, string) error {
	return errValidationCandidateUnavailable
}
