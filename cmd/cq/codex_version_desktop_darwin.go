//go:build darwin

package main

func probeCodexDesktopVersion() (string, bool) {
	paths := []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	}
	for _, path := range paths {
		if version, ok := probeCodexVersionCommand(path); ok {
			return version, true
		}
	}
	return "", false
}
