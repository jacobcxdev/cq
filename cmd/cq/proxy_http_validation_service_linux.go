//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

const linuxInstalledHTTPValidationUnitMaxBytes = 64 << 10

type linuxInstalledHTTPValidationServiceOperations struct {
	currentExecutable func() (string, error)
	unitPath          func() (string, error)
	readUnit          func(string) ([]byte, string, error)
	captureExecutable func(string) (proxy.LinuxExecutableIdentity, error)
	inspect           func(context.Context) (serviceStatus, error)
	candidatePort     func() (int, bool)
}

type linuxInstalledHTTPValidationCandidateOperations struct {
	resolveService  func(string) (installedHTTPValidationServiceBinding, error)
	controller      func() (int, int, bool)
	captureProcess  func(int) (proxy.LinuxProcessIdentity, error)
	captureListener func(int, int) (proxy.LinuxListenerIdentity, error)
}

type linuxInstalledHTTPValidationCandidateProcess struct {
	command *exec.Cmd
	pid     int
	port    int
}

var linuxInstalledHTTPValidationCandidateState struct {
	sync.Mutex
	process   *linuxInstalledHTTPValidationCandidateProcess
	childPort int
	starting  bool
}

func init() {
	activateProxyValidationCandidateFn = activateLinuxValidationCandidate
}

func resolveInstalledHTTPValidationService(expectedLabel string) (installedHTTPValidationServiceBinding, error) {
	return resolveInstalledHTTPValidationServiceWithLinuxOperations(context.Background(), expectedLabel, linuxInstalledHTTPValidationServiceOperations{})
}

func resolveInstalledHTTPValidationServiceWithLinuxOperations(
	ctx context.Context,
	expectedLabel string,
	operations linuxInstalledHTTPValidationServiceOperations,
) (installedHTTPValidationServiceBinding, error) {
	operations, err := defaultLinuxInstalledHTTPValidationServiceOperations(operations)
	if err != nil || ctx == nil || ctx.Err() != nil ||
		(expectedLabel != "" && expectedLabel != systemdProxyUnit && expectedLabel != candidateProxyAgentLabel) {
		return installedHTTPValidationServiceBinding{}, errors.New("installed Linux proxy service binding is unavailable")
	}
	currentPath, err := operations.currentExecutable()
	if err != nil || !filepath.IsAbs(currentPath) || filepath.Clean(currentPath) != currentPath {
		return installedHTTPValidationServiceBinding{}, errors.New("resolve current CQ executable")
	}
	executable, err := operations.captureExecutable(currentPath)
	if err != nil || !validLinuxInstalledHTTPValidationExecutable(executable) {
		return installedHTTPValidationServiceBinding{}, errors.New("capture current CQ executable")
	}
	unitPath, err := operations.unitPath()
	if err != nil || !validLinuxInstalledHTTPValidationUnitPath(unitPath) {
		return installedHTTPValidationServiceBinding{}, errors.New("resolve installed proxy unit")
	}
	unit, unitDigest, err := operations.readUnit(unitPath)
	if err != nil || !isLowerHexSHA256(unitDigest) {
		return installedHTTPValidationServiceBinding{}, errors.New("read installed proxy unit")
	}
	definitions, err := renderSystemdServiceDefinitions(executable.Path)
	if err != nil || !bytes.Equal(unit, definitions[systemdProxyUnit]) {
		return installedHTTPValidationServiceBinding{}, errors.New("installed proxy unit is not canonical")
	}
	status, err := operations.inspect(ctx)
	if err != nil || !validLinuxInstalledHTTPValidationServiceStatus(status.Proxy, executable.Path) {
		return installedHTTPValidationServiceBinding{}, errors.New("installed proxy manager proof is unavailable")
	}
	label := systemdProxyUnit
	port := 0
	if expectedLabel == candidateProxyAgentLabel {
		label = candidateProxyAgentLabel
		var ok bool
		port, ok = operations.candidatePort()
		if !ok || port < 1 || port > 65_535 || port == proxy.DefaultPort {
			return installedHTTPValidationServiceBinding{}, errors.New("installed validation candidate port is unavailable")
		}
	}
	executableAfter, err := operations.captureExecutable(currentPath)
	if err != nil || executableAfter != executable {
		return installedHTTPValidationServiceBinding{}, errors.New("current CQ executable changed")
	}
	unitAfter, unitDigestAfter, err := operations.readUnit(unitPath)
	if err != nil || unitDigestAfter != unitDigest || !bytes.Equal(unitAfter, unit) {
		return installedHTTPValidationServiceBinding{}, errors.New("installed proxy unit changed")
	}
	statusAfter, err := operations.inspect(ctx)
	if err != nil || statusAfter.Proxy != status.Proxy {
		return installedHTTPValidationServiceBinding{}, errors.New("installed proxy manager proof changed")
	}
	payload, err := json.Marshal(struct {
		Version          int    `json:"version"`
		Label            string `json:"label"`
		Unit             string `json:"unit"`
		UnitPath         string `json:"unit_path"`
		ExecutablePath   string `json:"executable_path"`
		ExecutableSHA256 string `json:"executable_sha256"`
		UnitSHA256       string `json:"unit_sha256"`
		Port             int    `json:"port,omitempty"`
	}{
		Version: 1, Label: label, Unit: systemdProxyUnit, UnitPath: unitPath,
		ExecutablePath: executable.Path, ExecutableSHA256: hex.EncodeToString(executable.SHA256[:]),
		UnitSHA256: unitDigest, Port: port,
	})
	if err != nil {
		return installedHTTPValidationServiceBinding{}, errors.New("encode installed proxy service binding")
	}
	serviceDigest := sha256.Sum256(payload)
	return installedHTTPValidationServiceBinding{
		label: label, executableSHA256: hex.EncodeToString(executable.SHA256[:]),
		serviceSHA256: hex.EncodeToString(serviceDigest[:]), port: port,
	}, nil
}

