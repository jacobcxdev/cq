//go:build darwin

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/unix"
)

type darwinUpgradeHandoffKey struct{}
type darwinUpgradeHandoff struct {
	lifecycle  *os.File
	holder     proxy.LifecycleHolderProof
	release    proxy.RuntimeWorkerReleaseV1
	sequence   uint64
	checkpoint string
	jobTarget  string
}

func init() {
	runPlatformRuntimeUpgradeEntry = runDarwinRuntimeUpgradeEntry
	configurePlatformRuntimeUpgrade = configureDarwinRuntimeUpgrade
}

func configureDarwinRuntimeUpgrade(ctx context.Context, supervisor *proxy.RuntimeSupervisor, lifecycle *os.File, holder proxy.LifecycleHolderProof) error {
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	ownership := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots}
	record, err := ownership.Load()
	if err != nil && !errors.Is(err, installstate.ErrNotInstalled) {
		return err
	}
	if err == nil && record.Owner != installstate.OwnerHomebrew {
		return nil
	}
	executable, err := currentUnixRuntimeExecutable()
	if err != nil {
		return err
	}
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	previous, err := darwinExistingRuntimeArtifact(ctx, artifacts, executable)
	if errors.Is(err, ErrServiceUpgradeMaintenance) {
		return nil
	}
	if err != nil {
		return err
	}
	// Protocol-aware services must already run the retained executable. Adopting
	// a legacy package path needs the explicit first-install maintenance step.
	if executable != previous.Path {
		return nil
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), proxyAgentLabel)
	if !darwinUpgradeJobHasPID(ctx, target, os.Getpid()) {
		return nil
	}
	_, err = proxy.NewRuntimeUpgradeController(supervisor, proxy.RuntimeUpgradeControllerOptions{
		Store: proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}, Artifacts: artifacts, Ownership: ownership, Previous: previous,
		Handoff: func(handoffCtx context.Context, receipt proxy.RuntimeUpgradeReceiptV1, release proxy.RuntimeWorkerReleaseV1) error {
			file, sequence, checkpoint, err := supervisor.RuntimeUpgradeHandoffSnapshot()
			if err != nil {
				return err
			}
			defer file.Close()
			state := darwinUpgradeHandoff{lifecycle: lifecycle, holder: holder, release: release, sequence: sequence, checkpoint: checkpoint, jobTarget: target}
			return execDarwinRuntimeUpgrade(context.WithValue(handoffCtx, darwinUpgradeHandoffKey{}, state), file, receipt)
		},
	})
	return err
}

func darwinUpgradeJobHasPID(ctx context.Context, target string, pid int) bool {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probe, "launchctl", "print", target).Output()
	if err != nil || len(output) > 128<<10 {
		return false
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == "pid = "+strconv.Itoa(pid) {
			return true
		}
	}
	return false
}

