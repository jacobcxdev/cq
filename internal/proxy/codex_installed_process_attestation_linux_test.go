//go:build linux

package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxCodexInstalledProcessVerifierAcceptsExactSystemdOwner(t *testing.T) {
	pid, uid := 4242, 501
	executable := testCodexInstalledLinuxExecutableProof()
	process := testCodexInstalledLinuxProcess(pid, uid, executable)
	listener := LinuxListenerIdentity{
		Address: "127.0.0.1:19280",
		Inode:   9001,
		Process: process,
	}
	verifier := newCodexInstalledLinuxProcessVerifier(codexInstalledLinuxProcessVerifierDependencies{
		pid:               func() int { return pid },
		uid:               func() int { return uid },
		executablePath:    func() (string, error) { return executable.path, nil },
		captureExecutable: func(string) (codexInstalledExecutableProof, error) { return executable, nil },
		captureService: func(int, codexInstalledExecutableProof) (codexInstalledLinuxServiceProof, error) {
			return testCodexInstalledLinuxServiceProof(executable), nil
		},
		loadPort:        func() (int, error) { return DefaultPort, nil },
		captureProcess:  func(int) (LinuxProcessIdentity, error) { return process, nil },
		captureListener: func(int, int) (LinuxListenerIdentity, error) { return listener, nil },
	})

	proof, err := verifier.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if proof.pid != pid || proof.serviceKind != codexInstalledListenerServiceSystemdUser || !proof.persistent ||
		proof.executable != executable || proof.serviceIdentitySHA256 == ([sha256.Size]byte{}) {
		t.Fatalf("proof = %#v", proof)
	}

	pid = 5252
	process = testCodexInstalledLinuxProcess(pid, uid, executable)
	listener = LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 9012, Process: process}
	restarted, err := verifier.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if restarted.pid == proof.pid || restarted.serviceIdentitySHA256 != proof.serviceIdentitySHA256 {
		t.Fatal("systemd restart changed persistent service identity")
	}
}

func TestLinuxCodexInstalledProcessVerifierBindsWorkerToSystemdSupervisor(t *testing.T) {
	executable := testCodexInstalledLinuxExecutableProof()
	supervisor := testCodexInstalledLinuxProcess(4242, 501, executable)
	worker := testCodexInstalledLinuxWorkerProcess(5252, supervisor, executable)
	listener := LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 9001, Process: supervisor}
	verifier := newCodexInstalledLinuxProcessVerifier(codexInstalledLinuxProcessVerifierDependencies{
		pid:               func() int { return worker.PID },
		uid:               func() int { return 501 },
		executablePath:    func() (string, error) { return executable.path, nil },
		captureExecutable: func(string) (codexInstalledExecutableProof, error) { return executable, nil },
		captureService: func(int, codexInstalledExecutableProof) (codexInstalledLinuxServiceProof, error) {
			return testCodexInstalledLinuxServiceProof(executable), nil
		},
		loadPort: func() (int, error) { return DefaultPort, nil },
		captureProcess: func(pid int) (LinuxProcessIdentity, error) {
			switch pid {
			case worker.PID:
				return worker, nil
			case supervisor.PID:
				return supervisor, nil
			default:
				return LinuxProcessIdentity{}, errors.New("unknown pid")
			}
		},
		captureListener: func(pid, port int) (LinuxListenerIdentity, error) {
			if pid != supervisor.PID || port != DefaultPort {
				t.Fatalf("listener inputs = (%d, %d)", pid, port)
			}
			return listener, nil
		},
	})

	proof, err := verifier.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if proof.pid != supervisor.PID || proof.serviceIdentitySHA256 == ([sha256.Size]byte{}) {
		t.Fatalf("proof = %#v", proof)
	}
}