func defaultLinuxInstalledHTTPValidationServiceOperations(operations linuxInstalledHTTPValidationServiceOperations) (linuxInstalledHTTPValidationServiceOperations, error) {
	if operations.currentExecutable == nil {
		operations.currentExecutable = os.Executable
	}
	if operations.unitPath == nil {
		operations.unitPath = func() (string, error) {
			directory, err := linuxSystemdUserDirectory()
			return filepath.Join(directory, systemdProxyUnit), err
		}
	}
	if operations.readUnit == nil {
		operations.readUnit = readLinuxInstalledHTTPValidationUnit
	}
	if operations.captureExecutable == nil {
		operations.captureExecutable = proxy.CaptureLinuxExecutable
	}
	if operations.inspect == nil {
		operations.inspect = func(ctx context.Context) (serviceStatus, error) {
			platform, _, _, err := defaultLinuxSystemdPlatform()
			if err != nil {
				return serviceStatus{}, err
			}
			return platform.Inspect(ctx)
		}
	}
	if operations.candidatePort == nil {
		operations.candidatePort = currentLinuxInstalledHTTPValidationCandidatePort
	}
	if operations.currentExecutable == nil || operations.unitPath == nil || operations.readUnit == nil ||
		operations.captureExecutable == nil || operations.inspect == nil || operations.candidatePort == nil {
		return operations, errors.New("incomplete installed Linux proxy service resolver")
	}
	return operations, nil
}

func validLinuxInstalledHTTPValidationExecutable(executable proxy.LinuxExecutableIdentity) bool {
	uid := uint64(os.Geteuid())
	return executable.Valid() && (executable.Owner == 0 || executable.Owner == uid)
}

func validLinuxInstalledHTTPValidationUnitPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Base(path) == systemdProxyUnit &&
		filepath.Base(filepath.Dir(path)) == "user" && filepath.Base(filepath.Dir(filepath.Dir(path))) == "systemd"
}

func validLinuxInstalledHTTPValidationServiceStatus(status componentStatus, executable string) bool {
	if status.ID != systemdProxyUnit || status.Manager != "systemd-user" || !status.Registered || !status.Running || !status.Healthy ||
		status.PID <= 1 || status.Error != "" || status.ConfiguredExecutable != executable || status.LiveExecutable != executable {
		return false
	}
	host, port, err := net.SplitHostPort(status.Listener)
	parsedPort, portErr := strconv.Atoi(port)
	return err == nil && portErr == nil && host == "127.0.0.1" && parsedPort >= 1 && parsedPort <= 65_535
}

