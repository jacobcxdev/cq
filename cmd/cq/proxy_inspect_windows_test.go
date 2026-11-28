//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestWindowsProxyInspection(t *testing.T) {
	const executable = `C:\Users\me\cq.exe`
	const listenerImage = `C:\Users\Me\cq.exe`
	const address = "127.0.0.1:19280"
	running := componentStatus{Registered: true, Running: true, ConfiguredExecutable: executable}
	stopped := componentStatus{Registered: true, ConfiguredExecutable: executable}

	tests := []struct {
		name          string
		task          componentStatus
		enginePIDs    []uint32
		taskErr       error
		listenerPID   uint32
		listenerImage string
		listenerErr   error
		healthErr     error
		changedPID    uint32
		want          proxy.ProxyVerdict
		wantRuntime   string
		wantProbes    int
		wantTaskState proxy.FactStatus
	}{
		{name: "owned and locally healthy", task: running, enginePIDs: []uint32{41}, listenerPID: 41, want: proxy.ProxyVerdictDegraded, wantRuntime: "healthy", wantProbes: 1},
		{name: "owned but failing health", task: running, enginePIDs: []uint32{41}, listenerPID: 41, healthErr: errors.New("HTTP 500"), want: proxy.ProxyVerdictDegraded, wantRuntime: "unhealthy", wantProbes: 1},
		{name: "listener changes during health probe", task: running, enginePIDs: []uint32{41}, listenerPID: 41, changedPID: 42, want: proxy.ProxyVerdictIndeterminate, wantProbes: 1},
		{name: "stopped", task: stopped, listenerErr: errWindowsProxyListenerAbsent, want: proxy.ProxyVerdictDown},
		{name: "foreign PID", task: running, enginePIDs: []uint32{41}, listenerPID: 42, want: proxy.ProxyVerdictConflicted},
		{name: "foreign executable", task: running, enginePIDs: []uint32{41}, listenerPID: 41, listenerImage: `C:\Users\me\other.exe`, want: proxy.ProxyVerdictConflicted},
		{name: "unregistered listener", listenerPID: 42, want: proxy.ProxyVerdictConflicted, wantTaskState: proxy.FactAbsent},
		{name: "ambiguous task instances", task: running, enginePIDs: []uint32{41, 42}, listenerPID: 41, want: proxy.ProxyVerdictConflicted, wantTaskState: proxy.FactInvalid},
		{name: "ambiguous listener owners", task: running, enginePIDs: []uint32{41}, listenerErr: errWindowsProxyListenerOwners, want: proxy.ProxyVerdictConflicted},
		{name: "task definition conflict", taskErr: installstate.ErrOwnershipConflict, listenerErr: errWindowsProxyListenerAbsent, want: proxy.ProxyVerdictConflicted, wantTaskState: proxy.FactInvalid},
		{name: "task access denied", taskErr: os.ErrPermission, listenerPID: 41, want: proxy.ProxyVerdictIndeterminate, wantTaskState: proxy.FactPermissionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var observations, probes int
			image := test.listenerImage
			if image == "" {
				image = listenerImage
			}
			target := windowsProxyInspectionTargetWithDependencies("", windowsProxyInspectionDependencies{
				executable: func() (string, error) { return executable, nil },
				port:       func() (int, error) { return 19280, nil },
				task: func(context.Context, string) (componentStatus, windowsTaskRuntimeState, error) {
					return test.task, windowsTaskRuntimeState{EnginePIDs: test.enginePIDs}, test.taskErr
				},
				listener: func(_ context.Context, port int) (uint32, string, error) {
					if port != 19280 {
						t.Fatalf("listener port = %d", port)
					}
					observations++
					if observations > 1 && test.changedPID != 0 {
						return test.changedPID, image, nil
					}
					return test.listenerPID, image, test.listenerErr
				},
				health: func(_ context.Context, received string) error {
					probes++
					if received != address {
						t.Fatalf("health address = %s, want %s", received, address)
					}
					return test.healthErr
				},
			})
			snapshot := InspectProxy(context.Background(), target)
			if snapshot.Verdict != test.want {
				t.Fatalf("verdict = %s, want %s: %+v", snapshot.Verdict, test.want, snapshot)
			}
			if test.wantTaskState != "" && snapshot.Service.Status != test.wantTaskState {
				t.Errorf("task status = %s, want %s", snapshot.Service.Status, test.wantTaskState)
			}
			if test.wantRuntime != "" && (snapshot.Runtime.Value == nil || snapshot.Runtime.Value.Health != test.wantRuntime) {
				t.Errorf("runtime = %+v, want %s", snapshot.Runtime, test.wantRuntime)
			}
			if snapshot.DataPlane.Value == nil || snapshot.DataPlane.Value.Proven || snapshot.DataPlane.Value.Code != "unproven" {
				t.Errorf("data plane = %+v, want unproven", snapshot.DataPlane)
			}
			if probes != test.wantProbes {
				t.Errorf("health probes = %d, want %d", probes, test.wantProbes)
			}
		})
	}
}

func TestWindowsProxyInspectionInstanceRootDoesNotUseDefaultListener(t *testing.T) {
	target := windowsProxyInspectionTargetWithDependencies(`C:\isolated\cq`, windowsProxyInspectionDependencies{
		executable: func() (string, error) { return `C:\Users\me\cq.exe`, nil },
		port: func() (int, error) {
			t.Fatal("named inspection fell back to the production port")
			return 0, nil
		},
	})
	snapshot := InspectProxy(context.Background(), target)
	if snapshot.Verdict != proxy.ProxyVerdictIndeterminate || snapshot.Desired.ErrorCode == nil || *snapshot.Desired.ErrorCode != "instance_root_unsupported" {
		t.Fatalf("named inspection = %+v", snapshot)
	}
}
