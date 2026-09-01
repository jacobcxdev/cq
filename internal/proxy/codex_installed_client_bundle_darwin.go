//go:build darwin

package proxy

import "os"

func resolveCodexInstalledBundledClientExecutable() (string, bool) {
	for _, path := range []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, true
		}
	}
	return "", false
}
