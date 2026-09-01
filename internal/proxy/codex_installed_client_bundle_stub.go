//go:build !darwin && !linux

package proxy

func resolveCodexInstalledBundledClientExecutable() (string, bool) {
	return "", false
}
