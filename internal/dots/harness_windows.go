//go:build windows

package dots

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/windows"
)

// RunWindowsHarnessStdio applies profiles to the native app-server session
// started by the explicit Windows Desktop launch command.
func RunWindowsHarnessStdio(ctx context.Context, executable string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	env = slices.DeleteFunc(slices.Clone(env), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return strings.EqualFold(key, "CODEX_CLI_PATH") || strings.EqualFold(key, "CODEX_APP_RUNTIME_CLI_PATH")
	})
	env = append(env, "CODEX_CLI_PATH="+executable, "CODEX_APP_RUNTIME_CLI_PATH="+executable)
	fsys := fsutil.OSFileSystem{}
	policy := filepath.Join(roots.Config, "harness.json")
	deps := cloudStdioDependencies{
		localExecutable: executable,
		environment:     env,
		launch: func(ctx context.Context, path string, arguments, environment []string, stderr io.Writer) (*cloudLocal, error) {
			command := exec.CommandContext(ctx, path, arguments...)
			command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			return startCloudCommand(command, environment, stderr)
		},
		now:             time.Now,
		harnessReceipts: harness.Receipts{FS: fsys, Dir: filepath.Join(roots.State, "harness")},
		readHarnessContextPolicy: func(cwd string) (harness.Policy, error) {
			return cloudRuntimeHarnessPolicy(fsys, policy, cwd)
		},
		nativeHarnessInventory: func(ctx context.Context, cwd string) (harness.Inventory, error) {
			return (harness.NativeInventory{Executable: executable}).Snapshot(ctx, cwd)
		},
	}
	return runCloudStdio(ctx, &Settings{}, nil, args, stdin, stdout, stderr, deps)
}
