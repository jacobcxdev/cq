package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	codexLeaseMigrationArchiveTarget = "archive"
	codexLeaseMigrationV2Target      = "v2"
)

func TestCodexLeaseV2MigrationCrashOutcomesRemainRetryable(t *testing.T) {
	tests := []struct {
		name            string
		target          string
		phase           string
		wantOutcome     fsutil.CommitOutcome
		wantCanonicalV2 bool
		wantArchive     bool
	}{
		{name: "archive temp create", target: codexLeaseMigrationArchiveTarget, phase: "create", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive partial write", target: codexLeaseMigrationArchiveTarget, phase: "write", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive file sync", target: codexLeaseMigrationArchiveTarget, phase: "sync", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive temporary descriptor validation", target: codexLeaseMigrationArchiveTarget, phase: "temp-descriptor-validation", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive close", target: codexLeaseMigrationArchiveTarget, phase: "close", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive pre-replace directory validation", target: codexLeaseMigrationArchiveTarget, phase: "pre-replace-directory-validation", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive temporary path identity revalidation", target: codexLeaseMigrationArchiveTarget, phase: "temp-path-identity-revalidation", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive precondition", target: codexLeaseMigrationArchiveTarget, phase: "precondition", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive rename", target: codexLeaseMigrationArchiveTarget, phase: "rename", wantOutcome: fsutil.CommitNotCommitted},
		{name: "archive installed validation", target: codexLeaseMigrationArchiveTarget, phase: "installed-validation", wantOutcome: fsutil.CommitIndeterminate, wantArchive: true},
		{name: "archive post-replace directory validation", target: codexLeaseMigrationArchiveTarget, phase: "post-replace-directory-validation", wantOutcome: fsutil.CommitIndeterminate, wantArchive: true},
		{name: "archive directory sync", target: codexLeaseMigrationArchiveTarget, phase: "dir-sync", wantOutcome: fsutil.CommitIndeterminate, wantArchive: true},
		{name: "archive installed post-sync revalidation", target: codexLeaseMigrationArchiveTarget, phase: "post-sync-installed-revalidation", wantOutcome: fsutil.CommitIndeterminate, wantArchive: true},
		{name: "archive final directory validation", target: codexLeaseMigrationArchiveTarget, phase: "final-directory-validation", wantOutcome: fsutil.CommitIndeterminate, wantArchive: true},
		{name: "archive post-success read", target: codexLeaseMigrationArchiveTarget, phase: "post-success-read", wantOutcome: fsutil.CommitIndeterminate, wantArchive: true},
		{name: "archive post-success mismatch", target: codexLeaseMigrationArchiveTarget, phase: "post-success-mismatch", wantOutcome: fsutil.CommitIndeterminate, wantArchive: true},
		{name: "v2 temp create", target: codexLeaseMigrationV2Target, phase: "create", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 partial write", target: codexLeaseMigrationV2Target, phase: "write", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 file sync", target: codexLeaseMigrationV2Target, phase: "sync", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 temporary descriptor validation", target: codexLeaseMigrationV2Target, phase: "temp-descriptor-validation", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 close", target: codexLeaseMigrationV2Target, phase: "close", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 pre-replace directory validation", target: codexLeaseMigrationV2Target, phase: "pre-replace-directory-validation", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 temporary path identity revalidation", target: codexLeaseMigrationV2Target, phase: "temp-path-identity-revalidation", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 precondition", target: codexLeaseMigrationV2Target, phase: "precondition", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 rename", target: codexLeaseMigrationV2Target, phase: "rename", wantOutcome: fsutil.CommitNotCommitted, wantArchive: true},
		{name: "v2 installed validation", target: codexLeaseMigrationV2Target, phase: "installed-validation", wantOutcome: fsutil.CommitIndeterminate, wantCanonicalV2: true, wantArchive: true},
		{name: "v2 post-replace directory validation", target: codexLeaseMigrationV2Target, phase: "post-replace-directory-validation", wantOutcome: fsutil.CommitIndeterminate, wantCanonicalV2: true, wantArchive: true},
		{name: "v2 directory sync", target: codexLeaseMigrationV2Target, phase: "dir-sync", wantOutcome: fsutil.CommitIndeterminate, wantCanonicalV2: true, wantArchive: true},
		{name: "v2 installed post-sync revalidation", target: codexLeaseMigrationV2Target, phase: "post-sync-installed-revalidation", wantOutcome: fsutil.CommitIndeterminate, wantCanonicalV2: true, wantArchive: true},
		{name: "v2 final directory validation", target: codexLeaseMigrationV2Target, phase: "final-directory-validation", wantOutcome: fsutil.CommitIndeterminate, wantCanonicalV2: true, wantArchive: true},
		{name: "v2 post-success read", target: codexLeaseMigrationV2Target, phase: "post-success-read", wantOutcome: fsutil.CommitIndeterminate, wantCanonicalV2: true, wantArchive: true},
		{name: "v2 post-success mismatch", target: codexLeaseMigrationV2Target, phase: "post-success-mismatch", wantOutcome: fsutil.CommitIndeterminate, wantCanonicalV2: true, wantArchive: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mem := fsutil.NewMemFS()
			key, legacy := writeCodexLeaseV1Fixture(t, mem)
			archivePath := filepath.Join("/state", "leases.json.v1-"+codexLeaseSHA256(legacy)+".archive")
			fsys := newCodexLeaseMigrationCrashFS(mem, test.target, test.phase)

			coordinator, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{})
			if coordinator != nil {
				_ = coordinator.Close()
				t.Fatal("injected migration failure returned a coordinator")
			}
			if err == nil {
				t.Fatal("injected migration failure returned nil error")
			}
			if got := fsutil.AtomicWriteOutcome(err); got != test.wantOutcome {
				t.Fatalf("migration outcome = %s error %T %v, want %s", got, err, err, test.wantOutcome)
			}
			wantTyped := fsutil.ErrCommitNotCommitted
			if test.wantOutcome == fsutil.CommitIndeterminate {
				wantTyped = fsutil.ErrCommitIndeterminate
			}
			if !errors.Is(err, wantTyped) {
				t.Fatalf("migration error = %T %v, want typed %v", err, err, wantTyped)
			}
			if test.phase == "temp-descriptor-validation" && !strings.Contains(err.Error(), "validate durable temporary descriptor") {
				t.Fatalf("migration error = %v, want temporary descriptor validation operation", err)
			}
			if !fsys.injected {
				t.Fatalf("migration never reached injected %s %s phase; events=%v", test.target, test.phase, fsys.events)
			}
			gotOrder := fsys.targetEvents(test.target)
			wantOrder := codexLeaseMigrationOrderThrough(test.phase)
			if len(gotOrder) < len(wantOrder) || !slices.Equal(gotOrder[:len(wantOrder)], wantOrder) {
				t.Fatalf("migration order through %s = %v, want prefix %v", test.phase, gotOrder, wantOrder)
			}

			assertCodexLeaseMigrationCrashState(t, mem, key, legacy, archivePath, test.wantCanonicalV2, test.wantArchive)

			fsys.disableFailure()
			reopened, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{})
			if err != nil {
				t.Fatalf("retry after %s failed: %v; events=%v", test.name, err, fsys.events)
			}
			if reopened.Store().Generation() != 8 {
				t.Fatalf("retry generation = %d, want 8", reopened.Store().Generation())
			}
			if reopened.Store().v2 == nil || reopened.Store().v2.Cutover.State != CodexLeaseCutoverLegacyQuarantine || len(reopened.Store().v2.Lanes) != 0 || len(reopened.Store().v2.Records) != 0 {
				t.Fatalf("retry did not restore empty quarantined v2 authority: %#v", reopened.Store().v2)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			assertCodexLeaseMigrationCrashState(t, mem, key, legacy, archivePath, true, true)
		})
	}
}

