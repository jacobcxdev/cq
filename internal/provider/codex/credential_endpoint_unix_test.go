//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

type endpointSidecarFixture struct {
	Version         int                      `json:"version"`
	ProtocolVersion int                      `json:"protocol_version"`
	Generation      string                   `json:"generation"`
	State           string                   `json:"state"`
	TemporaryName   string                   `json:"temporary_name"`
	FinalName       string                   `json:"final_name"`
	Device          uint64                   `json:"device"`
	Inode           uint64                   `json:"inode"`
	UID             uint64                   `json:"uid"`
	Type            string                   `json:"type"`
	Mode            uint32                   `json:"mode"`
	LockDevice      uint64                   `json:"lock_device"`
	LockInode       uint64                   `json:"lock_inode"`
	LockLinks       uint64                   `json:"lock_links"`
	Previous        *endpointPreviousFixture `json:"previous,omitempty"`
}

type endpointPreviousFixture struct {
	Generation    string `json:"generation"`
	TemporaryName string `json:"temporary_name"`
	Device        uint64 `json:"device"`
	Inode         uint64 `json:"inode"`
	UID           uint64 `json:"uid"`
	Type          string `json:"type"`
	Mode          uint32 `json:"mode"`
}

type countingLegacyCredentialEndpoint struct{ calls atomic.Int32 }

func (endpoint *countingLegacyCredentialEndpoint) Ping(_ struct{}, _ *struct{}) error {
	endpoint.calls.Add(1)
	return nil
}

func TestCredentialEndpointSidecarDecoderRejectsUnprovenState(t *testing.T) {
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	valid := endpointSidecarFixture{
		Version: 1, ProtocolVersion: 1, Generation: "generation-1", State: "published",
		TemporaryName: ".credential-generation-1.sock", FinalName: "credential.sock",
		Device: 11, Inode: 12, UID: uint64(os.Geteuid()), Type: "socket", Mode: 0o600,
		LockDevice: 21, LockInode: 22, LockLinks: 1,
	}
	validJSON, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if sidecar, err := decodeCredentialEndpointSidecar(validJSON, path); err != nil || sidecar.Generation != "generation-1" {
		t.Fatalf("valid sidecar = %+v, %v", sidecar, err)
	}
	mutated := func(change func(*endpointSidecarFixture)) []byte {
		t.Helper()
		fixture := valid
		change(&fixture)
		data, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	prepared := valid
	prepared.Generation = "generation-2"
	prepared.State = "prepared"
	prepared.TemporaryName = ".credential-generation-2.sock"
	prepared.Device = 31
	prepared.Inode = 32
	prepared.Previous = &endpointPreviousFixture{
		Generation: valid.Generation, TemporaryName: valid.TemporaryName,
		Device: valid.Device, Inode: valid.Inode, UID: valid.UID, Type: valid.Type, Mode: valid.Mode,
	}
	preparedJSON, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	caseFoldedPrevious := append(append([]byte(nil), preparedJSON[:len(preparedJSON)-2]...), []byte(",\"Generation\":\"generation-1\"}}")...)

	invalid := map[string][]byte{
		"empty":                       nil,
		"malformed":                   []byte("{"),
		"trailing":                    append(append([]byte(nil), validJSON...), []byte(" {}")...),
		"unknown field":               append(append([]byte(nil), validJSON[:len(validJSON)-1]...), []byte(",\"future\":true}")...),
		"duplicate field":             append(append([]byte(nil), validJSON[:len(validJSON)-1]...), []byte(",\"generation\":\"generation-2\"}")...),
		"case-folded duplicate field": append(append([]byte(nil), validJSON[:len(validJSON)-1]...), []byte(",\"Version\":1}")...),
		"case-folded previous field":  caseFoldedPrevious,
		"wrong version":               mutated(func(fixture *endpointSidecarFixture) { fixture.Version = 2 }),
		"wrong final":                 mutated(func(fixture *endpointSidecarFixture) { fixture.FinalName = "other.sock" }),
		"wrong type":                  mutated(func(fixture *endpointSidecarFixture) { fixture.Type = "file" }),
		"permissive mode":             mutated(func(fixture *endpointSidecarFixture) { fixture.Mode = 0o644 }),
	}
	for name, data := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCredentialEndpointSidecar(data, path); !errors.Is(err, ErrCredentialOwnerStale) {
				t.Fatalf("decode error = %v, want typed stale proof", err)
			}
		})
	}
}

func TestCredentialEndpointPathRequiresAbsoluteName(t *testing.T) {
	if _, _, err := validateCredentialEndpointPath(filepath.Join("state", "credential.sock")); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("validate relative path error = %v, want unsafe path", err)
	}
}

func TestCredentialEndpointProbeRetriesConnectionRefusal(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	server := rpc.NewServer()
	if err := server.RegisterName("CredentialEndpoint", &credentialEndpointRPC{generation: "generation-1"}); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan struct{})
	defer func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
		<-serveDone
	}()

	attempts := 0
	dial := func(_, _ string, _ time.Duration) (net.Conn, error) {
		attempts++
		if attempts == 1 {
			return nil, syscall.ECONNREFUSED
		}
		go func() {
			defer close(serveDone)
			server.ServeConn(serverConn)
		}()
		return clientConn, nil
	}
	client, protocol, generation, err := probeCredentialOwnerWithDial("ignored", 100*time.Millisecond, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if attempts != 2 || protocol != credentialOwnerVersioned || generation != "generation-1" {
		t.Fatalf("probe = attempts %d, protocol %d, generation %q", attempts, protocol, generation)
	}
}

