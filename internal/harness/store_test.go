package harness

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestLoadMissingPolicyIsDisabledWithoutCreatingState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	policy, err := Load(fsys, "/state/harness.json")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Version != 1 || policy.Enabled || policy.Projects == nil || policy.Chats == nil {
		t.Fatalf("missing policy = %#v, want disabled version 1 with usable maps", policy)
	}
	if _, err := fsys.Stat("/state"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing load created state: %v", err)
	}
}

func TestSaveAndLoadPrivatePolicy(t *testing.T) {
	fsys := fsutil.NewMemFS()
	want := Policy{Version: 1, Enabled: true, Projects: map[string]Profile{"/work/project": Full}, Chats: map[string]Profile{testChat: Minimal}}
	if err := Save(fsys, "/state/harness.json", want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(fsys, "/state/harness.json")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded = %#v, %v; want %#v", got, err, want)
	}
	for path, mode := range map[string]os.FileMode{"/state": 0o700, "/state/harness.json": 0o600, "/state/harness.json.lock": 0o600} {
		info, err := fsys.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private state %s = %v, %v; want %04o", path, info, err, mode)
		}
	}
}

func TestLoadRejectsInvalidPolicy(t *testing.T) {
	tests := map[string]string{
		"unknown field":   `{"version":1,"enabled":true,"fallback":"full"}`,
		"future version":  `{"version":3,"enabled":true}`,
		"missing version": `{"enabled":true}`,
		"null policy":     `null`,
		"trailing data":   `{"version":1} {"version":1}`,
		"trailing junk":   `{"version":1} trailing`,
		"wrong boolean":   `{"version":1,"enabled":"true"}`,
		"auto profile":    `{"version":1,"projects":{"/work":"auto"}}`,
		"unknown profile": `{"version":1,"projects":{"/work":"tiny"}}`,
		"profile case":    `{"version":1,"projects":{"/work":"Full"}}`,
		"null profile":    `{"version":1,"projects":{"/work":null}}`,
		"relative path":   `{"version":1,"projects":{"work/project":"full"}}`,
		"unclean path":    `{"version":1,"projects":{"/work/../other":"full"}}`,
		"trailing slash":  `{"version":1,"projects":{"/work/":"full"}}`,
		"nul path":        `{"version":1,"projects":{"/work\u0000":"full"}}`,
		"bad chat":        `{"version":1,"chats":{"chat-123":"minimal"}}`,
		"chat uri":        `{"version":1,"chats":{"codex://threads/019c6e27-e55b-73d1-87d8-4e01f1f75043":"full"}}`,
		"uppercase uuid":  `{"version":1,"chats":{"019C6E27-E55B-73D1-87D8-4E01F1F75043":"full"}}`,
		"chat auto":       `{"version":1,"chats":{"019c6e27-e55b-73d1-87d8-4e01f1f75043":"auto"}}`,
		"oversized":       `{"version":1}` + strings.Repeat(" ", 1<<20),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if err := fsys.MkdirAll("/state", 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile("/state/harness.json", []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(fsys, "/state/harness.json"); err == nil {
				t.Fatalf("accepted invalid policy %s", name)
			}
		})
	}
}

func TestLoadRejectsNonPrivateState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/harness.json", []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fsys, "/state/harness.json"); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("read broad policy error = %v, want unsafe path", err)
	}
}

func TestInvalidDestinationAndUpdateDoNotCreateState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	for _, path := range []string{"", "relative/harness.json", "/", "/state/../harness.json", "/state/harness.json\x00"} {
		if _, err := Load(fsys, path); err == nil {
			t.Errorf("load accepted destination %q", path)
		}
		if _, err := Update(fsys, path, func(*Policy) error { return nil }); err == nil {
			t.Errorf("update accepted destination %q", path)
		}
	}
	if _, err := Update(fsys, "/state/harness.json", nil); err == nil {
		t.Fatal("accepted missing update callback")
	}
	if err := Save(fsys, "/state/harness.json", Policy{Version: 1, Chats: map[string]Profile{testChat: "auto"}}); err == nil {
		t.Fatal("persisted auto as a profile")
	}
	if _, err := fsys.Stat("/state"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid input created state: %v", err)
	}
}

