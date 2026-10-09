//go:build darwin

package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/unix"
)

type RuntimeUpgradeReadyV1 struct {
	TransactionID  string `json:"transaction_id"`
	Generation     uint64 `json:"generation"`
	PID            int    `json:"pid"`
	ArtifactDigest string `json:"artifact_digest"`
}
type runtimeUpgradeRecoveryRequest struct {
	Retire            bool                 `json:"retire,omitempty"`
	TransactionID     string               `json:"transaction_id"`
	Generation        uint64               `json:"generation"`
	Holder            LifecycleHolderProof `json:"holder"`
	LifecycleIdentity [32]byte             `json:"lifecycle_identity"`
}
type runtimeUpgradeGuardRecovery struct {
	peer    *os.File
	control net.Conn
	resume  RuntimeUpgradeResumeV1
}
type runtimeUpgradeGuardRead struct {
	frame RuntimeControlFrameV1
	err   error
}

func DecodeRuntimeUpgradePayload(payload []byte, value any) error {
	if len(payload) == 0 || len(payload) > RuntimeControlFrameLimit {
		return ErrRuntimeUpgradeReceipt
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrRuntimeUpgradeReceipt
	}
	return nil
}

func RuntimeUpgradeGuardSocketPath(state string) string { return filepath.Join(state, "upgrade.sock") }

func NewRuntimeUpgradeControlFiles() (*os.File, *os.File, error) {
	return newRuntimePrivateSocketFiles()
}

func RunRuntimeUpgradeGuard(ctx context.Context, listener *os.File, receipt RuntimeUpgradeReceiptV1) error {
	control := os.NewFile(RuntimeControlFD, "upgrade-guard-control")
	secret := os.NewFile(RuntimeSecretFD, "upgrade-guard-secret")
	defer control.Close()
	defer secret.Close()
	guard, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return RunRuntimeUpgradeGuardWithFiles(guard, listener, control, secret, receipt)
}

