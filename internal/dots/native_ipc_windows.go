//go:build windows

package dots

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

type nativeAcceptResult struct {
	conn net.Conn
	err  error
}

type windowsNativeListener struct {
	net.Listener
	mu      sync.Mutex
	pending chan nativeAcceptResult
}

type nativeAcceptTimeout struct{}

func (nativeAcceptTimeout) Error() string   { return "native debugger accept timeout" }
func (nativeAcceptTimeout) Timeout() bool   { return true }
func (nativeAcceptTimeout) Temporary() bool { return true }

func nativeEndpointCheck(string) error { return nil }

func dotsCurrentSID() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", errors.New("Dots current user SID unavailable")
	}
	return user.User.Sid.String(), nil
}

func dotsPipePath(root, purpose string) (string, string, error) {
	if !cleanAbsolute(root) {
		return "", "", errors.New("Dots pipe root invalid")
	}
	sid, err := dotsCurrentSID()
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte("cq/dots-pipe/windows/v1\x00" + sid + "\x00" + strings.ToLower(filepath.Clean(root)) + "\x00" + purpose))
	return `\\.\pipe\cq-dots-` + hex.EncodeToString(digest[:16]), sid, nil
}

func dotsPipeSDDL(sid string) string {
	return "O:" + sid + "G:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)"
}

func dotsPipePeer(c net.Conn, server bool) bool {
	handle, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	var pid uint32
	var err error
	if server {
		err = windows.GetNamedPipeClientProcessId(windows.Handle(handle.Fd()), &pid)
	} else {
		err = windows.GetNamedPipeServerProcessId(windows.Handle(handle.Fd()), &pid)
	}
	if err != nil || pid <= 1 {
		return false
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	peer, err := token.GetTokenUser()
	if err != nil || peer.User.Sid == nil {
		return false
	}
	current, err := dotsCurrentSID()
	return err == nil && peer.User.Sid.String() == current
}

func nativeDial(ctx context.Context, root string) (net.Conn, error) {
	path, _, err := dotsPipePath(root, "native-debug")
	if err != nil {
		return nil, err
	}
	c, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if !nativeClientPeer(c) {
		_ = c.Close()
		return nil, errors.New("native debugger peer mismatch")
	}
	return c, nil
}

func nativeClientPeer(c net.Conn) bool { return dotsPipePeer(c, false) }
func nativeServerPeer(c net.Conn) bool { return dotsPipePeer(c, true) }

func nativeAccept(listener net.Listener) (net.Conn, error) {
	l, ok := listener.(*windowsNativeListener)
	if !ok {
		return nil, errors.New("native debugger listener invalid")
	}
	l.mu.Lock()
	if l.pending == nil {
		l.pending = make(chan nativeAcceptResult, 1)
		pending := l.pending
		go func() {
			result := nativeAcceptResult{}
			defer func() {
				if recover() != nil {
					if result.conn != nil {
						_ = result.conn.Close()
					}
					result = nativeAcceptResult{err: errors.New("native debugger accept failed")}
				}
				pending <- result
			}()
			result.conn, result.err = l.Listener.Accept()
		}()
	}
	pending := l.pending
	l.mu.Unlock()
	select {
	case result := <-pending:
		l.mu.Lock()
		if l.pending == pending {
			l.pending = nil
		}
		l.mu.Unlock()
		return result.conn, result.err
	case <-time.After(250 * time.Millisecond):
		return nil, nativeAcceptTimeout{}
	}
}

func newNativeBroker(root string) (*nativeBroker, error) {
	if err := nativeRoot(root); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(root, "native-debug.lock")
	fsys, err := fsutil.NewPrivateDestinationFileSystem(lockPath)
	if err != nil {
		return nil, errors.New("native debugger lock directory unavailable")
	}
	lock, err := fsutil.AcquireExclusiveLock(fsys, lockPath)
	if err != nil {
		return nil, errors.New("native debugger is already running or its lock is unavailable")
	}
	path, sid, err := dotsPipePath(root, "native-debug")
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: dotsPipeSDDL(sid), InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		_ = lock.Close()
		return nil, errors.New("native debugger listener unavailable")
	}
	return &nativeBroker{listener: &windowsNativeListener{Listener: listener}, lock: lock, path: path}, nil
}

func nativeRemoveEndpoint(*nativeBroker) {}