func execDarwinRuntimeUpgrade(ctx context.Context, listener *os.File, receipt proxy.RuntimeUpgradeReceiptV1) error {
	state, ok := ctx.Value(darwinUpgradeHandoffKey{}).(darwinUpgradeHandoff)
	if !ok {
		return proxy.ErrRuntimeUpgradeReceipt
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	if err := artifacts.Verify(ctx, receipt.Candidate); err != nil {
		return err
	}
	digest, err := proxy.RuntimeDescriptorIdentityDigest(state.lifecycle)
	if err != nil {
		return err
	}
	resume := proxy.RuntimeUpgradeResumeV1{SchemaVersion: 1, Receipt: receipt, Release: state.release, SupervisorHolder: state.holder, LifecycleIdentity: digest, JobTarget: state.jobTarget, PauseStartedUnixNano: time.Now().UnixNano() - receipt.AdmissionPauseNanos, WorkerSequence: state.sequence, PreviousCheckpointDigest: state.checkpoint}
	if err := resume.ValidateFiles(listener, state.lifecycle); err != nil {
		return err
	}
	if err := removeStaleDarwinUpgradeSocket(roots.State); err != nil {
		return err
	}
	parent, child, err := proxy.NewRuntimeUpgradeControlFiles()
	if err != nil {
		return err
	}
	defer parent.Close()
	defer child.Close()
	secretFile, secret, err := proxy.NewRuntimeUpgradeSecretFile(roots.State)
	if err != nil {
		return err
	}
	defer secretFile.Close()
	defer secret.Destroy()
	placeholder, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer placeholder.Close()
	guard := exec.Command(receipt.Previous.Path, "--runtime-upgrade-guard", receipt.TransactionID)
	guard.ExtraFiles = []*os.File{listener, placeholder, child, secretFile}
	guard.Stderr = os.Stderr
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := guard.Start(); err != nil {
		return err
	}
	guardDone := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				guardDone <- proxy.ErrRuntimeUpgradeReceipt
			}
		}()
		guardDone <- guard.Wait()
	}()
	abortGuard := true
	defer func() {
		if abortGuard {
			guard.Process.Kill()
			<-guardDone
		}
	}()
	receipt.GuardPID = guard.Process.Pid
	resume.Receipt = receipt
	store := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	if err := store.Save(receipt); err != nil {
		return err
	}
	control, err := net.FileConn(parent)
	if err != nil {
		return err
	}
	defer control.Close()
	deadline := time.Now().Add(30 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	control.SetDeadline(deadline)
	payload, err := json.Marshal(resume)
	if err != nil {
		return err
	}
	if err := proxy.WriteRuntimeControlMessage(control, secret, proxy.RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "upgrade_prepare", Payload: payload}); err != nil {
		return err
	}
	frame, err := proxy.ReadRuntimeControlMessage(control, proxy.NewRuntimeControlReceiver(secret))
	if err != nil || frame.Kind != "upgrade_guard_ready" {
		return errors.Join(proxy.ErrRuntimeUpgradeReceipt, err)
	}
	control.Close()
	if err := artifacts.Verify(ctx, receipt.Candidate); err != nil {
		return err
	}
	err = darwinUpgradeExecFiles(ctx, receipt.Candidate.Path, []*os.File{listener, state.lifecycle, parent, secretFile}, []string{receipt.Candidate.Path, "--runtime-upgrade-resume"})
	// Exec failed in the predecessor. Its listener remains owned here; retire
	// the guard before controller recovery boots the selected previous worker.
	return err
}

func removeStaleDarwinUpgradeSocket(state string) error {
	path := proxy.RuntimeUpgradeGuardSocketPath(state)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return proxy.ErrRuntimeUpgradeReceipt
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		return proxy.ErrRuntimeUpgradeBusy
	}
	if !errors.Is(err, unix.ECONNREFUSED) {
		return err
	}
	return os.Remove(path)
}