func validateInstalledHTTPValidationCandidate(port int) (installedHTTPValidationCandidateAuthority, error) {
	if port < 1 || port > 65_535 || port == proxy.DefaultPort {
		return installedHTTPValidationCandidateAuthority{}, errors.New("invalid installed validation candidate port")
	}
	if _, _, ok := currentLinuxInstalledHTTPValidationCandidateController(); !ok {
		if err := startLinuxInstalledHTTPValidationCandidate(port); err != nil {
			return installedHTTPValidationCandidateAuthority{}, err
		}
	}
	return validateInstalledHTTPValidationCandidateWithLinuxOperations(port, linuxInstalledHTTPValidationCandidateOperations{})
}

func validateInstalledHTTPValidationCandidateWithLinuxOperations(
	port int,
	operations linuxInstalledHTTPValidationCandidateOperations,
) (installedHTTPValidationCandidateAuthority, error) {
	if operations.resolveService == nil {
		operations.resolveService = resolveInstalledHTTPValidationService
	}
	if operations.controller == nil {
		operations.controller = currentLinuxInstalledHTTPValidationCandidateController
	}
	if operations.captureProcess == nil {
		operations.captureProcess = proxy.CaptureLinuxProcess
	}
	if operations.captureListener == nil {
		operations.captureListener = proxy.CaptureLinuxListener
	}
	if port < 1 || port > 65_535 || port == proxy.DefaultPort || operations.resolveService == nil ||
		operations.controller == nil || operations.captureProcess == nil || operations.captureListener == nil {
		return installedHTTPValidationCandidateAuthority{}, errors.New("incomplete installed validation candidate authority")
	}
	binding, err := operations.resolveService(candidateProxyAgentLabel)
	if err != nil || binding.validate() != nil || binding.label != candidateProxyAgentLabel || binding.port != port {
		return installedHTTPValidationCandidateAuthority{}, errors.New("installed validation candidate binding is unavailable")
	}
	pid, controlledPort, ok := operations.controller()
	if !ok || pid <= 1 || controlledPort != port {
		return installedHTTPValidationCandidateAuthority{}, errors.New("installed validation candidate controller changed")
	}
	process, err := operations.captureProcess(pid)
	if err != nil || !validLinuxInstalledHTTPValidationCandidateProcess(process, pid, port, binding) {
		return installedHTTPValidationCandidateAuthority{}, errors.New("installed validation candidate process is unavailable")
	}
	listener, err := operations.captureListener(pid, port)
	if err != nil || !listener.Valid() || listener.Address != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) || !listener.Process.Equal(process) {
		return installedHTTPValidationCandidateAuthority{}, errors.New("installed validation candidate listener is unavailable")
	}
	processAfter, err := operations.captureProcess(pid)
	if err != nil || !processAfter.Equal(process) {
		return installedHTTPValidationCandidateAuthority{}, errors.New("installed validation candidate process changed")
	}
	listenerAfter, err := operations.captureListener(pid, port)
	if err != nil || !listenerAfter.Valid() || listenerAfter.Address != listener.Address || listenerAfter.Inode != listener.Inode || !listenerAfter.Process.Equal(listener.Process) {
		return installedHTTPValidationCandidateAuthority{}, errors.New("installed validation candidate listener changed")
	}
	pidAfter, portAfter, ok := operations.controller()
	if !ok || pidAfter != pid || portAfter != port {
		return installedHTTPValidationCandidateAuthority{}, errors.New("installed validation candidate controller changed")
	}
	return installedHTTPValidationCandidateAuthority{binding: binding, pid: pid}, nil
}

func validLinuxInstalledHTTPValidationCandidateProcess(
	process proxy.LinuxProcessIdentity,
	pid int,
	port int,
	binding installedHTTPValidationServiceBinding,
) bool {
	return process.Valid() && process.PID == pid && len(process.Arguments) == 7 &&
		process.Arguments[0] == process.Executable.Path && process.Arguments[1] == "proxy" && process.Arguments[2] == "start" &&
		process.Arguments[3] == "--port" && process.Arguments[4] == strconv.Itoa(port) &&
		process.Arguments[5] == "--linux-validation-candidate-fd" && process.Arguments[6] == "3" &&
		hex.EncodeToString(process.Executable.SHA256[:]) == binding.executableSHA256
}

