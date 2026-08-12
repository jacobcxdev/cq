//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateInstalledHTTPValidationCandidateBindsListenerToLoadedPID(t *testing.T) {
	t.Parallel()
	binding := validInstalledHTTPValidationServiceBinding()
	target := "gui/777/" + binding.label
	validLaunchctl := []byte(target + " = {\n\tpid = 4242\n}\n")
	validOps := installedHTTPValidationCandidateOperations{
		resolveService: func(string) (installedHTTPValidationServiceBinding, error) { return binding, nil },
		launchctlPrint: func(string) ([]byte, error) { return validLaunchctl, nil },
		lsof:           func(int) ([]byte, error) { return []byte("p4242\n"), nil },
		effectiveUID:   func() int { return 777 },
	}
	authority, err := validateInstalledHTTPValidationCandidateWithOperations(29280, validOps)
	if err != nil || authority.binding != binding || authority.pid != 4242 {
		t.Fatalf("candidate authority = (%#v, %v)", authority, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*installedHTTPValidationCandidateOperations)
	}{
		{name: "live service owns different port", mutate: func(ops *installedHTTPValidationCandidateOperations) {
			ops.lsof = func(int) ([]byte, error) { return []byte("p99735\n"), nil }
		}},
		{name: "candidate port has multiple owners", mutate: func(ops *installedHTTPValidationCandidateOperations) {
			ops.lsof = func(int) ([]byte, error) { return []byte("p4242\np99735\n"), nil }
		}},
		{name: "loaded generation changed", mutate: func(ops *installedHTTPValidationCandidateOperations) {
			ops.launchctlPrint = func(string) ([]byte, error) { return []byte(target + " = {\n\tpid = 4343\n}\n"), nil }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ops := validOps
			test.mutate(&ops)
			if _, err := validateInstalledHTTPValidationCandidateWithOperations(29280, ops); err == nil {
				t.Fatal("unsafe candidate authority accepted")
			}
		})
	}
}

func TestRestartInstalledHTTPValidationCandidateTargetsExactLabel(t *testing.T) {
	oldRunner := runProxyLaunchctl
	t.Cleanup(func() { runProxyLaunchctl = oldRunner })
	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	if err := restartInstalledHTTPValidationCandidate(homebrewProxyAgentLabel); err != nil {
		t.Fatal(err)
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Geteuid(), homebrewProxyAgentLabel)}
	if len(calls) != 1 || fmt.Sprint(calls[0]) != fmt.Sprint(want) {
		t.Fatalf("candidate restart = %v, want %v", calls, want)
	}
}

func TestResolveInstalledHTTPValidationServiceBindsLoadedPlistAndCurrentExecutable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	executable := filepath.Join(dir, "cq")
	if err := os.WriteFile(executable, []byte("exact installed cq binary\n"), 0o500); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	plist := filepath.Join(dir, "homebrew.mxcl.cq.plist")
	writeInstalledHTTPValidationPlist(t, plist, homebrewProxyAgentLabel, executable, "/tmp/proxy.log")
	launchctlCalls := make([]string, 0, 2)
	ops := installedHTTPValidationServiceOperations{
		executable: func() (string, error) { return executable, nil },
		plistPath: func(label string) (string, error) {
			if label != homebrewProxyAgentLabel {
				return "", errors.New("not installed")
			}
			return plist, nil
		},
		launchctlPrint: func(label string) error {
			launchctlCalls = append(launchctlCalls, label)
			if label == homebrewProxyAgentLabel {
				return nil
			}
			return errors.New("not loaded")
		},
	}

	binding, err := resolveInstalledHTTPValidationServiceWithOperations("", ops)
	if err != nil {
		t.Fatalf("resolve service: %v", err)
	}
	if binding.label != homebrewProxyAgentLabel {
		t.Fatalf("binding label = %q, want %q", binding.label, homebrewProxyAgentLabel)
	}
	if binding.executableSHA256 != "d6bcd41ef3b0069335ee98d74068074f2901e3eb901b1785f244e7d9d7c1147a" {
		t.Fatalf("executable digest = %q", binding.executableSHA256)
	}
	if !isLowerHexSHA256(binding.serviceSHA256) {
		t.Fatalf("service digest = %q, want lowercase SHA-256", binding.serviceSHA256)
	}
	if len(launchctlCalls) != 2 || launchctlCalls[0] != proxyAgentLabel || launchctlCalls[1] != homebrewProxyAgentLabel {
		t.Fatalf("launchctl labels = %v, want [%s %s]", launchctlCalls, proxyAgentLabel, homebrewProxyAgentLabel)
	}

	writeInstalledHTTPValidationPlist(t, plist, homebrewProxyAgentLabel, executable, "/tmp/changed.log")
	changed, err := resolveInstalledHTTPValidationServiceWithOperations(homebrewProxyAgentLabel, ops)
	if err != nil {
		t.Fatalf("resolve changed service: %v", err)
	}
	if changed.serviceSHA256 == binding.serviceSHA256 {
		t.Fatal("service digest did not change with installed plist")
	}
}

