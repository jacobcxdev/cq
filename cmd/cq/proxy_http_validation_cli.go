package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var (
	defaultInstalledHTTPValidationRequestStoreFn   = defaultInstalledHTTPValidationRequestStore
	runInstalledHTTPValidationStartupFn            = runProxyInstalledHTTPValidationStartup
	runCodexInstalledHTTPValidationFn              = proxy.RunCodexInstalledHTTPValidation
	installedHTTPValidationClientBuildFn           = defaultCodexRoutingClientBuild
	invalidateInstalledHTTPValidationMarkerFn      = proxy.InvalidateDefaultCodexHTTPReadinessMarker
	loadProxyStartConfigFn                         = proxy.LoadConfig
	validateInstalledHTTPValidationCandidateFn     = validateInstalledHTTPValidationCandidate
	restartInstalledHTTPValidationCandidateFn      = restartInstalledHTTPValidationCandidate
	cleanupInstalledHTTPValidationCandidateFn      = cleanupInstalledHTTPValidationCandidate
	consumeInstalledHTTPValidationStartupRequestFn = func(build string) (*installedHTTPValidationConsumedRequest, error) {
		store, err := defaultInstalledHTTPValidationRequestStoreFn()
		if err != nil {
			return nil, err
		}
		return consumeInstalledHTTPValidationRequestWithIntent(store, build)
	}
)

type proxyValidateHTTPDependencies struct {
	ctx        context.Context
	store      installedHTTPValidationRequestStore
	restart    func() error
	invalidate func() error
}

type installedHTTPValidationCandidateAuthority struct {
	binding       installedHTTPValidationServiceBinding
	pid           int
	processStart  uint64
	listenerInode uint64
	worker        int
	workerStart   uint64
}

func runDefaultProxyValidateHTTP(args []string, build string) (returnErr error) {
	opts, err := parseProxyCommandOptionsFor("proxy validate-http", args)
	if err != nil {
		return err
	}
	if opts.Port == 0 {
		return errors.New("proxy validate-http: --port is required")
	}
	if opts.Port == proxy.DefaultPort {
		return errors.New("proxy validate-http: live proxy port is forbidden")
	}
	store, err := defaultInstalledHTTPValidationRequestStoreFn()
	if err != nil {
		return err
	}
	authority, err := validateInstalledHTTPValidationCandidateFn(opts.Port)
	if err != nil {
		return fmt.Errorf("proxy validate-http: candidate service unavailable: %w", err)
	}
	defer func() {
		if cleanupErr := cleanupInstalledHTTPValidationCandidateFn(); cleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("proxy validate-http: clean candidate service: %w", cleanupErr))
		}
	}()
	binding := authority.binding
	if binding.label != candidateProxyAgentLabel || binding.port != opts.Port {
		return errors.New("proxy validate-http: candidate service port mismatch")
	}
	resolveService := store.resolveService
	store.resolveService = func(label string) (installedHTTPValidationServiceBinding, error) {
		current, err := resolveService(binding.label)
		if err != nil || current != binding {
			return installedHTTPValidationServiceBinding{}, errors.New("installed candidate service binding changed")
		}
		return current, nil
	}
	return runProxyValidateHTTP(nil, proxyValidateHTTPDependencies{
		store: store,
		restart: func() error {
			current, err := resolveService(binding.label)
			if err != nil || current != binding {
				return errors.New("installed candidate service binding changed")
			}
			revalidated, err := validateInstalledHTTPValidationCandidateFn(opts.Port)
			if err != nil || revalidated != authority {
				return errors.Join(err, errors.New("installed candidate listener authority changed"))
			}
			return restartInstalledHTTPValidationCandidateFn(binding.label)
		},
		invalidate: invalidateInstalledHTTPValidationMarkerFn,
	}, build)
}

func runProxyValidateHTTP(args []string, deps proxyValidateHTTPDependencies, build string) (returnErr error) {
	if len(args) != 0 {
		return errors.New("usage: cq proxy validate-http")
	}
	if deps.restart == nil {
		return errors.New("installed HTTP validation restart is unavailable")
	}
	var receipt installedHTTPValidationRequestReceipt
	requestCreated := false
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = errors.New("request installed HTTP validation startup panicked")
		}
		if returnErr != nil && requestCreated {
			_, cancelErr := cancelInstalledHTTPValidationRequestContext(deps.ctx, deps.store, receipt)
			invalidateErr := invalidateInstalledHTTPValidationMarkerSafely(deps.invalidate)
			if cancelErr != nil || invalidateErr != nil {
				returnErr = errors.Join(returnErr, errValidationCleanupFailed)
			}
			returnErr = errors.Join(returnErr, cancelErr, invalidateErr)
		}
	}()
	var err error
	receipt, err = createInstalledHTTPValidationRequestWithReceipt(deps.store, build)
	if err != nil {
		return err
	}
	requestCreated = true
	if err := deps.restart(); err != nil {
		return fmt.Errorf("restart installed proxy service: %w", err)
	}
	return nil
}

