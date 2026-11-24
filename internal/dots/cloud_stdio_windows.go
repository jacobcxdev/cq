//go:build windows

package dots

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// RunWindowsDotsCloudStdio binds the private launcher's stdio session to the
// stock CLI verified by the Windows package registration.
func RunWindowsDotsCloudStdio(ctx context.Context, root, registeredStock string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !cleanAbsolute(root) {
		return errors.New("Dots cloud helper root must be a clean absolute path")
	}
	cfg, err := LoadSettings(filepath.Join(root, "settings.json"))
	if err != nil {
		return err
	}
	if cfg.IntegrationMode != "native" || !strings.EqualFold(cfg.RuntimeRoot, filepath.Join(root, "runtime")) ||
		!strings.EqualFold(cloudStockExecutable(cfg), registeredStock) {
		return errors.New("Dots cloud helper does not match the registered installation")
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if !windowsCloudAppSession(args) {
		path, arguments, environment, _, err := cloudInvocation(cfg, args, os.Environ())
		if err != nil {
			return err
		}
		return runCloudStockCommand(windowsHarnessCommand(ctx, path, arguments), environment, stdin, stdout, stderr)
	}

	inventory, _ := (&CatalogueClient{Settings: cfg}).Inventory()
	deps := cloudStdioDefaults(cfg)
	deps.gatewayListenerForPID = func(pid int) (net.Listener, error) {
		return newWindowsCloudGatewayListener(cfg.RuntimeRoot, pid)
	}
	deps.launch = func(ctx context.Context, path string, arguments, environment []string, stderr io.Writer) (*cloudLocal, error) {
		return startCloudCommand(windowsHarnessCommand(ctx, path, arguments), environment, stderr)
	}
	return runCloudStdio(ctx, cfg, inventory.Accounts, args, stdin, stdout, stderr, deps)
}