func TestCredentialEndpointPublishesDurableProofAndLifetimeLock(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Owner() {
		t.Fatal("control did not own endpoint")
	}

	socketInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	socketStat := socketInfo.Sys().(*syscall.Stat_t)
	sidecarData, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var sidecar endpointSidecarFixture
	if err := json.Unmarshal(sidecarData, &sidecar); err != nil {
		t.Fatal(err)
	}
	if sidecar.Version != 1 || sidecar.ProtocolVersion != 1 || sidecar.Generation == "" || sidecar.State != "published" {
		t.Fatalf("sidecar header = %+v", sidecar)
	}
	if sidecar.FinalName != filepath.Base(path) || sidecar.TemporaryName == "" || sidecar.TemporaryName == sidecar.FinalName {
		t.Fatalf("sidecar paths = %+v", sidecar)
	}
	if sidecar.Device != uint64(socketStat.Dev) || sidecar.Inode != uint64(socketStat.Ino) || sidecar.UID != uint64(socketStat.Uid) || sidecar.Type != "socket" || sidecar.Mode != 0o600 {
		t.Fatalf("sidecar identity = %+v, socket = %+v", sidecar, socketStat)
	}
	lockInfo, err := os.Lstat(credentialEndpointLockPath(path))
	if err != nil {
		t.Fatal(err)
	}
	lockStat := lockInfo.Sys().(*syscall.Stat_t)
	if sidecar.LockDevice != uint64(lockStat.Dev) || sidecar.LockInode != uint64(lockStat.Ino) || sidecar.LockLinks != uint64(lockStat.Nlink) {
		t.Fatalf("sidecar lock identity = %+v, lock = %+v", sidecar, lockStat)
	}
	assertSecureRegularFile(t, credentialEndpointSidecarPath(path), 0o600)
	assertSecureRegularFile(t, credentialEndpointLockPath(path), 0o600)
	lockFD, err := unix.Open(credentialEndpointLockPath(path), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		unix.Close(lockFD)
		t.Fatalf("second flock error = %v, want busy", err)
	}
	if err := unix.Close(lockFD); err != nil {
		t.Fatal(err)
	}

	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after close = %v", err)
	}
	if _, err := os.Lstat(credentialEndpointSidecarPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar after close = %v", err)
	}
	assertSecureRegularFile(t, credentialEndpointLockPath(path), 0o600)
}

func TestCredentialEndpointOrdinaryOpenLeavesExactOrphanUntouched(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	beforeInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}

	control, err := OpenCredentialControl(path, coordinator)
	if control != nil || !errors.Is(err, ErrCredentialOwnerStale) {
		t.Fatalf("OpenCredentialControl = %v, %v, want stale error", control, err)
	}
	afterInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat := beforeInfo.Sys().(*syscall.Stat_t)
	afterStat := afterInfo.Sys().(*syscall.Stat_t)
	if beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino {
		t.Fatal("ordinary open replaced exact orphan")
	}
	afterSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeSidecar) != string(afterSidecar) {
		t.Fatal("ordinary open changed exact orphan sidecar")
	}
}

func TestCredentialEndpointReachableLegacyOwnerFailsClosedWithoutArtifacts(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	server := rpc.NewServer()
	if err := server.RegisterName("CredentialRPC", &credentialRPC{Coordinator: coordinator}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			server.ServeConn(conn)
		}
	}()

	control, openErr := OpenCredentialControl(path, coordinator)
	if control != nil {
		_ = control.Close()
	}
	if control != nil || !errors.Is(openErr, ErrCredentialOwnerStale) {
		t.Fatalf("OpenCredentialControl = %v, %v, want legacy authority failure", control, openErr)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	for _, artifact := range []string{credentialEndpointLockPath(path), credentialEndpointSidecarPath(path)} {
		if _, statErr := os.Lstat(artifact); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("legacy delegation created %s: %v", filepath.Base(artifact), statErr)
		}
	}
}

