package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	controller := newHarnessController(fsys, roots)
	controller.Report = func(chatID string, err error) {
		fmt.Fprintf(os.Stderr, "cq: harness chat %s: %v; inspect cq codex harness status --chat CHAT_ID\n", chatID, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); controller.Run(ctx) }()
	return controller.Observe, func() { cancel(); <-done }
}

func newHarnessController(fsys fsutil.FileSystem, roots userdirs.Roots) *harness.Controller {
	path := filepath.Join(roots.Config, "harness.json")
	controller := harness.NewController(func() (harness.Policy, error) {
		return harness.Load(fsys, path)
	}, harness.DesktopIPC{}, harness.Receipts{FS: fsys, Dir: filepath.Join(roots.State, "harness")}, harness.PluginCLI{
		Executable: harnessNativeExecutable(),
	})
	controller.ContextPolicy = func(cwd string) (harness.Policy, error) {
		policy, err := harness.Load(fsys, path)
		if err != nil || !policy.Enabled {
			return policy, err
		}
		return harness.LoadForContext(fsys, path, cwd)
	}
	controller.Inventory = harness.NativeInventory{Executable: harnessNativeExecutable()}
	return controller
}

func harnessNativeExecutable() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(nativeNetworkApplication, "Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex")
	}
	path, _ := exec.LookPath("codex")
	return path
}

func configureHarnessRunner(runner *codexHarnessRunner) {
	runner.snapshot = (harness.NativeInventory{Executable: harnessNativeExecutable()}).Snapshot
	runner.readModel = (harness.DesktopIPC{}).ReadModel
	runner.apply = func(ctx context.Context, chatID string) error {
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("standard Desktop profile application is unavailable on this platform")
		}
		roots, err := userdirs.Default()
		if err != nil {
			return err
		}
		return newHarnessController(runner.fs, roots).Apply(ctx, chatID)
	}
}
