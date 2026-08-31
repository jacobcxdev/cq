//go:build !linux

package proxy

import (
	"os/exec"
	"path/filepath"
)

func resolveCodexInstalledClientExecutableFromPath() (string, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", errCodexInstalledProcessAttestation
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", errCodexInstalledProcessAttestation
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errCodexInstalledProcessAttestation
	}
	return resolved, nil
}
