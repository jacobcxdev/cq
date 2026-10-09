//go:build darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

type darwinUpgradeExecEvidence struct {
	PID       int
	Listener  string
	Lifecycle [32]byte
	ExecError bool
}

func TestDarwinRuntimeUpgradeRetainsPIDAndListener(t *testing.T) {
	before, after := darwinUpgradeExecFixture(t, "exec")
	if before.PID != after.PID || before.Listener != after.Listener || before.Lifecycle != after.Lifecycle || after.ExecError {
		t.Fatalf("exec lost managed identity or capabilities: before=%+v after=%+v", before, after)
	}
}
func TestDarwinRuntimeUpgradeExecFailureResumesOldRuntime(t *testing.T) {
	before, after := darwinUpgradeExecFixture(t, "exec-fail")
	if before.PID != after.PID || before.Listener != after.Listener || before.Lifecycle != after.Lifecycle || !after.ExecError {
		t.Fatalf("failed exec lost predecessor: before=%+v after=%+v", before, after)
	}
}

func darwinUpgradeExecFixture(t *testing.T, mode string) (darwinUpgradeExecEvidence, darwinUpgradeExecEvidence) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	file, err := listener.File()
	listener.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lifecycle, err := os.OpenFile(filepath.Join(t.TempDir(), "lifecycle"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	control, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	secret, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer secret.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=TestDarwinRuntimeUpgradeExecHelperProcess")
	command.Env = append(os.Environ(), "CQ_UPGRADE_EXEC_TEST="+mode)
	command.ExtraFiles = []*os.File{file, lifecycle, control, secret}
	command.Stderr = os.Stderr
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	decoder := json.NewDecoder(bufio.NewReader(output))
	var before, after darwinUpgradeExecEvidence
	if err := decoder.Decode(&before); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&after); err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + after.Listener + "/proof")
	if err != nil {
		t.Fatalf("fresh traffic failed after real exec: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "retained" {
		t.Fatalf("fresh traffic: %d %q %v", response.StatusCode, body, err)
	}
	return before, after
}

func TestDarwinRuntimeUpgradeExecHelperProcess(t *testing.T) {
	mode := os.Getenv("CQ_UPGRADE_EXEC_TEST")
	if mode == "" {
		return
	}
	resumed := false
	fds := []uintptr{3, 4, 5, 6}
	for index, arg := range os.Args {
		if arg == "resume-record" {
			resumed = true
			if len(os.Args)-index != 5 {
				t.Fatal("exec did not pass private inherited descriptor numbers")
			}
			for n := range fds {
				fd, err := strconv.Atoi(os.Args[index+n+1])
				if err != nil || fd < 100 {
					t.Fatal("exec overwrote runtime descriptor range")
				}
				fds[n] = uintptr(fd)
			}
		}
	}
	listenerFile := os.NewFile(fds[0], "listener")
	lifecycle := os.NewFile(fds[1], "lifecycle")
	control := os.NewFile(fds[2], "control")
	secret := os.NewFile(fds[3], "secret")
	listener, err := net.FileListener(listenerFile)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := proxy.RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	evidence := darwinUpgradeExecEvidence{PID: os.Getpid(), Listener: listener.Addr().String(), Lifecycle: digest}
	if !resumed {
		if err := json.NewEncoder(os.Stdout).Encode(evidence); err != nil {
			t.Fatal(err)
		}
		executable := os.Args[0]
		if mode == "exec-fail" {
			executable = filepath.Join(os.TempDir(), "cq-upgrade-executable-does-not-exist")
		}
		err = darwinUpgradeExecFiles(context.Background(), executable, []*os.File{listenerFile, lifecycle, control, secret}, []string{os.Args[0], "-test.run=TestDarwinRuntimeUpgradeExecHelperProcess", "--", "resume-record"})
		if err == nil {
			t.Fatal("exec unexpectedly returned success")
		}
		if mode != "exec-fail" {
			t.Fatal(err)
		}
		evidence.ExecError = true
	}
	if err := json.NewEncoder(os.Stdout).Encode(evidence); err != nil {
		t.Fatal(err)
	}
	if err := http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "retained") })); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinRuntimeUpgradeRejectsWrongDescriptorIdentity(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lifecycle, err := os.OpenFile(filepath.Join(t.TempDir(), "lifecycle"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	holder, err := proxy.RuntimeLifecycleHolder(lifecycle, "native-test")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := proxy.RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	artifact := proxyUpgradeArtifactFixture(t)
	payload := proxy.RuntimeUpgradeResumeV1{SchemaVersion: 1, Receipt: proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "native-test", Generation: 1, Phase: "handoff", Previous: artifact, Candidate: artifact, ListenerIdentity: "tcp|" + listener.Addr().String(), SupervisorPID: os.Getpid()}, Release: proxy.RuntimeWorkerReleaseV1{ProcessIdentityDigest: "process", ProcessTreeAbsenceProofDigest: "absence", HolderReleaseProofDigest: "release"}, SupervisorHolder: holder, LifecycleIdentity: digest}
	if err := payload.ValidateFiles(file, lifecycle); err != nil {
		t.Fatal(err)
	}
	other, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherFile, err := other.File()
	if err != nil {
		t.Fatal(err)
	}
	defer otherFile.Close()
	if err := payload.ValidateFiles(otherFile, lifecycle); !errors.Is(err, proxy.ErrRuntimeUpgradeReceipt) {
		t.Fatalf("substituted listener: %v", err)
	}
	wrong, err := os.OpenFile(filepath.Join(t.TempDir(), "wrong"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if err := payload.ValidateFiles(file, wrong); !errors.Is(err, proxy.ErrRuntimeUpgradeReceipt) {
		t.Fatalf("substituted lifecycle: %v", err)
	}
}

func proxyUpgradeArtifactFixture(t *testing.T) installer.RuntimeArtifact {
	t.Helper()
	return installer.RuntimeArtifact{Path: filepath.Join(t.TempDir(), "cq"), Version: "native-test", SHA256: strings.Repeat("a", 64), ProtocolVersion: 1}
}

func TestDarwinRuntimeUpgradeSetupFailureKeepsRollbackProof(t *testing.T) {
	resume := proxy.RuntimeUpgradeResumeV1{Release: proxy.RuntimeWorkerReleaseV1{ProcessIdentityDigest: "previous", ProcessTreeAbsenceProofDigest: "absent", HolderReleaseProofDigest: "released"}, WorkerSequence: 7, PreviousCheckpointDigest: strings.Repeat("a", 64)}
	release, sequence, checkpoint, err := darwinRuntimeUpgradeRollbackState(nil, resume)
	if err != nil || release != resume.Release || sequence != resume.WorkerSequence || checkpoint != resume.PreviousCheckpointDigest {
		t.Fatalf("setup failure discarded proven predecessor release: %+v %d %q %v", release, sequence, checkpoint, err)
	}
}

func TestDarwinRuntimeUpgradeRejectsInvalidInheritedNumbers(t *testing.T) {
	for _, args := range [][]string{{"--runtime-upgrade-resume"}, {"--runtime-upgrade-resume", "3", "4", "5", "6"}, {"--runtime-upgrade-resume", "100", "100", "102", "103"}, {"--runtime-upgrade-resume", "100", "101", "102", "1048577"}, {"--runtime-upgrade-resume", "100", "101", "102", "x"}} {
		handled, err := runDarwinRuntimeUpgradeEntry(args)
		if !handled || !errors.Is(err, proxy.ErrRuntimeUpgradeReceipt) {
			t.Fatalf("invalid inherited descriptors accepted: %v %v", args, err)
		}
	}
}
