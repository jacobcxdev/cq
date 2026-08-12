package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestRunProxyValidateHTTPRequestsConfiguredCandidateService(t *testing.T) {
	oldLoad := loadProxyStartConfigFn
	oldStore := defaultInstalledHTTPValidationRequestStoreFn
	oldRestart := restartInstalledHTTPValidationCandidateFn
	oldValidateCandidate := validateInstalledHTTPValidationCandidateFn
	t.Cleanup(func() {
		loadProxyStartConfigFn = oldLoad
		defaultInstalledHTTPValidationRequestStoreFn = oldStore
		restartInstalledHTTPValidationCandidateFn = oldRestart
		validateInstalledHTTPValidationCandidateFn = oldValidateCandidate
	})
	loadProxyStartConfigFn = func() (*proxy.Config, error) {
		return &proxy.Config{Port: 29280}, nil
	}
	path := filepath.Join(t.TempDir(), "private", "request.json")
	defaultInstalledHTTPValidationRequestStoreFn = func() (installedHTTPValidationRequestStore, error) {
		return installedHTTPValidationTestStore(t, path, time.Now().UTC(), validInstalledHTTPValidationServiceBinding()), nil
	}
	restarts := 0
	restartInstalledHTTPValidationCandidateFn = func(label string) error {
		restarts++
		if label != "homebrew.mxcl.cq" {
			t.Fatalf("candidate label = %q", label)
		}
		return nil
	}
	validateInstalledHTTPValidationCandidateFn = func(port int) (installedHTTPValidationCandidateAuthority, error) {
		if port != 29280 {
			t.Fatalf("candidate port = %d", port)
		}
		return installedHTTPValidationCandidateAuthority{binding: validInstalledHTTPValidationServiceBinding(), pid: 4242}, nil
	}

	if err := runProxy([]string{"validate-http", "--port", "29280"}); err != nil {
		t.Fatalf("runProxy validate-http: %v", err)
	}
	if restarts != 1 {
		t.Fatalf("candidate restarts = %d, want 1", restarts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("candidate validation request: %v", err)
	}
}

func TestRunProxyValidateHTTPDoesNotRestartAbsentCandidate(t *testing.T) {
	oldLoad := loadProxyStartConfigFn
	oldStore := defaultInstalledHTTPValidationRequestStoreFn
	oldRestart := restartInstalledHTTPValidationCandidateFn
	oldValidateCandidate := validateInstalledHTTPValidationCandidateFn
	t.Cleanup(func() {
		loadProxyStartConfigFn = oldLoad
		defaultInstalledHTTPValidationRequestStoreFn = oldStore
		restartInstalledHTTPValidationCandidateFn = oldRestart
		validateInstalledHTTPValidationCandidateFn = oldValidateCandidate
	})
	loadProxyStartConfigFn = func() (*proxy.Config, error) { return &proxy.Config{Port: 29280}, nil }
	validateInstalledHTTPValidationCandidateFn = func(int) (installedHTTPValidationCandidateAuthority, error) {
		return installedHTTPValidationCandidateAuthority{}, errors.New("candidate unavailable")
	}
	defaultInstalledHTTPValidationRequestStoreFn = func() (installedHTTPValidationRequestStore, error) {
		return installedHTTPValidationTestStore(t, filepath.Join(t.TempDir(), "request.json"), time.Now(), validInstalledHTTPValidationServiceBinding()), nil
	}
	restartInstalledHTTPValidationCandidateFn = func(string) error {
		t.Fatal("absent candidate restarted service")
		return nil
	}
	if err := runDefaultProxyValidateHTTP([]string{"--port", "29280"}, "cq-build-42"); err == nil {
		t.Fatal("absent candidate accepted")
	}
}

func TestRunProxyValidateHTTPCancelsWhenCandidateAuthorityChanges(t *testing.T) {
	oldLoad := loadProxyStartConfigFn
	oldStore := defaultInstalledHTTPValidationRequestStoreFn
	oldRestart := restartInstalledHTTPValidationCandidateFn
	oldValidateCandidate := validateInstalledHTTPValidationCandidateFn
	oldInvalidate := invalidateInstalledHTTPValidationMarkerFn
	t.Cleanup(func() {
		loadProxyStartConfigFn = oldLoad
		defaultInstalledHTTPValidationRequestStoreFn = oldStore
		restartInstalledHTTPValidationCandidateFn = oldRestart
		validateInstalledHTTPValidationCandidateFn = oldValidateCandidate
		invalidateInstalledHTTPValidationMarkerFn = oldInvalidate
	})
	loadProxyStartConfigFn = func() (*proxy.Config, error) { return &proxy.Config{Port: 29280}, nil }
	path := filepath.Join(t.TempDir(), "private", "request.json")
	binding := validInstalledHTTPValidationServiceBinding()
	defaultInstalledHTTPValidationRequestStoreFn = func() (installedHTTPValidationRequestStore, error) {
		return installedHTTPValidationTestStore(t, path, time.Now().UTC(), binding), nil
	}
	checks := 0
	validateInstalledHTTPValidationCandidateFn = func(int) (installedHTTPValidationCandidateAuthority, error) {
		checks++
		return installedHTTPValidationCandidateAuthority{binding: binding, pid: 4241 + checks}, nil
	}
	restartInstalledHTTPValidationCandidateFn = func(string) error {
		t.Fatal("changed candidate authority restarted service")
		return nil
	}
	invalidateInstalledHTTPValidationMarkerFn = func() error { return nil }

	if err := runDefaultProxyValidateHTTP([]string{"--port", "29280"}, "cq-build-42"); err == nil {
		t.Fatal("changed candidate authority accepted")
	}
	if checks != 2 {
		t.Fatalf("candidate authority checks = %d, want 2", checks)
	}
	assertNoPendingInstalledHTTPValidationRequest(t, path)
}

func TestRunProxyValidateHTTPRejectsLiveMissingOrMismatchedPort(t *testing.T) {
	oldLoad := loadProxyStartConfigFn
	t.Cleanup(func() { loadProxyStartConfigFn = oldLoad })
	loadProxyStartConfigFn = func() (*proxy.Config, error) { return &proxy.Config{Port: 29280}, nil }
	for _, args := range [][]string{nil, {"--port", "19280"}, {"--port", "29281"}} {
		if err := runDefaultProxyValidateHTTP(args, "cq-build-42"); err == nil {
			t.Fatalf("validate args %v unexpectedly accepted", args)
		}
	}
}

func TestRunProxyValidateHTTPHelpHasNoSideEffects(t *testing.T) {
	oldStore := defaultInstalledHTTPValidationRequestStoreFn
	t.Cleanup(func() { defaultInstalledHTTPValidationRequestStoreFn = oldStore })
	storeCalls := 0
	defaultInstalledHTTPValidationRequestStoreFn = func() (installedHTTPValidationRequestStore, error) {
		storeCalls++
		return installedHTTPValidationRequestStore{}, errors.New("must not resolve store")
	}

	if err := runProxy([]string{"validate-http", "--help"}); err != nil {
		t.Fatalf("runProxy validate-http --help: %v", err)
	}
	if storeCalls != 0 {
		t.Fatalf("request store calls = %d, want 0", storeCalls)
	}
}

func TestInstalledHTTPValidationStartupBranchRunsOnlyForConsumedRequest(t *testing.T) {
	cfg := &proxy.Config{Port: 19280}
	for _, test := range []struct {
		name        string
		present     bool
		consumeErr  error
		wantHandled bool
		wantErr     bool
		wantRuns    int
	}{
		{name: "absent", wantHandled: false},
		{name: "present", present: true, wantHandled: true, wantRuns: 1},
		{name: "consume failure", consumeErr: errors.New("invalid request"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runs := 0
			handled, err := runInstalledHTTPValidationStartupIfRequested(
				context.Background(), cfg, "cq-build-42",
				func(build string) (*installedHTTPValidationConsumedRequest, error) {
					if build != "cq-build-42" {
						t.Fatalf("consume build = %q", build)
					}
					if test.present {
						return &installedHTTPValidationConsumedRequest{}, test.consumeErr
					}
					return nil, test.consumeErr
				},
				func(_ context.Context, gotConfig *proxy.Config, build string, guard proxy.CodexInstalledHTTPValidationGuard) error {
					runs++
					if gotConfig != cfg || build != "cq-build-42" || guard == nil {
						t.Fatalf("startup args = (%p, %q), want (%p, cq-build-42)", gotConfig, build, cfg)
					}
					return nil
				},
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("startup branch error = %v, wantErr %t", err, test.wantErr)
			}
			if handled != test.wantHandled {
				t.Fatalf("startup branch handled = %t, want %t", handled, test.wantHandled)
			}
			if runs != test.wantRuns {
				t.Fatalf("startup runs = %d, want %d", runs, test.wantRuns)
			}
		})
	}
}

func TestRunProxyStartClaimsRequestAndInvalidatesMarkerBeforeConfigFailure(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "readiness.json")
	if err := os.WriteFile(markerPath, []byte("prior marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldConsume := consumeInstalledHTTPValidationStartupRequestFn
	oldInvalidate := invalidateInstalledHTTPValidationMarkerFn
	oldLoad := loadProxyStartConfigFn
	t.Cleanup(func() {
		consumeInstalledHTTPValidationStartupRequestFn = oldConsume
		invalidateInstalledHTTPValidationMarkerFn = oldInvalidate
		loadProxyStartConfigFn = oldLoad
	})
	order := make([]string, 0, 3)
	consumeInstalledHTTPValidationStartupRequestFn = func(string) (*installedHTTPValidationConsumedRequest, error) {
		order = append(order, "claim")
		return &installedHTTPValidationConsumedRequest{}, nil
	}
	invalidateInstalledHTTPValidationMarkerFn = func() error {
		order = append(order, "invalidate")
		return os.Remove(markerPath)
	}
	loadProxyStartConfigFn = func() (*proxy.Config, error) {
		order = append(order, "load")
		if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prior marker survived before config load: %v", err)
		}
		return nil, errors.New("corrupt proxy config")
	}

	if err := runProxyStart(proxyCommandOptions{}); err == nil {
		t.Fatal("config failure error = nil")
	}
	if got := strings.Join(order, ","); got != "claim,invalidate,load" {
		t.Fatalf("startup order = %q, want claim,invalidate,load", got)
	}
}

func TestClaimInstalledHTTPValidationStartupRequestInvalidatesOnlyExplicitAttempt(t *testing.T) {
	for _, test := range []struct {
		name          string
		consume       func(string) (*installedHTTPValidationConsumedRequest, error)
		wantErr       bool
		wantRemoved   bool
		invalidations int
	}{
		{
			name: "malformed or claim failure",
			consume: func(string) (*installedHTTPValidationConsumedRequest, error) {
				return nil, errors.New("malformed claimed request")
			},
			wantErr: true, wantRemoved: true, invalidations: 1,
		},
		{
			name: "no request",
			consume: func(string) (*installedHTTPValidationConsumedRequest, error) {
				return nil, nil
			},
			wantRemoved: false, invalidations: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			markerPath := filepath.Join(t.TempDir(), "readiness.json")
			if err := os.WriteFile(markerPath, []byte("prior marker"), 0o600); err != nil {
				t.Fatal(err)
			}
			invalidations := 0
			intent, err := claimInstalledHTTPValidationStartupRequest("cq-build-42", test.consume, func() error {
				invalidations++
				return os.Remove(markerPath)
			})
			if intent != nil || (err != nil) != test.wantErr {
				t.Fatalf("claim result = (%#v, %v), wantErr %t", intent, err, test.wantErr)
			}
			if invalidations != test.invalidations {
				t.Fatalf("invalidations = %d, want %d", invalidations, test.invalidations)
			}
			_, statErr := os.Stat(markerPath)
			if errors.Is(statErr, os.ErrNotExist) != test.wantRemoved {
				t.Fatalf("marker stat = %v, wantRemoved %t", statErr, test.wantRemoved)
			}
		})
	}
}