func RunRuntimeUpgradeGuardWithFiles(ctx context.Context, listener, controlFile, secretFile *os.File, receipt RuntimeUpgradeReceiptV1) error {
	if ctx == nil || listener == nil || controlFile == nil || secretFile == nil {
		return ErrRuntimeUpgradeReceipt
	}
	defer listener.Close()
	secret, err := ReadRuntimeUpgradeSecretFile(secretFile)
	if err != nil {
		return err
	}
	defer secret.Destroy()
	control, err := net.FileConn(controlFile)
	if err != nil {
		return err
	}
	defer func() {
		if control != nil {
			control.Close()
		}
	}()
	control.SetDeadline(time.Now().Add(30 * time.Second))
	frame, err := ReadRuntimeControlMessage(control, NewRuntimeControlReceiver(secret))
	if err != nil || frame.Kind != "upgrade_prepare" {
		return errors.Join(ErrRuntimeUpgradeReceipt, err)
	}
	var resume RuntimeUpgradeResumeV1
	if DecodeRuntimeUpgradePayload(frame.Payload, &resume) != nil || resume.SchemaVersion != 1 || !resume.Release.valid() || resume.Receipt.TransactionID != receipt.TransactionID || resume.Receipt.Generation != receipt.Generation || resume.Receipt.GuardPID != os.Getpid() {
		return ErrRuntimeUpgradeReceipt
	}
	if err := ValidateRuntimeUpgradeListener(listener, resume.Receipt.ListenerIdentity); err != nil {
		return err
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	store := RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	loaded, err := store.Load()
	if err != nil || loaded != resume.Receipt || loaded.Phase != "handoff" {
		return errors.Join(ErrRuntimeUpgradeReceipt, err)
	}
	if err := fsutil.ValidateSecureDirectory(store.FS, roots.State); err != nil {
		return err
	}
	private, err := net.ListenUnix("unix", &net.UnixAddr{Name: RuntimeUpgradeGuardSocketPath(roots.State), Net: "unix"})
	if err != nil {
		return err
	}
	defer private.Close()
	if err := os.Chmod(private.Addr().String(), 0o600); err != nil {
		return err
	}
	recoveries := make(chan runtimeUpgradeGuardRecovery, 1)
	retire := make(chan struct{}, 1)
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		defer func() {
			if recover() != nil {
				cancel()
			}
		}()
		serveRuntimeUpgradeRecovery(guardCtx, private, listener, secretFile, store, resume, recoveries, retire)
	}()
	defer func() { cancel(); private.Close(); <-serveDone }()
	if err := WriteRuntimeControlMessage(control, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "upgrade_guard_ready", Payload: json.RawMessage(`{}`)}); err != nil {
		return err
	}
	control.SetDeadline(time.Time{})
	sendResume := func() error {
		payload, err := json.Marshal(resume)
		if err != nil {
			return err
		}
		return WriteRuntimeControlMessage(control, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 1, Kind: "upgrade_resume", Payload: payload})
	}
	if err := sendResume(); err != nil {
		return err
	}
	receiver := NewRuntimeControlReceiver(secret)
	var retainedPeer *os.File
	defer func() {
		if retainedPeer != nil {
			retainedPeer.Close()
		}
	}()
	verified := false
	for {
		var reads chan runtimeUpgradeGuardRead
		if control != nil {
			reads = make(chan runtimeUpgradeGuardRead, 1)
			connection, currentReceiver := control, receiver
			go func() {
				defer func() {
					if recover() != nil {
						reads <- runtimeUpgradeGuardRead{err: ErrRuntimeUpgradeReceipt}
					}
				}()
				frame, err := ReadRuntimeControlMessage(connection, currentReceiver)
				reads <- runtimeUpgradeGuardRead{frame: frame, err: err}
			}()
		}
		select {
		case <-retire:
			listener.Close()
			return nil
		case <-guardCtx.Done():
			if control != nil {
				control.Close()
			}
			return guardCtx.Err()
		case recovered := <-recoveries:
			if control != nil {
				control.Close()
			}
			if retainedPeer != nil {
				retainedPeer.Close()
			}
			retainedPeer = recovered.peer
			control = recovered.control
			resume = recovered.resume
			receiver = NewRuntimeControlReceiver(secret)
			verified = false
			if err := sendResume(); err != nil {
				fmt.Fprintf(os.Stderr, "cq: recovered upgrade control resume failed: %v\n", err)
				control.Close()
				control = nil
			}
		case result := <-reads:
			if result.err != nil {
				control.Close()
				control = nil
				continue
			}
			var ready RuntimeUpgradeReadyV1
			var rollback RuntimeUpgradeResumeV1
			if result.frame.Kind == "upgrade_rollback" {
				if DecodeRuntimeUpgradePayload(result.frame.Payload, &rollback) != nil || !rollback.Release.valid() || rollback.SchemaVersion != 1 || rollback.SupervisorHolder != resume.SupervisorHolder || rollback.LifecycleIdentity != resume.LifecycleIdentity || rollback.JobTarget != resume.JobTarget || !rollback.Recovery {
					return ErrRuntimeUpgradeReceipt
				}
				ready = RuntimeUpgradeReadyV1{TransactionID: rollback.Receipt.TransactionID, Generation: rollback.Receipt.Generation, PID: os.Getpid()}
				ready.PID = rollback.Receipt.SupervisorPID
				if rollback.Receipt.RecoveredSupervisorPID != 0 {
					ready.PID = rollback.Receipt.RecoveredSupervisorPID
				}
			} else if DecodeRuntimeUpgradePayload(result.frame.Payload, &ready) != nil {
				return ErrRuntimeUpgradeReceipt
			}
			if ready.TransactionID != resume.Receipt.TransactionID || ready.Generation != resume.Receipt.Generation {
				return ErrRuntimeUpgradeReceipt
			}
			loaded, err := store.Load()
			if err != nil {
				return err
			}
			pid := loaded.SupervisorPID
			if loaded.RecoveredSupervisorPID != 0 {
				pid = loaded.RecoveredSupervisorPID
			}
			if ready.PID != pid {
				return ErrRuntimeUpgradeReceipt
			}
			switch result.frame.Kind {
			case "upgrade_abort":
				return nil
			case "upgrade_rollback":
				if loaded.Phase != "rolling_back" {
					return ErrRuntimeUpgradeReceipt
				}
				if rollback.Receipt != loaded {
					return ErrRuntimeUpgradeReceipt
				}
				resume = rollback
				receiver = NewRuntimeControlReceiver(secret)
				verified = false
				if err := sendResume(); err != nil {
					return err
				}
			case "upgrade_ready":
				if retainedPeer != nil {
					retainedPeer.Close()
					retainedPeer = nil
				}
				digest := loaded.Candidate.SHA256
				phase := "verifying"
				if resume.Recovery {
					digest = loaded.Previous.SHA256
					phase = "rolling_back"
				}
				if loaded.Phase != phase || ready.ArtifactDigest != digest {
					return ErrRuntimeUpgradeReceipt
				}
				verified = true
				if err := WriteRuntimeControlMessage(control, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 2, Kind: "upgrade_verified", Payload: json.RawMessage(`{}`)}); err != nil {
					return err
				}
			case "upgrade_commit":
				phase := "committed"
				if resume.Recovery {
					phase = "rolled_back"
				}
				if !verified || loaded.Phase != phase {
					return ErrRuntimeUpgradeReceipt
				}
				return WriteRuntimeControlMessage(control, secret, RuntimeControlFrameV1{SchemaVersion: 1, Sequence: 3, Kind: "upgrade_guard_retired", Payload: json.RawMessage(`{}`)})
			default:
				return ErrRuntimeUpgradeReceipt
			}
		}
	}
}

