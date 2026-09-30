//go:build darwin

package main

func probeCodexDesktopVersion() (string, bool) {
	return probeCodexDesktopVersionWith(probeCodexVersionCommand)
}

func probeCodexDesktopVersionWith(probe func(string) (string, bool)) (string, bool) {
	paths := []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex",
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	}
	for _, path := range paths {
		if version, ok := probe(path); ok {
			return version, true
		}
	}
	return "", false
}
