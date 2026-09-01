//go:build !darwin

package main

func probeCodexDesktopVersion() (string, bool) {
	return "", false
}
