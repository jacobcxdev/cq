//go:build !darwin && !linux

package main

import (
	"errors"
	"runtime"
)

func installedHTTPValidationPlatformError() error {
	if runtime.GOOS == "windows" {
		return errors.New("installed Windows HTTP validation requires a native Codex client confined to the validation listener and broker")
	}
	return errors.New("installed HTTP validation is unavailable on this platform")
}

func resolveInstalledHTTPValidationService(string) (installedHTTPValidationServiceBinding, error) {
	return installedHTTPValidationServiceBinding{}, installedHTTPValidationPlatformError()
}

func validateInstalledHTTPValidationCandidate(int) (installedHTTPValidationCandidateAuthority, error) {
	return installedHTTPValidationCandidateAuthority{}, installedHTTPValidationPlatformError()
}

func restartInstalledHTTPValidationCandidate(string) error {
	return installedHTTPValidationPlatformError()
}

func cleanupInstalledHTTPValidationCandidate() error { return nil }