func TestResolveInstalledHTTPValidationServiceRejectsAmbiguousOrSubstitutedService(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	currentExecutable := filepath.Join(dir, "cq-current")
	serviceExecutable := filepath.Join(dir, "cq-service")
	if err := os.WriteFile(currentExecutable, []byte("current\n"), 0o500); err != nil {
		t.Fatalf("write current executable: %v", err)
	}
	if err := os.WriteFile(serviceExecutable, []byte("service\n"), 0o500); err != nil {
		t.Fatalf("write service executable: %v", err)
	}
	plist := filepath.Join(dir, "proxy.plist")
	writeInstalledHTTPValidationPlist(t, plist, proxyAgentLabel, serviceExecutable, "/tmp/proxy.log")
	ops := installedHTTPValidationServiceOperations{
		executable: func() (string, error) { return currentExecutable, nil },
		plistPath:  func(string) (string, error) { return plist, nil },
		launchctlPrint: func(string) error {
			return nil
		},
	}

	if _, err := resolveInstalledHTTPValidationServiceWithOperations("", ops); err == nil {
		t.Fatal("ambiguous loaded services error = nil")
	}
	if _, err := resolveInstalledHTTPValidationServiceWithOperations(proxyAgentLabel, ops); err == nil {
		t.Fatal("substituted service executable error = nil")
	}
}

func TestResolveInstalledHTTPValidationServiceRejectsNonProxyStartPlist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	executable := filepath.Join(dir, "cq")
	if err := os.WriteFile(executable, []byte("exact installed cq binary\n"), 0o500); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	plist := filepath.Join(dir, "proxy.plist")
	data := `<?xml version="1.0"?><plist version="1.0"><dict>
<key>Label</key><string>dev.jacobcx.cq.proxy</string>
<key>ProgramArguments</key><array><string>` + executable + `</string><string>proxy</string><string>status</string></array>
</dict></plist>`
	if err := os.WriteFile(plist, []byte(data), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	ops := installedHTTPValidationServiceOperations{
		executable:     func() (string, error) { return executable, nil },
		plistPath:      func(string) (string, error) { return plist, nil },
		launchctlPrint: func(string) error { return nil },
	}

	if _, err := resolveInstalledHTTPValidationServiceWithOperations(proxyAgentLabel, ops); err == nil {
		t.Fatal("non-proxy-start plist error = nil")
	}
}

func TestResolveInstalledHTTPValidationServiceRejectsTrailingPlistAuthority(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	executable := filepath.Join(dir, "cq")
	if err := os.WriteFile(executable, []byte("exact installed cq binary\n"), 0o500); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	plist := filepath.Join(dir, "proxy.plist")
	data := `<?xml version="1.0"?><plist version="1.0"><dict>
<key>Label</key><string>dev.jacobcx.cq.proxy</string>
<key>ProgramArguments</key><array><string>` + executable + `</string><string>proxy</string><string>start</string></array>
</dict><dict><key>Label</key><string>homebrew.mxcl.cq</string></dict></plist>`
	if err := os.WriteFile(plist, []byte(data), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	ops := installedHTTPValidationServiceOperations{
		executable:     func() (string, error) { return executable, nil },
		plistPath:      func(string) (string, error) { return plist, nil },
		launchctlPrint: func(string) error { return nil },
	}

	if _, err := resolveInstalledHTTPValidationServiceWithOperations(proxyAgentLabel, ops); err == nil {
		t.Fatal("trailing plist authority error = nil")
	}
}

func TestReadInstalledHTTPValidationRegularFileRejectsSpecialModes(t *testing.T) {
	for _, mode := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service-file")
			if err := os.WriteFile(path, []byte("service"), 0o500); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o500|mode); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readInstalledHTTPValidationRegularFile(path, 1<<20, true); err == nil {
				t.Fatalf("special mode %v accepted", mode)
			}
		})
	}
}

func TestInstalledHTTPValidationLaunchctlTargetUsesEffectiveUIDAuthority(t *testing.T) {
	target, err := installedHTTPValidationLaunchctlTarget(proxyAgentLabel, func() int { return 777 })
	if err != nil {
		t.Fatal(err)
	}
	if target != "gui/777/"+proxyAgentLabel {
		t.Fatalf("launchctl target = %q", target)
	}
}

func writeInstalledHTTPValidationPlist(t *testing.T, path, label, executable, logPath string) {
	t.Helper()
	data := `<?xml version="1.0"?><plist version="1.0"><dict>
<key>Label</key><string>` + label + `</string>
<key>ProgramArguments</key><array><string>` + executable + `</string><string>proxy</string><string>start</string></array>
<key>KeepAlive</key><true/><key>StandardErrorPath</key><string>` + logPath + `</string>
</dict></plist>`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
}
