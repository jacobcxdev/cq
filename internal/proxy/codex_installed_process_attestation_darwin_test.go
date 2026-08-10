//go:build darwin

package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureCodexInstalledServiceConfigurationRejectsSymlinkRetarget(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.plist")
	second := filepath.Join(directory, "second.plist")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("plist"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	_, _, err := captureCodexInstalledServiceConfigurationWithResolver(filepath.Join(directory, "service.plist"), func(string) (string, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	})
	if !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("retargeted service capture = %v", err)
	}
}

func TestDarwinCodexInstalledProcessVerifierDefaultsToEffectiveUID(t *testing.T) {
	verifier := newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{})
	if got := verifier.dependencies.uid(); got != os.Geteuid() {
		t.Fatalf("verifier uid = %d, want effective uid %d", got, os.Geteuid())
	}
}

func TestCaptureCodexInstalledServiceConfigurationRejectsSpecialModes(t *testing.T) {
	for _, mode := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service.plist")
			if err := os.WriteFile(path, []byte("plist"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o600|mode); err != nil {
				t.Fatal(err)
			}
			if _, _, err := captureCodexInstalledServiceConfiguration(path); !errors.Is(err, errCodexInstalledProcessAttestation) {
				t.Fatalf("special-mode service capture = %v", err)
			}
		})
	}
}

func TestDarwinCodexInstalledProcessVerifierAcceptsExactPersistentServiceOwner(t *testing.T) {
	for _, test := range []struct {
		label string
		kind  codexInstalledListenerServiceKind
	}{
		{label: codexInstalledLaunchdServiceLabel, kind: codexInstalledListenerServiceLaunchd},
		{label: codexInstalledHomebrewServiceLabel, kind: codexInstalledListenerServiceHomebrew},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			pid, uid := 4242, 501
			binary, plist := writeTestCodexInstalledDarwinService(t, test.label, true)
			secret := "first-private-value"
			verifier := newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{
				pid:                    func() int { return pid },
				uid:                    func() int { return uid },
				executablePath:         func() (string, error) { return binary, nil },
				verifyMappedExecutable: testCodexInstalledMappedExecutable,
				launchctlPrint: func(_ context.Context, target string) ([]byte, error) {
					want := fmt.Sprintf("gui/%d/%s", uid, test.label)
					if target != want {
						return nil, errors.New("service unavailable")
					}
					return []byte(testCodexInstalledLaunchctlJob(target, pid, plist, binary, secret, true)), nil
				},
			})

			proof, err := verifier.Capture(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if proof.pid != pid || proof.serviceKind != test.kind || !proof.persistent ||
				proof.executable.sha256 != sha256.Sum256([]byte("exact cq executable")) ||
				proof.serviceIdentitySHA256 == ([sha256.Size]byte{}) {
				t.Fatalf("proof = %#v", proof)
			}

			secret = "second-private-value"
			after, err := verifier.Capture(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after != proof {
				t.Fatal("private launchd environment changed fixed service identity")
			}
		})
	}
}

