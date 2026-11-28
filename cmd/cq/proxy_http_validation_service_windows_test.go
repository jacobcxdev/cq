//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestWindowsHTTPValidationUnavailablePreservesLiveState(t *testing.T) {
	oldStore := defaultInstalledHTTPValidationRequestStoreFn
	oldRestart := restartInstalledHTTPValidationCandidateFn
	oldInvalidate := invalidateInstalledHTTPValidationMarkerFn
	t.Cleanup(func() {
		defaultInstalledHTTPValidationRequestStoreFn = oldStore
		restartInstalledHTTPValidationCandidateFn = oldRestart
		invalidateInstalledHTTPValidationMarkerFn = oldInvalidate
	})
	defaultInstalledHTTPValidationRequestStoreFn = func() (installedHTTPValidationRequestStore, error) {
		return installedHTTPValidationRequestStore{
			resolveService: func(string) (installedHTTPValidationServiceBinding, error) {
				t.Fatal("unsupported validation attempted to bind a service")
				return installedHTTPValidationServiceBinding{}, nil
			},
		}, nil
	}
	restartInstalledHTTPValidationCandidateFn = func(string) error {
		t.Fatal("unsupported validation tried to restart a service")
		return nil
	}
	invalidateInstalledHTTPValidationMarkerFn = func() error {
		t.Fatal("unsupported validation tried to invalidate readiness")
		return nil
	}
	err := runDefaultProxyValidateHTTP([]string{"--port", "29280"}, "test-build")
	if err == nil || !strings.Contains(err.Error(), "requires a native Codex client confined to the validation listener and broker") {
		t.Fatalf("missing Windows confinement error: %v", err)
	}
}
