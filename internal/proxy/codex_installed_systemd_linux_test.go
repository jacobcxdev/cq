//go:build linux

package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureCodexInstalledLinuxServiceAcceptsCanonicalUnit(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	executable := testCodexInstalledLinuxExecutableProof()
	unit, err := renderCodexInstalledLinuxProxyUnit(executable.path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "systemd", "user", codexInstalledLinuxProxyUnit)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, unit, 0o600); err != nil {
		t.Fatal(err)
	}

	proof, err := captureCodexInstalledLinuxService(os.Geteuid(), executable)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.valid() || proof.path != path || proof.executable != executable.path {
		t.Fatalf("proof = %#v", proof)
	}

	if err := os.WriteFile(path, append(unit, []byte("Environment=UNSAFE=1\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCodexInstalledLinuxService(os.Geteuid(), executable); err == nil {
		t.Fatal("accepted non-canonical unit")
	}
}

func TestRenderCodexInstalledLinuxProxyUnitMatchesPackagingEncoding(t *testing.T) {
	tests := map[string][]byte{
		"/opt/cq/bin/cq":             nil,
		"/home/test/bin & tools/%cq": []byte(`ExecStart="/home/test/bin & tools/%%cq" proxy start`),
	}
	for executable, wantLine := range tests {
		t.Run(executable, func(t *testing.T) {
			unit, err := renderCodexInstalledLinuxProxyUnit(executable)
			if err != nil {
				t.Fatal(err)
			}
			if len(wantLine) == 0 {
				wantLine = []byte("ExecStart=" + executable + " proxy start")
			}
			if !bytes.Contains(unit, wantLine) || !bytes.HasSuffix(unit, []byte("WantedBy=default.target\n")) {
				t.Fatalf("unit = %q", unit)
			}
		})
	}
}

func TestRenderCodexInstalledLinuxProxyUnitRejectsInvalidPath(t *testing.T) {
	for _, path := range []string{"", "relative/cq", "/opt/cq/../bin/cq", "/opt/cq\nother"} {
		if _, err := renderCodexInstalledLinuxProxyUnit(path); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
}
