//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

func TestLinuxInstalledHTTPValidationServiceBindsCanonicalSystemdUnit(t *testing.T) {
	executable := testLinuxInstalledHTTPValidationExecutable()
	definitions, err := renderSystemdServiceDefinitions(executable.Path)
	if err != nil {
		t.Fatal(err)
	}
	unit := definitions[systemdProxyUnit]
	unitDigest := sha256.Sum256(unit)
	status := testLinuxInstalledHTTPValidationServiceStatus(executable.Path)
	operations := linuxInstalledHTTPValidationServiceOperations{
		currentExecutable: func() (string, error) { return executable.Path, nil },
		unitPath:          func() (string, error) { return "/home/test/.config/systemd/user/cq-proxy.service", nil },
		readUnit:          func(string) ([]byte, string, error) { return unit, hex.EncodeToString(unitDigest[:]), nil },
		captureExecutable: func(string) (proxy.LinuxExecutableIdentity, error) { return executable, nil },
		inspect:           func(context.Context) (serviceStatus, error) { return status, nil },
		candidatePort:     func() (int, bool) { return 29280, true },
	}

	persistent, err := resolveInstalledHTTPValidationServiceWithLinuxOperations(context.Background(), "", operations)
	if err != nil {
		t.Fatal(err)
	}
	if persistent.label != systemdProxyUnit || persistent.port != 0 || persistent.executableSHA256 != hex.EncodeToString(executable.SHA256[:]) {
		t.Fatalf("persistent binding = %#v", persistent)
	}
	candidate, err := resolveInstalledHTTPValidationServiceWithLinuxOperations(context.Background(), candidateProxyAgentLabel, operations)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.label != candidateProxyAgentLabel || candidate.port != 29280 || candidate.executableSHA256 != persistent.executableSHA256 || candidate.serviceSHA256 == persistent.serviceSHA256 {
		t.Fatalf("candidate binding = %#v, persistent = %#v", candidate, persistent)
	}
}

func TestLinuxInstalledHTTPValidationServiceFailsClosed(t *testing.T) {
	executable := testLinuxInstalledHTTPValidationExecutable()
	definitions, err := renderSystemdServiceDefinitions(executable.Path)
	if err != nil {
		t.Fatal(err)
	}
	unit := definitions[systemdProxyUnit]
	unitDigest := sha256.Sum256(unit)
	valid := linuxInstalledHTTPValidationServiceOperations{
		currentExecutable: func() (string, error) { return executable.Path, nil },
		unitPath:          func() (string, error) { return "/home/test/.config/systemd/user/cq-proxy.service", nil },
		readUnit:          func(string) ([]byte, string, error) { return unit, hex.EncodeToString(unitDigest[:]), nil },
		captureExecutable: func(string) (proxy.LinuxExecutableIdentity, error) { return executable, nil },
		inspect: func(context.Context) (serviceStatus, error) {
			return testLinuxInstalledHTTPValidationServiceStatus(executable.Path), nil
		},
		candidatePort: func() (int, bool) { return 29280, true },
	}
	tests := map[string]func(*linuxInstalledHTTPValidationServiceOperations){
		"unit changed": func(operations *linuxInstalledHTTPValidationServiceOperations) {
			operations.readUnit = func(string) ([]byte, string, error) {
				changed := append(append([]byte(nil), unit...), []byte("Environment=unsafe\n")...)
				digest := sha256.Sum256(changed)
				return changed, hex.EncodeToString(digest[:]), nil
			}
		},
		"manager unhealthy": func(operations *linuxInstalledHTTPValidationServiceOperations) {
			operations.inspect = func(context.Context) (serviceStatus, error) {
				status := testLinuxInstalledHTTPValidationServiceStatus(executable.Path)
				status.Proxy.Healthy = false
				return status, nil
			}
		},
		"executable changed": func(operations *linuxInstalledHTTPValidationServiceOperations) {
			calls := 0
			operations.captureExecutable = func(string) (proxy.LinuxExecutableIdentity, error) {
				calls++
				changed := executable
				if calls > 1 {
					changed.SHA256 = sha256.Sum256([]byte("changed"))
				}
				return changed, nil
			}
		},
		"candidate port unavailable": func(operations *linuxInstalledHTTPValidationServiceOperations) {
			operations.candidatePort = func() (int, bool) { return 0, false }
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			operations := valid
			mutate(&operations)
			if _, err := resolveInstalledHTTPValidationServiceWithLinuxOperations(context.Background(), candidateProxyAgentLabel, operations); err == nil {
				t.Fatal("unsafe service binding accepted")
			}
		})
	}
}

