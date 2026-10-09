//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestDarwinRuntimeStageUsesStablePackageLink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "package-cq")
	command := exec.Command("go", "build", "-o", source, "-ldflags", "-X main.version=0.34.0", ".")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v %s", err, out)
	}
	link := filepath.Join(root, "cq")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	store := darwinServiceRuntimeArtifacts{installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: filepath.Join(root, "state")}}}
	artifact, err := store.Stage(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "0.34.0" || artifact.Path == source || artifact.Path == link {
		t.Fatalf("package not retained: %+v", artifact)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), artifact); err != nil {
		t.Fatalf("package purge affected runtime: %v", err)
	}
	if _, err := store.Stage(context.Background(), link); err == nil {
		t.Fatal("dangling package link accepted")
	}
}
