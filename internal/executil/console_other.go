//go:build !windows

package executil

import "os/exec"

// SuppressConsoleWindow leaves child process attributes unchanged on other platforms.
func SuppressConsoleWindow(*exec.Cmd) {}