func runDarwinRuntimeUpgradeEntry(args []string) (bool, error) {
	if len(args) == 2 && args[0] == "--runtime-upgrade-guard" {
		roots, err := userdirs.Default()
		if err != nil {
			return true, err
		}
		receipt, err := (proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}).Load()
		if err != nil {
			return true, err
		}
		if receipt.TransactionID != args[1] || (receipt.GuardPID != 0 && receipt.GuardPID != os.Getpid()) {
			return true, proxy.ErrRuntimeUpgradeReceipt
		}
		listener := os.NewFile(3, "upgrade-listener")
		placeholder := os.NewFile(4, "upgrade-placeholder")
		defer placeholder.Close()
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return true, proxy.RunRuntimeUpgradeGuard(ctx, listener, receipt)
	}
	if len(args) > 0 && args[0] == "--runtime-upgrade-resume" {
		if len(args) != 5 {
			return true, proxy.ErrRuntimeUpgradeReceipt
		}
		fds := make([]int, 4)
		seen := make(map[int]bool, 4)
		for index, value := range args[1:] {
			fd, err := strconv.Atoi(value)
			if err != nil || fd < 100 || fd > 1<<20 || seen[fd] {
				return true, proxy.ErrRuntimeUpgradeReceipt
			}
			seen[fd] = true
			fds[index] = fd
		}
		files := make([]*os.File, 4)
		for index, fd := range fds {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
				return true, err
			}
			files[index] = os.NewFile(uintptr(fd), "upgrade-inherited")
			if files[index] == nil {
				return true, proxy.ErrRuntimeUpgradeReceipt
			}
			defer files[index].Close()
		}
		return true, runDarwinRuntimeUpgradeResume(files[0], files[1], files[2], files[3])
	}

	// Only launchd's regular startup selects a committed retained runtime. CLI,
	// worker roles and read-only runtime-check must not consume recovery state.
	if len(args) != 2 || args[0] != "proxy" || args[1] != "start" {
		return false, nil
	}
	roots, err := userdirs.Default()
	if err != nil {
		return false, err
	}
	ownership := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots}
	record, err := ownership.Load()
	if errors.Is(err, installstate.ErrNotInstalled) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if record.Owner != installstate.OwnerHomebrew {
		return false, nil
	}
	store := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	receipt, err := store.Load()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if receipt.Phase == "handoff" || receipt.Phase == "verifying" || receipt.Phase == "rolling_back" {
		path, err := proxy.DefaultRuntimeLifecyclePath()
		if err != nil {
			return true, err
		}
		lifecycle, holder, err := openUnixRuntimeLifecycle(path, "supervisor")
		if err != nil {
			return true, err
		}
		defer lifecycle.Close()
		listener, control, secret, _, err := proxy.RecoverRuntimeUpgradeListener(ctx, lifecycle, holder, receipt)
		if err != nil {
			return true, fmt.Errorf("retained listener recovery failed: %w", err)
		}
		defer listener.Close()
		defer control.Close()
		defer secret.Close()
		artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
		if err := artifacts.Verify(ctx, receipt.Previous); err != nil {
			return true, err
		}
		return true, darwinUpgradeExecFiles(ctx, receipt.Previous.Path, []*os.File{listener, lifecycle, control, secret}, []string{receipt.Previous.Path, "--runtime-upgrade-resume"})
	}
	if receipt.ErrorCode == "candidate_release_unproven" || receipt.ErrorCode == "recovery_release_unproven" {
		return true, proxy.ErrRuntimeOwnerReleaseUnproven
	}
	selected := receipt.Previous
	switch receipt.Phase {
	case "committed":
		selected = receipt.Candidate
	case "prepared", "waiting":
		receipt.Phase = "deferred"
		receipt.ErrorCode = "supervisor_restarted_before_handoff"
		if err := store.Save(receipt); err != nil {
			return true, err
		}
	}
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	if err := artifacts.Verify(ctx, selected); err != nil {
		return true, err
	}
	executable, err := currentUnixRuntimeExecutable()
	if err != nil {
		return true, err
	}
	if executable == selected.Path {
		return false, nil
	}
	return true, unix.Exec(selected.Path, append([]string{selected.Path}, args...), os.Environ())
}