func TestLinuxCodexInstalledProcessVerifierBindsIsolatedCandidateToPersistentUnit(t *testing.T) {
	executable := testCodexInstalledLinuxExecutableProof()
	candidate := testCodexInstalledLinuxProcess(6262, 501, executable)
	candidate.CgroupPath = "/user.slice/user-501.slice/session-7.scope"
	candidate.Arguments = []string{executable.path, "proxy", "start", "--port", "29280", "--linux-validation-candidate-fd", "3"}
	listener := LinuxListenerIdentity{Address: "127.0.0.1:29280", Inode: 9012, Process: candidate}
	service := testCodexInstalledLinuxServiceProof(executable)
	verifier := newCodexInstalledLinuxProcessVerifier(codexInstalledLinuxProcessVerifierDependencies{
		pid:               func() int { return candidate.PID },
		uid:               func() int { return 501 },
		executablePath:    func() (string, error) { return executable.path, nil },
		captureExecutable: func(string) (codexInstalledExecutableProof, error) { return executable, nil },
		captureService:    func(int, codexInstalledExecutableProof) (codexInstalledLinuxServiceProof, error) { return service, nil },
		loadPort:          func() (int, error) { return 0, errors.New("candidate must use exact argument port") },
		captureProcess:    func(int) (LinuxProcessIdentity, error) { return candidate, nil },
		captureListener: func(pid, port int) (LinuxListenerIdentity, error) {
			if pid != candidate.PID || port != 29280 {
				t.Fatalf("listener inputs = (%d, %d)", pid, port)
			}
			return listener, nil
		},
	})

	proof, err := verifier.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if proof.pid != candidate.PID || !proof.persistent || proof.serviceKind != codexInstalledListenerServiceSystemdUser ||
		proof.serviceIdentitySHA256 != codexInstalledLinuxServiceIdentity(501, service, executable) {
		t.Fatalf("proof = %#v", proof)
	}
}

func TestLinuxCodexInstalledProcessVerifierFailsClosed(t *testing.T) {
	executable := testCodexInstalledLinuxExecutableProof()
	validProcess := testCodexInstalledLinuxProcess(4242, 501, executable)
	validListener := LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 9001, Process: validProcess}

	tests := map[string]func(*codexInstalledLinuxProcessVerifierDependencies){
		"wrong pid": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.captureProcess = func(int) (LinuxProcessIdentity, error) {
				process := validProcess
				process.PID++
				return process, nil
			}
		},
		"wrong uid": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.captureProcess = func(int) (LinuxProcessIdentity, error) {
				process := validProcess
				process.UID++
				return process, nil
			}
		},
		"wrong arguments": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.captureProcess = func(int) (LinuxProcessIdentity, error) {
				process := validProcess
				process.Arguments = []string{executable.path, "proxy", "start", "--port", "19280"}
				return process, nil
			}
		},
		"wrong cgroup": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.captureProcess = func(int) (LinuxProcessIdentity, error) {
				process := validProcess
				process.CgroupPath = "/user.slice/user-501.slice/user@501.service/app.slice/other.service"
				return process, nil
			}
		},
		"wrong executable": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.captureProcess = func(int) (LinuxProcessIdentity, error) {
				process := validProcess
				process.Executable.SHA256 = sha256.Sum256([]byte("other"))
				return process, nil
			}
		},
		"wrong listener process": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.captureListener = func(int, int) (LinuxListenerIdentity, error) {
				listener := validListener
				listener.Process.StartTime++
				return listener, nil
			}
		},
		"process changed": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			calls := 0
			dependencies.captureProcess = func(int) (LinuxProcessIdentity, error) {
				calls++
				process := validProcess
				if calls > 1 {
					process.StartTime++
				}
				return process, nil
			}
		},
		"listener changed": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			calls := 0
			dependencies.captureListener = func(int, int) (LinuxListenerIdentity, error) {
				calls++
				listener := validListener
				if calls > 1 {
					listener.Inode++
				}
				return listener, nil
			}
		},
		"port unavailable": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.loadPort = func() (int, error) { return 0, errors.New("unavailable") }
		},
		"service unavailable": func(dependencies *codexInstalledLinuxProcessVerifierDependencies) {
			dependencies.captureService = func(int, codexInstalledExecutableProof) (codexInstalledLinuxServiceProof, error) {
				return codexInstalledLinuxServiceProof{}, errors.New("unavailable")
			}
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			dependencies := codexInstalledLinuxProcessVerifierDependencies{
				pid:               func() int { return 4242 },
				uid:               func() int { return 501 },
				executablePath:    func() (string, error) { return executable.path, nil },
				captureExecutable: func(string) (codexInstalledExecutableProof, error) { return executable, nil },
				captureService: func(int, codexInstalledExecutableProof) (codexInstalledLinuxServiceProof, error) {
					return testCodexInstalledLinuxServiceProof(executable), nil
				},
				loadPort:        func() (int, error) { return DefaultPort, nil },
				captureProcess:  func(int) (LinuxProcessIdentity, error) { return validProcess, nil },
				captureListener: func(int, int) (LinuxListenerIdentity, error) { return validListener, nil },
			}
			mutate(&dependencies)
			proof, err := newCodexInstalledLinuxProcessVerifier(dependencies).Capture(context.Background())
			if !errors.Is(err, errCodexInstalledProcessAttestation) || proof != (codexInstalledProcessPlatformProof{}) {
				t.Fatalf("capture = (%#v, %v)", proof, err)
			}
		})
	}
}

