//go:build !darwin

package main

import "fmt"

func installProxyAgent() error {
	return fmt.Errorf("proxy LaunchAgent is only supported on macOS")
}

func uninstallProxyAgent() error {
	return fmt.Errorf("proxy LaunchAgent is only supported on macOS")
}

func restartProxyAgent() error {
	return fmt.Errorf("proxy LaunchAgent is only supported on macOS")
}

