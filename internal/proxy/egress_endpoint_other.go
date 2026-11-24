//go:build !windows

package proxy

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func listenRuntimeEgress(dir string) (string, net.Listener, func(), error) {
	socketDir, err := os.MkdirTemp(dir, "e-")
	if err != nil {
		return "", nil, nil, errors.New("create private egress directory failed")
	}
	socketPath := filepath.Join(socketDir, "s")
	cleanup := func() { _ = os.Remove(socketPath); _ = os.Remove(socketDir) }
	if len(socketPath) > 103 {
		cleanup()
		return "", nil, nil, errors.New("runtime egress socket path exceeds platform limit")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		cleanup()
		return "", nil, nil, errors.New("create private egress listener failed")
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		cleanup()
		return "", nil, nil, errors.New("secure private egress listener failed")
	}
	return socketPath, listener, cleanup, nil
}

func validRuntimeEgressEndpoint(fs fsutil.FileSystem, dir, endpoint string) bool {
	if !fsutil.IsCleanAbsoluteNonRootPath(endpoint) || len(endpoint) > 103 {
		return false
	}
	socketDir := filepath.Dir(endpoint)
	if filepath.Dir(socketDir) != dir || !strings.HasPrefix(filepath.Base(socketDir), "e-") || filepath.Base(endpoint) != "s" {
		return false
	}
	if err := fsutil.ValidateSecureDirectory(fs, socketDir); err != nil {
		return false
	}
	inspector, ok := fs.(fsutil.SecurePathInspector)
	if !ok {
		return false
	}
	info, err := inspector.Lstat(endpoint)
	return err == nil && info.Mode()&os.ModeSocket != 0 && info.Mode().Perm() == 0o600 &&
		fsutil.ValidateSecureOwner(inspector, info) == nil
}

func dialRuntimeEgress(ctx context.Context, endpoint string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
}
