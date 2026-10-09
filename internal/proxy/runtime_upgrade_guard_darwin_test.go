//go:build darwin

package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/unix"
)

func guardTestStore(t *testing.T) RuntimeUpgradeStore {
	t.Helper()
	home, err := os.MkdirTemp("/private/tmp", "cq-upgrade-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	return RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
}

func TestRuntimeUpgradeGuardRetainsSocketAfterControlDeath(t *testing.T) {
	store := guardTestStore(t)
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	address := tcp.Addr().String()
	listener, err := tcp.File()
	tcp.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	previous := installer.RuntimeArtifact{Path: filepath.Join(store.Roots.State, "cq"), SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Version: "test", ProtocolVersion: 1}
	receipt := RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "guard-test", Generation: 1, Phase: "prepared", Previous: previous, Candidate: previous, ListenerIdentity: "tcp|" + address, SupervisorPID: os.Getpid()}
	for _, phase := range []string{"prepared", "waiting", "handoff"} {
		receipt.Phase = phase
		if err := store.Save(receipt); err != nil {
			t.Fatal(err)
		}
	}
	receipt.GuardPID = os.Getpid()
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	secretFile, secret, err := NewRuntimeUpgradeSecretFile(store.Roots.State)
	if err != nil {
		t.Fatal(err)
	}
	defer secretFile.Close()
	defer secret.Destroy()
	parentFile, guardFile, err := newRuntimePrivateSocketFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer parentFile.Close()
	defer guardFile.Close()
	parent, err := net.FileConn(parentFile)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	parent.SetDeadline(time.Now().Add(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guardFD, err := unix.Dup(int(listener.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	guardListener := os.NewFile(uintptr(guardFD), "guard-listener")
	finished := make(chan error, 1)
	go func() {
		finished <- RunRuntimeUpgradeGuardWithFiles(ctx, guardListener, guardFile, secretFile, receipt)
	}()
	resume := RuntimeUpgradeResumeV1{SchemaVersion: 1, Receipt: receipt, Release: RuntimeWorkerReleaseV1{ProcessIdentityDigest: "process", ProcessTreeAbsenceProofDigest: "absence", HolderReleaseProofDigest: "release"}}
	payload, _ := json.Marshal(resume)
	if err := WriteRuntimeControlMessage(parent, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "upgrade_prepare", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if frame, err := ReadRuntimeControlMessage(parent, NewRuntimeControlReceiver(secret)); err != nil || frame.Kind != "upgrade_guard_ready" {
		t.Fatalf("guard ready: %+v %v", frame, err)
	}
	if frame, err := ReadRuntimeControlMessage(parent, NewRuntimeControlReceiver(secret)); err != nil || frame.Kind != "upgrade_resume" {
		t.Fatalf("resume: %+v %v", frame, err)
	}
	parent.Close()
	parentFile.Close()
	listener.Close()
	// No process accepts connections while guard retains the duplicate.
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("guard lost public socket: %v", err)
	}
	conn.Close()
	if contender, err := net.Listen("tcp4", address); err == nil {
		contender.Close()
		t.Fatal("guard allowed competing bind")
	}
	select {
	case err := <-finished:
		t.Fatalf("guard retired before terminal acknowledgement: %v", err)
	default:
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("guard did not stop")
	}
}

func TestDarwinRuntimeUpgradeCrashBeforeAckRecoversListener(t *testing.T) {
	for _, backoff := range []bool{false, true} {
		t.Run(fmt.Sprintf("backoff_%t", backoff), func(t *testing.T) { guardRecoveryLaunchdFixture(t, backoff) })
	}
}

func guardRecoveryLaunchdFixture(t *testing.T, backoff bool) {
	t.Helper()
	store := guardTestStore(t)
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	address := tcp.Addr().String()
	listener, err := tcp.File()
	tcp.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := fsutil.EnsureSecureDirectory(store.FS, store.Roots.State); err != nil {
		t.Fatal(err)
	}
	lifecyclePath := RuntimeLifecyclePath(store.Roots.State)
	lifecycle, err := os.OpenFile(lifecyclePath, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	holder, err := RuntimeLifecycleHolder(lifecycle, "native-predecessor")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	secretFile, secret, err := NewRuntimeUpgradeSecretFile(store.Roots.State)
	if err != nil {
		t.Fatal(err)
	}
	defer secretFile.Close()
	defer secret.Destroy()
	guardFile, childFile, err := newRuntimePrivateSocketFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer guardFile.Close()
	defer childFile.Close()
	label := fmt.Sprintf("dev.jacobcx.cq.upgrade-validation.%d.%d", os.Getpid(), time.Now().UnixNano())
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	previous := installer.RuntimeArtifact{Path: os.Args[0], SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Version: "fixture", ProtocolVersion: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=TestRuntimeUpgradeGuardDyingHelperProcess")
	child.Env = append(os.Environ(), "CQ_UPGRADE_DYING_HELPER=1")
	child.ExtraFiles = []*os.File{listener, lifecycle, childFile, secretFile}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	receipt := RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "native-recovery", Generation: 1, Phase: "prepared", Previous: previous, Candidate: previous, ListenerIdentity: "tcp|" + address, SupervisorPID: child.Process.Pid}
	for _, phase := range []string{"prepared", "waiting", "handoff"} {
		receipt.Phase = phase
		if err := store.Save(receipt); err != nil {
			t.Fatal(err)
		}
	}
	receipt.GuardPID = os.Getpid()
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	resume := RuntimeUpgradeResumeV1{SchemaVersion: 1, Receipt: receipt, Release: RuntimeWorkerReleaseV1{ProcessIdentityDigest: "process", ProcessTreeAbsenceProofDigest: "absence", HolderReleaseProofDigest: "release"}, SupervisorHolder: holder, LifecycleIdentity: digest, JobTarget: target, PauseStartedUnixNano: time.Now().UnixNano()}
	data, _ := json.Marshal(resume)
	if err := fsutil.SecureAtomicWrite(store.FS, filepath.Join(store.Roots.State, "fixture-resume.json"), data); err != nil {
		t.Fatal(err)
	}
	guardFD, err := unix.Dup(int(listener.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- RunRuntimeUpgradeGuardWithFiles(ctx, os.NewFile(uintptr(guardFD), "guard-listener"), guardFile, secretFile, receipt)
	}()
	if err := child.Wait(); err == nil {
		t.Fatal("successor did not die before acknowledgement")
	}
	// Retain the dead control peer until recovery to force recovery while the
	// guard is still reading its previous control connection.
	listener.Close()
	lifecycle.Close()
	if conn, err := net.DialTimeout("tcp", address, time.Second); err != nil {
		t.Fatalf("listener lost after successor death: %v", err)
	} else {
		conn.Close()
	}
	backoffValue := "0"
	if backoff {
		backoffValue = "1"
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>-test.run=TestRuntimeUpgradeGuardRecoveryManagedHelperProcess</string></array><key>EnvironmentVariables</key><dict><key>HOME</key><string>%s</string><key>XDG_CONFIG_HOME</key><string></string><key>XDG_CACHE_HOME</key><string></string><key>CQ_UPGRADE_MANAGED_HELPER</key><string>1</string><key>CQ_UPGRADE_BACKOFF</key><string>%s</string></dict><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>2</integer><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, label, os.Args[0], os.Getenv("HOME"), backoffValue, filepath.Join(store.Roots.State, "managed.log"), filepath.Join(store.Roots.State, "managed.log"))
	plistPath := filepath.Join(store.Roots.State, "validation.plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if output, err := exec.CommandContext(ctx, "launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath).CombinedOutput(); err != nil {
		t.Fatalf("isolated launchd bootstrap: %v %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("launchctl", "bootout", target).CombinedOutput(); err != nil {
			t.Errorf("isolated launchd cleanup: %v %s", err, output)
		}
	})
	readyPath := filepath.Join(store.Roots.State, "managed-ready.json")
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			log, _ := os.ReadFile(filepath.Join(store.Roots.State, "managed.log"))
			t.Fatalf("managed recovery timed out: %s", log)
		case <-time.After(100 * time.Millisecond):
		}
	}
	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/proof")
	if err != nil {
		t.Fatalf("fresh recovered traffic: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(body) != "retained" {
		t.Fatalf("recovered traffic: %d %q %v", response.StatusCode, body, err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Phase != "rolled_back" || loaded.RecoveryAttempts != 1 || loaded.RecoveredSupervisorPID <= 1 || loaded.RecoveredSupervisorPID == receipt.SupervisorPID {
		log, _ := os.ReadFile(filepath.Join(store.Roots.State, "managed.log"))
		t.Fatalf("recovery receipt: %+v %v\nmanaged log: %s", loaded, err, log)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("guard did not retire after rollback acknowledgement")
	}
	t.Logf("native launchd recovery: backoff=%t elapsed=%s original_pid=%d recovered_pid=%d listener=%s", backoff, time.Since(started), receipt.SupervisorPID, loaded.RecoveredSupervisorPID, address)
}

func TestRuntimeUpgradeGuardDyingHelperProcess(t *testing.T) {
	if os.Getenv("CQ_UPGRADE_DYING_HELPER") == "" {
		return
	}
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	var resume RuntimeUpgradeResumeV1
	for count := 0; count < 100; count++ {
		data, err := os.ReadFile(filepath.Join(roots.State, "fixture-resume.json"))
		if err == nil && json.Unmarshal(data, &resume) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resume.Receipt.SupervisorPID != os.Getpid() {
		t.Fatal("fixture ownership missing")
	}
	secret, err := ReadRuntimeUpgradeSecretFile(os.NewFile(6, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer secret.Destroy()
	control, err := net.FileConn(os.NewFile(5, "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	control.SetDeadline(time.Now().Add(5 * time.Second))
	payload, _ := json.Marshal(resume)
	if err := WriteRuntimeControlMessage(control, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "upgrade_prepare", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if frame, err := ReadRuntimeControlMessage(control, NewRuntimeControlReceiver(secret)); err != nil || frame.Kind != "upgrade_guard_ready" {
		t.Fatalf("guard: %+v %v", frame, err)
	}
	if frame, err := ReadRuntimeControlMessage(control, NewRuntimeControlReceiver(secret)); err != nil || frame.Kind != "upgrade_resume" {
		t.Fatalf("resume: %+v %v", frame, err)
	}
	os.Exit(42)
}

func TestRuntimeUpgradeGuardRecoveryManagedHelperProcess(t *testing.T) {
	if os.Getenv("CQ_UPGRADE_MANAGED_HELPER") == "" {
		return
	}
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CQ_UPGRADE_BACKOFF") == "1" {
		marker := filepath.Join(roots.State, "backoff-started")
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
				t.Fatal(err)
			}
			os.Exit(42)
		}
	}
	store := RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	receipt, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := os.Open(RuntimeLifecyclePath(roots.State))
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	holder, err := RuntimeLifecycleHolder(lifecycle, fmt.Sprintf("recovered-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	listenerFile, controlFile, secretFile, resume, err := RecoverRuntimeUpgradeListener(context.Background(), lifecycle, holder, receipt)
	if err != nil {
		t.Fatal(err)
	}
	defer listenerFile.Close()
	defer controlFile.Close()
	defer secretFile.Close()
	secret, err := ReadRuntimeUpgradeSecretFile(secretFile)
	if err != nil {
		t.Fatal(err)
	}
	defer secret.Destroy()
	control, err := net.FileConn(controlFile)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	control.SetDeadline(time.Now().Add(5 * time.Second))
	receiver := NewRuntimeControlReceiver(secret)
	frame, err := ReadRuntimeControlMessage(control, receiver)
	if err != nil || frame.Kind != "upgrade_resume" {
		t.Fatalf("resume: %+v %v", frame, err)
	}
	ready, _ := json.Marshal(RuntimeUpgradeReadyV1{TransactionID: resume.Receipt.TransactionID, Generation: resume.Receipt.Generation, PID: os.Getpid(), ArtifactDigest: resume.Receipt.Previous.SHA256})
	if err := WriteRuntimeControlMessage(control, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "upgrade_ready", Payload: ready}); err != nil {
		t.Fatal(err)
	}
	if frame, err := ReadRuntimeControlMessage(control, receiver); err != nil || frame.Kind != "upgrade_verified" {
		t.Fatalf("verify: %+v %v", frame, err)
	}
	resume.Receipt.Phase = "rolled_back"
	if err := store.Save(resume.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeControlMessage(control, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 2, Kind: "upgrade_commit", Payload: ready}); err != nil {
		t.Fatal(err)
	}
	if frame, err := ReadRuntimeControlMessage(control, receiver); err != nil || frame.Kind != "upgrade_guard_retired" {
		t.Fatalf("retire: %+v %v", frame, err)
	}
	listener, err := net.FileListener(listenerFile)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "listener": listener.Addr().String()})
	if err := os.WriteFile(filepath.Join(roots.State, "managed-ready.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "retained") })); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeUpgradeGuardHelperProcess(t *testing.T) {
	if os.Getenv("CQ_UPGRADE_GUARD_HELPER") != "1" {
		t.Skip("helper process")
	}
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := (RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}).Load()
	if err != nil {
		t.Fatal(err)
	}
	os.NewFile(4, "placeholder").Close()
	if err := RunRuntimeUpgradeGuard(context.Background(), os.NewFile(3, "listener"), receipt); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeUpgradeGuardDeathReportsListenerGap(t *testing.T) {
	store := guardTestStore(t)
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	address := tcp.Addr().String()
	listener, err := tcp.File()
	tcp.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	artifact := installer.RuntimeArtifact{Path: filepath.Join(store.Roots.State, "cq"), SHA256: strings.Repeat("a", 64), Version: "test", ProtocolVersion: 1}
	receipt := RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "guard-death", Generation: 1, Previous: artifact, Candidate: artifact, ListenerIdentity: "tcp|" + address, SupervisorPID: os.Getpid()}
	for _, phase := range []string{"prepared", "waiting", "handoff"} {
		receipt.Phase = phase
		if err := store.Save(receipt); err != nil {
			t.Fatal(err)
		}
	}
	parentFile, childFile, err := NewRuntimeUpgradeControlFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer parentFile.Close()
	defer childFile.Close()
	secretFile, secret, err := NewRuntimeUpgradeSecretFile(store.Roots.State)
	if err != nil {
		t.Fatal(err)
	}
	defer secretFile.Close()
	defer secret.Destroy()
	placeholder, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer placeholder.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	guard := exec.Command(executable, "-test.run=^TestRuntimeUpgradeGuardHelperProcess$")
	guard.Env = append(os.Environ(), "CQ_UPGRADE_GUARD_HELPER=1")
	guard.ExtraFiles = []*os.File{listener, placeholder, childFile, secretFile}
	guard.Stderr = os.Stderr
	if err := guard.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { guard.Process.Kill(); guard.Wait() })
	childFile.Close()
	receipt.GuardPID = guard.Process.Pid
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	parent, err := net.FileConn(parentFile)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	parent.SetDeadline(time.Now().Add(5 * time.Second))
	resume := RuntimeUpgradeResumeV1{SchemaVersion: 1, Receipt: receipt, Release: RuntimeWorkerReleaseV1{ProcessIdentityDigest: "process", ProcessTreeAbsenceProofDigest: "absence", HolderReleaseProofDigest: "release"}}
	payload, _ := json.Marshal(resume)
	if err := WriteRuntimeControlMessage(parent, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "upgrade_prepare", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"upgrade_guard_ready", "upgrade_resume"} {
		frame, err := ReadRuntimeControlMessage(parent, NewRuntimeControlReceiver(secret))
		if err != nil || frame.Kind != kind {
			t.Fatalf("guard handshake %s: %+v %v", kind, frame, err)
		}
	}
	listener.Close()
	parent.Close()
	parentFile.Close()
	connected, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connected.Close()
	if err := guard.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	guard.Wait()
	connected, err = net.DialTimeout("tcp", address, time.Second)
	if connected != nil {
		connected.Close()
	}
	if !errors.Is(err, unix.ECONNREFUSED) {
		t.Fatalf("expected explicit last-descriptor gap after guard death, got %v", err)
	}
	durable, err := store.Load()
	if err != nil || durable.Phase != "handoff" {
		t.Fatalf("guard death fabricated commit: %+v %v", durable, err)
	}
	t.Log("guard plus successor death loses final socket descriptor; refused connect confirmed, no false seamless claim")
}

func TestRuntimeUpgradeRecoveryRightsHandlesFragmentedStream(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "cq-rights-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "rights.sock")
	server, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := server.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	accepted.SetDeadline(time.Now().Add(time.Second))
	descriptor, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer descriptor.Close()
	payload := []byte(`{"transaction_id":"fragmented"}`)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	sent := make(chan error, 1)
	go func() {
		_, _, err := client.WriteMsgUnix(header[:2], unix.UnixRights(int(descriptor.Fd())), nil)
		if err == nil {
			_, err = client.Write(append(header[2:], payload...))
		}
		sent <- err
	}()
	decoded, rights, err := runtimeUpgradeReadRights(accepted, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rights[0])
	if string(decoded) != string(payload) {
		t.Fatalf("fragmented payload lost: %q", decoded)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}
