package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// openProxyCodexCanary opens only an active run and validates it against the
// current readiness marker before any credential endpoint recovery can occur.
func openProxyCodexCanary(fsys fsutil.DurableFileSystem, path, configDirectory, markerDirectory string, required proxy.CodexTransportRequirements) (*proxy.CodexCanaryRecorder, error) {
	if fsys == nil || path == "" || configDirectory == "" || markerDirectory == "" {
		return nil, errors.New("Codex canary startup configuration is unavailable")
	}
	home, err := fsys.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve Codex canary protected state: %w", err)
	}
	protected, err := codexCanaryProtections(home, configDirectory)
	if err != nil {
		return nil, err
	}
	recorder, err := proxy.OpenServingCodexCanary(fsys, path, protected)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Codex canary startup evidence: %w", err)
	}
	keepRecorder := false
	defer func() {
		if !keepRecorder {
			_ = recorder.Close()
		}
	}()
	if !recorder.State().Active {
		return nil, nil
	}
	marker, err := proxy.LoadCodexReadinessMarker(markerDirectory, proxy.CodexRoutingHTTP)
	if err != nil {
		return nil, fmt.Errorf("load current HTTP readiness marker: %w", err)
	}
	tuple, err := proxy.BuildCodexCanaryTuple(required, marker)
	if err != nil {
		return nil, err
	}
	if err := recorder.ValidateTuple(tuple); err != nil {
		return nil, err
	}
	keepRecorder = true
	return recorder, nil
}

func validateProxyCodexCanaryRuntime(recorder *proxy.CodexCanaryRecorder, runtime *proxy.CodexRoutingRuntime) error {
	if recorder == nil {
		return nil
	}
	if runtime == nil || runtime.HTTP.Configured != proxy.CodexRoutingEnforce || runtime.HTTP.Effective != proxy.CodexRoutingEnforce ||
		runtime.HTTP.ModeEpoch == 0 || runtime.HTTP.AuthoritativeEpoch != runtime.HTTP.ModeEpoch || runtime.WebSocket.Effective == proxy.CodexRoutingEnforce {
		return errors.New("Codex canary requires exact effective HTTP enforcement")
	}
	return nil
}

func newProxyCodexCanaryStop(recorder *proxy.CodexCanaryRecorder, continuity *proxyCodexContinuity, native proxy.CodexNativeHTTPRoutingHandler) (proxy.CodexCanaryStopFunc, error) {
	if recorder == nil {
		return nil, nil
	}
	if continuity == nil || continuity.Runtime == nil || native == nil {
		return nil, errors.New("Codex canary stop authority is unavailable")
	}
	stop, err := proxy.NewCodexCanaryStopFunc(recorder, continuity.Runtime, native)
	if err != nil {
		return nil, errors.New("Codex canary stop authority is unavailable")
	}
	return stop, nil
}

func codexCanaryEndpointRecoveryRecorder(recorder *proxy.CodexCanaryRecorder) func() error {
	return func() error {
		if recorder == nil {
			return nil
		}
		return recorder.RecordLiveSessionRepair()
	}
}
