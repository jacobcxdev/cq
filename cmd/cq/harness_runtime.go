package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func startHarnessController(ctx context.Context, fsys fsutil.FileSystem, roots userdirs.Roots) (func(string, string), func()) {
	if runtime.GOOS != "darwin" {
		return nil, func() {}
	}
	controller := harness.NewController(func() (harness.Policy, error) {
		return harness.Load(fsys, filepath.Join(roots.Config, "harness.json"))
	}, harness.DesktopIPC{}, harness.Receipts{FS: fsys, Dir: filepath.Join(roots.State, "harness")}, harness.PluginCLI{
		Executable: filepath.Join(nativeNetworkApplication, "Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"),
	})
	controller.Report = func(chatID string, err error) {
		fmt.Fprintf(os.Stderr, "cq: harness chat %s: %v; inspect cq codex harness status --chat CHAT_ID\n", chatID, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); controller.Run(ctx) }()
	return controller.Observe, func() { cancel(); <-done }
}
