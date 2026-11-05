package harness

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCurationSkillInstallIsPrivateExactAndIdempotent(t *testing.T) {
	fsys := fsutil.NewMemFS()
	const directory = "/codex/skills/curate-harness"
	const skillPath = directory + "/SKILL.md"
	if len(curationSkill) == 0 || !bytes.Contains(curationSkill, []byte(curationSkillMarker)) {
		t.Fatal("embedded asset must exist and carry its CQ ownership marker")
	}
	first, err := InstallCurationSkill(fsys, directory)
	if err != nil || !first.Changed || first.Path != skillPath {
		t.Fatalf("first install = %#v, %v", first, err)
	}
	content, err := fsys.ReadFile(skillPath)
	if err != nil || !bytes.Equal(content, curationSkill) {
		t.Fatalf("installed content differs from embedded asset: %v", err)
	}
	for path, mode := range map[string]os.FileMode{
		"/codex": 0o700, "/codex/skills": 0o700, directory: 0o700,
		skillPath: 0o600, directory + "/cq-install.lock": 0o600,
	} {
		info, err := fsys.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions for %s = %v, %v; want %04o", path, info, err, mode)
		}
	}
	before, err := fsys.Stat(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeID, _ := fsys.FileIdentity(before)
	second, err := InstallCurationSkill(fsys, directory)
	if err != nil || second.Changed || second.Path != skillPath {
		t.Fatalf("repeat install = %#v, %v", second, err)
	}
	after, err := fsys.Stat(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	afterID, _ := fsys.FileIdentity(after)
	if !fsutil.SameSecureObject(beforeID, afterID) {
		t.Fatal("idempotent install unnecessarily replaced the skill")
	}
}

func TestCurationSkillUpdatesOnlyCQOwnedContent(t *testing.T) {
	for name, previous := range map[string][]byte{
		"CQ-owned older asset": []byte(curationSkillMarker + "\nold curation instructions\n"),
		"unrelated skill":      []byte("---\nname: curate-harness\n---\nMy own skill.\n"),
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			const directory = "/codex/skills/curate-harness"
			if err := fsys.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile(directory+"/SKILL.md", previous, 0o600); err != nil {
				t.Fatal(err)
			}
			const notes = "independent notes must survive installation"
			if err := fsys.WriteFile(directory+"/NOTES.md", []byte(notes), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := InstallCurationSkill(fsys, directory)
			owned := bytes.Contains(previous, []byte(curationSkillMarker))
			if owned && (err != nil || !result.Changed) || !owned && (err == nil || result.Changed) {
				t.Fatalf("owned = %t; install = %#v, %v", owned, result, err)
			}
			want := previous
			if owned {
				want = curationSkill
			}
			got, readErr := fsys.ReadFile(directory + "/SKILL.md")
			if readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("installed content = %q, %v; want preserved or upgraded asset", got, readErr)
			}
			got, readErr = fsys.ReadFile(directory + "/NOTES.md")
			if readErr != nil || string(got) != notes {
				t.Fatalf("unrelated file changed: %q, %v", got, readErr)
			}
		})
	}
}

func TestCurationSkillInstallRejectsHeldLease(t *testing.T) {
	fsys := fsutil.NewMemFS()
	const directory = "/codex/skills/curate-harness"
	if err := fsutil.EnsureSecureDirectory(fsys, directory); err != nil {
		t.Fatal(err)
	}
	lease, err := fsutil.AcquireExclusiveLock(fsys, directory+"/cq-install.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	result, err := InstallCurationSkill(fsys, directory)
	if !errors.Is(err, fsutil.ErrExclusiveLockHeld) || result.Changed {
		t.Fatalf("concurrent install = %#v, %v; want held lock", result, err)
	}
	if _, err := fsys.Stat(directory + "/SKILL.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked installation created an asset: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if result, err := InstallCurationSkill(fsys, directory); err != nil || !result.Changed {
		t.Fatalf("install after release = %#v, %v", result, err)
	}
}

func TestCurationSkillRejectsInvalidTargetsBeforeCreatingState(t *testing.T) {
	for _, path := range []string{"", "relative/curate-harness", "/codex/skills/other", "/codex/skills/curate-harness/", "/codex/skills/../curate-harness", "/codex/skills/curate-harness/child"} {
		fsys := fsutil.NewMemFS()
		if result, err := InstallCurationSkill(fsys, path); err == nil || result.Changed {
			t.Errorf("accepted target %q: %#v, %v", path, result, err)
		}
		if _, err := fsys.Stat("/codex"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid target %q created state: %v", path, err)
		}
	}
}

func TestCurationSkillSymlinksNeverOverwriteExternalContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("temporary secure roots and symlink privileges are not enabled on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := []byte(curationSkillMarker + "\nexternal older instructions\n")
	externalFile := filepath.Join(external, "SKILL.md")
	if err := os.WriteFile(externalFile, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			parent := filepath.Join(root, kind)
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(parent, "curate-harness")
			link := target
			if kind == "file" {
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(target, "SKILL.md")
			}
			source := external
			if kind == "file" {
				source = externalFile
			}
			if err := os.Symlink(source, link); err != nil {
				t.Fatal(err)
			}
			if result, err := InstallCurationSkill(fsutil.OSFileSystem{}, target); err == nil || result.Changed {
				t.Fatalf("followed %s symlink: %#v, %v", kind, result, err)
			}
			got, err := os.ReadFile(externalFile)
			if err != nil || !bytes.Equal(got, previous) {
				t.Fatalf("external content changed: %q, %v", got, err)
			}
			if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("installation replaced the original link: %v, %v", info, err)
			}
		})
	}
}

func TestCurationSkillPathUsesCodexHomeAndUserFallback(t *testing.T) {
	t.Setenv("CODEX_HOME", "/configured/codex")
	if got, err := CurationSkillPath(); err != nil || got != "/configured/codex/skills/curate-harness" {
		t.Fatalf("configured path = %q, %v", got, err)
	}
	t.Setenv("CODEX_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".codex", "skills", "curate-harness")
	if got, err := CurationSkillPath(); err != nil || got != want {
		t.Fatalf("fallback path = %q, %v; want %q", got, err, want)
	}
	for _, home := range []string{"relative/codex", "/codex/../other", "/codex/"} {
		t.Setenv("CODEX_HOME", home)
		if got, err := CurationSkillPath(); err == nil {
			t.Errorf("accepted CODEX_HOME %q: %q", home, got)
		}
	}
}