func TestCredentialEndpointVersionedProofNeverCallsLegacyRPC(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, credentialEndpointLockPath(path))
	if err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	socketStat := socketInfo.Sys().(*syscall.Stat_t)
	lockInfo, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	lockStat := lockInfo.Sys().(*syscall.Stat_t)
	sidecar, err := json.Marshal(endpointSidecarFixture{
		Version: 1, ProtocolVersion: 1, Generation: "generation-1", State: "published",
		TemporaryName: ".cq-credential-published.sock", FinalName: filepath.Base(path),
		Device: uint64(socketStat.Dev), Inode: uint64(socketStat.Ino), UID: uint64(socketStat.Uid), Type: "socket", Mode: 0o600,
		LockDevice: uint64(lockStat.Dev), LockInode: uint64(lockStat.Ino), LockLinks: uint64(lockStat.Nlink),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialEndpointSidecarPath(path), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := &countingLegacyCredentialEndpoint{}
	server := rpc.NewServer()
	if err := server.RegisterName("CredentialRPC", legacy); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()

	control, openErr := OpenCredentialControl(path, coordinator)
	if control != nil {
		_ = control.Close()
	}
	if control != nil || !errors.Is(openErr, ErrCredentialOwnerStale) {
		t.Fatalf("OpenCredentialControl = %v, %v, want versioned authority failure", control, openErr)
	}
	if legacy.calls.Load() != 0 {
		t.Fatalf("legacy CredentialRPC.Ping calls = %d, want zero", legacy.calls.Load())
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialEndpointRejectsPermissiveLegacyOwner(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer os.Remove(path)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	server := rpc.NewServer()
	if err := server.RegisterName("CredentialRPC", &credentialRPC{Coordinator: coordinator}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			server.ServeConn(conn)
		}
	}()

	control, openErr := OpenCredentialControl(path, coordinator)
	if control != nil || !errors.Is(openErr, ErrCredentialOwnerStale) {
		t.Fatalf("OpenCredentialControl = %v, %v, want stale authority error", control, openErr)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	for _, artifact := range []string{credentialEndpointLockPath(path), credentialEndpointSidecarPath(path)} {
		if _, statErr := os.Lstat(artifact); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("unsafe legacy endpoint created %s: %v", filepath.Base(artifact), statErr)
		}
	}
}

func TestCredentialEndpointUnreachableLegacyOwnerRemainsUntouched(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	control, err := OpenRecoveringCredentialControl(path, coordinator)
	if control != nil || !errors.Is(err, ErrCredentialOwnerStale) {
		t.Fatalf("OpenRecoveringCredentialControl = %v, %v, want stale error", control, err)
	}
	afterInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat := beforeInfo.Sys().(*syscall.Stat_t)
	afterStat := afterInfo.Sys().(*syscall.Stat_t)
	if beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino {
		t.Fatal("legacy endpoint changed")
	}
	if _, err := os.Lstat(credentialEndpointLockPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy recovery created lock: %v", err)
	}
}

func TestCredentialEndpointSupervisedOpenRecoversExactOrphan(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	orphanInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	owner, err := OpenRecoveringCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("supervised open did not own recovered endpoint")
	}
	recoveredInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	orphanStat := orphanInfo.Sys().(*syscall.Stat_t)
	recoveredStat := recoveredInfo.Sys().(*syscall.Stat_t)
	if orphanStat.Dev == recoveredStat.Dev && orphanStat.Ino == recoveredStat.Ino {
		t.Fatal("supervised open reused stale socket inode")
	}
}

func TestCredentialEndpointSupervisedOpenRecoversRebootRenumberedOrphan(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin device rollover")
	}
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	rewriteCredentialEndpointSidecar(t, path, func(sidecar *endpointSidecarFixture) {
		oldDevice := sidecar.Device + 1
		sidecar.Device = oldDevice
		sidecar.LockDevice = oldDevice
	})

	owner, err := OpenRecoveringCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("supervised open did not own reboot-renumbered endpoint")
	}
}

func TestCredentialEndpointLockBusyNeverRecoversOrUnlinks(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, credentialEndpointLockPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	beforeInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}

	control, err := OpenRecoveringCredentialControl(path, coordinator)
	if control != nil || !errors.Is(err, ErrCredentialOwnerStale) {
		t.Fatalf("OpenRecoveringCredentialControl = %v, %v, want stale error", control, err)
	}
	afterInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat := beforeInfo.Sys().(*syscall.Stat_t)
	afterStat := afterInfo.Sys().(*syscall.Stat_t)
	if beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino {
		t.Fatal("lock-busy open replaced endpoint")
	}
	afterSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeSidecar) != string(afterSidecar) {
		t.Fatal("lock-busy open changed sidecar")
	}
}

