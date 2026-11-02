package installer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestRuntimeArtifactPruningRetainsReferences(t *testing.T) {
	store := upgradeArtifactStore(t)
	ctx := context.Background()
	var artifacts []RuntimeArtifact
	for _, version := range []string{"0.34.0", "0.34.1", "0.34.2", "0.34.3"} {
		artifact, err := store.Stage(ctx, buildUpgradeArtifact(t, version))
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, artifact)
	}
	retained := func() ([]string, error) {
		return []string{artifacts[0].Path, artifacts[2].Path, artifacts[3].Path}, nil
	}
	lock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(ctx, retained); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("pruning bypassed transaction lock: %v", err)
	}
	lock.Close()
	if err := store.Prune(ctx, func() ([]string, error) { return nil, errors.New("references unavailable") }); err == nil {
		t.Fatal("missing references accepted")
	}
	if err := store.Verify(ctx, artifacts[1]); err != nil {
		t.Fatalf("failed reference lookup removed artifact: %v", err)
	}
	if err := store.Prune(ctx, retained); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(artifacts[1].Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced runtime directory retained: %v", err)
	}
	for _, i := range []int{0, 2, 3} {
		if err := store.Verify(ctx, artifacts[i]); err != nil {
			t.Fatalf("referenced runtime removed: %v", err)
		}
	}
	if err := store.Prune(ctx, retained); err != nil {
		t.Fatalf("repeat pruning: %v", err)
	}
}

func TestRuntimeArtifactPruningRejectsUnsafeReferencesAndSkipsUnknownEntries(t *testing.T) {
	store := upgradeArtifactStore(t)
	ctx := context.Background()
	artifact, err := store.Stage(ctx, buildUpgradeArtifact(t, "0.34.0"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", "/foreign/cq", filepath.Join(store.Roots.State, "runtime-artifacts", strings.Repeat("A", 64), "cq")} {
		if err := store.Prune(ctx, func() ([]string, error) { return []string{path}, nil }); err == nil {
			t.Fatalf("unsafe reference accepted: %s", path)
		}
	}
	root := filepath.Dir(filepath.Dir(artifact.Path))
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "cq")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, strings.Repeat("b", 64))); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, "notes")
	if err := os.WriteFile(unknown, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(root, strings.Repeat("c", 64))
	if err := os.Mkdir(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalid, "cq"), []byte("foreign"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(ctx, func() ([]string, error) { return []string{artifact.Path}, nil }); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{artifact.Path, outsideFile, unknown, filepath.Join(invalid, "cq")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("pruning removed unknown or referenced entry: %s %v", path, err)
		}
	}
}

func buildUpgradeArtifact(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cq")
	cmd := exec.Command("go", "build", "-o", path, "-ldflags", "-X main.version="+version, "../../cmd/cq")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return path
}
func upgradeArtifactStore(t *testing.T) RuntimeArtifactStore {
	t.Helper()
	return RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: filepath.Join(t.TempDir(), "state")}}
}

func TestRuntimeArtifactStageChecksOnceAndRejectsStorageTampering(t *testing.T) {
	source := buildRuntimeArtifactProbe(t)
	for _, mode := range []string{"none", "contents", "mode", "symlink"} {
		t.Run("tamper_"+mode, func(t *testing.T) {
			if runtime.GOOS == "windows" && mode != "none" {
				t.Skip("Windows does not allow replacing a running executable")
			}
			checks := filepath.Join(t.TempDir(), "checks")
			t.Setenv("CQ_RUNTIME_ARTIFACT_TEST_CHECKS", checks)
			t.Setenv("CQ_RUNTIME_ARTIFACT_TEST_TAMPER", mode)
			store := upgradeArtifactStore(t)
			artifact, err := store.Stage(context.Background(), source)
			if mode == "none" {
				if err != nil || artifact.Version != "0.34.0" {
					t.Fatalf("valid runtime was not staged: %+v %v", artifact, err)
				}
			} else if err == nil {
				t.Fatalf("runtime storage %s changed during its check was accepted", mode)
			}
			data, readErr := os.ReadFile(checks)
			if readErr != nil || string(data) != "check\n" {
				t.Fatalf("runtime checks = %q, want exactly one completed check: %v", data, readErr)
			}
		})
	}
}

