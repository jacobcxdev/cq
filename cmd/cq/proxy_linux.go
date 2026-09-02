//go:build linux

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

var runLinuxUnixProxyOwnedRuntime = runUnixProxyOwnedRuntime
var runLinuxUnixProxyAdoptedRuntime = runUnixProxyAdoptedRuntime

func init() {
	defaultProxyInspectionTarget = linuxProxyInspectionTarget
	proxyInspectionTargetForRoot = linuxProxyInspectionTargetForRoot
	adoptProxyListenerFn = adoptUnixProxyListener
	newProxyRuntimeWorkerLauncherFn = newUnixProxyRuntimeWorkerLauncher
	runProxyAdoptedRuntimeFn = runLinuxProxyAdoptedRuntime
	runProxyOwnedRuntimeFn = runLinuxProxyOwnedRuntime
	runProxyValidationCandidateFn = runLinuxProxyValidationCandidate
	runProxyValidationCandidateWorkerFn = runLinuxProxyValidationCandidateWorker
}

func runLinuxProxyOwnedRuntime(ctx context.Context, port int, serve func(context.Context, net.Listener, http.Handler) error) (bool, error) {
	terminationCtx, stop := linuxProxyTerminationContext(ctx)
	defer stop()
	return runLinuxUnixProxyOwnedRuntime(terminationCtx, port, serve)
}

func runLinuxProxyAdoptedRuntime(ctx context.Context, listener net.Listener, serve func(context.Context, net.Listener, http.Handler) error) error {
	terminationCtx, stop := linuxProxyTerminationContext(ctx)
	defer stop()
	return runLinuxUnixProxyAdoptedRuntime(terminationCtx, listener, serve)
}

func linuxProxyTerminationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

func runtimeDescriptorRoot() string { return "/proc/self/fd" }
