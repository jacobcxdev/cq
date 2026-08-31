//go:build linux

package main

import (
	"reflect"
	"testing"
)

func TestLinuxProxyStartWiresOwnedRuntime(t *testing.T) {
	if reflect.ValueOf(runProxyOwnedRuntimeFn).Pointer() != reflect.ValueOf(runUnixProxyOwnedRuntime).Pointer() {
		t.Fatal("Linux owned runtime remains unavailable")
	}
	if reflect.ValueOf(runProxyAdoptedRuntimeFn).Pointer() != reflect.ValueOf(runUnixProxyAdoptedRuntime).Pointer() {
		t.Fatal("Linux adopted runtime remains unavailable")
	}
	if reflect.ValueOf(newProxyRuntimeWorkerLauncherFn).Pointer() != reflect.ValueOf(newUnixProxyRuntimeWorkerLauncher).Pointer() {
		t.Fatal("Linux runtime worker launcher remains unavailable")
	}
	if reflect.ValueOf(adoptProxyListenerFn).Pointer() != reflect.ValueOf(adoptUnixProxyListener).Pointer() {
		t.Fatal("Linux listener adoption remains unavailable")
	}
}
