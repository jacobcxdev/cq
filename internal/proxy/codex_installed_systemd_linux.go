//go:build linux

package proxy

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const codexInstalledLinuxSystemdUnitMaxBytes = 64 << 10

func captureCodexInstalledLinuxService(uid int, executable codexInstalledExecutableProof) (codexInstalledLinuxServiceProof, error) {
	if uid < 0 || uid != os.Geteuid() || !executable.valid() {
		return codexInstalledLinuxServiceProof{}, errCodexInstalledProcessAttestation
	}
	path, err := codexInstalledLinuxSystemdUnitPath()
	if err != nil {
		return codexInstalledLinuxServiceProof{}, errCodexInstalledProcessAttestation
	}
	want, err := renderCodexInstalledLinuxProxyUnit(executable.path)
	if err != nil {
		return codexInstalledLinuxServiceProof{}, errCodexInstalledProcessAttestation
	}
	data, err := readCodexInstalledLinuxSystemdUnit(path, uint32(uid))
	if err != nil || !bytes.Equal(data, want) {
		return codexInstalledLinuxServiceProof{}, errCodexInstalledProcessAttestation
	}
	proof := codexInstalledLinuxServiceProof{
		unit: codexInstalledLinuxProxyUnit, path: path, executable: executable.path, sha256: sha256.Sum256(data),
	}
	if !proof.valid() {
		return codexInstalledLinuxServiceProof{}, errCodexInstalledProcessAttestation
	}
	return proof, nil
}

func codexInstalledLinuxSystemdUnitPath() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) || filepath.Clean(home) != home {
			return "", errCodexInstalledProcessAttestation
		}
		configHome = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configHome) || filepath.Clean(configHome) != configHome {
		return "", errCodexInstalledProcessAttestation
	}
	return filepath.Join(configHome, "systemd", "user", codexInstalledLinuxProxyUnit), nil
}

func renderCodexInstalledLinuxProxyUnit(executable string) ([]byte, error) {
	encoded, err := encodeCodexInstalledLinuxSystemdArgument(executable)
	if err != nil {
		return nil, err
	}
	return []byte(`[Unit]
Description=CQ local proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + encoded + ` proxy start
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`), nil
}

func encodeCodexInstalledLinuxSystemdArgument(argument string) (string, error) {
	if argument == "" || !filepath.IsAbs(argument) || filepath.Clean(argument) != argument || strings.ContainsAny(argument, "\x00\r\n") {
		return "", errCodexInstalledProcessAttestation
	}
	argument = strings.ReplaceAll(argument, "%", "%%")
	safe := true
	for _, character := range argument {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && !strings.ContainsRune("/_+.,:=@-%", character) {
			safe = false
			break
		}
	}
	if safe {
		return argument, nil
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`)
	return `"` + replacer.Replace(argument) + `"`, nil
}

func readCodexInstalledLinuxSystemdUnit(path string, uid uint32) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errCodexInstalledProcessAttestation
	}
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, errCodexInstalledProcessAttestation
	}
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(descriptor, &before); err != nil || !validCodexInstalledLinuxUnitStat(before, uid) {
		return nil, errCodexInstalledProcessAttestation
	}
	data, err := io.ReadAll(io.LimitReader(file, codexInstalledLinuxSystemdUnitMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > codexInstalledLinuxSystemdUnitMaxBytes || int64(len(data)) != before.Size {
		return nil, errCodexInstalledProcessAttestation
	}
	var after unix.Stat_t
	if err := unix.Fstat(descriptor, &after); err != nil || !sameCodexInstalledLinuxUnitStat(before, after) {
		return nil, errCodexInstalledProcessAttestation
	}
	var pathStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil || !sameCodexInstalledLinuxUnitStat(before, pathStat) {
		return nil, errCodexInstalledProcessAttestation
	}
	return data, nil
}

func validCodexInstalledLinuxUnitStat(stat unix.Stat_t, uid uint32) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 && stat.Uid == uid &&
		stat.Mode&0o022 == 0 && stat.Size > 0 && stat.Size <= codexInstalledLinuxSystemdUnitMaxBytes
}

func sameCodexInstalledLinuxUnitStat(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Nlink == right.Nlink && left.Uid == right.Uid &&
		left.Gid == right.Gid && left.Mode == right.Mode && left.Size == right.Size &&
		left.Mtim == right.Mtim && left.Ctim == right.Ctim
}
