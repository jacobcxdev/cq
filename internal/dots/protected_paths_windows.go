//go:build windows

package dots

import (
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

// ProtectedAppRoots names current-user Desktop and Codex state that a Dots
// installation may read but must never contain or replace.
func ProtectedAppRoots(home string) ([]string, error) {
	anchors, err := userdirs.WindowsAppDataAnchors()
	if err != nil {
		return nil, err
	}
	paths := []string{
		filepath.Join(home, ".codex"),
		filepath.Join(anchors.LocalAppData, "Packages", "OpenAI.Codex_2p2nqsd0c76g0"),
	}
	for _, base := range []string{anchors.RoamingAppData, anchors.LocalAppData} {
		for _, name := range []string{"Codex", "ChatGPT", "OpenAI", "com.openai.codex", "com.openai.chat"} {
			paths = append(paths, filepath.Join(base, name))
		}
	}
	return paths, nil
}
