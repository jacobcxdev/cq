package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

const agentLabel = "dev.jacobcx.cq.refresh"

func agentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}

func agentLogPath(logsDir string) string {
	return filepath.Join(logsDir, "refresh.log")
}

func resolveExecutable() (string, error) {
	return resolveServiceExecutable("")
}

func installAgent(interval int) error {
	if interval <= 0 {
		interval = 1800
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}

	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve absolute executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	exe = filepath.Clean(exe)
	platform := newDarwinCommandServicePlatform(home, roots, exe)
	if err := platform.installRefresh(context.Background(), exe, interval); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "cq: installed LaunchAgent (every %ds)\n", interval)
	fmt.Fprintf(os.Stderr, "cq: plist: %s\n", platform.plistPath(agentLabel))
	fmt.Fprintf(os.Stderr, "cq: log:   %s\n", agentLogPath(roots.Logs))
	return nil
}

// ensureAgent auto-installs the LaunchAgent on first run if not present.
func ensureAgent() {
	path, err := agentPlistPath()
	if err != nil {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return // already installed
	}
	if err := installAgent(1800); err != nil {
		fmt.Fprintf(os.Stderr, "cq: auto-install refresh agent failed: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "cq: to disable: cq agent uninstall\n")
}

func uninstallAgent() error {
	plistPath, err := agentPlistPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "cq: no LaunchAgent installed\n")
		return nil
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	platform := newDarwinCommandServicePlatform(home, roots, "")
	if err := platform.RemoveRefresh(context.Background()); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "cq: uninstalled LaunchAgent\n")
	return nil
}
