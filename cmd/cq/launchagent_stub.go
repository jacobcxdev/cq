//go:build !darwin

package main

import "fmt"

func ensureAgent() {}

func installAgent(interval int) error {
	return fmt.Errorf("background refresh agent is only supported on macOS")
}

func uninstallAgent() error {
	return fmt.Errorf("background refresh agent is only supported on macOS")
}
