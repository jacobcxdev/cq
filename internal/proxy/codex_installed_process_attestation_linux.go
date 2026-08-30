//go:build linux

package proxy

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strconv"
)

const codexInstalledLinuxProxyUnit = "cq-proxy.service"

type codexInstalledLinuxServiceProof struct {
	unit       string
	path       string
	executable string
	sha256     [sha256.Size]byte
}

func (proof codexInstalledLinuxServiceProof) valid() bool {
	return proof.unit == codexInstalledLinuxProxyUnit && filepath.IsAbs(proof.path) &&
		filepath.Clean(proof.path) == proof.path && filepath.Base(proof.path) == proof.unit &&
		filepath.IsAbs(proof.executable) && filepath.Clean(proof.executable) == proof.executable &&
		proof.sha256 != ([sha256.Size]byte{})
}

type codexInstalledLinuxProcessVerifierDependencies struct {
	pid               func() int
	uid               func() int
	executablePath    func() (string, error)
	captureExecutable func(string) (codexInstalledExecutableProof, error)
	captureService    func(int, codexInstalledExecutableProof) (codexInstalledLinuxServiceProof, error)
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
	if dependencies.captureService == nil {
		dependencies.captureService = captureCodexInstalledLinuxService
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
		dependencies.captureService == nil || dependencies.loadPort == nil || dependencies.captureProcess == nil || dependencies.captureListener == nil {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}

	currentPID, uid := dependencies.pid(), dependencies.uid()
	if currentPID <= 1 || uid < 0 {
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
	service, err := dependencies.captureService(uid, executable)
	if err != nil || !service.valid() || service.executable != executable.path {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	current, err := dependencies.captureProcess(currentPID)
	if err != nil {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	listenerOwner, port, err := codexInstalledLinuxListenerOwner(current, uid, executable, dependencies)
	if err != nil {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	listener, err := dependencies.captureListener(listenerOwner.PID, port)
	if err != nil || !listener.Valid() || !listener.Process.Equal(listenerOwner) {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	if err := ctx.Err(); err != nil {
		return codexInstalledProcessPlatformProof{}, err
	}
	currentAfter, err := dependencies.captureProcess(currentPID)
	if err != nil || !currentAfter.Equal(current) {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	if listenerOwner.PID != currentPID {
		listenerOwnerAfter, captureErr := dependencies.captureProcess(listenerOwner.PID)
		if captureErr != nil || !listenerOwnerAfter.Equal(listenerOwner) {
			return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
		}
	}
	listenerAfter, err := dependencies.captureListener(listenerOwner.PID, port)
	if err != nil || !equalCodexInstalledLinuxListener(listenerAfter, listener) {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	currentExecutable, err := dependencies.captureExecutable(executablePath)
	if err != nil || currentExecutable != executable {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	serviceAfter, err := dependencies.captureService(uid, executable)
	if err != nil || serviceAfter != service {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	proof := codexInstalledProcessPlatformProof{
		pid:                   listenerOwner.PID,
		serviceKind:           codexInstalledListenerServiceSystemdUser,
		persistent:            true,
		executable:            executable,
		serviceIdentitySHA256: codexInstalledLinuxServiceIdentity(uid, service, executable),
	}
	if !proof.valid() {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	return proof, nil
}

func codexInstalledLinuxListenerOwner(
	current LinuxProcessIdentity,
	uid int,
	executable codexInstalledExecutableProof,
	dependencies codexInstalledLinuxProcessVerifierDependencies,
) (LinuxProcessIdentity, int, error) {
	if !codexInstalledLinuxProcessIdentityMatches(current, uid, executable, dependencies.captureExecutable) {
		return LinuxProcessIdentity{}, 0, errCodexInstalledProcessAttestation
	}
	if codexInstalledLinuxSystemdSupervisorMatches(current, uid, executable, dependencies.captureExecutable) {
		port, err := dependencies.loadPort()
		if err != nil || !validCodexInstalledLinuxPort(port) {
			return LinuxProcessIdentity{}, 0, errCodexInstalledProcessAttestation
		}
		return current, port, nil
	}
	if port, ok := codexInstalledLinuxCandidatePort(current.Arguments, executable.path); ok {
		return current, port, nil
	}
	if len(current.Arguments) != 23 || current.ParentPID <= 1 {
		return LinuxProcessIdentity{}, 0, errCodexInstalledProcessAttestation
	}
	manifest, err := ParseRuntimeRoleArguments(current.Arguments[3:])
	if err != nil || manifest.Role != RuntimeRoleWorker || manifest.ManifestDigest != executable.sha256 {
		return LinuxProcessIdentity{}, 0, errCodexInstalledProcessAttestation
	}
	supervisor, err := dependencies.captureProcess(current.ParentPID)
	if err != nil || !codexInstalledLinuxSystemdSupervisorMatches(supervisor, uid, executable, dependencies.captureExecutable) ||
		supervisor.CgroupPath != current.CgroupPath {
		return LinuxProcessIdentity{}, 0, errCodexInstalledProcessAttestation
	}
	port, err := dependencies.loadPort()
	if err != nil || !validCodexInstalledLinuxPort(port) {
		return LinuxProcessIdentity{}, 0, errCodexInstalledProcessAttestation
	}
	return supervisor, port, nil
}

func codexInstalledLinuxProcessIdentityMatches(
	process LinuxProcessIdentity,
	uid int,
	executable codexInstalledExecutableProof,
	captureExecutable func(string) (codexInstalledExecutableProof, error),
) bool {
	if !process.Valid() || process.UID != uint64(uid) || len(process.Arguments) < 3 ||
		process.Arguments[0] != executable.path || process.Arguments[1] != "proxy" || process.Arguments[2] != "start" ||
		!codexInstalledLinuxExecutableMatches(process.Executable, executable) || captureExecutable == nil {
		return false
	}
	current, err := captureExecutable(process.Arguments[0])
	return err == nil && current == executable
}

func codexInstalledLinuxSystemdSupervisorMatches(
	process LinuxProcessIdentity,
	uid int,
	executable codexInstalledExecutableProof,
	captureExecutable func(string) (codexInstalledExecutableProof, error),
) bool {
	return len(process.Arguments) == 3 && linuxProxyRuntimeCgroupMatches(process.CgroupPath, uint64(uid)) &&
		codexInstalledLinuxProcessIdentityMatches(process, uid, executable, captureExecutable)
}

func codexInstalledLinuxCandidatePort(arguments []string, executable string) (int, bool) {
	if len(arguments) != 7 || arguments[0] != executable || arguments[1] != "proxy" || arguments[2] != "start" ||
		arguments[3] != "--port" || arguments[5] != "--linux-validation-candidate-fd" || arguments[6] != "3" {
		return 0, false
	}
	port, err := strconv.Atoi(arguments[4])
	return port, err == nil && strconv.Itoa(port) == arguments[4] && validCodexInstalledLinuxPort(port) && port != DefaultPort
}

func validCodexInstalledLinuxPort(port int) bool {
	return port >= 1 && port <= 65_535
}

func codexInstalledLinuxExecutableMatches(actual LinuxExecutableIdentity, expected codexInstalledExecutableProof) bool {
	return actual.Valid() && expected.valid() && actual.Path == expected.path && actual.Device == expected.device &&
		actual.Inode == expected.inode && actual.Links == expected.links && actual.Owner == expected.owner &&
		actual.Size == expected.size && os.FileMode(actual.Mode&0o777) == expected.mode.Perm() && actual.SHA256 == expected.sha256
}

func equalCodexInstalledLinuxListener(left, right LinuxListenerIdentity) bool {
	return left.Valid() && right.Valid() && left.Address == right.Address && left.Inode == right.Inode && left.Process.Equal(right.Process)
}

func codexInstalledLinuxServiceIdentity(uid int, service codexInstalledLinuxServiceProof, executable codexInstalledExecutableProof) [sha256.Size]byte {
	destination := sha256.New()
	writeCodexInstalledProcessBindingField(destination, []byte("cq-codex-installed-linux-systemd-service-v2"))
	writeCodexInstalledProcessBindingField(destination, []byte(codexInstalledListenerServiceSystemdUser))
	writeCodexInstalledProcessBindingField(destination, []byte(service.unit))
	writeCodexInstalledProcessBindingField(destination, []byte(service.path))
	writeCodexInstalledProcessBindingInt(destination, uid)
	writeCodexInstalledProcessBindingField(destination, []byte(service.executable))
	writeCodexInstalledProcessBindingField(destination, service.sha256[:])
	writeCodexInstalledProcessBindingField(destination, []byte(executable.path))
	writeCodexInstalledProcessBindingField(destination, executable.sha256[:])
	var digest [sha256.Size]byte
	copy(digest[:], destination.Sum(nil))
	return digest
}

func runCodexInstalledVersionCommand(ctx context.Context, path string, expected codexInstalledExecutableProof) ([]byte, error) {
	return runCodexInstalledVersionCommandWithRunner(ctx, path, expected, osCodexAcceptanceRunner{})
}