func buildRuntimeArtifactProbe(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mainDirectory := filepath.Join(root, "cmd", "cq")
	if err := os.MkdirAll(mainDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/jacobcxdev/cq\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := `package main

import (
    "encoding/json"
    "os"
    "runtime"
)

func main() {
    if len(os.Args) != 4 || os.Args[1] != "proxy" || os.Args[2] != "runtime-check" || os.Args[3] != "--json" {
        panic("unexpected probe command")
    }
    executable, err := os.Executable()
    if err != nil {
        panic(err)
    }
    switch os.Getenv("CQ_RUNTIME_ARTIFACT_TEST_TAMPER") {
    case "contents", "symlink":
        original := executable + ".original"
        if err := os.Rename(executable, original); err != nil {
            panic(err)
        }
        if os.Getenv("CQ_RUNTIME_ARTIFACT_TEST_TAMPER") == "symlink" {
            if err := os.Symlink(original, executable); err != nil {
                panic(err)
            }
        } else if err := os.WriteFile(executable, []byte("tampered"), 0500); err != nil {
            panic(err)
        }
    case "mode":
        if err := os.Chmod(executable, 0700); err != nil {
            panic(err)
        }
    }
    file, err := os.OpenFile(os.Getenv("CQ_RUNTIME_ARTIFACT_TEST_CHECKS"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
    if err != nil {
        panic(err)
    }
    if _, err := file.WriteString("check\n"); err != nil {
        panic(err)
    }
    if err := file.Close(); err != nil {
        panic(err)
    }
    result := map[string]any{
        "schema_version": 1, "version": "0.34.0", "protocol_version": 1,
        "goos": runtime.GOOS, "goarch": runtime.GOARCH,
    }
    if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
        panic(err)
    }
}
`
	if err := os.WriteFile(filepath.Join(mainDirectory, "main.go"), []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cq")
	command := exec.Command("go", "build", "-o", path, "./cmd/cq")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build runtime probe: %v\n%s", err, output)
	}
	return path
}

func TestRuntimeArtifactSurvivesCaskRenameAndPurge(t *testing.T) {
	source := buildUpgradeArtifact(t, "0.34.0")
	store := upgradeArtifactStore(t)
	artifact, err := store.Stage(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(artifact.Path)
	if err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("runtime permissions: %v %v", info, err)
	}
}
func TestRuntimeArtifactRejectsForeignOrChangedExecutable(t *testing.T) {
	store := upgradeArtifactStore(t)
	source := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(source, []byte("#!/bin/sh\necho cq\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stage(context.Background(), source); err == nil {
		t.Fatal("foreign executable accepted")
	}
	artifact, err := store.Stage(context.Background(), buildUpgradeArtifact(t, "0.34.0"))
	if err != nil {
		t.Fatal(err)
	}
	substituted := artifact
	substituted.SHA256 = strings.Repeat("0", 64)
	if err := store.Verify(context.Background(), substituted); err == nil {
		t.Fatal("digest substitution accepted")
	}
	if err := os.Chmod(artifact.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact.Path, []byte("changed"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), artifact); err == nil {
		t.Fatal("changed executable accepted")
	}
}
func TestRuntimeArtifactsRetainSelectedPrevious(t *testing.T) {
	store := upgradeArtifactStore(t)
	old, err := store.Stage(context.Background(), buildUpgradeArtifact(t, "0.34.0"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Stage(context.Background(), buildUpgradeArtifact(t, "0.34.1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []RuntimeArtifact{old, next} {
		if err := store.Verify(context.Background(), artifact); err != nil {
			t.Fatal(err)
		}
	}
	if old.Path == next.Path {
		t.Fatal("candidate overwrote previous artifact")
	}
}

func TestRuntimeArtifactRejectsSymlinkedDestination(t *testing.T) {
	store := upgradeArtifactStore(t)
	artifact, err := store.Stage(context.Background(), buildUpgradeArtifact(t, "0.34.0"))
	if err != nil {
		t.Fatal(err)
	}
	displaced := artifact.Path + ".old"
	if err := os.Rename(artifact.Path, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(displaced, artifact.Path); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), artifact); err == nil {
		t.Fatal("symlinked destination accepted")
	}
}

func TestRuntimeArtifactRejectsForgedVersion(t *testing.T) {
	store := upgradeArtifactStore(t)
	artifact, err := store.Stage(context.Background(), buildUpgradeArtifact(t, "0.34.0"))
	if err != nil {
		t.Fatal(err)
	}
	artifact.Version = "0.34.99"
	if err := store.Verify(context.Background(), artifact); err == nil {
		t.Fatal("client-supplied version disagreed with executable")
	}
}