func TestCredentialEndpointSupervisedOpenRejectsUnverifiableOrphan(t *testing.T) {
	tests := map[string]func(t *testing.T, path string){
		"missing sidecar": func(t *testing.T, path string) {
			if err := os.Remove(credentialEndpointSidecarPath(path)); err != nil {
				t.Fatal(err)
			}
		},
		"malformed sidecar": func(t *testing.T, path string) {
			if err := os.WriteFile(credentialEndpointSidecarPath(path), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"mismatched inode": func(t *testing.T, path string) {
			data, err := os.ReadFile(credentialEndpointSidecarPath(path))
			if err != nil {
				t.Fatal(err)
			}
			var sidecar endpointSidecarFixture
			if err := json.Unmarshal(data, &sidecar); err != nil {
				t.Fatal(err)
			}
			sidecar.Inode++
			data, err = json.Marshal(sidecar)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credentialEndpointSidecarPath(path), data, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"permissive sidecar": func(t *testing.T, path string) {
			if err := os.Chmod(credentialEndpointSidecarPath(path), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"linked sidecar": func(t *testing.T, path string) {
			if err := os.Link(credentialEndpointSidecarPath(path), credentialEndpointSidecarPath(path)+".alias"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			coordinator, _ := testCoordinator(t)
			path := filepath.Join(shortEndpointDir(t), "credential.sock")
			makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
			corrupt(t, path)
			beforeInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			control, err := OpenRecoveringCredentialControl(path, coordinator)
			if control != nil || !errors.Is(err, ErrCredentialOwnerStale) {
				t.Fatalf("supervised open = %v, %v, want typed stale error", control, err)
			}
			afterInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeStat := beforeInfo.Sys().(*syscall.Stat_t)
			afterStat := afterInfo.Sys().(*syscall.Stat_t)
			if beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino {
				t.Fatal("unverifiable orphan was replaced")
			}
		})
	}
}

func TestCredentialEndpointSupervisedOpenRejectsUnexpectedRecordedTemporaryName(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	temporaryPath := filepath.Join(filepath.Dir(path), "orphan.sock")
	if err := os.WriteFile(temporaryPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeFinal, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}

	control, openErr := OpenRecoveringCredentialControl(path, coordinator)
	if control != nil {
		_ = control.Close()
	}
	if control != nil || !errors.Is(openErr, ErrCredentialOwnerStale) {
		t.Fatalf("OpenRecoveringCredentialControl = %v, %v, want stale proof", control, openErr)
	}
	afterFinal, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat := beforeFinal.Sys().(*syscall.Stat_t)
	afterStat := afterFinal.Sys().(*syscall.Stat_t)
	if beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino {
		t.Fatal("unexpected temporary name allowed final replacement")
	}
	afterSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSidecar) != string(beforeSidecar) {
		t.Fatal("unexpected temporary name allowed sidecar replacement")
	}
	replacement, err := os.ReadFile(temporaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(replacement) != "replacement" {
		t.Fatalf("unexpected temporary replacement changed to %q", replacement)
	}
}

func TestCredentialEndpointRecoveryLeavesReplacementUntouched(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	control, err := OpenRecoveringCredentialControl(path, coordinator)
	if control != nil || !errors.Is(err, ErrCredentialOwnerStale) {
		t.Fatalf("OpenRecoveringCredentialControl = %v, %v, want stale error", control, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "replacement" {
		t.Fatalf("replacement changed to %q", data)
	}
}

func TestCredentialEndpointVersionedPingMatchesDurableGeneration(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	client, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var reply CredentialEndpointPingReply
	if err := client.client.Call("CredentialEndpoint.Ping", CredentialEndpointPingArgs{ProtocolVersion: 1}, &reply); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var sidecar endpointSidecarFixture
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatal(err)
	}
	if reply.ProtocolVersion != 1 || reply.Generation != sidecar.Generation {
		t.Fatalf("ping = %+v, sidecar generation = %q", reply, sidecar.Generation)
	}
}

func TestCredentialEndpointLiveVersionedOwnerRequiresHeldLifetimeLock(t *testing.T) {
	tests := []struct {
		name       string
		createLock bool
		lockMode   os.FileMode
	}{
		{name: "missing lock"},
		{name: "unlocked lock", createLock: true, lockMode: 0o600},
		{name: "permissive lock", createLock: true, lockMode: 0o644},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _ := testCoordinator(t)
			path := filepath.Join(shortEndpointDir(t), "credential.sock")
			cleanup := startVersionedCredentialEndpoint(t, path, "generation-1", test.createLock, test.lockMode)
			beforeFinal, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
			if err != nil {
				t.Fatal(err)
			}

			control, openErr := OpenCredentialControl(path, coordinator)
			if control != nil {
				_ = control.Close()
			}
			cleanup()
			if control != nil || !errors.Is(openErr, ErrCredentialOwnerStale) {
				t.Fatalf("OpenCredentialControl = %v, %v, want unproved-owner error", control, openErr)
			}
			afterFinal, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeStat := beforeFinal.Sys().(*syscall.Stat_t)
			afterStat := afterFinal.Sys().(*syscall.Stat_t)
			if beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino {
				t.Fatal("unproved live owner endpoint changed")
			}
			afterSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
			if err != nil {
				t.Fatal(err)
			}
			if string(afterSidecar) != string(beforeSidecar) {
				t.Fatal("unproved live owner sidecar changed")
			}
		})
	}
}

func TestCredentialEndpointLiveOwnerRejectsLockedReplacementIdentity(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	cleanup := startVersionedCredentialEndpoint(t, path, "generation-1", true, 0o600)
	defer cleanup()
	lockPath := credentialEndpointLockPath(path)
	replacementPath := lockPath + ".replacement"
	replacementLock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	defer replacementLock.Close()
	defer os.Remove(replacementPath)
	beforeLock, err := replacementLock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	beforeIdentity := beforeLock.Sys().(*syscall.Stat_t)
	if err := os.Rename(replacementPath, lockPath); err != nil {
		t.Fatal(err)
	}
	beforeSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}

	control, openErr := OpenCredentialControl(path, coordinator)
	if control != nil {
		_ = control.Close()
	}
	if control != nil || !errors.Is(openErr, ErrCredentialOwnerStale) {
		t.Fatalf("OpenCredentialControl = %v, %v, want lock identity failure", control, openErr)
	}
	afterLock, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	afterIdentity := afterLock.Sys().(*syscall.Stat_t)
	if beforeIdentity.Dev != afterIdentity.Dev || beforeIdentity.Ino != afterIdentity.Ino {
		t.Fatal("locked replacement changed")
	}
	afterSidecar, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeSidecar) != string(afterSidecar) {
		t.Fatal("locked replacement changed sidecar")
	}
}

func TestCredentialEndpointCloseLeavesReplacementUntouched(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := owner.Close(); !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("Close error = %v, want identity change", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "replacement" {
		t.Fatalf("replacement changed to %q", data)
	}
}

func TestCredentialEndpointRequiresSecureStateDirectory(t *testing.T) {
	tests := map[string]func(t *testing.T) string{
		"permissive directory": func(t *testing.T) string {
			dir := shortEndpointDir(t)
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(dir, "credential.sock")
		},
		"symlink directory": func(t *testing.T) string {
			target := shortEndpointDir(t)
			parent := shortEndpointDir(t)
			link := filepath.Join(parent, "state")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(link, "credential.sock")
		},
	}
	for name, pathForTest := range tests {
		t.Run(name, func(t *testing.T) {
			coordinator, _ := testCoordinator(t)
			path := pathForTest(t)
			control, err := OpenCredentialControl(path, coordinator)
			if control != nil || !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
				t.Fatalf("OpenCredentialControl = %v, %v, want unsafe path", control, err)
			}
			for _, artifact := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointLockPath(path)} {
				if _, statErr := os.Lstat(artifact); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("unsafe directory artifact %s = %v", filepath.Base(artifact), statErr)
				}
			}
		})
	}
}

