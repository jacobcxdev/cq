package installer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

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