func activateLinuxValidationCandidate(fd, port int) error {
	if fd != 3 || port < 1 || port > 65_535 || port == proxy.DefaultPort {
		return errors.New("invalid Linux validation candidate controller")
	}
	file := os.NewFile(uintptr(fd), "linux-validation-candidate-controller")
	if file == nil {
		return errors.New("Linux validation candidate controller is unavailable")
	}
	if err := readLinuxValidationCandidateController(file); err != nil {
		return err
	}
	linuxInstalledHTTPValidationCandidateState.Lock()
	linuxInstalledHTTPValidationCandidateState.childPort = port
	linuxInstalledHTTPValidationCandidateState.Unlock()
	return nil
}

func readLinuxValidationCandidateController(reader io.ReadCloser) error {
	if reader == nil {
		return errors.New("Linux validation candidate controller is unavailable")
	}
	defer reader.Close()
	token := make([]byte, 32)
	defer clearLinuxInstalledHTTPValidationBytes(token)
	if _, err := io.ReadFull(reader, token); err != nil {
		return errors.New("Linux validation candidate controller framing is invalid")
	}
	var trailing [1]byte
	if count, err := reader.Read(trailing[:]); count != 0 || !errors.Is(err, io.EOF) {
		return errors.New("Linux validation candidate controller framing is invalid")
	}
	zero := true
	for _, value := range token {
		zero = zero && value == 0
	}
	if zero {
		return errors.New("Linux validation candidate controller framing is invalid")
	}
	return nil
}

func currentLinuxInstalledHTTPValidationCandidatePort() (int, bool) {
	linuxInstalledHTTPValidationCandidateState.Lock()
	defer linuxInstalledHTTPValidationCandidateState.Unlock()
	if linuxInstalledHTTPValidationCandidateState.childPort != 0 {
		return linuxInstalledHTTPValidationCandidateState.childPort, true
	}
	if process := linuxInstalledHTTPValidationCandidateState.process; process != nil {
		return process.port, true
	}
	return 0, false
}

func currentLinuxInstalledHTTPValidationCandidateController() (int, int, bool) {
	linuxInstalledHTTPValidationCandidateState.Lock()
	defer linuxInstalledHTTPValidationCandidateState.Unlock()
	process := linuxInstalledHTTPValidationCandidateState.process
	if process == nil || process.pid <= 1 {
		return 0, 0, false
	}
	return process.pid, process.port, true
}

func startLinuxInstalledHTTPValidationCandidate(port int) (returnErr error) {
	linuxInstalledHTTPValidationCandidateState.Lock()
	if linuxInstalledHTTPValidationCandidateState.process != nil || linuxInstalledHTTPValidationCandidateState.starting {
		linuxInstalledHTTPValidationCandidateState.Unlock()
		return errors.New("installed validation candidate is already running")
	}
	linuxInstalledHTTPValidationCandidateState.starting = true
	linuxInstalledHTTPValidationCandidateState.Unlock()
	defer func() {
		linuxInstalledHTTPValidationCandidateState.Lock()
		linuxInstalledHTTPValidationCandidateState.starting = false
		linuxInstalledHTTPValidationCandidateState.Unlock()
	}()
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return errors.New("resolve installed validation candidate executable")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	token := make([]byte, 32)
	defer clearLinuxInstalledHTTPValidationBytes(token)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		_ = writer.Close()
		return err
	}
	command := exec.Command(executable, "proxy", "start", "--port", strconv.Itoa(port), "--linux-validation-candidate-fd", "3")
	command.ExtraFiles = []*os.File{reader}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = writer.Close()
		return err
	}
	process := &linuxInstalledHTTPValidationCandidateProcess{command: command, pid: command.Process.Pid, port: port}
	linuxInstalledHTTPValidationCandidateState.Lock()
	linuxInstalledHTTPValidationCandidateState.process = process
	linuxInstalledHTTPValidationCandidateState.Unlock()
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, stopLinuxInstalledHTTPValidationCandidate())
		}
	}()
	if count, err := writer.Write(token); err != nil || count != len(token) {
		_ = writer.Close()
		return errors.Join(err, errors.New("write installed validation candidate controller"))
	}
	if err := writer.Close(); err != nil {
		return err
	}
	_ = reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := validateInstalledHTTPValidationCandidateWithLinuxOperations(port, linuxInstalledHTTPValidationCandidateOperations{}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("installed validation candidate did not become ready")
		case <-ticker.C:
		}
	}
}

