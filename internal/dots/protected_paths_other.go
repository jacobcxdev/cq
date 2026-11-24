//go:build !windows

package dots

import "path/filepath"

func ProtectedAppRoots(home string) ([]string, error) {
	paths := []string{filepath.Join(home, ".codex"), "/Applications/ChatGPT.app"}
	for _, name := range []string{"Codex", "ChatGPT", "com.openai.codex", "com.openai.chat", "OpenAI"} {
		paths = append(paths, filepath.Join(home, "Library", "Application Support", name))
	}
	return paths, nil
}
