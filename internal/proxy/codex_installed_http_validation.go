package proxy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func newCodexInstalledHTTPValidationToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errCodexInstalledListenerAcceptance
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	clearBytes(raw[:])
	if !validCodexInstalledHTTPValidationToken(token) {
		return "", errCodexInstalledListenerAcceptance
	}
	return token, nil
}

// NewCodexInstalledHTTPValidationToken creates one credential for an isolated
// installed-client validation runtime. Callers must keep it process-local or
// in owner-only temporary storage until the runtime exits.
func NewCodexInstalledHTTPValidationToken() (string, error) {
	return newCodexInstalledHTTPValidationToken()
}

// RunCodexInstalledHTTPValidation is the sole production entrypoint for the
// explicit one-shot installed HTTP validation startup. It accepts no evidence
// or marker and keeps marker construction/publication private under the held
// process and serving authority.
func RunCodexInstalledHTTPValidation(
	ctx context.Context,
	cfg *Config,
	cqBuild string,
	clientBuild string,
	guard CodexInstalledHTTPValidationGuard,
) (returnErr error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return err
	}
	return runCodexInstalledHTTPValidationWithDependencies(ctx, cfg, cqBuild, clientBuild, codexInstalledHTTPValidationDependencies{
		markerDir:  paths.StateDir,
		invalidate: invalidateCodexHTTPReadinessMarkerDurably,
		run:        runCodexInstalledHTTPValidationListener,
		guard:      guard,
	})
}

// CodexInstalledHTTPValidationRuntime exposes one validation handler and its
// listener-bound proof authority to an external owned-runtime supervisor.
type CodexInstalledHTTPValidationRuntime struct {
	Handler           http.Handler
	ServingAttestor   *ServingAttestor
	StartupValidation func(context.Context, CodexHTTPStartupValidationRuntime, func() error) error
	AbortUnexpected   func() <-chan struct{}
}

// RunCodexInstalledHTTPValidationRuntime keeps validation state in the
// supervisor while an exact worker process carries every data-plane request.
func RunCodexInstalledHTTPValidationRuntime(
	ctx context.Context,
	cfg *Config,
	cqBuild string,
	clientBuild string,
	guard CodexInstalledHTTPValidationGuard,
	localToken string,
	serve func(context.Context, CodexInstalledHTTPValidationRuntime) error,
) (returnErr error) {
	if !validCodexInstalledHTTPValidationToken(localToken) {
		return codexInstalledHTTPValidationStageError("local authority")
	}
	if serve == nil {
		return codexInstalledHTTPValidationStageError("runtime serve")
	}
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return err
	}
	return runCodexInstalledHTTPValidationWithDependencies(ctx, cfg, cqBuild, clientBuild, codexInstalledHTTPValidationDependencies{
		markerDir: paths.StateDir, invalidate: invalidateCodexHTTPReadinessMarkerDurably,
		run: func(ctx context.Context, cfg *Config, cqBuild, clientBuild, markerDir string, guard CodexInstalledHTTPValidationGuard) error {
			return runCodexInstalledHTTPValidationListenerOn(ctx, cfg, cqBuild, clientBuild, markerDir, guard, localToken, serve)
		},
		guard: guard,
	})
}

// InvalidateDefaultCodexHTTPReadinessMarker removes any marker that could
// have raced ahead of a failed explicit installed-validation request.
func InvalidateDefaultCodexHTTPReadinessMarker() error {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return err
	}
	return invalidateCodexHTTPReadinessMarkerDurably(paths.StateDir)
}

type codexInstalledHTTPValidationDependencies struct {
	markerDir  string
	invalidate func(string) error
	run        func(context.Context, *Config, string, string, string, CodexInstalledHTTPValidationGuard) error
	guard      CodexInstalledHTTPValidationGuard
}

func runCodexInstalledHTTPValidationWithDependencies(
	ctx context.Context,
	cfg *Config,
	cqBuild string,
	clientBuild string,
	dependencies codexInstalledHTTPValidationDependencies,
) (returnErr error) {
	markerDir := dependencies.markerDir
	if !filepath.IsAbs(markerDir) || dependencies.invalidate == nil {
		return errCodexInstalledListenerAcceptance
	}
	defer func() {
		if recover() != nil {
			returnErr = errCodexInstalledListenerAcceptance
		}
		if returnErr != nil {
			returnErr = errors.Join(returnErr, invalidateCodexInstalledHTTPReadinessSafely(dependencies.invalidate, markerDir))
		}
	}()
	if err := invalidateCodexInstalledHTTPReadinessSafely(dependencies.invalidate, markerDir); err != nil {
		return fmt.Errorf("invalidate prior Codex HTTP readiness marker: %w", err)
	}
	if ctx == nil || ctx.Err() != nil || cfg == nil || strings.TrimSpace(cqBuild) == "" ||
		clientBuild != strings.TrimSpace(clientBuild) || !codexInstalledHTTPClientBuildPattern.MatchString(clientBuild) || dependencies.run == nil || dependencies.guard == nil {
		return errCodexInstalledListenerAcceptance
	}
	return dependencies.run(ctx, cfg, cqBuild, clientBuild, markerDir, dependencies.guard)
}

