package dots

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func requirePrivateModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Dots requires POSIX private file modes")
	}
}

func TestExtractContainsOnlyPublicSources(t *testing.T) {
	requirePrivateModes(t)
	root := filepath.Join(t.TempDir(), "dots")
	if err := Extract(root); err != nil {
		t.Fatal(err)
	}
	sources, err := Sources()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release.py", "control.py", "runtime/settings.py", "runtime/awareness.py",
		"runtime/app_patch.py", "runtime/native_bootstrap.py", "runtime/native_startup.cjs"} {
		if len(sources[name]) == 0 {
			t.Errorf("missing %s", name)
		}
	}
	for name, data := range sources {
		if strings.Contains(name, "test_") {
			t.Fatalf("test fixture bundled: %s", name)
		}
		path := filepath.Join(root, "payload", filepath.FromSlash(name))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(data) {
			t.Errorf("source mismatch %s", name)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode %s = %o", name, info.Mode().Perm())
		}
	}
	info, _ := os.Stat(root)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("root mode %o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "runtime")); !os.IsNotExist(err) {
		t.Fatalf("extract touched runtime: %v", err)
	}
}

func TestExtractRejectsSymlinksBeforeReplacingPayload(t *testing.T) {
	requirePrivateModes(t)
	for _, part := range []string{"root", "payload", "runtime", "source"} {
		t.Run(part, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "dots")
			victim := filepath.Join(base, "victim")
			if err := os.Mkdir(victim, 0o700); err != nil {
				t.Fatal(err)
			}
			if part == "root" {
				if err := os.Symlink(victim, root); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				payload := filepath.Join(root, "payload")
				if part == "payload" {
					if err := os.Symlink(victim, payload); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(payload, 0o700); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(payload, "runtime")
					if part == "source" {
						path = filepath.Join(payload, "release.py")
					}
					if err := os.Symlink(victim, path); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := Extract(root); err == nil {
				t.Fatal("symlink accepted")
			}
			entries, err := os.ReadDir(victim)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatal("symlink destination changed")
			}
		})
	}
}

func TestExtractRejectsNonPrivateRoot(t *testing.T) {
	requirePrivateModes(t)
	root := filepath.Join(t.TempDir(), "dots")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Extract(root); err == nil {
		t.Fatal("non-private root accepted")
	}
	if _, err := PrivateRoot("relative", true); err == nil {
		t.Fatal("relative root accepted")
	}
}

func TestExtractPreservesLiveRuntimeAndPrivateConfig(t *testing.T) {
	requirePrivateModes(t)
	root := filepath.Join(t.TempDir(), "dots")
	if err := Extract(root); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime, "probe.py")
	if err := os.WriteFile(path, []byte("live source"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "settings.json")
	if err := os.WriteFile(config, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Extract(root); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{path: "live source", config: "private"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("private state changed: %s %v", path, err)
		}
	}
}