func invalidateInstalledHTTPValidationMarkerSafely(invalidate func() error) (returnErr error) {
	if invalidate == nil {
		return errors.New("installed HTTP validation marker invalidation is unavailable")
	}
	defer func() {
		if recover() != nil {
			returnErr = errors.New("installed HTTP validation marker invalidation failed")
		}
	}()
	return invalidate()
}

type installedHTTPValidationCancellationOutcome uint8

const (
	installedHTTPValidationCancellationMissing installedHTTPValidationCancellationOutcome = iota
	installedHTTPValidationCancellationPoisoned
	installedHTTPValidationCancellationAlreadyPoisoned
)

func cancelInstalledHTTPValidationRequest(store installedHTTPValidationRequestStore, receipt installedHTTPValidationRequestReceipt) (installedHTTPValidationCancellationOutcome, error) {
	return cancelInstalledHTTPValidationRequestContext(context.Background(), store, receipt)
}
func cancelInstalledHTTPValidationRequestContext(ctx context.Context, store installedHTTPValidationRequestStore, receipt installedHTTPValidationRequestReceipt) (installedHTTPValidationCancellationOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if store.fs == nil || store.path == "" || !filepath.IsAbs(store.path) {
		return installedHTTPValidationCancellationMissing, errors.New("incomplete installed HTTP validation request store")
	}
	directoryPath := filepath.Dir(store.path)
	if _, err := store.fs.Lstat(directoryPath); errors.Is(err, os.ErrNotExist) {
		return installedHTTPValidationCancellationMissing, nil
	} else if err != nil {
		return installedHTTPValidationCancellationMissing, fmt.Errorf("inspect installed HTTP validation request directory: %w", err)
	}
	if err := fsutil.ValidateSecureDirectory(store.fs, directoryPath); err != nil {
		return installedHTTPValidationCancellationMissing, fmt.Errorf("validate installed HTTP validation request directory: %w", err)
	}
	directory, err := store.fs.OpenSecureDirectory(directoryPath)
	if err != nil {
		return installedHTTPValidationCancellationMissing, fmt.Errorf("open installed HTTP validation request directory: %w", err)
	}
	defer directory.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return installedHTTPValidationCancellationMissing, fmt.Errorf("fence installed HTTP validation cancellation directory: %w", err)
	}
	lock, err := acquireInstalledHTTPValidationCancellationLockContext(ctx, store, directory)
	if err != nil {
		return installedHTTPValidationCancellationMissing, fmt.Errorf("lock installed HTTP validation request store: %w", err)
	}
	defer lock.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath); err != nil {
		return installedHTTPValidationCancellationMissing, fmt.Errorf("refence installed HTTP validation cancellation directory: %w", err)
	}
	for _, name := range []string{filepath.Base(store.path), installedHTTPValidationClaimName, receipt.usedName} {
		data, identity, readErr := fsutil.ReadSecureFileInDirectoryWithIdentity(store.fs, directory, name, installedHTTPValidationRequestMaxSize)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return installedHTTPValidationCancellationMissing, fmt.Errorf("read installed HTTP validation cancellation candidate: %w", readErr)
		}
		if sha256.Sum256(data) != receipt.payloadSHA256 || identity != receipt.identity {
			continue
		}
		if err := fsutil.SecurePromoteNoReplaceInDirectoryChecked(store.fs, directory, name, receipt.cancelledName, data, identity, func() error {
			return fsutil.ValidateSecureDirectoryHandle(store.fs, directory, directoryPath)
		}); err != nil {
			return installedHTTPValidationCancellationMissing, fmt.Errorf("poison cancelled installed HTTP validation request: %w", err)
		}
		return installedHTTPValidationCancellationPoisoned, nil
	}
	data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.fs, directory, receipt.cancelledName, installedHTTPValidationRequestMaxSize)
	if err == nil && sha256.Sum256(data) == receipt.payloadSHA256 && identity == receipt.identity {
		return installedHTTPValidationCancellationAlreadyPoisoned, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return installedHTTPValidationCancellationMissing, fmt.Errorf("inspect cancelled installed HTTP validation request: %w", err)
	}
	return installedHTTPValidationCancellationMissing, nil
}

