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
	binding.label = candidateProxyAgentLabel
	binding.port = 29280
	target := "gui/777/" + binding.label
	validLaunchctl := []byte(target + " = {\n\tpid = 4242\n}\n")
	validOps := installedHTTPValidationCandidateOperations{
		resolveService: func(label string) (installedHTTPValidationServiceBinding, error) {
			if label != candidateProxyAgentLabel {
				t.Fatalf("resolved label = %q", label)
			}
			return binding, nil
		},
		launchctlPrint: func(string) ([]byte, error) { return validLaunchctl, nil },
		lsof:           func(int) ([]byte, error) { return []byte("p4242\nf15\n"), nil },
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
	if err := restartInstalledHTTPValidationCandidate(candidateProxyAgentLabel); err != nil {
		t.Fatal(err)
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Geteuid(), candidateProxyAgentLabel)}
	if len(calls) != 1 || fmt.Sprint(calls[0]) != fmt.Sprint(want) {
		t.Fatalf("candidate restart = %v, want %v", calls, want)
	}
}

func TestResolveInstalledHTTPValidationCandidateBindsExactPortOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	executable := filepath.Join(dir, "cq")
	if err := os.WriteFile(executable, []byte("candidate cq binary\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(dir, candidateProxyAgentLabel+".plist")
	data := `<?xml version="1.0"?><plist version="1.0"><dict>
<key>Label</key><string>` + candidateProxyAgentLabel + `</string>
<key>ProgramArguments</key><array><string>` + executable + `</string><string>proxy</string><string>start</string><string>--port</string><string>29280</string></array>
<key>KeepAlive</key><true/></dict></plist>`
	if err := os.WriteFile(plist, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := installedHTTPValidationServiceOperations{
		executable: func() (string, error) { return executable, nil },
		plistPath:  func(string) (string, error) { return plist, nil },
		launchctlPrint: func(label string) error {
			if label == candidateProxyAgentLabel {
				return nil
			}
			return errors.New("not loaded")
		},
	}
	binding, err := resolveInstalledHTTPValidationServiceWithOperations(candidateProxyAgentLabel, ops)
	if err != nil {
		t.Fatal(err)
	}
	if binding.label != candidateProxyAgentLabel || binding.port != 29280 {
		t.Fatalf("candidate binding = %#v", binding)
	}
}

func TestInstalledHTTPValidationServicePortRejectsUnsafeOverrides(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		label string
		args  []string
	}{
		{label: proxyAgentLabel, args: []string{"/cq", "proxy", "start", "--port", "29280"}},
		{label: homebrewProxyAgentLabel, args: []string{"/cq", "proxy", "start", "--port", "29280"}},
		{label: candidateProxyAgentLabel, args: []string{"/cq", "proxy", "start"}},
		{label: candidateProxyAgentLabel, args: []string{"/cq", "proxy", "start", "--port", "19280"}},
		{label: candidateProxyAgentLabel, args: []string{"/cq", "proxy", "start", "--port", "0"}},
		{label: candidateProxyAgentLabel, args: []string{"/cq", "proxy", "start", "--port", "29280", "extra"}},
	} {
		if port, ok := installedHTTPValidationServicePort(test.label, test.args); ok || port != 0 {
			t.Fatalf("unsafe service accepted: label=%q args=%v port=%d", test.label, test.args, port)
		}
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
	if len(launchctlCalls) != 3 || launchctlCalls[0] != proxyAgentLabel || launchctlCalls[1] != homebrewProxyAgentLabel || launchctlCalls[2] != candidateProxyAgentLabel {
		t.Fatalf("launchctl labels = %v, want [%s %s %s]", launchctlCalls, proxyAgentLabel, homebrewProxyAgentLabel, candidateProxyAgentLabel)
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
