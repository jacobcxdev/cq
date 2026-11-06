//go:build darwin

package dots

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

func nativeCloudGatewayPeer(ctx context.Context, cfg *Settings, remote string) (int, bool) {
	data, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", "-iTCP:8000", "-sTCP:ESTABLISHED", "-Fpn").Output()
	if err != nil {
		return 0, false
	}
	pid, ok := cloudGatewayPeerPID(remote, data)
	if !ok {
		return 0, false
	}
	uid, arguments, err := dotsProcessArguments(ctx, pid)
	if err != nil || !cloudGatewayNativeProcess(cfg, uid, arguments) {
		return 0, false
	}
	// argv[0] is writable by the process. Check the kernel's executable path too.
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 5 {
		return 0, false
	}
	end := bytes.IndexByte(raw[4:], 0)
	if end < 1 {
		return 0, false
	}
	expected, expectedErr := CanonicalPath(arguments[0])
	actual, actualErr := CanonicalPath(string(raw[4 : 4+end]))
	return pid, expectedErr == nil && actualErr == nil && expected == actual
}

func cloudBridgeGateway(cfg *Settings) (net.Listener, error) {
	file := os.NewFile(5, "native-gateway")
	if file != nil {
		defer file.Close()
	}
	return importCloudGatewayListener(cfg.RuntimeRoot, os.Getppid(), file)
}
