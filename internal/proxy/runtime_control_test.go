package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRuntimeControlRoleABIRejectsAmbientOrReorderedArguments(t *testing.T) {
	manifestDigest := sha256.Sum256([]byte("worker-manifest"))
	holderDigest := sha256.Sum256([]byte("worker-lifecycle-holder"))
	manifest := RuntimeRoleManifestV1{
		SchemaVersion:                 1,
		Role:                          RuntimeRoleSupervisor,
		ManifestDigest:                manifestDigest,
		ProxyInstanceID:               "proxy-a",
		RuntimeInstanceID:             "runtime-a",
		ListenerFD:                    RuntimeListenerFD,
		LifecycleFD:                   RuntimeLifecycleFD,
		ControlFD:                     RuntimeControlFD,
		LifecycleHolderIdentityDigest: holderDigest,
		SecretFD:                      RuntimeSecretFD,
	}
	want := []string{
		"--runtime-role", "supervisor",
		"--runtime-schema", "1",
		"--runtime-manifest-digest", hex.EncodeToString(manifestDigest[:]),
		"--proxy-instance", "proxy-a",
		"--runtime-instance", "runtime-a",
		"--listener-fd", "3",
		"--lifecycle-fd", "4",
		"--control-fd", "5",
		"--lifecycle-holder-digest", hex.EncodeToString(holderDigest[:]),
		"--secret-fd", "6",
	}
	if got := RuntimeRoleArguments(manifest); !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
	parsed, err := ParseRuntimeRoleArguments(want)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != manifest {
		t.Fatalf("parsed = %#v, want %#v", parsed, manifest)
	}
	for _, invalid := range [][]string{
		append(append([]string{}, want...), "--ambient-helper", "true"),
		append([]string{want[2], want[3], want[0], want[1]}, want[4:]...),
	} {
		if _, err := ParseRuntimeRoleArguments(invalid); !errors.Is(err, ErrRuntimeRoleManifest) {
			t.Fatalf("ParseRuntimeRoleArguments(%q) error = %v", invalid, err)
		}
	}
	for _, arg := range want {
		if strings.Contains(arg, "Wlpa") {
			t.Fatal("role argv contains channel secret material")
		}
	}
}

func TestRuntimeControlWorkerABIRejectsPublicListenerFDAndNonCanonicalValues(t *testing.T) {
	manifestDigest := sha256.Sum256([]byte("worker-manifest"))
	holderDigest := sha256.Sum256([]byte("worker-holder"))
	worker := RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: RuntimeRoleWorker, ManifestDigest: manifestDigest,
		ProxyInstanceID: "proxy-a", RuntimeInstanceID: "runtime-a",
		ListenerFD: RuntimeNoListenerFD, LifecycleFD: RuntimeLifecycleFD,
		ControlFD: RuntimeControlFD, SecretFD: RuntimeSecretFD,
		LifecycleHolderIdentityDigest: holderDigest,
	}
	arguments := RuntimeRoleArguments(worker)
	if slices.Contains(arguments, "--listener-fd") {
		t.Fatalf("worker argv contains public listener: %q", arguments)
	}
	if parsed, err := ParseRuntimeRoleArguments(arguments); err != nil || parsed != worker {
		t.Fatalf("worker parse = %#v, %v", parsed, err)
	}
	supervisor := worker
	supervisor.Role = RuntimeRoleSupervisor
	supervisor.ListenerFD = RuntimeListenerFD
	valid := RuntimeRoleArguments(supervisor)
	for _, mutate := range []func([]string){
		func(args []string) { args[3] = "+1" },
		func(args []string) { args[11] = "03" },
		func(args []string) { args[5] = strings.ToUpper(args[5]) },
	} {
		candidate := append([]string(nil), valid...)
		mutate(candidate)
		if _, err := ParseRuntimeRoleArguments(candidate); !errors.Is(err, ErrRuntimeRoleManifest) {
			t.Fatalf("noncanonical argv %q error = %v", candidate, err)
		}
	}
	workerWithListener := append([]string(nil), arguments...)
	workerWithListener = append(workerWithListener, "--listener-fd", "3")
	if _, err := ParseRuntimeRoleArguments(workerWithListener); !errors.Is(err, ErrRuntimeRoleManifest) {
		t.Fatalf("worker public listener error = %v", err)
	}
}