func TestCredentialEndpointDirectoryFDRejectsSpecialMode(t *testing.T) {
	directory := shortEndpointDir(t)
	if err := os.Chmod(directory, 0o700|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	fd, err := openCredentialEndpointDirectory(directory)
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	if !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("openCredentialEndpointDirectory error = %v, want unsafe path", err)
	}
}

func TestCredentialEndpointSocketIdentityRejectsSpecialMode(t *testing.T) {
	stat := unix.Stat_t{
		Mode: unix.S_IFSOCK | unix.S_IRUSR | unix.S_IWUSR | unix.S_ISUID,
		Uid:  uint32(os.Geteuid()),
	}
	if _, err := credentialEndpointIdentityFromStat(&stat, true); !errors.Is(err, ErrCredentialOwnerStale) {
		t.Fatalf("credentialEndpointIdentityFromStat error = %v, want stale proof", err)
	}
}

func TestCredentialEndpointCreatesSecureStateDirectory(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	root := shortEndpointDir(t)
	dir := filepath.Join(root, "state")
	path := filepath.Join(dir, "credential.sock")
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode = %v", info.Mode())
	}
}

func TestCredentialEndpointRejectsSidecarFromReplacementDirectory(t *testing.T) {
	root := shortEndpointDir(t)
	directory := filepath.Join(root, "state")
	path := filepath.Join(directory, "credential.sock")
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client != nil {
		_ = client.Close()
		t.Fatal("unexpected delegate")
	}
	defer endpoint.Close()

	originalDirectory := directory + ".original"
	if err := os.Rename(directory, originalDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	sidecarData, err := os.ReadFile(filepath.Join(originalDirectory, filepath.Base(credentialEndpointSidecarPath(path))))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialEndpointSidecarPath(path), sidecarData, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := endpoint.readSidecar(); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("readSidecar error = %v, want replaced-directory rejection", err)
	}
}

func TestCredentialEndpointDirectoryReplacementBeforeLockCreatesNoArtifacts(t *testing.T) {
	root := shortEndpointDir(t)
	directory := filepath.Join(root, "state")
	path := filepath.Join(directory, "credential.sock")
	originalDirectory := directory + ".original"
	endpoint, client, err := openCredentialEndpoint(path, false, func(phase credentialEndpointPhase) {
		if phase != credentialEndpointPhaseNamespacePinned {
			return
		}
		if renameErr := os.Rename(directory, originalDirectory); renameErr != nil {
			t.Fatal(renameErr)
		}
		if mkdirErr := os.Mkdir(directory, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	})
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if endpoint != nil || client != nil || !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("open after directory replacement = %v, %v, %v, want unsafe path", endpoint, client, err)
	}
	for _, dir := range []string{directory, originalDirectory} {
		for _, name := range []string{filepath.Base(path), filepath.Base(credentialEndpointSidecarPath(path)), filepath.Base(credentialEndpointLockPath(path))} {
			if _, statErr := os.Lstat(filepath.Join(dir, name)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("directory replacement created %s in %s: %v", name, filepath.Base(dir), statErr)
			}
		}
	}
}

func TestCredentialEndpointRecoveryRecorderRunsOnceBeforeNamespaceMutation(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	before := snapshotCredentialEndpointRecoveryArtifacts(t, path)

	var calls atomic.Int32
	recorder := CredentialEndpointRecoveryRecorderFunc(func() error {
		calls.Add(1)
		if got := snapshotCredentialEndpointRecoveryArtifacts(t, path); !reflect.DeepEqual(got, before) {
			t.Fatalf("endpoint artifacts changed before recovery record: got %+v, want %+v", got, before)
		}
		return nil
	})
	owner, err := OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), path, coordinator, nil, nil, recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("recorded recovery did not produce an owner")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("recovery recorder calls = %d, want 1", got)
	}
}

func TestCredentialEndpointRecoveryRecorderFailurePreventsMutation(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	before := snapshotCredentialEndpointRecoveryArtifacts(t, path)

	const privateDetail = "synthetic-private-recorder-detail"
	var calls atomic.Int32
	control, err := OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), path, coordinator, nil, nil,
		CredentialEndpointRecoveryRecorderFunc(func() error {
			calls.Add(1)
			return errors.New(privateDetail)
		}),
	)
	if control != nil || !errors.Is(err, ErrCredentialEndpointRecoveryUnrecorded) {
		t.Fatalf("recovery with failed recorder = %v, %v, want observation failure", control, err)
	}
	if strings.Contains(err.Error(), privateDetail) || strings.Contains(err.Error(), path) {
		t.Fatalf("recovery error disclosed private recorder data: %q", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("recovery recorder calls = %d, want 1", got)
	}
	if after := snapshotCredentialEndpointRecoveryArtifacts(t, path); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed recovery recorder mutated endpoint: got %+v, want %+v", after, before)
	}

	owner, err := OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), path, coordinator, nil, nil,
		CredentialEndpointRecoveryRecorderFunc(func() error { return nil }),
	)
	if err != nil {
		t.Fatalf("recovery after recorder failure: %v", err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("recorder failure retained the endpoint lock")
	}
}