func assertCodexLeaseMigrationCrashState(t *testing.T, fsys *fsutil.MemFS, key, legacy []byte, archivePath string, wantV2, wantArchive bool) {
	t.Helper()

	storedKey, err := fsys.ReadFile("/state/leases.key")
	if err != nil || !bytes.Equal(storedKey, key) {
		t.Fatalf("migration changed key: error=%v", err)
	}
	journal, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatalf("migration lost canonical journal: %v", err)
	}
	if !wantV2 {
		if !bytes.Equal(journal, legacy) {
			t.Fatal("pre-rename failure did not retain byte-identical valid v1 authority")
		}
	} else {
		var envelope codexLeaseJournalEnvelopeV2
		if err := decodeCodexLeaseV2StrictJSON(journal, &envelope); err != nil {
			t.Fatalf("post-rename canonical journal is not strict v2: %v", err)
		}
		if envelope.Version != codexLeaseJournalVersionV3 || envelope.Generation != 8 || envelope.Cutover.CompatibilityEpoch != 4 || envelope.Cutover.State != CodexLeaseCutoverLegacyQuarantine || len(envelope.Lanes) != 0 || len(envelope.Records) != 0 || !validCodexLeaseV2TestMAC(key, envelope) {
			t.Fatalf("post-rename canonical authority is not valid empty quarantined v3: %#v", envelope)
		}
	}

	archive, archiveErr := fsys.ReadFile(archivePath)
	if wantArchive {
		if archiveErr != nil || !bytes.Equal(archive, legacy) {
			t.Fatalf("durable archive = %q error %v, want byte-identical v1", archive, archiveErr)
		}
		info, err := fsys.Stat(archivePath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("archive metadata = %#v error %v, want mode 0600", info, err)
		}
	} else if !errors.Is(archiveErr, os.ErrNotExist) {
		t.Fatalf("pre-archive-rename failure left archive: error=%v", archiveErr)
	}

	entries, err := fsys.ReadDir("/state")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".leases.json") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("migration left partial temporary authority %q", entry.Name())
		}
	}
}