func invalidateCodexInstalledHTTPReadinessSafely(invalidate func(string) error, markerDir string) (returnErr error) {
	defer func() {
		if recover() != nil {
			returnErr = errCodexInstalledListenerAcceptance
		}
	}()
	return invalidate(markerDir)
}

func runCodexInstalledHTTPValidationListener(
	ctx context.Context,
	cfg *Config,
	cqBuild string,
	clientBuild string,
	markerDir string,
	guard CodexInstalledHTTPValidationGuard,
) (returnErr error) {
	return runCodexInstalledHTTPValidationListenerOn(ctx, cfg, cqBuild, clientBuild, markerDir, guard, "", nil)
}

func runCodexInstalledHTTPValidationListenerOn(
	ctx context.Context,
	cfg *Config,
	cqBuild string,
	clientBuild string,
	markerDir string,
	guard CodexInstalledHTTPValidationGuard,
	localToken string,
	runtimeServe func(context.Context, CodexInstalledHTTPValidationRuntime) error,
) (returnErr error) {
	if localToken == "" {
		var err error
		localToken, err = newCodexInstalledHTTPValidationToken()
		if err != nil {
			return codexInstalledHTTPValidationStageError("local authority")
		}
	} else if !validCodexInstalledHTTPValidationToken(localToken) {
		return codexInstalledHTTPValidationStageError("local authority")
	}
	corpus, err := loadCodexStage11CorpusBuildManifest(cqBuild, codexStage11CorpusBuildProvenanceSHA256)
	if err != nil {
		return codexInstalledHTTPValidationStageError("build provenance")
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(ctx)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, core.close()) }()
	routes, err := newCodexInstalledHTTPRouteAudit(clientBuild, localToken)
	if err != nil {
		return codexInstalledHTTPValidationStageError("route audit")
	}
	protectedPaths, err := defaultCodexInstalledHTTPProtectedPaths(markerDir)
	if err != nil {
		return codexInstalledHTTPValidationStageError("protected authority")
	}
	servingAttestor := NewServingAttestor()
	required, _ := DefaultCodexRoutingRequirements(cqBuild, clientBuild)
	validationFor := func(beforeCommit func() error) CodexHTTPStartupValidationFunc {
		return CodexHTTPStartupValidationFunc(func(validationCtx context.Context, runtime CodexHTTPStartupValidationRuntime) error {
			if runtime.ServingAttestor != servingAttestor {
				return codexInstalledHTTPValidationStageError("serving authority")
			}
			authority, err := newCodexInstalledListenerProcessAuthority(validationCtx, codexInstalledListenerProcessAuthorityConfig{
				cqBuild:         cqBuild,
				clientBuild:     clientBuild,
				listenerAddress: runtime.ListenerAddress,
				servingAttestor: runtime.ServingAttestor,
				nativeHTTP:      core.nativeHTTPHandler(),
			})
			if err != nil {
				return codexInstalledHTTPValidationStageError("process authority")
			}
			outcome := &codexInstalledHTTPClientOutcome{}
			installedClient, err := newCodexInstalledHTTPClientExercise(
				runtime.ListenerAddress, authority.client.baseline, localToken, osCodexAcceptanceRunner{}, outcome,
			)
			if err != nil {
				return codexInstalledHTTPValidationStageError("client setup")
			}
			syntheticTraffic, err := core.installedListenerExercise(runtime.ListenerAddress, localToken)
			if err != nil {
				return codexInstalledHTTPValidationStageError("synthetic setup")
			}
			exercise := &codexInstalledHTTPCompositeExercise{first: installedClient, second: syntheticTraffic}
			audit := newCodexInstalledHTTPAuditAuthority(codexInstalledHTTPAuditAuthorityConfig{
				routes:         routes,
				client:         outcome,
				protectedPaths: protectedPaths,
				privacyRoot:    core.tempRoot,
				privacyNeedles: codexInstalledHTTPValidationPrivacyNeedles(localToken),
			})
			harness := &codexInstalledListenerHarness{dependencies: codexInstalledListenerHarnessDependencies{
				authority:   authority,
				clientBuild: authority,
				exercise:    exercise,
				audit:       audit,
				quiesce:     core.nativeHTTPHandler(),
				corpus:      corpus,
				guard:       guard,
				runtime:     &codexProcessRuntimeObservability,
				admissions:  core,
			}}
			commit := func(marker CodexReadinessMarker) error { return saveCodexHTTPReadinessMarkerDurably(markerDir, marker) }
			if beforeCommit == nil {
				_, err = harness.RunAndCommit(validationCtx, required, commit)
			} else {
				_, err = harness.RunVerifyAndCommit(validationCtx, required, beforeCommit, commit)
			}
			if err != nil {
				return err
			}
			return err
		})
	}
	validation := validationFor(nil)

	validationConfig := *cfg
	validationConfig.LocalToken = localToken
	validationConfig.ClaudeUpstream = "http://" + core.upstream.address
	validationConfig.CodexUpstream = validationConfig.ClaudeUpstream
	server := &Server{
		Config:                       &validationConfig,
		CodexNativeHTTP:              core.nativeHTTPHandler(),
		ServingAttestor:              servingAttestor,
		CodexHTTPStartupValidation:   validation,
		codexInstalledHTTPRouteAudit: routes,
		Catalog:                      modelregistry.NewCatalog(modelregistry.Snapshot{}),
		shutdownGracePeriod:          codexInstalledHTTPValidationQuiesceTimeout,
	}
	if runtimeServe != nil {
		handler, err := server.handler()
		if err != nil {
			return err
		}
		return runtimeServe(ctx, CodexInstalledHTTPValidationRuntime{
			Handler: handler, ServingAttestor: servingAttestor,
			StartupValidation: func(ctx context.Context, runtime CodexHTTPStartupValidationRuntime, beforeCommit func() error) error {
				if beforeCommit == nil {
					return codexInstalledHTTPValidationStageError("precommit authority")
				}
				validationCtx, cancel := context.WithCancel(ctx)
				ready := make(chan struct{})
				close(ready)
				run := runCodexHTTPStartupValidation(validationCtx, cancel, ready, runtime, validationFor(beforeCommit))
				select {
				case <-run.complete:
					return codexHTTPStartupValidationResult(run)
				case <-ctx.Done():
					cancel()
					<-servingAttestor.abortUnexpected()
					return ctx.Err()
				}
			},
			AbortUnexpected: servingAttestor.abortUnexpected,
		})
	}
	return server.ListenAndServe(ctx)
}