func serveRuntimeUpgradeRecovery(ctx context.Context, private *net.UnixListener, listener, secretFile *os.File, store RuntimeUpgradeStore, original RuntimeUpgradeResumeV1, recoveries chan<- runtimeUpgradeGuardRecovery, retire chan<- struct{}) {
	for {
		private.SetDeadline(time.Now().Add(100 * time.Millisecond))
		conn, err := private.AcceptUnix()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			return
		}
		func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			pid, err := runtimeUpgradePeer(conn)
			if err != nil {
				return
			}
			payload, rights, err := runtimeUpgradeReadRights(conn, 1)
			if err != nil {
				return
			}
			lifecycle := os.NewFile(uintptr(rights[0]), "recovery-lifecycle")
			defer lifecycle.Close()
			var request runtimeUpgradeRecoveryRequest
			if DecodeRuntimeUpgradePayload(payload, &request) != nil {
				return
			}
			loaded, err := store.Load()
			if err != nil || loaded.TransactionID != request.TransactionID || loaded.Generation != request.Generation {
				return
			}
			priorPID := loaded.SupervisorPID
			if loaded.RecoveredSupervisorPID != 0 {
				priorPID = loaded.RecoveredSupervisorPID
			}
			if !errors.Is(unix.Kill(priorPID, 0), unix.ESRCH) {
				return
			}
			digest, err := RuntimeDescriptorIdentityDigest(lifecycle)
			if err != nil || digest != request.LifecycleIdentity {
				return
			}
			holder, err := RuntimeLifecycleHolder(lifecycle, request.Holder.DescriptionID)
			if err != nil || holder != request.Holder || holder.LockIdentity != original.SupervisorHolder.LockIdentity {
				return
			}
			if request.Retire {
				if err := runtimeUpgradeJobAbsent(ctx, original.JobTarget); err != nil {
					return
				}
				if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					return
				}
				if _, err := conn.Write([]byte{1}); err != nil {
					return
				}
				select {
				case retire <- struct{}{}:
				case <-ctx.Done():
				}
				return
			}
			if loaded.terminal() || loaded.RecoveryAttempts >= 3 {
				return
			}
			managed, err := runtimeUpgradeManagedPeer(ctx, conn, original.JobTarget)
			if err != nil || managed != pid {
				return
			}
			// The dead supervisor's control EOF must also release any child
			// worker's lifecycle holder before a restarted owner can boot.
			if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				return
			}
			if err := unix.Flock(int(lifecycle.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
				return
			}
			absence := sha256.Sum256([]byte(fmt.Sprintf("dead-supervisor:%d:lifecycle:%v", priorPID, holder.LockIdentity)))
			releaseDigest := hex.EncodeToString(absence[:])
			loaded.Phase = "rolling_back"
			loaded.RecoveryAttempts++
			loaded.RecoveredSupervisorPID = pid
			loaded.ErrorCode = "successor_died_before_ack"
			if err := store.Save(loaded); err != nil {
				return
			}
			resume := original
			resume.Receipt = loaded
			resume.Recovery = true
			resume.Release = RuntimeWorkerReleaseV1{ProcessIdentityDigest: releaseDigest, ProcessTreeAbsenceProofDigest: releaseDigest, HolderReleaseProofDigest: releaseDigest}
			resume.SupervisorHolder = holder
			resume.LifecycleIdentity = digest
			guardFile, childFile, err := newRuntimePrivateSocketFiles()
			if err != nil {
				return
			}
			defer guardFile.Close()
			peerTransferred := false
			defer func() {
				if !peerTransferred {
					childFile.Close()
				}
			}()
			control, err := net.FileConn(guardFile)
			if err != nil {
				return
			}
			payload, err = json.Marshal(resume)
			if err != nil {
				control.Close()
				return
			}
			if err := runtimeUpgradeWriteRights(conn, payload, []int{int(listener.Fd()), int(childFile.Fd()), int(secretFile.Fd())}); err != nil {
				control.Close()
				return
			}
			select {
			case recoveries <- runtimeUpgradeGuardRecovery{control: control, peer: childFile, resume: resume}:
				peerTransferred = true
			case <-ctx.Done():
				control.Close()
			}
		}()
	}
}

