//go:build !windows

package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRuntimeSupervisorRoleBootstrapsWorkerAndCheckpointBeforeServe(t *testing.T) {
	lifecyclePath := t.TempDir() + "/lifecycle.lock"
	if err := os.WriteFile(lifecyclePath, []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := os.Open(lifecyclePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	holderDigest, err := RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	supervisorHolder, err := RuntimeLifecycleHolder(lifecycle, "supervisor-description")
	if err != nil {
		t.Fatal(err)
	}
	workerHolder := supervisorHolder
	workerHolder.DescriptionID = "worker-description"
	listenerFile, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	controlFile, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	secretReader, secretWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secretWriter.Write(bytes.Repeat([]byte{0x28}, RuntimeSecretSize)); err != nil {
		t.Fatal(err)
	}
	_ = secretWriter.Close()
	manifestDigest := sha256.Sum256([]byte("runtime-manifest"))
	manifest := RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: RuntimeRoleSupervisor, ManifestDigest: manifestDigest,
		ProxyInstanceID: "proxy-a", RuntimeInstanceID: "runtime-a",
		ListenerFD: RuntimeListenerFD, LifecycleFD: RuntimeLifecycleFD,
		ControlFD: RuntimeControlFD, SecretFD: RuntimeSecretFD, WorkFD: RuntimeNoWorkFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	events := []string{}
	launcher := &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{{holder: workerHolder, events: &events}}}
	checkpoint := &runtimeTestCheckpointStore{events: &events}
	err = RunRuntimeSupervisorRole(context.Background(), manifest, RuntimeSupervisorRoleDependencies{
		Files:            RuntimeRoleFiles{Listener: listenerFile, Lifecycle: lifecycle, Control: controlFile, Secret: secretReader},
		SupervisorHolder: supervisorHolder, Launcher: launcher, Checkpoints: checkpoint,
		WorkerManifest: WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"},
		AdoptListener:  func(*os.File) (net.Listener, error) { return &runtimeTestListener{}, nil },
		Serve: func(_ context.Context, _ net.Listener, handler http.Handler) error {
			if handler == nil {
				t.Fatal("missing selected worker handler")
			}
			events = append(events, "serve")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"boot:worker-description", "checkpoint:worker-description", "serve"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}
