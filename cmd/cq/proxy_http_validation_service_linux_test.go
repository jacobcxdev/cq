//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

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
	manifest := proxy.RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: proxy.RuntimeRoleWorker, ManifestDigest: executable.SHA256,
		ProxyInstanceID: linuxValidationCandidateProxyInstanceID, RuntimeInstanceID: "runtime", ListenerFD: proxy.RuntimeNoListenerFD,
		LifecycleFD: proxy.RuntimeLifecycleFD, ControlFD: proxy.RuntimeControlFD, SecretFD: proxy.RuntimeSecretFD,
		WorkFD: proxy.RuntimeWorkFD, LifecycleHolderIdentityDigest: sha256.Sum256([]byte("worker holder")),
	}
	worker := proxy.LinuxProcessIdentity{
		PID: 4243, ParentPID: process.PID, StartTime: 92, UID: process.UID,
		Arguments:  append([]string{executable.Path, "proxy", "start"}, proxy.RuntimeRoleArguments(manifest)...),
		CgroupPath: process.CgroupPath, Executable: executable,
	}
	listener := proxy.LinuxListenerIdentity{Address: "127.0.0.1:29280", Inode: 702, Process: process}
	operations := linuxInstalledHTTPValidationCandidateOperations{
		resolveService: func(string) (installedHTTPValidationServiceBinding, error) { return binding, nil },
		controller:     func() (int, int, bool) { return 4242, 29280, true },
		captureProcess: func(int) (proxy.LinuxProcessIdentity, error) { return process, nil },
		captureWorker: func(context.Context, proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
			return worker, nil
		},
		captureListener: func(int, int) (proxy.LinuxListenerIdentity, error) {
			return listener, nil
		},
		ready: func(int) error { return nil },
	}
	authority, err := validateInstalledHTTPValidationCandidateWithLinuxOperations(29280, operations)
	if err != nil || authority.binding != binding || authority.pid != 4242 || authority.processStart != 91 ||
		authority.listenerInode != 702 || authority.worker != 4243 || authority.workerStart != 92 {
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

	operations.captureProcess = func(int) (proxy.LinuxProcessIdentity, error) { return process, nil }
	operations.captureWorker = func(context.Context, proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
		return proxy.LinuxProcessIdentity{}, errors.New("missing worker")
	}
	if _, err := validateInstalledHTTPValidationCandidateWithLinuxOperations(29280, operations); err == nil {
		t.Fatal("candidate without worker accepted")
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

func TestAwaitLinuxInstalledHTTPValidationCandidateStartsKillGraceAfterKill(t *testing.T) {
	wait := make(chan error)
	timeout := make(chan time.Time, 1)
	killTimeout := make(chan time.Time, 1)
	timers := make(chan time.Duration, 2)
	timerCalls := 0
	timer := func(duration time.Duration) <-chan time.Time {
		timers <- duration
		timerCalls++
		if timerCalls == 1 {
			return timeout
		}
		return killTimeout
	}
	killed := make(chan struct{}, 1)
	timedOut := errors.New("candidate timed out")
	result := make(chan error, 1)
	go func() {
		result <- awaitLinuxInstalledHTTPValidationCandidateWithTimer(
			wait,
			time.Minute,
			5*time.Second,
			timer,
			func() error {
				killed <- struct{}{}
				return nil
			},
			timedOut,
			errors.New("candidate cleanup timed out"),
		)
	}()
	if duration := <-timers; duration != time.Minute {
		t.Fatalf("initial timeout = %s", duration)
	}
	select {
	case duration := <-timers:
		t.Fatalf("kill grace started early: %s", duration)
	default:
	}
	timeout <- time.Now()
	<-killed
	if duration := <-timers; duration != 5*time.Second {
		t.Fatalf("kill timeout = %s", duration)
	}
	wait <- nil
	if err := <-result; !errors.Is(err, timedOut) {
		t.Fatalf("error = %v", err)
	}
}

func TestAwaitLinuxInstalledHTTPValidationCandidateBoundsKillWait(t *testing.T) {
	wait := make(chan error)
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	killTimeout := make(chan time.Time, 1)
	killTimeout <- time.Now()

	err := awaitLinuxInstalledHTTPValidationCandidateWithTimer(
		wait,
		time.Minute,
		5*time.Second,
		func(duration time.Duration) <-chan time.Time {
			if duration == time.Minute {
				return timeout
			}
			return killTimeout
		},
		func() error { return nil },
		errors.New("candidate timed out"),
		errors.New("candidate cleanup timed out"),
	)
	if err == nil || err.Error() != "candidate cleanup timed out" {
		t.Fatalf("error = %v", err)
	}
}

func TestLinuxInstalledHTTPValidationCandidateSharesWait(t *testing.T) {
	process := &linuxInstalledHTTPValidationCandidateProcess{}
	result := make(chan error, 1)
	process.waitOnce.Do(func() {
		process.waitResult = result
	})

	if first, second := waitLinuxInstalledHTTPValidationCandidate(process), waitLinuxInstalledHTTPValidationCandidate(process); first != result || second != result {
		t.Fatal("candidate wait channel was not shared")
	}
}

func TestLinuxValidationCandidateDiesWithControllerParent(t *testing.T) {
	const modeName = "CQ_TEST_LINUX_CANDIDATE_PARENT_DEATH"
	switch os.Getenv(modeName) {
	case "parent":
		command := exec.Command(os.Args[0], "-test.run=^TestLinuxValidationCandidateDiesWithControllerParent$")
		command.Env = linuxValidationCandidateTestEnvironment(modeName, "child")
		configureLinuxInstalledHTTPValidationCandidateCommand(command)
		stdout, err := command.StdoutPipe()
		if err != nil {
			os.Exit(2)
		}
		if err := command.Start(); err != nil {
			os.Exit(2)
		}
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			os.Exit(3)
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			os.Exit(3)
		}
		connection, err := net.DialTimeout("tcp4", fields[1], time.Second)
		if err != nil {
			os.Exit(4)
		}
		_ = connection.Close()
		fmt.Println(scanner.Text())
		_ = os.Stdout.Sync()
		os.Exit(0)
	case "child":
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			os.Exit(5)
		}
		fmt.Printf("%d %s\n", os.Getpid(), listener.Addr().String())
		_ = os.Stdout.Sync()
		for {
			time.Sleep(time.Hour)
		}
	}

	parent := exec.Command(os.Args[0], "-test.run=^TestLinuxValidationCandidateDiesWithControllerParent$")
	parent.Env = linuxValidationCandidateTestEnvironment(modeName, "parent")
	output, err := parent.Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		t.Fatalf("child authority = %q", output)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		t.Fatalf("child pid = %q, %v", output, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, processErr := os.Stat("/proc/" + strconv.Itoa(pid))
		connection, listenerErr := net.DialTimeout("tcp4", fields[1], 50*time.Millisecond)
		if connection != nil {
			_ = connection.Close()
		}
		if errors.Is(processErr, os.ErrNotExist) && listenerErr != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("candidate child %d survived controller exit", pid)
}

func linuxValidationCandidateTestEnvironment(name, value string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	prefix := name + "="
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, prefix) {
			environment = append(environment, variable)
		}
	}
	return append(environment, prefix+value)
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
