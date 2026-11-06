//go:build darwin || linux

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

func TestPortableWorkspaceUpdateUnderPrivateUmask(t *testing.T) {
	if os.Getenv("CQ_TEST_WORKSPACE_UMASK_HELPER") != "1" {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(os.Args[0], "-test.run=^TestPortableWorkspaceUpdateUnderPrivateUmask$")
		command.Env = append(os.Environ(), "CQ_TEST_WORKSPACE_UMASK_HELPER=1", "CQ_TEST_WORKSPACE_UMASK_ROOT="+root)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("workspace update with umask 077 failed: %v\n%s", err, output)
		}
		return
	}
	// Umask is process-wide. Change it only in this isolated helper process,
	// where no parallel tests or goroutines can observe the temporary setting.
	previous := unix.Umask(0o077)
	defer unix.Umask(previous)
	project := filepath.Join(os.Getenv("CQ_TEST_WORKSPACE_UMASK_ROOT"), "work")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, ".cq", "harness.json")
	want := Definition{Description: "Portable private profile", Changes: Changes{Skills: map[string]Decision{"repo:review": Enabled}}}
	fsys := fsutil.OSFileSystem{}
	if _, err := UpdateWorkspace(fsys, path, func(policy *WorkspacePolicy) error {
		policy.Profiles["review"] = want
		return nil
	}); err != nil {
		t.Fatalf("create portable policy: %v", err)
	}
	if _, err := UpdateWorkspace(fsys, path, func(policy *WorkspacePolicy) error {
		policy.Rules.All.Profiles = []string{"review"}
		return nil
	}); err != nil {
		t.Fatalf("replace portable policy: %v", err)
	}
	policy, err := LoadWorkspace(fsys, path)
	if err != nil || policy.Version != 2 || !reflect.DeepEqual(policy.Profiles["review"], want) || !reflect.DeepEqual(policy.Rules.All.Profiles, []string{"review"}) {
		t.Fatalf("private portable policy round trip = %#v, %v", policy, err)
	}
	for _, file := range []string{path, path + ".lock"} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private policy file mode = %v, %v", info, err)
		}
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private policy directory mode = %v, %v", info, err)
	}
}
