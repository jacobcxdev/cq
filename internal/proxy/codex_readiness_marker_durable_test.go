package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestSaveCodexHTTPReadinessMarkerDurablyPersistsPrivateMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	marker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))

	if err := saveCodexHTTPReadinessMarkerDurably(dir, marker); err != nil {
		t.Fatalf("save durable readiness marker: %v", err)
	}

	got, err := LoadCodexReadinessMarker(dir, CodexRoutingHTTP)
	if err != nil {
		t.Fatalf("load durable readiness marker: %v", err)
	}
	if !reflect.DeepEqual(got, marker) {
		t.Fatalf("loaded marker = %#v, want %#v", got, marker)
	}
	info, err := os.Stat(codexReadinessPath(dir, CodexRoutingHTTP))
	if err != nil {
		t.Fatalf("stat durable readiness marker: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("marker permissions = %04o, want 0600", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, codexReadinessPoisonName(CodexRoutingHTTP))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readiness poison survived successful commit: %v", err)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".codex-readiness-http.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temporary markers: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary markers remain after success: %v", temps)
	}
}

func TestInvalidateCodexHTTPReadinessMarkerDurablyRemovesPriorMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	marker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))
	if err := saveCodexHTTPReadinessMarkerDurably(dir, marker); err != nil {
		t.Fatalf("save prior marker: %v", err)
	}

	if err := invalidateCodexHTTPReadinessMarkerDurably(dir); err != nil {
		t.Fatalf("invalidate marker: %v", err)
	}
	if _, err := os.Lstat(codexReadinessPath(dir, CodexRoutingHTTP)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker lstat error = %v, want not exist", err)
	}
}

func TestSaveCodexHTTPReadinessMarkerDurablyOrdersDurabilityBarriers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	marker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))
	var events []string
	ops := defaultCodexReadinessMarkerDurableOps()
	ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
		return &testCodexReadinessMarkerDirectory{
			SecureDirectory: directory,
			wrapFile: func(file fsutil.DurableFile) fsutil.DurableFile {
				return &testCodexReadinessMarkerFile{DurableFile: file, sync: func() error {
					events = append(events, "file-sync")
					return file.Sync()
				}}
			},
			rename: func(oldName, newName string) error {
				file, err := directory.OpenNoFollow(oldName)
				if err != nil {
					return err
				}
				info, statErr := file.Stat()
				closeErr := file.Close()
				if statErr != nil || closeErr != nil {
					return errors.Join(statErr, closeErr)
				}
				if got := info.Mode().Perm(); got != 0o600 {
					return fmt.Errorf("temporary marker permissions = %04o, want 0600", got)
				}
				switch newName {
				case codexReadinessPoisonName(CodexRoutingHTTP):
					events = append(events, "rename-poison")
				case filepath.Base(codexReadinessPath(dir, CodexRoutingHTTP)):
					events = append(events, "rename-marker")
				default:
					return fmt.Errorf("unexpected rename destination %q", newName)
				}
				return directory.Rename(oldName, newName)
			},
			sync: func() error {
				events = append(events, "directory-sync")
				return directory.Sync()
			},
		}
	}

	if err := saveCodexHTTPReadinessMarkerDurablyWithOps(dir, marker, ops); err != nil {
		t.Fatalf("save durable readiness marker: %v", err)
	}

	want := []string{
		"file-sync", "rename-poison", "directory-sync",
		"file-sync", "rename-marker", "directory-sync",
		"directory-sync",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("durability events = %v, want %v", events, want)
	}
}

func TestSaveCodexHTTPReadinessMarkerDurablyRejectsSymlinkAuthority(t *testing.T) {
	t.Run("state directory", func(t *testing.T) {
		root := t.TempDir()
		realDirectory := filepath.Join(root, "real")
		if err := os.Mkdir(realDirectory, 0o700); err != nil {
			t.Fatalf("mkdir real directory: %v", err)
		}
		linkedDirectory := filepath.Join(root, "state")
		if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
			t.Fatalf("symlink state directory: %v", err)
		}

		if err := saveCodexHTTPReadinessMarkerDurably(linkedDirectory, testCodexMarker(testCodexRequirements(CodexRoutingHTTP))); err == nil {
			t.Fatal("save through symlink state directory succeeded")
		}
		if _, err := os.Lstat(codexReadinessPath(realDirectory, CodexRoutingHTTP)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marker behind symlink exists or cannot be checked: %v", err)
		}
	})

	t.Run("marker destination", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir state directory: %v", err)
		}
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
			t.Fatalf("write outside file: %v", err)
		}
		target := codexReadinessPath(dir, CodexRoutingHTTP)
		if err := os.Symlink(outside, target); err != nil {
			t.Fatalf("symlink marker destination: %v", err)
		}

		if err := saveCodexHTTPReadinessMarkerDurably(dir, testCodexMarker(testCodexRequirements(CodexRoutingHTTP))); err == nil {
			t.Fatal("save over symlink marker destination succeeded")
		}
		got, err := os.ReadFile(outside)
		if err != nil {
			t.Fatalf("read outside file: %v", err)
		}
		if string(got) != "outside\n" {
			t.Fatalf("outside file = %q, want unchanged sentinel", got)
		}
	})
}

