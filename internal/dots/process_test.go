//go:build darwin || linux

package dots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

func processSettings(t *testing.T) *Settings {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Settings{RuntimeRoot: root, AppPath: "/Applications/Original app.app", IntegrationMode: "native"}
}

type fakeProcess struct {
	uid  int
	args []string
	cwd  string
}

func fakeProcessOperations(rows map[int]fakeProcess) processOperations {
	return processOperations{
		list: func(context.Context) ([]int, error) {
			var pids []int
			for pid := range rows {
				pids = append(pids, pid)
			}
			slices.Sort(pids)
			return pids, nil
		},
		arguments: func(_ context.Context, pid int) (int, []string, error) {
			row, ok := rows[pid]
			if !ok {
				return 0, nil, os.ErrNotExist
			}
			return row.uid, row.args, nil
		},
		cwd: func(_ context.Context, pid int) (string, error) {
			row, ok := rows[pid]
			if !ok {
				return "", os.ErrNotExist
			}
			return row.cwd, nil
		},
		signal: func(pid int) error { delete(rows, pid); return nil },
	}
}

func TestNormalAppIdentityUsesExactOwnedArguments(t *testing.T) {
	s := processSettings(t)
	exe := filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT")
	for _, test := range []struct {
		name string
		uid  int
		args []string
		want bool
	}{
		{"ordinary", os.Getuid(), []string{exe}, true},
		{"native routed", os.Getuid(), []string{exe, "--cq-dots-runtime=" + s.RuntimeRoot, "--remote-debugging-pipe"}, true},
		{"isolated equals", os.Getuid(), []string{exe, "--user-data-dir=/private/test"}, false},
		{"isolated separate", os.Getuid(), []string{exe, "--user-data-dir", "/private/test"}, false},
		{"other user", os.Getuid() + 1, []string{exe}, false},
		{"helper suffix", os.Getuid(), []string{exe + " Helper"}, false},
		{"argument contains app", os.Getuid(), []string{"/bin/echo", exe}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows := map[int]fakeProcess{42: {test.uid, test.args, ""}}
			got, err := normalAppRunning(context.Background(), s, fakeProcessOperations(rows))
			if err != nil || got != test.want {
				t.Fatalf("running %v, %v; want %v", got, err, test.want)
			}
		})
	}
}

func TestRuntimeExecutableRejectsSpoofedArguments(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "cq-dots-runtime")
	for _, suffix := range []string{"", " (deleted)"} {
		if !runtimeExecutableMatches([]string{executable}, executable+suffix) {
			t.Fatal("staged executable rejected")
		}
	}
	if runtimeExecutableMatches([]string{executable}, "/usr/bin/python3") {
		t.Fatal("spoofed executable accepted")
	}
}

func TestRuntimeProcessesRequireOwnerRoleRootAndDirectory(t *testing.T) {
	s := processSettings(t)
	exe := filepath.Join(s.RuntimeRoot, "cq-dots-runtime")
	args := []string{exe, "__dots", "relay", filepath.Dir(s.RuntimeRoot)}
	rows := map[int]fakeProcess{
		42: {os.Getuid(), args, s.RuntimeRoot},
		43: {os.Getuid() + 1, args, s.RuntimeRoot},
		44: {os.Getuid(), []string{exe, "__dots", "relay", "/other"}, s.RuntimeRoot},
		45: {os.Getuid(), args, "/other"},
		46: {os.Getuid(), []string{exe, "__dots", "unknown", filepath.Dir(s.RuntimeRoot)}, s.RuntimeRoot},
		47: {os.Getuid(), []string{exe, "__dots", "desktop-recover", filepath.Dir(s.RuntimeRoot)}, s.RuntimeRoot},
	}
	got, err := runtimeProcesses(context.Background(), s, fakeProcessOperations(rows))
	if err != nil || !slices.Equal(got, []RuntimeProcess{{42, "relay"}}) {
		t.Fatalf("processes %#v, %v", got, err)
	}
}