func runDarwinRuntimeUpgradeResume(listener, lifecycle, controlFile, secretFile *os.File) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	secret, err := proxy.ReadRuntimeUpgradeSecretFile(secretFile)
	if err != nil {
		return err
	}
	defer secret.Destroy()
	control, err := net.FileConn(controlFile)
	if err != nil {
		return err
	}
	defer control.Close()
	control.SetDeadline(time.Now().Add(30 * time.Second))
	receiver := proxy.NewRuntimeControlReceiver(secret)
	frame, err := proxy.ReadRuntimeControlMessage(control, receiver)
	if err != nil || frame.Kind != "upgrade_resume" {
		return errors.Join(proxy.ErrRuntimeUpgradeReceipt, err)
	}
	var resume proxy.RuntimeUpgradeResumeV1
	if err := proxy.DecodeRuntimeUpgradePayload(frame.Payload, &resume); err != nil {
		return err
	}
	if err := resume.ValidateFiles(listener, lifecycle); err != nil {
		return err
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	store := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	receipt, err := store.Load()
	if err != nil || receipt != resume.Receipt {
		return errors.Join(proxy.ErrRuntimeUpgradeReceipt, err)
	}
	pid := receipt.SupervisorPID
	if receipt.RecoveredSupervisorPID != 0 {
		pid = receipt.RecoveredSupervisorPID
	}
	if pid != os.Getpid() {
		return proxy.ErrRuntimeUpgradeReceipt
	}
	selected := receipt.Candidate
	if resume.Recovery {
		selected = receipt.Previous
	}
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	bootCtx, bootCancel := context.WithTimeout(ctx, 30*time.Second)
	err = artifacts.Verify(bootCtx, selected)
	bootCancel()
	if err != nil {
		return err
	}
	executable, err := currentUnixRuntimeExecutable()
	if err != nil || executable != selected.Path {
		return errors.Join(proxy.ErrRuntimeUpgradeReceipt, err)
	}
	adopted, err := net.FileListener(listener)
	if err != nil {
		return err
	}
	defer adopted.Close()
	sequence := uint64(1)
	committed := false
	var candidateSupervisor *proxy.RuntimeSupervisor
	err = runUnixProxyAdoptedRuntimeWithLifecycle(ctx, adopted, func(serveCtx context.Context, active net.Listener, handler http.Handler) error {
		supervisor, ok := handler.(*proxy.RuntimeSupervisor)
		if !ok {
			return proxy.ErrRuntimeSupervisorUnavailable
		}
		candidateSupervisor = supervisor
		if !supervisor.RuntimeUpgradeBootReady(selected.SHA256) {
			return proxy.ErrRuntimeSupervisorUnavailable
		}
		if !resume.Recovery {
			receipt.Phase = "verifying"
			if err := store.Save(receipt); err != nil {
				return err
			}
		}
		ready := proxy.RuntimeUpgradeReadyV1{TransactionID: receipt.TransactionID, Generation: receipt.Generation, PID: os.Getpid(), ArtifactDigest: selected.SHA256}
		payload, err := json.Marshal(ready)
		if err != nil {
			return err
		}
		if err := proxy.WriteRuntimeControlMessage(control, secret, proxy.RuntimeControlFrameV1{SchemaVersion: 1, Sequence: sequence, Kind: "upgrade_ready", Payload: payload}); err != nil {
			return err
		}
		sequence++
		verified, err := proxy.ReadRuntimeControlMessage(control, receiver)
		if err != nil || verified.Kind != "upgrade_verified" {
			return errors.Join(proxy.ErrRuntimeUpgradeReceipt, err)
		}
		receipt.Phase = "committed"
		receipt.ErrorCode = ""
		if resume.Recovery {
			receipt.Phase = "rolled_back"
			receipt.ErrorCode = "candidate_boot_failed"
		}
		receipt.AdmissionPauseNanos = max(0, time.Now().UnixNano()-resume.PauseStartedUnixNano)
		if err := store.Save(receipt); err != nil {
			return err
		}
		committed = true
		retireErr := proxy.WriteRuntimeControlMessage(control, secret, proxy.RuntimeControlFrameV1{SchemaVersion: 1, Sequence: sequence, Kind: "upgrade_commit", Payload: payload})
		if retireErr == nil {
			retired, readErr := proxy.ReadRuntimeControlMessage(control, receiver)
			retireErr = readErr
			if readErr == nil && retired.Kind != "upgrade_guard_retired" {
				retireErr = proxy.ErrRuntimeUpgradeReceipt
			}
		}
		if retireErr != nil {
			fmt.Fprintf(os.Stderr, "cq: runtime committed; guard retirement acknowledgement failed: %v\n", retireErr)
		}
		control.Close()
		controlFile.Close()
		secretFile.Close()
		listener.Close()
		if retireErr == nil {
			reapDarwinUpgradeGuard(receipt.GuardPID)
		}
		return serveRuntimeSupervisor(serveCtx, active, supervisor)
	}, lifecycle, resume.SupervisorHolder, &resume, func(supervisor *proxy.RuntimeSupervisor) { candidateSupervisor = supervisor })
	if committed {
		return err
	}
	// RunAdopted has joined the failed candidate before this branch. Carry its
	// fresh release proof and checkpoint into predecessor boot; never overlap.
	if loaded, loadErr := store.Load(); loadErr == nil && loaded.TransactionID == receipt.TransactionID {
		receipt = loaded
	}
	receipt.Phase = "rolling_back"
	receipt.ErrorCode = "candidate_boot_failed"
	if saveErr := store.Save(receipt); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	if resume.Recovery || errors.Is(err, proxy.ErrRuntimeOwnerReleaseUnproven) {
		receipt.Phase = "failed"
		receipt.ErrorCode = "recovery_boot_failed"
		if errors.Is(err, proxy.ErrRuntimeOwnerReleaseUnproven) {
			receipt.ErrorCode = "candidate_release_unproven"
			if resume.Recovery {
				receipt.ErrorCode = "recovery_release_unproven"
			}
		}
		return errors.Join(err, store.Save(receipt))
	}
	release, workerSequence, checkpoint, snapshotErr := darwinRuntimeUpgradeRollbackState(candidateSupervisor, resume)
	if snapshotErr != nil {
		receipt.Phase = "failed"
		receipt.ErrorCode = "candidate_release_unproven"
		return errors.Join(err, snapshotErr, store.Save(receipt))
	}
	// The guard authenticates the updated release/checkpoint payload rather than
	// recycling predecessor's release evidence for a stopped candidate.
	resume.Receipt = receipt
	resume.Recovery = true
	resume.Release = release
	resume.WorkerSequence = workerSequence
	resume.PreviousCheckpointDigest = checkpoint
	payload, marshalErr := json.Marshal(resume)
	if marshalErr != nil {
		return errors.Join(err, marshalErr)
	}
	control.SetDeadline(time.Now().Add(30 * time.Second))
	if sendErr := proxy.WriteRuntimeControlMessage(control, secret, proxy.RuntimeControlFrameV1{SchemaVersion: 1, Sequence: sequence, Kind: "upgrade_rollback", Payload: payload}); sendErr != nil {
		return errors.Join(err, sendErr)
	}
	recovery, cancelRecovery := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelRecovery()
	if verifyErr := artifacts.Verify(recovery, receipt.Previous); verifyErr != nil {
		return errors.Join(err, verifyErr)
	}
	return errors.Join(err, darwinUpgradeExecFiles(recovery, receipt.Previous.Path, []*os.File{listener, lifecycle, controlFile, secretFile}, []string{receipt.Previous.Path, "--runtime-upgrade-resume"}))
}

func darwinRuntimeUpgradeRollbackState(supervisor *proxy.RuntimeSupervisor, resume proxy.RuntimeUpgradeResumeV1) (proxy.RuntimeWorkerReleaseV1, uint64, string, error) {
	if supervisor == nil {
		// Setup returned before constructing any candidate owner. The incoming
		// authenticated predecessor proof and checkpoint remain authoritative.
		checkpoint, err := hex.DecodeString(resume.PreviousCheckpointDigest)
		if err != nil || len(checkpoint) != 32 || resume.WorkerSequence == 0 || resume.Release.ProcessIdentityDigest == "" || resume.Release.ProcessTreeAbsenceProofDigest == "" || resume.Release.HolderReleaseProofDigest == "" {
			return proxy.RuntimeWorkerReleaseV1{}, 0, "", proxy.ErrRuntimeOwnerReleaseUnproven
		}
		return resume.Release, resume.WorkerSequence, resume.PreviousCheckpointDigest, nil
	}
	return supervisor.RuntimeUpgradeRollbackSnapshot()
}

func reapDarwinUpgradeGuard(pid int) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status unix.WaitStatus
		reaped, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
		if reaped == pid || errors.Is(err, unix.ECHILD) {
			return
		}
		if err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
