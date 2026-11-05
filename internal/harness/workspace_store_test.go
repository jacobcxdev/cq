package harness

import (
	"encoding/json"
	"errors"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writePortable(t *testing.T, fsys *fsutil.MemFS, path string, policy WorkspacePolicy) {
	t.Helper()
	if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPortableRejectsInvalidAndWritableFiles(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":          `{"version":2,"rules":{},"unknown":true}`,
		"trailing data":          `{"version":2,"rules":{}} {}`,
		"old version":            `{"version":1,"rules":{}}`,
		"invalid decision":       `{"version":2,"rules":{"all":{"changes":{"default":"auto"}}}}`,
		"absolute skill":         `{"version":2,"rules":{"all":{"changes":{"skills":{"/Users/me/skill":"enabled"}}}}}`,
		"windows absolute skill": `{"version":2,"profiles":{"custom":{"changes":{"skills":{"C:\\skills\\item":"enabled"}}}},"rules":{}}`,
		"oversized":              `{"version":2,"rules":{}}` + strings.Repeat(" ", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if err := fsys.MkdirAll("/work/.cq", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile("/work/.cq/harness.json", []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWorkspace(fsys, "/work/.cq/harness.json"); err == nil {
				t.Fatal("accepted invalid portable policy")
			}
		})
	}
	for _, mode := range []os.FileMode{0o666, 0o664} {
		fsys := fsutil.NewMemFS()
		writePortable(t, fsys, "/work/.cq/harness.json", WorkspacePolicy{Version: 2})
		if err := fsys.Chmod("/work/.cq/harness.json", mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadWorkspace(fsys, "/work/.cq/harness.json"); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
			t.Fatalf("writable file mode %o error = %v", mode, err)
		}
	}
}

func TestPortableConcurrentUpdateDoesNotLoseRules(t *testing.T) {
	fsys := fsutil.NewMemFS()
	writePortable(t, fsys, "/work/.cq/harness.json", WorkspacePolicy{Version: 2})
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := UpdateWorkspace(fsys, "/work/.cq/harness.json", func(policy *WorkspacePolicy) error {
			close(entered)
			<-release
			policy.Profiles["first"] = Definition{Changes: Changes{Default: Disabled}}
			return nil
		})
		done <- err
	}()
	<-entered
	_, err := UpdateWorkspace(fsys, "/work/.cq/harness.json", func(policy *WorkspacePolicy) error { policy.Profiles["second"] = Definition{}; return nil })
	close(release)
	firstErr := <-done
	if !errors.Is(err, fsutil.ErrExclusiveLockHeld) || firstErr != nil {
		t.Fatalf("concurrent update = %v, first = %v", err, firstErr)
	}
	policy, err := UpdateWorkspace(fsys, "/work/.cq/harness.json", func(policy *WorkspacePolicy) error { policy.Profiles["second"] = Definition{}; return nil })
	if err != nil || len(policy.Profiles) != 2 {
		t.Fatalf("retry lost rules: %#v,%v", policy, err)
	}
}

func TestPortableActualRenameFailurePreservesFile(t *testing.T) {
	fsys := fsutil.NewMemFS()
	writePortable(t, fsys, "/work/.cq/harness.json", WorkspacePolicy{Version: 2, Rules: Scope{All: Binding{Profiles: []string{"full"}}}})
	before, _ := fsys.ReadFile("/work/.cq/harness.json")
	_, err := UpdateWorkspace(workspaceFailureFS{fsys}, "/work/.cq/harness.json", func(policy *WorkspacePolicy) error { policy.Rules.All.Profiles = []string{"minimal"}; return nil })
	if !errors.Is(err, errTestRename) || !errors.Is(err, fsutil.ErrCommitNotCommitted) {
		t.Fatalf("rename failure = %v", err)
	}
	after, _ := fsys.ReadFile("/work/.cq/harness.json")
	if string(before) != string(after) {
		t.Fatal("failed rename altered previous file")
	}
	entries, err := fsys.ReadDir("/work/.cq")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("failed write left temporary file %s", entry.Name())
		}
	}
}

type portableTestDirectory interface {
	fsutil.DurableDirectory
	fsutil.SecureDirectory
}
type workspaceFailureFS struct{ *fsutil.MemFS }

func (fsys workspaceFailureFS) OpenDurableDirectory(path string) (fsutil.DurableDirectory, error) {
	directory, err := fsys.MemFS.OpenDurableDirectory(path)
	if err != nil {
		return nil, err
	}
	return workspaceFailureDirectory{directory.(portableTestDirectory)}, nil
}

type workspaceFailureDirectory struct{ portableTestDirectory }

func (workspaceFailureDirectory) RenameChecked(string, string, fsutil.SecureFileIdentity) error {
	return errTestRename
}
func (directory workspaceFailureDirectory) RenameNoReplaceChecked(old, new string, id fsutil.SecureFileIdentity) error {
	return directory.portableTestDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(old, new, id)
}
func (directory workspaceFailureDirectory) RemoveChecked(name string, id fsutil.SecureFileIdentity) error {
	return directory.portableTestDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, id)
}

func TestPortableRefusesLinksAndUnsafeDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("temporary secure roots are not enabled on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	actualFile := filepath.Join(actual, "harness.json")
	if err := os.WriteFile(actualFile, []byte(`{"version":2,"rules":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateWorkspace(fsutil.OSFileSystem{}, actualFile, func(policy *WorkspacePolicy) error { policy.Rules.All.Profiles = []string{"minimal"}; return nil }); err != nil {
		t.Fatalf("native portable update: %v", err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(actual, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(fsutil.OSFileSystem{}, filepath.Join(linked, "harness.json")); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("linked directory = %v", err)
	}
	fileLink := filepath.Join(actual, "linked.json")
	if err := os.Symlink(actualFile, fileLink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(fsutil.OSFileSystem{}, fileLink); err == nil {
		t.Fatal("followed policy file symlink")
	}
	if err := os.Chmod(actual, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(fsutil.OSFileSystem{}, actualFile); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("unsafe directory = %v", err)
	}
}

func TestPortableWorkspaceNamespacesAndPrecedence(t *testing.T) {
	fsys := fsutil.NewMemFS()
	user := Policy{Version: 2, Enabled: true, Profiles: map[string]Definition{"research": {Changes: Changes{Plugins: map[string]Decision{"drive": Enabled}}}}, Global: Scope{All: Binding{Profiles: []string{"minimal", "research"}}}, Tasks: map[string]Scope{testChat: {All: Binding{Profiles: []string{"research"}}}}}
	if err := Save(fsys, "/config/harness.json", user); err != nil {
		t.Fatal(err)
	}
	writePortable(t, fsys, "/work/.cq/harness.json", WorkspacePolicy{Version: 2, Profiles: map[string]Definition{"research": {Changes: Changes{Plugins: map[string]Decision{"drive": Disabled}}}}, Rules: Scope{All: Binding{Profiles: []string{"research"}}}})
	policy, err := LoadForContext(fsys, "/config/harness.json", "/work/project")
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.ResolveEffective("gpt-6", "/work/project", ""); got.Choose(PluginKind, "drive", true) || got.Selection.Source != "workspace" {
		t.Fatalf("portable override = %#v", got)
	}
	if got := policy.ResolveEffective("gpt-6", "/work/project", testChat); !got.Choose(PluginKind, "drive", false) {
		t.Fatalf("task did not use user namespace: %#v", got)
	}
	if policy.Definitions()["research"].Changes.Plugins["drive"] != Disabled {
		t.Fatal("discovery did not expose local definition")
	}
	writePortable(t, fsys, "/work/project/.cq/harness.json", WorkspacePolicy{Version: 2, Rules: Scope{All: Binding{Profiles: []string{"research"}}}})
	policy, err = LoadForContext(fsys, "/config/harness.json", "/work/project")
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.ResolveEffective("gpt-6", "/work/project", ""); !got.Choose(PluginKind, "drive", false) {
		t.Fatalf("child fallback incorrectly inherited parent namespace: %#v", got)
	}
}

func TestPortableWorkspaceMissingAndReferenceValidation(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := LoadWorkspace(fsys, "/work/.cq/harness.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
	user := Policy{Version: 2, Enabled: true}
	if err := Save(fsys, "/config/harness.json", user); err != nil {
		t.Fatal(err)
	}
	writePortable(t, fsys, "/work/.cq/harness.json", WorkspacePolicy{Version: 2, Rules: Scope{All: Binding{Profiles: []string{"missing"}}}})
	if _, err := LoadWorkspace(fsys, "/work/.cq/harness.json"); err != nil {
		t.Fatalf("standalone rejected external reference: %v", err)
	}
	if _, err := LoadForContext(fsys, "/config/harness.json", "/work"); err == nil {
		t.Fatal("context accepted unresolved reference")
	}
	if _, err := LoadForContext(fsys, "/config/harness.json", "/other"); err != nil {
		t.Fatalf("sibling file affected context: %v", err)
	}
}

func TestPortableUpdatePreservesRulesAndPrivateLock(t *testing.T) {
	fsys := fsutil.NewMemFS()
	writePortable(t, fsys, "/work/.cq/harness.json", WorkspacePolicy{Version: 2, Rules: Scope{All: Binding{Profiles: []string{"minimal"}}}})
	updated, err := UpdateWorkspace(fsys, "/work/.cq/harness.json", func(policy *WorkspacePolicy) error {
		policy.Profiles["research"] = Definition{Changes: Changes{Plugins: map[string]Decision{"drive": Enabled}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Rules.All.Profiles) != 1 || updated.Rules.All.Profiles[0] != "minimal" {
		t.Fatalf("lost rules: %#v", updated)
	}
	if _, err := LoadWorkspace(fsys, "/work/.cq/harness.json"); err != nil {
		t.Fatal(err)
	}
	if info, err := fsys.Stat("/work/.cq/harness.json.lock"); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %v,%v", info, err)
	}
	before, _ := fsys.ReadFile("/work/.cq/harness.json")
	if _, err := UpdateWorkspace(fsys, "/work/.cq/harness.json", func(policy *WorkspacePolicy) error { policy.Version = 1; return nil }); err == nil {
		t.Fatal("invalid update succeeded")
	}
	after, _ := fsys.ReadFile("/work/.cq/harness.json")
	if string(before) != string(after) {
		t.Fatal("failed update changed original")
	}
}

func TestPortableCreatesOnlyCQDirectoryInExistingProject(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/work", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateWorkspace(fsys, "/work/.cq/harness.json", func(policy *WorkspacePolicy) error {
		policy.Rules = Scope{All: Binding{Profiles: []string{"minimal"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadWorkspace(fsys, "/work/.cq/harness.json")
	if err != nil || len(policy.Rules.All.Profiles) != 1 {
		t.Fatalf("created policy = %#v,%v", policy, err)
	}
	if info, err := fsys.Stat("/work"); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("project permissions changed: %v,%v", info, err)
	}
}
