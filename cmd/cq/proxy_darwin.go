//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

const proxyAgentLabel = "dev.jacobcx.cq.proxy"

var proxyPlistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{ .Label }}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{ .Binary }}</string>
		<string>proxy</string>
		<string>start</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardErrorPath</key>
	<string>{{ .LogPath }}</string>
</dict>
</plist>
`))

type proxyPlistData struct {
	Label   string
	Binary  string
	LogPath string
}

func proxyAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", proxyAgentLabel+".plist"), nil
}

func proxyAgentLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "cq", "proxy.log"), nil
}

func installProxyAgent() error {
	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		return err
	}
	logPath, err := proxyAgentLogPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	_ = exec.Command("launchctl", "unload", plistPath).Run()

	f, err := os.Create(plistPath)
	if err != nil {
		return fmt.Errorf("create plist: %w", err)
	}
	data := proxyPlistData{
		Label:   proxyAgentLabel,
		Binary:  exe,
		LogPath: logPath,
	}
	if err := proxyPlistTemplate.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("write plist: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close plist: %w", err)
	}

	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}

	fmt.Fprintf(os.Stderr, "cq: installed proxy LaunchAgent (KeepAlive)\n")
	fmt.Fprintf(os.Stderr, "cq: plist: %s\n", plistPath)
	fmt.Fprintf(os.Stderr, "cq: log:   %s\n", logPath)
	return nil
}

func uninstallProxyAgent() error {
	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "cq: no proxy LaunchAgent installed\n")
		return nil
	}

	if err := exec.Command("launchctl", "unload", plistPath).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "cq: launchctl unload: %v\n", err)
	}

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("remove plist: %w", err)
	}

	fmt.Fprintf(os.Stderr, "cq: uninstalled proxy LaunchAgent\n")
	return nil
}
