package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var (
	defaultInstalledHTTPValidationRequestStoreFn   = defaultInstalledHTTPValidationRequestStore
	restartInstalledHTTPValidationProxyAgentFn     = restartProxyAgent
	runInstalledHTTPValidationStartupFn            = runProxyInstalledHTTPValidationStartup
	runCodexInstalledHTTPValidationFn              = proxy.RunCodexInstalledHTTPValidation
	installedHTTPValidationClientBuildFn           = defaultCodexRoutingClientBuild
	invalidateInstalledHTTPValidationMarkerFn      = proxy.InvalidateDefaultCodexHTTPReadinessMarker
	loadProxyStartConfigFn                         = proxy.LoadConfig
	consumeInstalledHTTPValidationStartupRequestFn = func(build string) (*installedHTTPValidationConsumedRequest, error) {
		store, err := defaultInstalledHTTPValidationRequestStoreFn()
		if err != nil {
			return nil, err
		}
		return consumeInstalledHTTPValidationRequestWithIntent(store, build)
	}
)

type proxyValidateHTTPDependencies struct {
	store      installedHTTPValidationRequestStore
	restart    func() error
	invalidate func() error
}

func runDefaultProxyValidateHTTP(args []string, build string) error {
	store, err := defaultInstalledHTTPValidationRequestStoreFn()
	if err != nil {
		return err
	}
	return runProxyValidateHTTP(args, proxyValidateHTTPDependencies{
		store:      store,
		restart:    restartInstalledHTTPValidationProxyAgentFn,
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
			_, cancelErr := cancelInstalledHTTPValidationRequest(deps.store, receipt)
			invalidateErr := invalidateInstalledHTTPValidationMarkerSafely(deps.invalidate)
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
	lock, err := fsutil.AcquireExclusiveLockInDirectory(store.fs, directory, installedHTTPValidationLockName)
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