func TestLinuxCodexInstalledProcessVerifierDefaultsToCurrentProcess(t *testing.T) {
	verifier := newCodexInstalledLinuxProcessVerifier(codexInstalledLinuxProcessVerifierDependencies{})
	if verifier.dependencies.pid() != os.Getpid() || verifier.dependencies.uid() != os.Geteuid() {
		t.Fatal("Linux verifier did not default to current process identity")
	}
}

func testCodexInstalledLinuxExecutableProof() codexInstalledExecutableProof {
	digest := sha256.Sum256([]byte("exact cq executable"))
	return codexInstalledExecutableProof{
		path: "/opt/cq/bin/cq", device: 17, inode: 23, links: 1, owner: 0,
		size: 19, mode: 0o755, sha256: digest,
	}
}

func testCodexInstalledLinuxProcess(pid, uid int, executable codexInstalledExecutableProof) LinuxProcessIdentity {
	return LinuxProcessIdentity{
		PID: pid, ParentPID: 1, StartTime: uint64(pid) + 100, UID: uint64(uid),
		Arguments:  []string{executable.path, "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
		Executable: LinuxExecutableIdentity{
			Path: executable.path, Device: executable.device, Inode: executable.inode, Links: executable.links,
			Owner: executable.owner, Size: executable.size, Mode: unix.S_IFREG | uint32(executable.mode.Perm()), SHA256: executable.sha256,
		},
	}
}

func testCodexInstalledLinuxWorkerProcess(pid int, supervisor LinuxProcessIdentity, executable codexInstalledExecutableProof) LinuxProcessIdentity {
	manifest := RuntimeRoleManifestV1{
		SchemaVersion: 1, Role: RuntimeRoleWorker, ManifestDigest: executable.sha256,
		ProxyInstanceID: "primary", RuntimeInstanceID: "runtime",
		ListenerFD: RuntimeNoListenerFD, WorkFD: RuntimeWorkFD, LifecycleFD: RuntimeLifecycleFD,
		ControlFD: RuntimeControlFD, SecretFD: RuntimeSecretFD, LifecycleHolderIdentityDigest: sha256.Sum256([]byte("worker holder")),
	}
	process := testCodexInstalledLinuxProcess(pid, int(supervisor.UID), executable)
	process.ParentPID = supervisor.PID
	process.CgroupPath = supervisor.CgroupPath
	process.Arguments = append([]string{executable.path, "proxy", "start"}, RuntimeRoleArguments(manifest)...)
	return process
}

func testCodexInstalledLinuxServiceProof(executable codexInstalledExecutableProof) codexInstalledLinuxServiceProof {
	return codexInstalledLinuxServiceProof{
		unit:       codexInstalledLinuxProxyUnit,
		path:       "/home/test/.config/systemd/user/cq-proxy.service",
		executable: executable.path,
		sha256:     sha256.Sum256([]byte("exact systemd unit")),
	}
}