func TestRunProxyInstalledHTTPValidationStartupUsesSoleProductionEntrypoint(t *testing.T) {
	oldRun := runCodexInstalledHTTPValidationFn
	oldBuild := installedHTTPValidationClientBuildFn
	t.Cleanup(func() {
		runCodexInstalledHTTPValidationFn = oldRun
		installedHTTPValidationClientBuildFn = oldBuild
	})
	installedHTTPValidationClientBuildFn = func() string { return "0.147.0-alpha.6.5" }
	calls := 0
	runCodexInstalledHTTPValidationFn = func(_ context.Context, cfg *proxy.Config, cqBuild, clientBuild string, guard proxy.CodexInstalledHTTPValidationGuard) error {
		calls++
		if cfg.Port != 19280 || cqBuild != "cq-build-42" || clientBuild != "0.147.0-alpha.6.5" || guard == nil {
			t.Fatalf("validation args = (%d, %q, %q)", cfg.Port, cqBuild, clientBuild)
		}
		return nil
	}

	if err := runProxyInstalledHTTPValidationStartup(context.Background(), &proxy.Config{Port: 19280}, "cq-build-42", testInstalledHTTPValidationGuard{}); err != nil {
		t.Fatalf("run installed HTTP validation startup: %v", err)
	}
	if calls != 1 {
		t.Fatalf("validation calls = %d, want 1", calls)
	}
}

