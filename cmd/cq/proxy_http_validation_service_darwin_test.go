//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
