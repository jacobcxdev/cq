//go:build !darwin && !linux

package main

import "errors"

func resolveInstalledHTTPValidationService(string) (installedHTTPValidationServiceBinding, error) {
	return installedHTTPValidationServiceBinding{}, errors.New("installed HTTP validation is only supported on macOS")
}

func validateInstalledHTTPValidationCandidate(int) (installedHTTPValidationCandidateAuthority, error) {
	return installedHTTPValidationCandidateAuthority{}, errors.New("installed HTTP validation is only supported on macOS")
}

func restartInstalledHTTPValidationCandidate(string) error {
	return errors.New("installed HTTP validation is only supported on macOS")
}

func cleanupInstalledHTTPValidationCandidate() {}