func TestDarwinCodexInstalledProcessVerifierRejectsOwnershipNearMatches(t *testing.T) {
	pid, uid := 4242, 501
	label := codexInstalledLaunchdServiceLabel
	binary, plist := writeTestCodexInstalledDarwinService(t, label, true)
	target := fmt.Sprintf("gui/%d/%s", uid, label)
	base := testCodexInstalledLaunchctlJob(target, pid, plist, binary, "private", true)
	for _, test := range []struct {
		name        string
		output      string
		currentPath string
	}{
		{name: "wrong pid", output: strings.Replace(base, "pid = 4242", "pid = 4243", 1), currentPath: binary},
		{name: "nested pid only", output: testCodexInstalledLaunchctlJob(target, pid, plist, binary, "private", false), currentPath: binary},
		{name: "wrong command", output: strings.Replace(base, "\t\tstart\n", "\t\tstatus\n", 1), currentPath: binary},
		{name: "not running", output: strings.Replace(base, "state = running", "state = waiting", 1), currentPath: binary},
	} {
		t.Run(test.name, func(t *testing.T) {
			verifier := newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{
				pid:                    func() int { return pid },
				uid:                    func() int { return uid },
				executablePath:         func() (string, error) { return test.currentPath, nil },
				verifyMappedExecutable: testCodexInstalledMappedExecutable,
				launchctlPrint: func(_ context.Context, got string) ([]byte, error) {
					if got != target {
						return nil, errors.New("service unavailable")
					}
					return []byte(test.output), nil
				},
			})
			if proof, err := verifier.Capture(context.Background()); proof != (codexInstalledProcessPlatformProof{}) || !errors.Is(err, errCodexInstalledProcessAttestation) {
				t.Fatalf("Capture() = %#v, %v", proof, err)
			}
		})
	}

	t.Run("non persistent plist", func(t *testing.T) {
		_, nonPersistentPlist := writeTestCodexInstalledDarwinService(t, label, false)
		output := testCodexInstalledLaunchctlJob(target, pid, nonPersistentPlist, binary, "private", true)
		verifier := newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{
			pid:                    func() int { return pid },
			uid:                    func() int { return uid },
			executablePath:         func() (string, error) { return binary, nil },
			verifyMappedExecutable: testCodexInstalledMappedExecutable,
			launchctlPrint: func(_ context.Context, got string) ([]byte, error) {
				if got != target {
					return nil, errors.New("service unavailable")
				}
				return []byte(output), nil
			},
		})
		if _, err := verifier.Capture(context.Background()); !errors.Is(err, errCodexInstalledProcessAttestation) {
			t.Fatalf("non-persistent service = %v", err)
		}
	})

	t.Run("loaded job is non persistent while disk plist claims keepalive", func(t *testing.T) {
		loadedWithoutKeepAlive := strings.Replace(base, "properties = keepalive | runatload", "properties = runatload", 1)
		verifier := newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{
			pid:                    func() int { return pid },
			uid:                    func() int { return uid },
			executablePath:         func() (string, error) { return binary, nil },
			verifyMappedExecutable: testCodexInstalledMappedExecutable,
			launchctlPrint: func(_ context.Context, got string) ([]byte, error) {
				if got != target {
					return nil, errors.New("service unavailable")
				}
				return []byte(loadedWithoutKeepAlive), nil
			},
		})
		if proof, err := verifier.Capture(context.Background()); proof != (codexInstalledProcessPlatformProof{}) || !errors.Is(err, errCodexInstalledProcessAttestation) {
			t.Fatalf("disk-only keepalive minted proof = %#v, %v", proof, err)
		}
	})

	t.Run("service executable is not current process", func(t *testing.T) {
		otherBinary, otherPlist := writeTestCodexInstalledDarwinService(t, label, true)
		output := testCodexInstalledLaunchctlJob(target, pid, otherPlist, otherBinary, "private", true)
		verifier := newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{
			pid:                    func() int { return pid },
			uid:                    func() int { return uid },
			executablePath:         func() (string, error) { return binary, nil },
			verifyMappedExecutable: testCodexInstalledMappedExecutable,
			launchctlPrint: func(_ context.Context, got string) ([]byte, error) {
				if got != target {
					return nil, errors.New("service unavailable")
				}
				return []byte(output), nil
			},
		})
		if _, err := verifier.Capture(context.Background()); !errors.Is(err, errCodexInstalledProcessAttestation) {
			t.Fatalf("different service executable = %v", err)
		}
	})

	t.Run("ambiguous owner", func(t *testing.T) {
		homebrewBinary, homebrewPlist := writeTestCodexInstalledDarwinService(t, codexInstalledHomebrewServiceLabel, true)
		homebrewData, err := os.ReadFile(homebrewPlist)
		if err != nil {
			t.Fatal(err)
		}
		homebrewData = []byte(strings.ReplaceAll(string(homebrewData), homebrewBinary, binary))
		if err := os.WriteFile(homebrewPlist, homebrewData, 0o644); err != nil {
			t.Fatal(err)
		}
		verifier := newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{
			pid:                    func() int { return pid },
			uid:                    func() int { return uid },
			executablePath:         func() (string, error) { return binary, nil },
			verifyMappedExecutable: testCodexInstalledMappedExecutable,
			launchctlPrint: func(_ context.Context, got string) ([]byte, error) {
				switch got {
				case target:
					return []byte(base), nil
				case fmt.Sprintf("gui/%d/%s", uid, codexInstalledHomebrewServiceLabel):
					return []byte(testCodexInstalledLaunchctlJob(got, pid, homebrewPlist, binary, "private", true)), nil
				default:
					return nil, errors.New("service unavailable")
				}
			},
		})
		if _, err := verifier.Capture(context.Background()); !errors.Is(err, errCodexInstalledProcessAttestation) {
			t.Fatalf("ambiguous service owner = %v", err)
		}
	})
}

func writeTestCodexInstalledDarwinService(t *testing.T, label string, keepAlive bool) (string, string) {
	t.Helper()
	directory := t.TempDir()
	binary := filepath.Join(directory, "cq")
	if err := os.WriteFile(binary, []byte("exact cq executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	keepAliveElement := "<true/>"
	if !keepAlive {
		keepAliveElement = "<false/>"
	}
	plist := filepath.Join(directory, label+".plist")
	data := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>proxy</string><string>start</string></array>
<key>KeepAlive</key>%s
<key>RunAtLoad</key><true/>
</dict></plist>`, label, binary, keepAliveElement)
	if err := os.WriteFile(plist, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return binary, plist
}

func testCodexInstalledMappedExecutable(int, codexInstalledExecutableProof) error { return nil }

func testCodexInstalledLaunchctlJob(target string, pid int, plist, binary, privateValue string, includeTopLevelPID bool) string {
	pidLine := ""
	if includeTopLevelPID {
		pidLine = fmt.Sprintf("\tpid = %d\n", pid)
	}
	return fmt.Sprintf(`%s = {
	active count = 1
	path = %s
	type = LaunchAgent
	state = running
	properties = keepalive | runatload
	program = %s
	arguments = {
		%s
		proxy
		start
	}
	environment = {
		pid = %d
		PRIVATE_AUTHORITY = %s
	}
%s}`, target, plist, binary, binary, pid+100, privateValue, pidLine)
}