func codexInstalledHTTPValidationStageError(stage string) error {
	return fmt.Errorf("%w: %s", errCodexInstalledListenerAcceptance, stage)
}

func defaultCodexInstalledHTTPProtectedPaths(markerDir string) ([]codexInstalledProtectedPath, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) || !filepath.IsAbs(markerDir) {
		return nil, errCodexInstalledListenerAcceptance
	}
	paths := []codexInstalledProtectedPath{
		{path: filepath.Join(home, ".codex", "auth.json"), ownerControlledDirectory: true},
		{path: filepath.Join(home, ".codex", "accounts", "registry.json")},
		{path: filepath.Join(home, ".codex", "accounts"), directory: true},
		{path: codexReadinessPath(markerDir, CodexRoutingHTTP)},
	}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		if !filepath.IsAbs(codexHome) {
			return nil, errCodexInstalledListenerAcceptance
		}
		paths = append(paths,
			codexInstalledProtectedPath{path: filepath.Join(codexHome, "auth.json"), ownerControlledDirectory: true},
			codexInstalledProtectedPath{path: filepath.Join(codexHome, "accounts", "registry.json")},
			codexInstalledProtectedPath{path: filepath.Join(codexHome, "accounts"), directory: true},
		)
	}
	seen := make(map[string]bool, len(paths))
	result := make([]codexInstalledProtectedPath, 0, len(paths))
	for _, protected := range paths {
		clean := filepath.Clean(protected.path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		protected.path = clean
		result = append(result, protected)
	}
	return result, nil
}

func codexInstalledHTTPValidationPrivacyNeedles(localToken string) [][]byte {
	needles := [][]byte{
		[]byte("validation-session-"),
		[]byte("validation-thread-"),
		[]byte("validation-turn-"),
		[]byte("validation-account-"),
		[]byte("validation-upstream-"),
		[]byte("validation-token-"),
		[]byte(codexTurnMetadataKey),
		[]byte("ChatGPT-Account-ID"),
		[]byte("Authorization"),
		[]byte("Reply with exactly PONG."),
	}
	if validCodexInstalledHTTPValidationToken(localToken) {
		needles = append(needles, []byte(localToken))
	}
	return needles
}