func TestRunProxyInstalledHTTPValidationStartupEntersSoleEntrypointAfterBuildFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		resolve func() string
	}{
		{name: "empty", resolve: func() string { return "" }},
		{name: "panic", resolve: func() string { panic("client build resolver panic") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldRun := runCodexInstalledHTTPValidationFn
			oldBuild := installedHTTPValidationClientBuildFn
			t.Cleanup(func() {
				runCodexInstalledHTTPValidationFn = oldRun
				installedHTTPValidationClientBuildFn = oldBuild
			})
			installedHTTPValidationClientBuildFn = test.resolve
			calls := 0
			runCodexInstalledHTTPValidationFn = func(_ context.Context, _ *proxy.Config, _, clientBuild string, guard proxy.CodexInstalledHTTPValidationGuard) error {
				calls++
				if clientBuild != "" || guard == nil {
					t.Fatalf("client build = %q, want empty fail-closed input", clientBuild)
				}
				return errors.New("validation rejected")
			}

			if err := runProxyInstalledHTTPValidationStartup(context.Background(), &proxy.Config{Port: 19280}, "cq-build-42", testInstalledHTTPValidationGuard{}); err == nil {
				t.Fatal("build failure returned nil error")
			}
			if calls != 1 {
				t.Fatalf("validation calls = %d, want 1 invalidating entrypoint call", calls)
			}
		})
	}
}

type testInstalledHTTPValidationGuard struct{}

func (testInstalledHTTPValidationGuard) Acquire() (func(), error) { return func() {}, nil }

func readInstalledHTTPValidationTestRequest(path string) (installedHTTPValidationRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return installedHTTPValidationRequest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var request installedHTTPValidationRequest
	err = decoder.Decode(&request)
	return request, err
}