func TestSaveCodexHTTPReadinessMarkerDurablyRejectsDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	marker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))
	ops := defaultCodexReadinessMarkerDurableOps()
	ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
		return &testCodexReadinessMarkerDirectory{SecureDirectory: directory, wrapFile: func(file fsutil.DurableFile) fsutil.DurableFile {
			return &testCodexReadinessMarkerFile{DurableFile: file, sync: func() error {
				if err := file.Sync(); err != nil {
					return err
				}
				displaced := filepath.Join(root, "displaced")
				if err := os.Rename(dir, displaced); err != nil {
					return err
				}
				return os.Mkdir(dir, 0o700)
			}}
		}}
	}

	if err := saveCodexHTTPReadinessMarkerDurablyWithOps(dir, marker, ops); err == nil {
		t.Fatal("save succeeded after state directory replacement")
	}
	if _, err := os.Lstat(codexReadinessPath(dir, CodexRoutingHTTP)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker published into replacement directory: %v", err)
	}
}

func TestSaveCodexHTTPReadinessMarkerDurablyInvalidatesPriorMarkerOnFailure(t *testing.T) {
	failure := errors.New("injected durable marker failure")
	tests := []struct {
		name   string
		mutate func(*codexReadinessMarkerDurableOps)
	}{
		{
			name: "temporary creation",
			mutate: func(ops *codexReadinessMarkerDurableOps) {
				ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
					return &testCodexReadinessMarkerDirectory{SecureDirectory: directory, create: func(string, os.FileMode) (fsutil.DurableFile, error) {
						return nil, failure
					}}
				}
			},
		},
		{
			name: "temporary sync",
			mutate: func(ops *codexReadinessMarkerDurableOps) {
				ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
					return &testCodexReadinessMarkerDirectory{SecureDirectory: directory, wrapFile: func(file fsutil.DurableFile) fsutil.DurableFile {
						return &testCodexReadinessMarkerFile{DurableFile: file, sync: func() error { return failure }}
					}}
				}
			},
		},
		{
			name: "rename",
			mutate: func(ops *codexReadinessMarkerDurableOps) {
				ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
					return &testCodexReadinessMarkerDirectory{SecureDirectory: directory, rename: func(string, string) error { return failure }}
				}
			},
		},
		{
			name: "directory sync",
			mutate: func(ops *codexReadinessMarkerDurableOps) {
				calls := 0
				ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
					return &testCodexReadinessMarkerDirectory{SecureDirectory: directory, sync: func() error {
						calls++
						if calls == 2 {
							return failure
						}
						return directory.Sync()
					}}
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			oldMarker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))
			if err := saveCodexHTTPReadinessMarkerDurably(dir, oldMarker); err != nil {
				t.Fatalf("save prior marker: %v", err)
			}
			ops := defaultCodexReadinessMarkerDurableOps()
			test.mutate(&ops)
			newMarker := oldMarker
			newMarker.CQBuild = "replacement-build"

			if err := saveCodexHTTPReadinessMarkerDurablyWithOps(dir, newMarker, ops); !errors.Is(err, failure) {
				t.Fatalf("save error = %v, want injected failure", err)
			}
			if _, err := os.Lstat(codexReadinessPath(dir, CodexRoutingHTTP)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("marker survived failed replacement: %v", err)
			}
			temporaries, err := filepath.Glob(filepath.Join(dir, ".codex-readiness-http.json.tmp-*"))
			if err != nil {
				t.Fatalf("glob temporary markers: %v", err)
			}
			if len(temporaries) != 0 {
				t.Fatalf("temporary markers survived failed replacement: %v", temporaries)
			}
		})
	}
}