func TestCredentialEndpointRecoveryRecorderMissingPreventsMutation(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	before := snapshotCredentialEndpointRecoveryArtifacts(t, path)

	control, err := OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), path, coordinator, nil, nil, nil,
	)
	if control != nil || !errors.Is(err, ErrCredentialEndpointRecoveryUnrecorded) {
		t.Fatalf("recovery without recorder = %v, %v, want observation failure", control, err)
	}
	if after := snapshotCredentialEndpointRecoveryArtifacts(t, path); !reflect.DeepEqual(after, before) {
		t.Fatalf("missing recovery recorder mutated endpoint: got %+v, want %+v", after, before)
	}
}

func TestCredentialEndpointRecoveryRecorderPanicPreventsMutation(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	before := snapshotCredentialEndpointRecoveryArtifacts(t, path)

	const privateDetail = "synthetic-private-recorder-panic"
	var control *CredentialControl
	var err error
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		control, err = OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
			context.Background(), path, coordinator, nil, nil,
			CredentialEndpointRecoveryRecorderFunc(func() error { panic(privateDetail) }),
		)
	}()
	if recovered != nil {
		t.Fatal("recovery recorder panic escaped privacy boundary")
	}
	if control != nil || !errors.Is(err, ErrCredentialEndpointRecoveryUnrecorded) {
		t.Fatalf("recovery with panicking recorder = %v, %v, want observation failure", control, err)
	}
	if strings.Contains(err.Error(), privateDetail) || strings.Contains(err.Error(), path) {
		t.Fatalf("recovery panic error disclosed private recorder data: %q", err)
	}
	if after := snapshotCredentialEndpointRecoveryArtifacts(t, path); !reflect.DeepEqual(after, before) {
		t.Fatalf("panicking recovery recorder mutated endpoint: got %+v, want %+v", after, before)
	}

	owner, err := OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), path, coordinator, nil, nil,
		CredentialEndpointRecoveryRecorderFunc(func() error { return nil }),
	)
	if err != nil {
		t.Fatalf("recovery after recorder panic: %v", err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("recorder panic retained the endpoint lock")
	}
}

func TestCredentialEndpointRecoveryRecorderSkipsFreshAndLiveEndpoints(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	var calls atomic.Int32
	recorder := CredentialEndpointRecoveryRecorderFunc(func() error {
		calls.Add(1)
		return nil
	})

	owner, err := OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), path, coordinator, nil, nil, recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	delegate, err := OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), path, coordinator, nil, nil, recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer delegate.Close()
	if !owner.Owner() || delegate.Owner() {
		t.Fatalf("fresh/live authority = %t/%t, want owner/delegate", owner.Owner(), delegate.Owner())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("non-recovery recorder calls = %d, want 0", got)
	}
}

func TestCredentialEndpointConcurrentVerifiedRecoveryHasOneOwner(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")
	controls := make(chan *CredentialControl, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			control, err := OpenRecoveringCredentialControl(path, coordinator)
			controls <- control
			errs <- err
		}()
	}
	owners := 0
	opened := make([]*CredentialControl, 0, 2)
	for range 2 {
		control := <-controls
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		opened = append(opened, control)
		if control.Owner() {
			owners++
		}
	}
	for _, control := range opened {
		defer control.Close()
	}
	if owners != 1 {
		t.Fatalf("owners = %d, want one", owners)
	}
}

func TestCredentialEndpointPublishedSidecarIndeterminateCommitRemainsRecoverable(t *testing.T) {
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client != nil {
		_ = client.Close()
		t.Fatal("unexpected delegate")
	}
	released := false
	defer func() {
		if released {
			return
		}
		if endpoint.listener != nil {
			_ = endpoint.listener.Close()
		}
		endpoint.release()
	}()
	initial := endpoint.sidecar
	endpoint.writeHook = func(sidecar credentialEndpointSidecar) error {
		if sidecar.State == credentialEndpointPublished && sidecar.Generation != initial.Generation {
			return &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: "test post-rename sync", Err: errors.New("injected sync failure")}
		}
		return nil
	}
	previous := &credentialEndpointPrevious{
		Generation: initial.Generation, TemporaryName: initial.TemporaryName,
		credentialEndpointIdentity: initial.credentialEndpointIdentity,
	}
	err = endpoint.publish(previous)
	if !errors.Is(err, fsutil.ErrCommitIndeterminate) {
		t.Fatalf("publish error = %v, want indeterminate commit", err)
	}
	actual, exists, readErr := endpoint.readSidecar()
	if readErr != nil || !exists || actual.State != credentialEndpointPublished || actual.Generation == initial.Generation {
		t.Fatalf("indeterminate sidecar = %+v, exists=%t, err=%v", actual, exists, readErr)
	}
	finalIdentity, finalExists, statErr := statCredentialEndpointSocketAt(endpoint.directoryFD, endpoint.finalName, true)
	if statErr != nil || !finalExists || finalIdentity != actual.credentialEndpointIdentity {
		t.Fatalf("indeterminate final = %+v, exists=%t, err=%v", finalIdentity, finalExists, statErr)
	}
	if endpoint.listener != nil {
		_ = endpoint.listener.Close()
	}
	endpoint.release()
	released = true

	coordinator, _ := testCoordinator(t)
	recovered, err := OpenRecoveringCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if !recovered.Owner() {
		t.Fatal("indeterminate publication was not recovered by a new owner")
	}
}

