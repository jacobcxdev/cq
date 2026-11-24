//go:build !windows

package dots

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func nativeEndpointCheck(root string) error {
	if len(filepath.Join(root, nativeSocketName)) > 103 {
		return errors.New("native debugger socket path is too long")
	}
	return nil
}

func nativeDial(ctx context.Context, root string) (net.Conn, error) {
	path := filepath.Join(root, nativeSocketName)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || !owned(info) || info.Mode().Perm() != 0o600 {
		return nil, errors.New("native debugger socket must be owned and mode 0600")
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if !nativeClientPeer(raw) {
		_ = raw.Close()
		return nil, errors.New("native debugger peer mismatch")
	}
	return raw, nil
}

func nativeClientPeer(c net.Conn) bool { return nativeServerPeer(c) }

func nativeServerPeer(c net.Conn) bool {
	unix, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	uid, err := nativePeerUID(unix)
	return err == nil && uid == uint32(os.Getuid())
}

func nativeAccept(listener net.Listener) (net.Conn, error) {
	unix, ok := listener.(*net.UnixListener)
	if !ok {
		return nil, errors.New("native debugger listener invalid")
	}
	_ = unix.SetDeadline(time.Now().Add(250 * time.Millisecond))
	return unix.AcceptUnix()
}

func newNativeBroker(root string) (*nativeBroker, error) {
	if err := nativeRoot(root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, nativeSocketName)
	// Keep this lock until the listener is closed and its exact path removed.
	// Otherwise simultaneous recovery could unlink another broker's new socket.
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, filepath.Join(root, "native-debug.lock"))
	if err != nil {
		return nil, errors.New("native debugger is already running or its lock is unavailable")
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = lock.Close()
		}
	}()
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || !owned(info) || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 {
			return nil, errors.New("native debugger socket must be owned and mode 0600")
		}
		// A legacy broker may not hold the lock. A successful connection, timeout,
		// permission error or other ambiguous failure must never displace it.
		conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil, errors.New("native debugger socket already exists")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errors.New("native debugger socket availability could not be verified")
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, current) {
			return nil, errors.New("native debugger socket changed during recovery")
		}
		if err := os.Remove(path); err != nil {
			return nil, errors.New("native debugger abandoned socket could not be removed")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("native debugger socket unavailable")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	b := &nativeBroker{listener: listener, lock: lock, path: path}
	keepLock = true
	b.identity, err = os.Lstat(path)
	if err != nil {
		b.close()
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}

func nativeRemoveEndpoint(b *nativeBroker) {
	if info, err := os.Lstat(b.path); err == nil && b.identity != nil && os.SameFile(info, b.identity) {
		_ = os.Remove(b.path)
	}
}
