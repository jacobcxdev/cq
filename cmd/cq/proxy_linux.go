//go:build linux

package main

func init() {
	defaultProxyInspectionTarget = linuxProxyInspectionTarget
	proxyInspectionTargetForRoot = linuxProxyInspectionTargetForRoot
	adoptProxyListenerFn = adoptUnixProxyListener
	newProxyRuntimeWorkerLauncherFn = newUnixProxyRuntimeWorkerLauncher
	runProxyAdoptedRuntimeFn = runUnixProxyAdoptedRuntime
	runProxyOwnedRuntimeFn = runUnixProxyOwnedRuntime
}

func runtimeDescriptorRoot() string { return "/proc/self/fd" }
