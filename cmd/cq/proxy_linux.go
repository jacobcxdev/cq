//go:build linux

package main

func init() {
	defaultProxyInspectionTarget = linuxProxyInspectionTarget
	proxyInspectionTargetForRoot = linuxProxyInspectionTargetForRoot
	adoptProxyListenerFn = adoptUnixProxyListener
	newProxyRuntimeWorkerLauncherFn = newUnixProxyRuntimeWorkerLauncher
	runProxyAdoptedRuntimeFn = runUnixProxyAdoptedRuntime
	runProxyOwnedRuntimeFn = runUnixProxyOwnedRuntime
	runProxyValidationCandidateFn = runLinuxProxyValidationCandidate
	runProxyValidationCandidateWorkerFn = runLinuxProxyValidationCandidateWorker
}

func runtimeDescriptorRoot() string { return "/proc/self/fd" }