func TestUpdatePreservesOverridesAndAutoRemovesThem(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := Update(fsys, "/state/harness.json", func(policy *Policy) error {
		policy.Enabled = true
		policy.Projects["/work"] = Full
		policy.Chats[testChat] = Minimal
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(fsys, "/state/harness.json", func(policy *Policy) error {
		policy.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := Update(fsys, "/state/harness.json", func(policy *Policy) error {
		delete(policy.Chats, testChat)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Enabled || policy.Projects["/work"] != Full || len(policy.Chats) != 0 {
		t.Fatalf("updated policy = %#v", policy)
	}
	policy.Enabled = true
	if got := policy.Resolve("glm-5", "/work/project", testChat); got.Profile != Full || got.Source != "project" {
		t.Fatalf("deleted chat override did not inherit project: %#v", got)
	}
}

func TestUpdateFailureLeavesOldPolicyIntact(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := Save(fsys, "/state/harness.json", Policy{Version: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	original, err := fsys.ReadFile("/state/harness.json")
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("test transaction failed")
	for name, update := range map[string]func(*Policy) error{
		"callback error": func(policy *Policy) error { policy.Enabled = false; return failed },
		"invalid update": func(policy *Policy) error { policy.Projects["/work"] = "auto"; return nil },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Update(fsys, "/state/harness.json", update); err == nil {
				t.Fatal("update succeeded")
			}
			got, err := fsys.ReadFile("/state/harness.json")
			if err != nil || string(got) != string(original) {
				t.Fatalf("failed update altered policy: %s, %v", got, err)
			}
		})
	}
	if _, err := Update(renameFailureFS{fsys}, "/state/harness.json", func(policy *Policy) error {
		policy.Enabled = false
		return nil
	}); !errors.Is(err, errTestRename) {
		t.Fatalf("update error = %v, want injected rename failure", err)
	}
	got, err := fsys.ReadFile("/state/harness.json")
	if err != nil || string(got) != string(original) {
		t.Fatalf("failed rename altered policy: %s, %v", got, err)
	}
	// A failed transaction must also release the lock.
	if _, err := Update(fsys, "/state/harness.json", func(policy *Policy) error { policy.Enabled = false; return nil }); err != nil {
		t.Fatalf("failed transaction retained lock: %v", err)
	}
}

func TestConcurrentUpdateRefusesStaleOverwrite(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := Save(fsys, "/state/harness.json", Policy{Version: 1}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Update(fsys, "/state/harness.json", func(policy *Policy) error {
			close(entered)
			<-release
			policy.Projects["/work"] = Full
			return nil
		})
		done <- err
	}()
	<-entered
	callbackCalled := false
	_, concurrentErr := Update(fsys, "/state/harness.json", func(policy *Policy) error {
		callbackCalled = true
		policy.Chats[testChat] = Minimal
		return nil
	})
	close(release)
	firstErr := <-done
	if firstErr != nil || !errors.Is(concurrentErr, fsutil.ErrExclusiveLockHeld) || callbackCalled {
		t.Fatalf("concurrent update = %v, first = %v, second callback = %v", concurrentErr, firstErr, callbackCalled)
	}
	policy, err := Update(fsys, "/state/harness.json", func(policy *Policy) error {
		policy.Chats[testChat] = Minimal
		return nil
	})
	if err != nil || policy.Projects["/work"] != Full || policy.Chats[testChat] != Minimal {
		t.Fatalf("retry lost another update: %#v, %v", policy, err)
	}
}

type renameFailureFS struct{ *fsutil.MemFS }

func (fsys renameFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return renameFailureDirectory{directory}, nil
}

type renameFailureDirectory struct{ fsutil.SecureDirectory }

var errTestRename = errors.New("test rename failed")

func (renameFailureDirectory) Rename(string, string) error {
	return errTestRename
}

func (renameFailureDirectory) RenameChecked(string, string, fsutil.SecureFileIdentity) error {
	return errTestRename
}

func (directory renameFailureDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory renameFailureDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}