func TestCredentialEndpointRecoversPublicationCrashPhases(t *testing.T) {
	for _, phase := range []credentialEndpointPhase{
		credentialEndpointPhasePrepared,
		credentialEndpointPhaseLinked,
		credentialEndpointPhaseTemporaryRemoved,
		credentialEndpointPhasePublished,
	} {
		t.Run(string(phase), func(t *testing.T) {
			path := filepath.Join(shortEndpointDir(t), "credential.sock")
			command := exec.Command(os.Args[0], "-test.run=^TestCredentialEndpointCrashHelper$")
			command.Env = append(os.Environ(),
				"CQ_ENDPOINT_CRASH_HELPER=1",
				"CQ_ENDPOINT_CRASH_PATH="+path,
				"CQ_ENDPOINT_CRASH_PHASE="+string(phase),
			)
			if err := command.Run(); err == nil {
				t.Fatal("crash helper exited successfully")
			} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 91 {
				t.Fatalf("crash helper error = %v", err)
			}

			coordinator, _ := testCoordinator(t)
			owner, err := OpenRecoveringCredentialControl(path, coordinator)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if !owner.Owner() {
				t.Fatal("recovered crash state did not produce owner")
			}
		})
	}
}

func TestCredentialEndpointRecoversReplacementPublicationCrashPhases(t *testing.T) {
	for _, phase := range []credentialEndpointPhase{
		credentialEndpointPhasePrepared,
		credentialEndpointPhaseLinked,
		credentialEndpointPhaseTemporaryRemoved,
		credentialEndpointPhasePublished,
	} {
		t.Run(string(phase), func(t *testing.T) {
			path := filepath.Join(shortEndpointDir(t), "credential.sock")
			runCredentialEndpointCrashHelper(t, path, phase, "republish")
			coordinator, _ := testCoordinator(t)
			owner, err := OpenRecoveringCredentialControl(path, coordinator)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if !owner.Owner() {
				t.Fatal("recovered replacement crash state did not produce owner")
			}
		})
	}
}

func TestCredentialEndpointRecoversCloseCrashPhases(t *testing.T) {
	for _, phase := range []credentialEndpointPhase{
		credentialEndpointPhaseClosing,
		credentialEndpointPhaseFinalRemoved,
		credentialEndpointPhaseSidecarRemoved,
	} {
		t.Run(string(phase), func(t *testing.T) {
			path := filepath.Join(shortEndpointDir(t), "credential.sock")
			runCredentialEndpointCrashHelper(t, path, phase, "close")
			coordinator, _ := testCoordinator(t)
			owner, err := OpenRecoveringCredentialControl(path, coordinator)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if !owner.Owner() {
				t.Fatal("recovered close crash state did not produce owner")
			}
		})
	}
}