var runtimeUpgradeJobTargetPattern = regexp.MustCompile(`^gui/[0-9]+/[a-zA-Z0-9_.-]+$`)

func runtimeUpgradePeer(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	pid := 0
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		credential, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil || credential.Uid != uint32(os.Geteuid()) {
			peerErr = ErrRuntimeUpgradeReceipt
			return
		}
		pid, peerErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err != nil || peerErr != nil || pid <= 1 {
		return 0, ErrRuntimeUpgradeReceipt
	}
	return pid, nil
}
func runtimeUpgradeManagedPeer(ctx context.Context, conn *net.UnixConn, target string) (int, error) {
	if !runtimeUpgradeJobTargetPattern.MatchString(target) || !strings.HasPrefix(target, "gui/"+strconv.Itoa(os.Geteuid())+"/") {
		return 0, ErrRuntimeUpgradeReceipt
	}
	pid, err := runtimeUpgradePeer(conn)
	if err != nil {
		return 0, err
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probe, "launchctl", "print", target).Output()
	if err != nil || len(output) > 128<<10 {
		return 0, ErrRuntimeUpgradeReceipt
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			managed, err := strconv.Atoi(strings.TrimPrefix(line, "pid = "))
			if err == nil && managed == pid {
				return pid, nil
			}
			break
		}
	}
	return 0, ErrRuntimeUpgradeReceipt
}
func runtimeUpgradeJobAbsent(ctx context.Context, target string) error {
	if !runtimeUpgradeJobTargetPattern.MatchString(target) || !strings.HasPrefix(target, "gui/"+strconv.Itoa(os.Geteuid())+"/") {
		return ErrRuntimeUpgradeReceipt
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := exec.CommandContext(probe, "launchctl", "print", target).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == 3 || exit.ExitCode() == 113) {
		return nil
	}
	return ErrRuntimeUpgradeReceipt
}

// RetireRuntimeUpgradeGuard releases its listener only after registered jobs and
// all lifecycle owners have gone. It never signals a PID inferred from a journal.
func RetireRuntimeUpgradeGuard(ctx context.Context, lifecycle *os.File, receipt RuntimeUpgradeReceiptV1) error {
	if lifecycle == nil {
		return ErrRuntimeUpgradeReceipt
	}
	if receipt.GuardPID == 0 || errors.Is(unix.Kill(receipt.GuardPID, 0), unix.ESRCH) {
		return nil
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	if err := fsutil.ValidateSecureDirectory(fsutil.OSFileSystem{}, roots.State); err != nil {
		return err
	}
	path := RuntimeUpgradeGuardSocketPath(roots.State)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return ErrRuntimeUpgradeReceipt
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer connection.Close()
	deadline := time.Now().Add(5 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	connection.SetDeadline(deadline)
	pid, err := runtimeUpgradePeer(connection)
	if err != nil || pid != receipt.GuardPID {
		return ErrRuntimeUpgradeReceipt
	}
	holder, err := RuntimeLifecycleHolder(lifecycle, "retirement")
	if err != nil {
		return err
	}
	digest, err := RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		return err
	}
	request, _ := json.Marshal(runtimeUpgradeRecoveryRequest{Retire: true, TransactionID: receipt.TransactionID, Generation: receipt.Generation, Holder: holder, LifecycleIdentity: digest})
	if err := runtimeUpgradeWriteRights(connection, request, []int{int(lifecycle.Fd())}); err != nil {
		return err
	}
	var ack [1]byte
	if _, err := io.ReadFull(connection, ack[:]); err != nil {
		return err
	}
	if ack[0] != 1 {
		return ErrRuntimeUpgradeReceipt
	}
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return ErrRuntimeUpgradeReceipt
}

func runtimeUpgradeRights(oob []byte, want int) ([]int, error) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var rights []int
	for _, message := range messages {
		received, err := unix.ParseUnixRights(&message)
		if err != nil {
			for _, fd := range rights {
				unix.Close(fd)
			}
			return nil, err
		}
		rights = append(rights, received...)
	}
	if len(rights) != want {
		for _, fd := range rights {
			unix.Close(fd)
		}
		return nil, ErrRuntimeUpgradeReceipt
	}
	for _, fd := range rights {
		unix.CloseOnExec(fd)
	}
	return rights, nil
}