func restartInstalledHTTPValidationCandidate(label string) error {
	if label != candidateProxyAgentLabel {
		return errors.New("unsupported installed candidate service label")
	}
	_, port, ok := currentLinuxInstalledHTTPValidationCandidateController()
	if !ok {
		return errors.New("installed validation candidate controller is unavailable")
	}
	if err := stopLinuxInstalledHTTPValidationCandidate(); err != nil {
		return err
	}
	if err := startLinuxInstalledHTTPValidationCandidate(port); err != nil {
		return err
	}
	linuxInstalledHTTPValidationCandidateState.Lock()
	process := linuxInstalledHTTPValidationCandidateState.process
	linuxInstalledHTTPValidationCandidateState.process = nil
	linuxInstalledHTTPValidationCandidateState.starting = false
	linuxInstalledHTTPValidationCandidateState.Unlock()
	if process == nil || process.command == nil || process.command.Process == nil {
		return errors.New("installed validation candidate controller is unavailable")
	}
	if err := process.command.Process.Release(); err != nil {
		_ = unix.Kill(-process.pid, unix.SIGKILL)
		return err
	}
	return nil
}

func cleanupInstalledHTTPValidationCandidate() {
	_ = stopLinuxInstalledHTTPValidationCandidate()
}

func stopLinuxInstalledHTTPValidationCandidate() error {
	linuxInstalledHTTPValidationCandidateState.Lock()
	process := linuxInstalledHTTPValidationCandidateState.process
	linuxInstalledHTTPValidationCandidateState.process = nil
	linuxInstalledHTTPValidationCandidateState.Unlock()
	if process == nil || process.command == nil || process.command.Process == nil {
		return nil
	}
	_ = unix.Kill(-process.pid, unix.SIGTERM)
	wait := make(chan error, 1)
	go func() { wait <- process.command.Wait() }()
	select {
	case err := <-wait:
		if err == nil || process.command.ProcessState != nil {
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		_ = unix.Kill(-process.pid, unix.SIGKILL)
		select {
		case <-wait:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("installed validation candidate cleanup timed out")
		}
	}
}

func readLinuxInstalledHTTPValidationUnit(path string) ([]byte, string, error) {
	if !validLinuxInstalledHTTPValidationUnitPath(path) {
		return nil, "", errors.New("invalid installed proxy unit path")
	}
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", err
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, "", errors.New("open installed proxy unit descriptor")
	}
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(descriptor, &before); err != nil || !validLinuxInstalledHTTPValidationUnitStat(before) {
		return nil, "", errors.New("unsafe installed proxy unit")
	}
	data, err := io.ReadAll(io.LimitReader(file, linuxInstalledHTTPValidationUnitMaxBytes+1))
	if err != nil || int64(len(data)) != before.Size || len(data) > linuxInstalledHTTPValidationUnitMaxBytes {
		return nil, "", errors.New("installed proxy unit changed while reading")
	}
	var after, pathStat unix.Stat_t
	if err := unix.Fstat(descriptor, &after); err != nil || !sameLinuxInstalledHTTPValidationUnitStat(before, after) {
		return nil, "", errors.New("installed proxy unit changed while reading")
	}
	if err := unix.Lstat(path, &pathStat); err != nil || !sameLinuxInstalledHTTPValidationUnitStat(before, pathStat) {
		return nil, "", errors.New("installed proxy unit path changed while reading")
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}

func validLinuxInstalledHTTPValidationUnitStat(stat unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid()) &&
		stat.Mode&0o022 == 0 && stat.Size > 0 && stat.Size <= linuxInstalledHTTPValidationUnitMaxBytes
}

func sameLinuxInstalledHTTPValidationUnitStat(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Nlink == right.Nlink && left.Uid == right.Uid &&
		left.Gid == right.Gid && left.Mode == right.Mode && left.Size == right.Size && left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func clearLinuxInstalledHTTPValidationBytes(values []byte) {
	for index := range values {
		values[index] = 0
	}
}