func runCredentialEndpointCrashHelper(t *testing.T, path string, phase credentialEndpointPhase, action string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestCredentialEndpointCrashHelper$")
	command.Env = append(os.Environ(),
		"CQ_ENDPOINT_CRASH_HELPER=1",
		"CQ_ENDPOINT_CRASH_PATH="+path,
		"CQ_ENDPOINT_CRASH_PHASE="+string(phase),
		"CQ_ENDPOINT_CRASH_ACTION="+action,
	)
	if err := command.Run(); err == nil {
		t.Fatal("crash helper exited successfully")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 91 {
		t.Fatalf("crash helper error = %v", err)
	}
}

func TestCredentialEndpointCrashHelper(t *testing.T) {
	if os.Getenv("CQ_ENDPOINT_CRASH_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	path := os.Getenv("CQ_ENDPOINT_CRASH_PATH")
	wantPhase := credentialEndpointPhase(os.Getenv("CQ_ENDPOINT_CRASH_PHASE"))
	coordinator, _ := testCoordinator(t)
	crash := func(phase credentialEndpointPhase) {
		if phase == wantPhase {
			os.Exit(91)
		}
	}
	switch os.Getenv("CQ_ENDPOINT_CRASH_ACTION") {
	case "republish":
		endpoint, client, err := openCredentialEndpoint(path, false, nil)
		if err != nil || client != nil {
			t.Fatalf("prepare replacement crash = %v, %v", client, err)
		}
		endpoint.hook = crash
		previous := &credentialEndpointPrevious{
			Generation: endpoint.sidecar.Generation, TemporaryName: endpoint.sidecar.TemporaryName,
			credentialEndpointIdentity: endpoint.sidecar.credentialEndpointIdentity,
		}
		if err := endpoint.publish(previous); err != nil {
			t.Fatal(err)
		}
	case "close":
		control, err := openCredentialControl(path, coordinator, false, crash)
		if err != nil {
			t.Fatal(err)
		}
		if err := control.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		_, _ = openCredentialControl(path, coordinator, false, func(phase credentialEndpointPhase) {
			if phase == wantPhase {
				os.Exit(91)
			}
		})
	}
	t.Fatalf("did not reach crash phase %q", wantPhase)
}

func TestCredentialEndpointNoReplacePublicationPreservesExistingTarget(t *testing.T) {
	dir := shortEndpointDir(t)
	temporaryPath := filepath.Join(dir, "temporary.sock")
	finalPath := filepath.Join(dir, "credential.sock")
	if err := os.WriteFile(temporaryPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("do-not-replace"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := publishCredentialSocketNoReplace(temporaryPath, finalPath)
	if !errors.Is(err, ErrCredentialEndpointOccupied) {
		t.Fatalf("publish error = %v, want occupied", err)
	}
	data, readErr := os.ReadFile(finalPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "do-not-replace" {
		t.Fatalf("final target changed to %q", data)
	}
	if _, statErr := os.Lstat(temporaryPath); statErr != nil {
		t.Fatalf("temporary socket changed: %v", statErr)
	}
}

func TestCredentialEndpointNoReplacePublicationMakesSocketConnectable(t *testing.T) {
	dir := shortEndpointDir(t)
	temporaryPath := filepath.Join(dir, "temporary.sock")
	finalPath := filepath.Join(dir, "credential.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: temporaryPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	defer os.Remove(temporaryPath)
	defer os.Remove(finalPath)

	if err := publishCredentialSocketNoReplace(temporaryPath, finalPath); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			acceptErr = conn.Close()
		}
		accepted <- acceptErr
	}()
	conn, err := net.DialTimeout("unix", finalPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func shortEndpointDir(t *testing.T) string {
	t.Helper()
	tempRoot := os.TempDir()
	if runtime.GOOS == "darwin" {
		tempRoot = "/private/tmp"
	}
	dir, err := os.MkdirTemp(tempRoot, "cqe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func makeExactCredentialEndpointOrphan(t *testing.T, path, generation string) {
	t.Helper()
	temporaryPath := filepath.Join(filepath.Dir(path), "orphan.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: temporaryPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishCredentialSocketNoReplace(temporaryPath, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if err := os.WriteFile(credentialEndpointLockPath(path), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lockInfo, err := os.Lstat(credentialEndpointLockPath(path))
	if err != nil {
		t.Fatal(err)
	}
	lockStat := lockInfo.Sys().(*syscall.Stat_t)
	sidecar, err := json.Marshal(endpointSidecarFixture{
		Version: 1, ProtocolVersion: 1, Generation: generation, State: "published",
		TemporaryName: filepath.Base(temporaryPath), FinalName: filepath.Base(path),
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid),
		Type: "socket", Mode: 0o600,
		LockDevice: uint64(lockStat.Dev), LockInode: uint64(lockStat.Ino), LockLinks: uint64(lockStat.Nlink),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialEndpointSidecarPath(path), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		t.Fatal(err)
	}
}

func rewriteCredentialEndpointSidecar(t *testing.T, path string, mutate func(*endpointSidecarFixture)) {
	t.Helper()
	data, err := os.ReadFile(credentialEndpointSidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var sidecar endpointSidecarFixture
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatal(err)
	}
	mutate(&sidecar)
	data, err = json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialEndpointSidecarPath(path), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type credentialEndpointRecoveryArtifactSnapshot struct {
	Names     []string
	Artifacts []credentialEndpointRecoveryArtifact
}

type credentialEndpointRecoveryArtifact struct {
	Name  string
	Mode  os.FileMode
	Size  int64
	Dev   uint64
	Inode uint64
	Links uint64
	Data  string
}

func snapshotCredentialEndpointRecoveryArtifacts(t *testing.T, path string) credentialEndpointRecoveryArtifactSnapshot {
	t.Helper()
	directory := filepath.Dir(path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := credentialEndpointRecoveryArtifactSnapshot{Names: make([]string, 0, len(entries))}
	for _, entry := range entries {
		snapshot.Names = append(snapshot.Names, entry.Name())
	}
	for _, artifactPath := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointLockPath(path)} {
		info, err := os.Lstat(artifactPath)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("endpoint artifact has no syscall identity")
		}
		artifact := credentialEndpointRecoveryArtifact{
			Name: filepath.Base(artifactPath), Mode: info.Mode(), Size: info.Size(),
			Dev: uint64(stat.Dev), Inode: uint64(stat.Ino), Links: uint64(stat.Nlink),
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			artifact.Data = string(data)
		}
		snapshot.Artifacts = append(snapshot.Artifacts, artifact)
	}
	return snapshot
}

func startVersionedCredentialEndpoint(t *testing.T, path, generation string, createLock bool, lockMode os.FileMode) func() {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if err := os.WriteFile(credentialEndpointLockPath(path), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lockInfo, err := os.Lstat(credentialEndpointLockPath(path))
	if err != nil {
		t.Fatal(err)
	}
	lockStat := lockInfo.Sys().(*syscall.Stat_t)
	sidecar, err := json.Marshal(endpointSidecarFixture{
		Version: 1, ProtocolVersion: 1, Generation: generation, State: "published",
		TemporaryName: ".cq-credential-published.sock", FinalName: filepath.Base(path),
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid),
		Type: "socket", Mode: 0o600,
		LockDevice: uint64(lockStat.Dev), LockInode: uint64(lockStat.Ino), LockLinks: uint64(lockStat.Nlink),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialEndpointSidecarPath(path), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if !createLock {
		if err := os.Remove(credentialEndpointLockPath(path)); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Chmod(credentialEndpointLockPath(path), lockMode); err != nil {
		t.Fatal(err)
	}
	server := rpc.NewServer()
	if err := server.RegisterName("CredentialEndpoint", &credentialEndpointRPC{generation: generation}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			server.ServeConn(conn)
		}
	}()
	return func() {
		_ = listener.Close()
		<-done
	}
}

func assertSecureRegularFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != mode || stat.Uid != uint32(os.Getuid()) {
		t.Fatalf("unsafe file %s: mode=%v stat=%+v", filepath.Base(path), info.Mode(), stat)
	}
}
