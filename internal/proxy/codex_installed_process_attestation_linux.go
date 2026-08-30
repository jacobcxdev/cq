//go:build linux

package proxy

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
)

const codexInstalledLinuxProxyUnit = "cq-proxy.service"

type codexInstalledLinuxProcessVerifierDependencies struct {
	pid               func() int
	uid               func() int
	executablePath    func() (string, error)
	captureExecutable func(string) (codexInstalledExecutableProof, error)
	loadPort          func() (int, error)
	captureProcess    func(int) (LinuxProcessIdentity, error)
	captureListener   func(int, int) (LinuxListenerIdentity, error)
}

type codexInstalledLinuxProcessVerifier struct {
	dependencies codexInstalledLinuxProcessVerifierDependencies
}

func defaultCodexInstalledProcessPlatformVerifier() codexInstalledProcessPlatformVerifier {
	return newCodexInstalledLinuxProcessVerifier(codexInstalledLinuxProcessVerifierDependencies{})
}

func newCodexInstalledLinuxProcessVerifier(dependencies codexInstalledLinuxProcessVerifierDependencies) *codexInstalledLinuxProcessVerifier {
	if dependencies.pid == nil {
		dependencies.pid = os.Getpid
	}
	if dependencies.uid == nil {
		dependencies.uid = os.Geteuid
	}
	if dependencies.executablePath == nil {
		dependencies.executablePath = os.Executable
	}
	if dependencies.captureExecutable == nil {
		dependencies.captureExecutable = captureCodexInstalledExecutable
	}
	if dependencies.loadPort == nil {
		dependencies.loadPort = func() (int, error) {
			config, err := LoadExistingConfig()
			if err != nil || config == nil {
				return 0, errCodexInstalledProcessAttestation
			}
			return config.Port, nil
		}
	}
	if dependencies.captureProcess == nil {
		dependencies.captureProcess = CaptureLinuxProcess
	}
	if dependencies.captureListener == nil {
		dependencies.captureListener = CaptureLinuxListener
	}
	return &codexInstalledLinuxProcessVerifier{dependencies: dependencies}
}

func (verifier *codexInstalledLinuxProcessVerifier) Capture(ctx context.Context) (codexInstalledProcessPlatformProof, error) {
	if ctx == nil || ctx.Err() != nil || verifier == nil {
		return codexInstalledProcessPlatformProof{}, codexInstalledAttestationError(ctx)
	}
	dependencies := verifier.dependencies
	if dependencies.pid == nil || dependencies.uid == nil || dependencies.executablePath == nil || dependencies.captureExecutable == nil ||
		dependencies.loadPort == nil || dependencies.captureProcess == nil || dependencies.captureListener == nil {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}

	pid, uid := dependencies.pid(), dependencies.uid()
	if pid <= 1 || uid < 0 {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	executablePath, err := dependencies.executablePath()
	if err != nil {
		return codexInstalledProcessPlatformProof{}, codexInstalledAttestationError(ctx)
	}
	executable, err := dependencies.captureExecutable(executablePath)
	if err != nil || !executable.valid() {
		return codexInstalledProcessPlatformProof{}, codexInstalledAttestationError(ctx)
	}
	port, err := dependencies.loadPort()
	if err != nil || port < 1 || port > 65_535 {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	process, err := dependencies.captureProcess(pid)
	if err != nil || !codexInstalledLinuxProcessMatches(process, pid, uid, executable, dependencies.captureExecutable) {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	listener, err := dependencies.captureListener(pid, port)
	if err != nil || !listener.Valid() || !listener.Process.Equal(process) {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	if err := ctx.Err(); err != nil {
		return codexInstalledProcessPlatformProof{}, err
	}
	processAfter, err := dependencies.captureProcess(pid)
	if err != nil || !processAfter.Equal(process) {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	listenerAfter, err := dependencies.captureListener(pid, port)
	if err != nil || !equalCodexInstalledLinuxListener(listenerAfter, listener) {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	currentExecutable, err := dependencies.captureExecutable(executablePath)
	if err != nil || currentExecutable != executable {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	serviceExecutable, err := dependencies.captureExecutable(process.Arguments[0])
	if err != nil || serviceExecutable != executable {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	serviceIdentity := codexInstalledLinuxServiceIdentity(uid, process.Arguments, executable)
	proof := codexInstalledProcessPlatformProof{
		pid:                   pid,
		serviceKind:           codexInstalledListenerServiceSystemdUser,
		persistent:            true,
		executable:            executable,
		serviceIdentitySHA256: serviceIdentity,
	}
	if !proof.valid() {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	return proof, nil
}

func codexInstalledLinuxProcessMatches(
	process LinuxProcessIdentity,
	pid int,
	uid int,
	executable codexInstalledExecutableProof,
	captureExecutable func(string) (codexInstalledExecutableProof, error),
) bool {
	if !process.Valid() || process.PID != pid || process.UID != uint64(uid) || len(process.Arguments) != 3 ||
		!filepath.IsAbs(process.Arguments[0]) || filepath.Clean(process.Arguments[0]) != process.Arguments[0] ||
		process.Arguments[1] != "proxy" || process.Arguments[2] != "start" ||
		!linuxProxyRuntimeCgroupMatches(process.CgroupPath, uint64(uid)) ||
		!codexInstalledLinuxExecutableMatches(process.Executable, executable) || captureExecutable == nil {
		return false
	}
	serviceExecutable, err := captureExecutable(process.Arguments[0])
	return err == nil && serviceExecutable == executable
}

func codexInstalledLinuxExecutableMatches(actual LinuxExecutableIdentity, expected codexInstalledExecutableProof) bool {
	return actual.Valid() && expected.valid() && actual.Path == expected.path && actual.Device == expected.device &&
		actual.Inode == expected.inode && actual.Links == expected.links && actual.Owner == expected.owner &&
		actual.Size == expected.size && os.FileMode(actual.Mode&0o777) == expected.mode.Perm() && actual.SHA256 == expected.sha256
}

func equalCodexInstalledLinuxListener(left, right LinuxListenerIdentity) bool {
	return left.Valid() && right.Valid() && left.Address == right.Address && left.Inode == right.Inode && left.Process.Equal(right.Process)
}

func codexInstalledLinuxServiceIdentity(uid int, arguments []string, executable codexInstalledExecutableProof) [sha256.Size]byte {
	destination := sha256.New()
	writeCodexInstalledProcessBindingField(destination, []byte("cq-codex-installed-linux-systemd-service-v1"))
	writeCodexInstalledProcessBindingField(destination, []byte(codexInstalledListenerServiceSystemdUser))
	writeCodexInstalledProcessBindingField(destination, []byte(codexInstalledLinuxProxyUnit))
	writeCodexInstalledProcessBindingInt(destination, uid)
	for _, argument := range arguments {
		writeCodexInstalledProcessBindingField(destination, []byte(argument))
	}
	writeCodexInstalledProcessBindingField(destination, []byte(executable.path))
	writeCodexInstalledProcessBindingField(destination, executable.sha256[:])
	var digest [sha256.Size]byte
	copy(digest[:], destination.Sum(nil))
	return digest
}

func runCodexInstalledVersionCommand(ctx context.Context, path string, expected codexInstalledExecutableProof) ([]byte, error) {
	return runCodexInstalledVersionCommandWithRunner(ctx, path, expected, osCodexAcceptanceRunner{})
}