func TestLinuxInstalledHTTPValidationCandidateBindsExactListener(t *testing.T) {
	executable := testLinuxInstalledHTTPValidationExecutable()
	serviceDigest := sha256.Sum256([]byte("service"))
	binding := installedHTTPValidationServiceBinding{
		label: candidateProxyAgentLabel, executableSHA256: hex.EncodeToString(executable.SHA256[:]),
		serviceSHA256: hex.EncodeToString(serviceDigest[:]), port: 29280,
	}
	process := proxy.LinuxProcessIdentity{
		PID: 4242, ParentPID: 1, StartTime: 91, UID: 501,
		Arguments:  []string{executable.Path, "proxy", "start", "--port", "29280", "--linux-validation-candidate-fd", "3"},
		CgroupPath: "/user.slice/user-501.slice/session-4.scope", Executable: executable,
	}
	listener := proxy.LinuxListenerIdentity{Address: "127.0.0.1:29280", Inode: 702, Process: process}
	operations := linuxInstalledHTTPValidationCandidateOperations{
		resolveService: func(string) (installedHTTPValidationServiceBinding, error) { return binding, nil },
		controller:     func() (int, int, bool) { return 4242, 29280, true },
		captureProcess: func(int) (proxy.LinuxProcessIdentity, error) { return process, nil },
		captureListener: func(int, int) (proxy.LinuxListenerIdentity, error) {
			return listener, nil
		},
	}
	authority, err := validateInstalledHTTPValidationCandidateWithLinuxOperations(29280, operations)
	if err != nil || authority.binding != binding || authority.pid != 4242 {
		t.Fatalf("authority = (%#v, %v)", authority, err)
	}

	calls := 0
	operations.captureProcess = func(int) (proxy.LinuxProcessIdentity, error) {
		calls++
		changed := process
		if calls > 1 {
			changed.StartTime++
		}
		return changed, nil
	}
	if _, err := validateInstalledHTTPValidationCandidateWithLinuxOperations(29280, operations); err == nil {
		t.Fatal("changed candidate process accepted")
	}
}

func TestLinuxValidationCandidateControllerTokenFraming(t *testing.T) {
	reader, writer := net.Pipe()
	token := make([]byte, 32)
	for index := range token {
		token[index] = byte(index + 1)
	}
	done := make(chan error, 1)
	go func() {
		_, writeErr := writer.Write(token)
		done <- errors.Join(writeErr, writer.Close())
	}()
	if err := readLinuxValidationCandidateController(reader); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLinuxValidationCandidateCommandOptionsAreExact(t *testing.T) {
	options, err := parseProxyCommandOptions([]string{"--port", "29280", "--linux-validation-candidate-fd", "3"})
	if err != nil || options.Port != 29280 || options.LinuxValidationCandidateFD != 3 {
		t.Fatalf("options = (%#v, %v)", options, err)
	}
	for _, arguments := range [][]string{
		{"--linux-validation-candidate-fd", "3"},
		{"--port", "19280", "--linux-validation-candidate-fd", "3"},
		{"--port", "29280", "--linux-validation-candidate-fd", "4"},
		{"--port", "29280", "--linux-validation-candidate-fd", "3", "--linux-validation-candidate-fd", "3"},
	} {
		options, err := parseProxyCommandOptions(arguments)
		if err == nil && (options.Port == 0 || options.Port == proxy.DefaultPort) {
			if runErr := runProxyStart(options); runErr == nil {
				t.Fatalf("unsafe candidate arguments accepted: %v", arguments)
			}
			continue
		}
		if err == nil {
			t.Fatalf("unsafe candidate arguments parsed: %v", arguments)
		}
	}
}

func testLinuxInstalledHTTPValidationExecutable() proxy.LinuxExecutableIdentity {
	return proxy.LinuxExecutableIdentity{
		Path: "/opt/cq/bin/cq", Device: 3, Inode: 7, Links: 1, Owner: uint64(os.Geteuid()), Size: 21,
		Mode: unix.S_IFREG | 0o500, SHA256: sha256.Sum256([]byte("exact CQ executable")),
	}
}

func testLinuxInstalledHTTPValidationServiceStatus(executable string) serviceStatus {
	return serviceStatus{Proxy: componentStatus{
		ID: systemdProxyUnit, Manager: "systemd-user", Registered: true, Running: true, Healthy: true,
		ConfiguredExecutable: executable, LiveExecutable: executable, PID: 3131, Listener: "127.0.0.1:19280",
	}}
}
