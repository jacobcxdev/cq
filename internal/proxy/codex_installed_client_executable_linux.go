//go:build linux

package proxy

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if linuxCodexELFExecutable(resolved) {
		return resolved, nil
	}
	packageRoot := filepath.Dir(filepath.Dir(resolved))
	if filepath.Base(resolved) != "codex.js" || filepath.Base(filepath.Dir(resolved)) != "bin" ||
		filepath.Base(packageRoot) != "codex" || filepath.Base(filepath.Dir(packageRoot)) != "@openai" {
		return "", errCodexInstalledProcessAttestation
	}
	packageName, target := linuxCodexNativePackage(runtime.GOARCH)
	if packageName == "" || target == "" {
		return "", errCodexInstalledProcessAttestation
	}
	candidate := filepath.Join(packageRoot, "node_modules", "@openai", packageName, "vendor", target, "bin", "codex")
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil || resolvedCandidate != candidate || !linuxCodexELFExecutable(candidate) {
		return "", errCodexInstalledProcessAttestation
	}
	return candidate, nil
}

func linuxCodexNativePackage(goarch string) (string, string) {
	switch goarch {
	case "amd64":
		return "codex-linux-x64", "x86_64-unknown-linux-musl"
	case "arm64":
		return "codex-linux-arm64", "aarch64-unknown-linux-musl"
	default:
		return "", ""
	}
}

func linuxCodexELFExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{0x7f, 'E', 'L', 'F'}
}