type recordingReadCloser struct {
	*bytes.Reader
	closed bool
}

func (reader *recordingReadCloser) Close() error { reader.closed = true; return nil }

func TestRuntimeControlReadsAndClosesOneShotSecretFD(t *testing.T) {
	material := bytes.Repeat([]byte{0x4d}, RuntimeSecretSize)
	reader := &recordingReadCloser{Reader: bytes.NewReader(material)}
	secret, err := ReadRuntimeSecret(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.closed {
		t.Fatal("one-shot secret descriptor remained open")
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	secret.Destroy()
}

func TestRuntimeControlPrivateTransportIsConnectedAndBounded(t *testing.T) {
	supervisor, worker, err := NewRuntimePrivateTransport()
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	defer worker.Close()
	written := make(chan error, 1)
	go func() { _, err := supervisor.Write([]byte("ready")); written <- err }()
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(worker, buffer); err != nil || string(buffer) != "ready" {
		t.Fatalf("private transport read = %q, %v", buffer, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeControlFrameBoundsAndSecretZeroisation(t *testing.T) {
	secret, err := NewRuntimeSecret(bytes.Repeat([]byte{0x5a}, RuntimeSecretSize))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := SealRuntimeControlFrame(secret, RuntimeControlFrameV1{
		SchemaVersion: 1,
		Sequence:      1,
		Kind:          "begin_drain",
		Payload:       []byte(`{"mode":"drain","generation":7}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenRuntimeControlFrame(secret, frame)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Sequence != 1 || opened.Kind != "begin_drain" {
		t.Fatalf("opened = %#v", opened)
	}
	frame[len(frame)-1] ^= 1
	if _, err := OpenRuntimeControlFrame(secret, frame); !errors.Is(err, ErrRuntimeControlFrame) {
		t.Fatalf("tampered frame error = %v", err)
	}
	if _, err := SealRuntimeControlFrame(secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 2, Kind: "work", Payload: make([]byte, RuntimeControlFrameLimit+1)}); !errors.Is(err, ErrRuntimeControlBackpressure) {
		t.Fatalf("oversize frame error = %v", err)
	}
	secretBytes := secret.value
	secret.Destroy()
	if !secret.Destroyed() {
		t.Fatal("secret remains live")
	}
	if !bytes.Equal(secretBytes, make([]byte, RuntimeSecretSize)) {
		t.Fatal("secret bytes were not zeroised")
	}
	if _, err := OpenRuntimeControlFrame(secret, []byte("frame")); !errors.Is(err, ErrRuntimeSecretDestroyed) {
		t.Fatalf("destroyed secret error = %v", err)
	}
}

func TestRuntimeControlDuplicateOrOutOfOrderFrameTerminatesReceiver(t *testing.T) {
	secret, err := NewRuntimeSecret(bytes.Repeat([]byte{0x3c}, RuntimeSecretSize))
	if err != nil {
		t.Fatal(err)
	}
	receiver := NewRuntimeControlReceiver(secret)
	first, err := SealRuntimeControlFrame(secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "hello", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(first); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(first); !errors.Is(err, ErrRuntimeControlFrame) {
		t.Fatalf("duplicate frame error = %v", err)
	}
	second, err := SealRuntimeControlFrame(secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 2, Kind: "ready", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(second); !errors.Is(err, ErrRuntimeControlFrame) {
		t.Fatalf("receiver continued after protocol failure: %v", err)
	}
	receiver.Close()
	if !secret.Destroyed() {
		t.Fatal("receiver close retained channel secret")
	}
}

func TestRuntimeControlQueueHonoursCancellationAndBackpressure(t *testing.T) {
	queue := NewRuntimeControlQueue(1)
	if err := queue.Enqueue(context.Background(), []byte("first")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := queue.Enqueue(ctx, []byte("second")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enqueue error = %v", err)
	}
	if err := queue.TryEnqueue([]byte("second")); !errors.Is(err, ErrRuntimeControlBackpressure) {
		t.Fatalf("full enqueue error = %v", err)
	}
}