func RecoverRuntimeUpgradeListener(ctx context.Context, lifecycle *os.File, holder LifecycleHolderProof, receipt RuntimeUpgradeReceiptV1) (*os.File, *os.File, *os.File, RuntimeUpgradeResumeV1, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, err
	}
	if err := fsutil.ValidateSecureDirectory(fsutil.OSFileSystem{}, roots.State); err != nil {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, err
	}
	path := RuntimeUpgradeGuardSocketPath(roots.State)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, ErrRuntimeUpgradeReceipt
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, err
	}
	defer connection.Close()
	deadline := time.Now().Add(5 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	connection.SetDeadline(deadline)
	pid, err := runtimeUpgradePeer(connection)
	if err != nil || pid != receipt.GuardPID {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, ErrRuntimeUpgradeReceipt
	}
	digest, err := RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, err
	}
	request, _ := json.Marshal(runtimeUpgradeRecoveryRequest{TransactionID: receipt.TransactionID, Generation: receipt.Generation, Holder: holder, LifecycleIdentity: digest})
	if err := runtimeUpgradeWriteRights(connection, request, []int{int(lifecycle.Fd())}); err != nil {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, err
	}
	payload, rights, err := runtimeUpgradeReadRights(connection, 3)
	if err != nil {
		return nil, nil, nil, RuntimeUpgradeResumeV1{}, err
	}
	listener := os.NewFile(uintptr(rights[0]), "recovered-listener")
	control := os.NewFile(uintptr(rights[1]), "recovered-control")
	secret := os.NewFile(uintptr(rights[2]), "recovered-secret")
	var resume RuntimeUpgradeResumeV1
	if DecodeRuntimeUpgradePayload(payload, &resume) != nil || resume.Receipt.RecoveredSupervisorPID != os.Getpid() || resume.ValidateFiles(listener, lifecycle) != nil {
		listener.Close()
		control.Close()
		secret.Close()
		return nil, nil, nil, resume, ErrRuntimeUpgradeReceipt
	}
	return listener, control, secret, resume, nil
}

// SCM_RIGHTS rides the first bytes of a bounded stream frame. Neither sendmsg
// nor recvmsg guarantees an entire JSON payload in one call.
func runtimeUpgradeWriteRights(conn *net.UnixConn, payload []byte, rights []int) error {
	if len(payload) == 0 || len(payload) > 64<<10 {
		return ErrRuntimeUpgradeReceipt
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)
	n, _, err := conn.WriteMsgUnix(frame, unix.UnixRights(rights...), nil)
	if err != nil {
		return err
	}
	for n < len(frame) {
		written, err := conn.Write(frame[n:])
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		n += written
	}
	return nil
}
func runtimeUpgradeReadRights(conn *net.UnixConn, want int) (payload []byte, rights []int, returnErr error) {
	header := make([]byte, 4)
	oob := make([]byte, unix.CmsgSpace(want*4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(header, oob)
	rights, rightsErr := runtimeUpgradeRights(oob[:oobn], want)
	defer func() {
		if returnErr != nil {
			for _, fd := range rights {
				unix.Close(fd)
			}
			rights = nil
		}
	}()
	if err != nil || rightsErr != nil || flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 {
		return nil, rights, errors.Join(ErrRuntimeUpgradeReceipt, err, rightsErr)
	}
	if _, err := io.ReadFull(conn, header[n:]); err != nil {
		return nil, rights, err
	}
	size := binary.BigEndian.Uint32(header)
	if size == 0 || size > 64<<10 {
		return nil, rights, ErrRuntimeUpgradeReceipt
	}
	payload = make([]byte, size)
	_, err = io.ReadFull(conn, payload)
	return payload, rights, err
}
