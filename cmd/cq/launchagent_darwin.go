package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

const agentLabel = "dev.jacobcx.cq.refresh"

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{ .Label }}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{ .Binary }}</string>
		<string>refresh</string>
	</array>
	<key>StartInterval</key>
	<integer>{{ .Interval }}</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardErrorPath</key>
	<string>{{ .LogPath }}</string>
</dict>
</plist>
`))

type plistData struct {
	Label    string
	Binary   string
	Interval int
	LogPath  string
}

func agentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}

func agentLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "cq", "refresh.log"), nil
}

func resolveExecutable() (string, error) {
	// Prefer PATH lookup — returns stable symlink path (e.g. /opt/homebrew/bin/cq)
	// which survives Homebrew upgrades.
	if exe, err := exec.LookPath("cq"); err == nil {
		return exe, nil
	}
	// Fall back to the current binary path for local/dev builds.
	return os.Executable()
}

func installAgent(interval int) error {
	if interval <= 0 {
		interval = 1800
	}

	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	plistPath, err := agentPlistPath()
	if err != nil {
		return err
	}
	logPath, err := agentLogPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	// Unload existing agent if present.
	_ = exec.Command("launchctl", "unload", plistPath).Run()

	f, err := os.Create(plistPath)
	if err != nil {
		return fmt.Errorf("create plist: %w", err)
	}
	data := plistData{
		Label:    agentLabel,
		Binary:   exe,
		Interval: interval,
		LogPath:  logPath,
	}
	if err := plistTemplate.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("write plist: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close plist: %w", err)
	}

	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}

	fmt.Fprintf(os.Stderr, "cq: installed LaunchAgent (every %ds)\n", interval)
	fmt.Fprintf(os.Stderr, "cq: plist: %s\n", plistPath)
	fmt.Fprintf(os.Stderr, "cq: log:   %s\n", logPath)
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

	if err := exec.Command("launchctl", "unload", plistPath).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "cq: launchctl unload: %v\n", err)
	}

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("remove plist: %w", err)
	}

	fmt.Fprintf(os.Stderr, "cq: uninstalled LaunchAgent\n")
	return nil
}