type codexLeaseMigrationCrashFS struct {
	*fsutil.MemFS
	target             string
	phase              string
	injected           bool
	events             []string
	tempClosed         map[string]bool
	renamed            map[string]bool
	prechecked         map[string]bool
	openedAfterRename  map[string]int
	preReplaceChecked  map[string]bool
	installedChecked   map[string]bool
	postReplaceChecked map[string]bool
	directorySynced    map[string]bool
	postSyncChecked    map[string]bool
	finalDirChecked    map[string]bool
	activeTarget       string
	lastRenamed        string
}

func newCodexLeaseMigrationCrashFS(mem *fsutil.MemFS, target, phase string) *codexLeaseMigrationCrashFS {
	return &codexLeaseMigrationCrashFS{
		MemFS:              mem,
		target:             target,
		phase:              phase,
		tempClosed:         make(map[string]bool),
		renamed:            make(map[string]bool),
		prechecked:         make(map[string]bool),
		openedAfterRename:  make(map[string]int),
		preReplaceChecked:  make(map[string]bool),
		installedChecked:   make(map[string]bool),
		postReplaceChecked: make(map[string]bool),
		directorySynced:    make(map[string]bool),
		postSyncChecked:    make(map[string]bool),
		finalDirChecked:    make(map[string]bool),
	}
}

func (fsys *codexLeaseMigrationCrashFS) disableFailure() {
	fsys.target = ""
	fsys.phase = ""
}

func (fsys *codexLeaseMigrationCrashFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &codexLeaseMigrationCrashDirectory{SecureDirectory: directory, fsys: fsys}, nil
}

func (fsys *codexLeaseMigrationCrashFS) fail(target, phase string) bool {
	if fsys.injected || fsys.target != target || fsys.phase != phase {
		return false
	}
	fsys.injected = true
	return true
}

func (fsys *codexLeaseMigrationCrashFS) targetEvents(target string) []string {
	events := make([]string, 0, len(fsys.events))
	prefix := target + ":"
	for _, event := range fsys.events {
		if strings.HasPrefix(event, prefix) {
			events = append(events, strings.TrimPrefix(event, prefix))
		}
	}
	return events
}

type codexLeaseMigrationCrashDirectory struct {
	fsutil.SecureDirectory
	fsys *codexLeaseMigrationCrashFS
}

func (directory *codexLeaseMigrationCrashDirectory) Stat() (os.FileInfo, error) {
	target := directory.fsys.activeTarget
	if target != "" {
		switch {
		case directory.fsys.tempClosed[target] && !directory.fsys.renamed[target] && !directory.fsys.preReplaceChecked[target]:
			directory.fsys.preReplaceChecked[target] = true
			directory.fsys.events = append(directory.fsys.events, target+":pre-replace-directory-validation")
			if directory.fsys.fail(target, "pre-replace-directory-validation") {
				return nil, errors.New("injected migration pre-replace directory validation failure")
			}
		case directory.fsys.renamed[target] && directory.fsys.installedChecked[target] && !directory.fsys.directorySynced[target] && !directory.fsys.postReplaceChecked[target]:
			directory.fsys.postReplaceChecked[target] = true
			directory.fsys.events = append(directory.fsys.events, target+":post-replace-directory-validation")
			if directory.fsys.fail(target, "post-replace-directory-validation") {
				return nil, errors.New("injected migration post-replace directory validation failure")
			}
		case directory.fsys.postSyncChecked[target] && !directory.fsys.finalDirChecked[target]:
			directory.fsys.finalDirChecked[target] = true
			directory.fsys.events = append(directory.fsys.events, target+":final-directory-validation")
			if directory.fsys.fail(target, "final-directory-validation") {
				return nil, errors.New("injected migration final directory validation failure")
			}
		}
	}
	return directory.SecureDirectory.Stat()
}

func (directory *codexLeaseMigrationCrashDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	target := codexLeaseMigrationTempTarget(name)
	if target != "" {
		directory.fsys.activeTarget = target
		directory.fsys.events = append(directory.fsys.events, target+":create")
		if mode.Perm() != 0o600 {
			return nil, fmt.Errorf("migration temporary mode = %04o, want 0600", mode.Perm())
		}
		if directory.fsys.fail(target, "create") {
			return nil, errors.New("injected migration temporary create failure")
		}
	}
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	if target == "" {
		return file, nil
	}
	return &codexLeaseMigrationCrashFile{DurableFile: file, fsys: directory.fsys, target: target}, nil
}

func (directory *codexLeaseMigrationCrashDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if target := codexLeaseMigrationTempTarget(name); target != "" && directory.fsys.tempClosed[target] && !directory.fsys.renamed[target] {
		directory.fsys.events = append(directory.fsys.events, target+":temp-path-identity-revalidation")
		if directory.fsys.fail(target, "temp-path-identity-revalidation") {
			return nil, errors.New("injected migration temporary path identity revalidation failure")
		}
	}
	for _, target := range []string{codexLeaseMigrationArchiveTarget, codexLeaseMigrationV2Target} {
		if directory.fsys.tempClosed[target] && !directory.fsys.renamed[target] && !directory.fsys.prechecked[target] && name == "leases.json" {
			directory.fsys.prechecked[target] = true
			directory.fsys.events = append(directory.fsys.events, target+":precondition")
			if directory.fsys.fail(target, "precondition") {
				return nil, errors.New("injected migration precondition failure")
			}
		}
	}
	target := codexLeaseMigrationCanonicalTarget(name)
	if target != "" && directory.fsys.renamed[target] {
		directory.fsys.openedAfterRename[target]++
		switch directory.fsys.openedAfterRename[target] {
		case 1:
			directory.fsys.installedChecked[target] = true
			directory.fsys.events = append(directory.fsys.events, target+":installed-validation")
			if directory.fsys.fail(target, "installed-validation") {
				return nil, errors.New("injected migration installed-target validation failure")
			}
		case 2:
			directory.fsys.postSyncChecked[target] = true
			directory.fsys.events = append(directory.fsys.events, target+":post-sync-installed-revalidation")
			if directory.fsys.fail(target, "post-sync-installed-revalidation") {
				return nil, errors.New("injected migration installed-target post-sync revalidation failure")
			}
		case 3:
			if directory.fsys.phase == "post-success-read" {
				directory.fsys.events = append(directory.fsys.events, target+":post-success-read")
			}
			if directory.fsys.fail(target, "post-success-read") {
				return nil, errors.New("injected migration post-success read failure")
			}
			if directory.fsys.phase == "post-success-mismatch" && directory.fsys.target == target {
				directory.fsys.events = append(directory.fsys.events, target+":post-success-mismatch")
				file, err := directory.SecureDirectory.OpenNoFollow(name)
				if err != nil {
					return nil, err
				}
				directory.fsys.injected = true
				return &codexLeaseMigrationCorruptReadFile{SecureReadFile: file}, nil
			}
		}
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}

func (directory *codexLeaseMigrationCrashDirectory) Rename(oldName, newName string) error {
	target := codexLeaseMigrationTempTarget(oldName)
	if target != "" {
		directory.fsys.events = append(directory.fsys.events, target+":rename")
		if directory.fsys.fail(target, "rename") {
			return errors.New("injected migration rename failure")
		}
	}
	if err := directory.SecureDirectory.Rename(oldName, newName); err != nil {
		return err
	}
	if target != "" {
		directory.fsys.renamed[target] = true
		directory.fsys.lastRenamed = target
	}
	return nil
}

func (directory *codexLeaseMigrationCrashDirectory) Sync() error {
	target := directory.fsys.lastRenamed
	if target != "" {
		directory.fsys.events = append(directory.fsys.events, target+":dir-sync")
		if directory.fsys.fail(target, "dir-sync") {
			return errors.New("injected migration directory sync failure")
		}
	}
	if err := directory.SecureDirectory.Sync(); err != nil {
		return err
	}
	if target != "" {
		directory.fsys.directorySynced[target] = true
	}
	return nil
}

type codexLeaseMigrationCrashFile struct {
	fsutil.DurableFile
	fsys   *codexLeaseMigrationCrashFS
	target string
}

func (file *codexLeaseMigrationCrashFile) Stat() (os.FileInfo, error) {
	file.fsys.events = append(file.fsys.events, file.target+":temp-descriptor-validation")
	inspector, ok := file.DurableFile.(fsutil.DurableFileInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Stat()
	if err != nil {
		return nil, err
	}
	if file.fsys.fail(file.target, "temp-descriptor-validation") {
		return codexLeaseMigrationUnsafeDescriptorInfo{FileInfo: info}, nil
	}
	return info, nil
}

type codexLeaseMigrationUnsafeDescriptorInfo struct {
	os.FileInfo
}

func (info codexLeaseMigrationUnsafeDescriptorInfo) Mode() os.FileMode {
	return 0o644
}

func (file *codexLeaseMigrationCrashFile) Write(data []byte) (int, error) {
	file.fsys.events = append(file.fsys.events, file.target+":write")
	if file.fsys.fail(file.target, "write") {
		partial := len(data) / 2
		if partial == 0 && len(data) != 0 {
			partial = 1
		}
		n, err := file.DurableFile.Write(data[:partial])
		if err != nil {
			return n, err
		}
		return n, errors.New("injected migration partial write failure")
	}
	return file.DurableFile.Write(data)
}

func (file *codexLeaseMigrationCrashFile) Sync() error {
	file.fsys.events = append(file.fsys.events, file.target+":sync")
	if file.fsys.fail(file.target, "sync") {
		return errors.New("injected migration file sync failure")
	}
	return file.DurableFile.Sync()
}

func (file *codexLeaseMigrationCrashFile) Close() error {
	file.fsys.events = append(file.fsys.events, file.target+":close")
	file.fsys.tempClosed[file.target] = true
	if file.fsys.fail(file.target, "close") {
		if err := file.DurableFile.Close(); err != nil {
			return fmt.Errorf("close before injected failure: %w", err)
		}
		return errors.New("injected migration close failure")
	}
	return file.DurableFile.Close()
}

func codexLeaseMigrationTempTarget(name string) string {
	if strings.Contains(name, ".archive.") && strings.HasSuffix(name, ".tmp") {
		return codexLeaseMigrationArchiveTarget
	}
	if strings.HasPrefix(name, ".leases.json.") && strings.HasSuffix(name, ".tmp") {
		return codexLeaseMigrationV2Target
	}
	return ""
}

func codexLeaseMigrationCanonicalTarget(name string) string {
	if strings.Contains(name, ".archive") {
		return codexLeaseMigrationArchiveTarget
	}
	if name == "leases.json" {
		return codexLeaseMigrationV2Target
	}
	return ""
}

func codexLeaseMigrationOrderThrough(phase string) []string {
	order := []string{
		"create",
		"write",
		"sync",
		"temp-descriptor-validation",
		"close",
		"pre-replace-directory-validation",
		"temp-path-identity-revalidation",
		"precondition",
		"rename",
		"installed-validation",
		"post-replace-directory-validation",
		"dir-sync",
		"post-sync-installed-revalidation",
		"final-directory-validation",
	}
	for index, candidate := range order {
		if candidate == phase {
			return order[:index+1]
		}
	}
	if phase == "post-success-read" || phase == "post-success-mismatch" {
		return append(order, phase)
	}
	return nil
}

type codexLeaseMigrationCorruptReadFile struct {
	fsutil.SecureReadFile
	corrupted bool
}

func (file *codexLeaseMigrationCorruptReadFile) Read(data []byte) (int, error) {
	n, err := file.SecureReadFile.Read(data)
	if n != 0 && !file.corrupted {
		data[0] ^= 0xff
		file.corrupted = true
	}
	return n, err
}