func acquireInstalledHTTPValidationCancellationLock(store installedHTTPValidationRequestStore, directory fsutil.SecureDirectory) (fsutil.ExclusiveLock, error) {
	return acquireInstalledHTTPValidationCancellationLockContext(context.Background(), store, directory)
}
func acquireInstalledHTTPValidationCancellationLockContext(ctx context.Context, store installedHTTPValidationRequestStore, directory fsutil.SecureDirectory) (fsutil.ExclusiveLock, error) {
	for {
		lock, err := fsutil.AcquireExclusiveLockInDirectory(store.fs, directory, installedHTTPValidationLockName)
		if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func runInstalledHTTPValidationStartupIfRequested(
	ctx context.Context,
	cfg *proxy.Config,
	build string,
	consume func(string) (*installedHTTPValidationConsumedRequest, error),
	startup func(context.Context, *proxy.Config, string, proxy.CodexInstalledHTTPValidationGuard) error,
) (bool, error) {
	if cfg == nil || build == "" || consume == nil || startup == nil {
		return false, errors.New("incomplete installed HTTP validation startup")
	}
	intent, err := consume(build)
	if err != nil {
		return false, fmt.Errorf("consume installed HTTP validation request: %w", err)
	}
	if intent == nil {
		return false, nil
	}
	if err := startup(ctx, cfg, build, intent); err != nil {
		return true, err
	}
	return true, nil
}

func claimInstalledHTTPValidationStartupRequest(
	build string,
	consume func(string) (*installedHTTPValidationConsumedRequest, error),
	invalidate func() error,
) (*installedHTTPValidationConsumedRequest, error) {
	if build == "" || consume == nil {
		return nil, errors.New("incomplete installed HTTP validation startup claim")
	}
	intent, consumeErr := consume(build)
	if consumeErr == nil && intent == nil {
		return nil, nil
	}
	invalidateErr := invalidateInstalledHTTPValidationMarkerSafely(invalidate)
	if consumeErr != nil {
		return nil, errors.Join(fmt.Errorf("consume installed HTTP validation request: %w", consumeErr), invalidateErr)
	}
	if invalidateErr != nil {
		return nil, fmt.Errorf("invalidate prior installed HTTP readiness marker: %w", invalidateErr)
	}
	return intent, nil
}

func runProxyInstalledHTTPValidationStartup(ctx context.Context, cfg *proxy.Config, build string, guard proxy.CodexInstalledHTTPValidationGuard) (returnErr error) {
	defer func() {
		if recover() != nil {
			returnErr = errors.Join(
				errors.New("installed Codex client build is unavailable"),
				runCodexInstalledHTTPValidationFn(ctx, cfg, build, "", guard),
			)
		}
	}()
	clientBuild := installedHTTPValidationClientBuildFn()
	if clientBuild == "" {
		return errors.Join(
			errors.New("installed Codex client build is unavailable"),
			runCodexInstalledHTTPValidationFn(ctx, cfg, build, "", guard),
		)
	}
	return runCodexInstalledHTTPValidationFn(ctx, cfg, build, clientBuild, guard)
}

var (
	errValidationCleanupFailed        = errors.New("validation request cleanup failed")
	errValidationCandidateUnavailable = errors.New("installed validation candidate unavailable")
	errValidationCandidateChanged     = errors.New("installed validation candidate changed")
)

type canonicalHTTPValidationOperations struct {
	store      func(context.Context) (installedHTTPValidationRequestStore, error)
	validate   func(context.Context, int) (installedHTTPValidationCandidateAuthority, error)
	restart    func(context.Context, string) error
	invalidate func() error
}

func runCanonicalProxyValidateHTTP(ctx context.Context, port int, build string) error {
	return runCanonicalProxyValidateHTTPWithOperations(ctx, port, build, canonicalHTTPValidationOperations{
		store: func(ctx context.Context) (installedHTTPValidationRequestStore, error) {
			store, err := defaultInstalledHTTPValidationRequestStore()
			store.resolveService = func(label string) (installedHTTPValidationServiceBinding, error) {
				return resolveCanonicalHTTPValidationService(ctx, label)
			}
			return store, err
		},
		validate:   validateCanonicalHTTPValidationCandidate,
		restart:    restartCanonicalHTTPValidationCandidate,
		invalidate: invalidateInstalledHTTPValidationMarkerFn,
	})
}
func runCanonicalProxyValidateHTTPWithOperations(ctx context.Context, port int, build string, ops canonicalHTTPValidationOperations) error {
	if port < 1 || port > 65535 || port == proxy.DefaultPort {
		return errors.New("invalid validation port")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	authority, err := ops.validate(ctx, port)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || authority.binding.validate() != nil || authority.binding.label != candidateProxyAgentLabel || authority.binding.port != port {
		return errors.Join(errValidationCandidateUnavailable, err)
	}
	store, err := ops.store(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	resolve := store.resolveService
	store.resolveService = func(string) (installedHTTPValidationServiceBinding, error) {
		if err := ctx.Err(); err != nil {
			return installedHTTPValidationServiceBinding{}, err
		}
		current, err := resolve(authority.binding.label)
		if err != nil || current != authority.binding {
			return installedHTTPValidationServiceBinding{}, errValidationCandidateChanged
		}
		return current, nil
	}
	return runProxyValidateHTTP(nil, proxyValidateHTTPDependencies{
		ctx: ctx, store: store, invalidate: ops.invalidate,
		restart: func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			current, err := ops.validate(ctx, port)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || current != authority {
				return errors.Join(errValidationCandidateChanged, err)
			}
			if err := ops.restart(ctx, authority.binding.label); err != nil {
				return err
			}
			return ctx.Err()
		},
	}, build)
}