func TestRuntimeProcessesRetainLegacyWorkersWithoutPythonExecution(t *testing.T) {
	s := processSettings(t)
	python := filepath.Join(s.RuntimeRoot, "venv", "bin", "python")
	if err := os.WriteFile(filepath.Join(s.RuntimeRoot, "account_watch.py"), []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("account_watch.py", filepath.Join(s.RuntimeRoot, "probe.py")); err != nil {
		t.Fatal(err)
	}
	rows := map[int]fakeProcess{
		42: {os.Getuid(), []string{python, "-u", "account_watch.py"}, s.RuntimeRoot},
		43: {os.Getuid(), []string{python, filepath.Join(s.RuntimeRoot, "account_watch.py")}, "/other"},
		44: {os.Getuid(), []string{python, "probe.py"}, s.RuntimeRoot},
		45: {os.Getuid(), []string{"/usr/bin/python3", "account_watch.py"}, s.RuntimeRoot},
	}
	got, err := runtimeProcesses(context.Background(), s, fakeProcessOperations(rows))
	if err != nil || !slices.Equal(got, []RuntimeProcess{{42, "account-watch"}}) {
		t.Fatalf("processes %#v, %v", got, err)
	}
}

func TestStopRuntimeNeverSignalsWhileNormalAppRuns(t *testing.T) {
	s := processSettings(t)
	rows := map[int]fakeProcess{42: {os.Getuid(), []string{filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT")}, ""}, 43: {os.Getuid(), []string{filepath.Join(s.RuntimeRoot, "cq-dots-runtime"), "__dots", "native-broker", filepath.Dir(s.RuntimeRoot)}, s.RuntimeRoot}}
	ops := fakeProcessOperations(rows)
	signals := 0
	ops.signal = func(int) error { signals++; return nil }
	if err := stopRuntime(context.Background(), s, false, ops); err == nil {
		t.Fatal("normal app stop accepted")
	}
	if signals != 0 {
		t.Fatalf("sent %d signals", signals)
	}
}

func TestStopRuntimePreservesWatcherAndNeverEscalates(t *testing.T) {
	s := processSettings(t)
	exe := filepath.Join(s.RuntimeRoot, "cq-dots-runtime")
	root := filepath.Dir(s.RuntimeRoot)
	rows := map[int]fakeProcess{42: {os.Getuid(), []string{exe, "__dots", "relay", root}, s.RuntimeRoot}, 43: {os.Getuid(), []string{exe, "__dots", "account-watch", root}, s.RuntimeRoot}}
	if err := stopRuntime(context.Background(), s, true, fakeProcessOperations(rows)); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[43].args == nil {
		t.Fatalf("remaining rows %#v", rows)
	}
	ops := fakeProcessOperations(rows)
	signals := 0
	ops.signal = func(int) error { signals++; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := stopRuntime(ctx, s, false, ops); err == nil {
		t.Fatal("stuck process accepted")
	}
	if signals != 1 {
		t.Fatalf("sent %d signals", signals)
	}
}

func TestStopRuntimeRechecksNewNormalAppBeforeHelperSignal(t *testing.T) {
	s := processSettings(t)
	exe := filepath.Join(s.RuntimeRoot, "cq-dots-runtime")
	rows := map[int]fakeProcess{43: {os.Getuid(), []string{exe, "__dots", "native-broker", filepath.Dir(s.RuntimeRoot)}, s.RuntimeRoot}}
	ops := fakeProcessOperations(rows)
	list := ops.list
	calls := 0
	ops.list = func(ctx context.Context) ([]int, error) {
		calls++
		if calls == 3 {
			rows[44] = fakeProcess{os.Getuid(), []string{filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT")}, ""}
		}
		return list(ctx)
	}
	signals := 0
	ops.signal = func(int) error { signals++; return nil }
	if err := stopRuntime(context.Background(), s, false, ops); err == nil {
		t.Fatal("new app accepted")
	}
	if signals != 0 {
		t.Fatalf("sent %d signals", signals)
	}
}

func TestStageExecutablePreservesPreviouslyOpenedInode(t *testing.T) {
	s := processSettings(t)
	source := filepath.Join(t.TempDir(), "cq")
	if err := os.WriteFile(source, []byte("first executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := StageExecutable(source, s.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := os.WriteFile(source, []byte("second executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := StageExecutable(source, s.RuntimeRoot); err != nil {
		t.Fatal(err)
	}
	oldData, _ := httputil.ReadBody(old)
	data, err := os.ReadFile(path)
	if err != nil || string(oldData) != "first executable" || string(data) != "second executable" {
		t.Fatalf("old %q, new %q, error %v", oldData, data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("staged mode %v, %v", info, err)
	}
}

func TestStageExecutableRejectedUnsafeDestination(t *testing.T) {
	s := processSettings(t)
	source := filepath.Join(t.TempDir(), "cq")
	if err := os.WriteFile(source, []byte("executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(s.RuntimeRoot, "cq-dots-runtime")
	if err := os.Symlink(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := StageExecutable(source, s.RuntimeRoot); err == nil {
		t.Fatal("symlink accepted")
	}
	data, _ := os.ReadFile(source)
	if string(data) != "executable" {
		t.Fatal("source modified")
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := StageExecutable(source, s.RuntimeRoot); err == nil {
		t.Fatal("shared-writable source accepted")
	}
}

func TestProcessQueriesPreserveCancellationAndErrors(t *testing.T) {
	s := processSettings(t)
	ops := fakeProcessOperations(nil)
	failure := errors.New("query failed")
	ops.list = func(context.Context) ([]int, error) { return nil, failure }
	if _, err := runtimeProcesses(context.Background(), s, ops); !errors.Is(err, failure) {
		t.Fatalf("error %v", err)
	}
}