func TestSaveCodexHTTPReadinessMarkerDurablyPoisonsIndeterminateResidualMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	marker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))
	if err := saveCodexHTTPReadinessMarkerDurably(dir, marker); err != nil {
		t.Fatalf("save prior marker: %v", err)
	}
	failure := errors.New("injected post-rename failure")
	syncCalls := 0
	markerRemoveCalls := 0
	ops := defaultCodexReadinessMarkerDurableOps()
	ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
		return &testCodexReadinessMarkerDirectory{
			SecureDirectory: directory,
			remove: func(name string) error {
				if name == filepath.Base(codexReadinessPath(dir, CodexRoutingHTTP)) {
					markerRemoveCalls++
					if markerRemoveCalls == 2 {
						return failure
					}
				}
				return directory.Remove(name)
			},
			sync: func() error {
				syncCalls++
				if syncCalls == 3 {
					return failure
				}
				return directory.Sync()
			},
		}
	}

	if err := saveCodexHTTPReadinessMarkerDurablyWithOps(dir, marker, ops); !errors.Is(err, failure) {
		t.Fatalf("indeterminate save error = %v, want injected failure", err)
	}
	if _, err := os.Lstat(codexReadinessPath(dir, CodexRoutingHTTP)); err != nil {
		t.Fatalf("residual marker missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, codexReadinessPoisonName(CodexRoutingHTTP))); err != nil {
		t.Fatalf("commit poison missing after indeterminate marker write: %v", err)
	}
	if _, err := LoadCodexReadinessMarker(dir, CodexRoutingHTTP); err == nil {
		t.Fatal("indeterminate residual marker remained acceptable")
	}
}

func TestSaveCodexHTTPReadinessMarkerDurablyCommitsWhenFinalPoisonSyncIsIndeterminate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	marker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))
	failure := errors.New("injected poison removal sync failure")
	syncCalls := 0
	ops := defaultCodexReadinessMarkerDurableOps()
	ops.wrapDirectory = func(directory fsutil.SecureDirectory) fsutil.SecureDirectory {
		return &testCodexReadinessMarkerDirectory{SecureDirectory: directory, sync: func() error {
			syncCalls++
			if syncCalls == 3 {
				return failure
			}
			return directory.Sync()
		}}
	}

	if err := saveCodexHTTPReadinessMarkerDurablyWithOps(dir, marker, ops); err != nil {
		t.Fatalf("committed marker returned final poison sync error: %v", err)
	}
	if _, err := LoadCodexReadinessMarker(dir, CodexRoutingHTTP); err != nil {
		t.Fatalf("load committed marker: %v", err)
	}
}

type testCodexReadinessMarkerDirectory struct {
	fsutil.SecureDirectory
	create   func(string, os.FileMode) (fsutil.DurableFile, error)
	wrapFile func(fsutil.DurableFile) fsutil.DurableFile
	rename   func(string, string) error
	remove   func(string) error
	sync     func() error
}

func (directory *testCodexReadinessMarkerDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	if directory.create != nil {
		return directory.create(name, mode)
	}
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil || directory.wrapFile == nil {
		return file, err
	}
	return directory.wrapFile(file), nil
}

func (directory *testCodexReadinessMarkerDirectory) Rename(oldName, newName string) error {
	if directory.rename != nil {
		return directory.rename(oldName, newName)
	}
	return directory.SecureDirectory.Rename(oldName, newName)
}

func (directory *testCodexReadinessMarkerDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	if directory.rename != nil {
		return directory.rename(oldName, newName)
	}
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *testCodexReadinessMarkerDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	if directory.rename != nil {
		return directory.rename(oldName, newName)
	}
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *testCodexReadinessMarkerDirectory) Remove(name string) error {
	if directory.remove != nil {
		return directory.remove(name)
	}
	return directory.SecureDirectory.Remove(name)
}

func (directory *testCodexReadinessMarkerDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	if directory.remove != nil {
		return directory.remove(name)
	}
	return directory.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *testCodexReadinessMarkerDirectory) Sync() error {
	if directory.sync != nil {
		return directory.sync()
	}
	return directory.SecureDirectory.Sync()
}

type testCodexReadinessMarkerFile struct {
	fsutil.DurableFile
	sync func() error
}

func (file *testCodexReadinessMarkerFile) Sync() error {
	if file.sync != nil {
		return file.sync()
	}
	return file.DurableFile.Sync()
}

func (file *testCodexReadinessMarkerFile) Stat() (os.FileInfo, error) {
	inspector, ok := file.DurableFile.(fsutil.DurableFileInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}
