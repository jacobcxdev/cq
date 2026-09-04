package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestInitialiseCodexContinuityAuthorityRestartMatrix(t *testing.T) {
	t.Parallel()
	seedFS := fsutil.NewMemFS()
	seedOptions := testCodexContinuityOptions(seedFS)
	if err := InitialiseCodexContinuityAuthority(seedOptions, testCodexLeaseOwner{}); err != nil {
		t.Fatal(err)
	}
	key, err := seedFS.ReadFile(seedOptions.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := seedFS.ReadFile(seedOptions.JournalPath)
	if err != nil {
		t.Fatal(err)
	}

	const (
		canonicalKey     = 1 << 3
		canonicalJournal = 1 << 2
		stagedKey        = 1 << 1
		stagedJournal    = 1 << 0
	)
	accepted := map[int]bool{
		0:                               true,
		stagedKey:                       true,
		stagedKey | stagedJournal:       true,
		canonicalKey | stagedJournal:    true,
		canonicalKey | canonicalJournal: true,
	}
	for shape := 0; shape < 16; shape++ {
		shape := shape
		t.Run(freshCodexLeaseShapeName(shape), func(t *testing.T) {
			t.Parallel()
			fsys := fsutil.NewMemFS()
			options := testCodexContinuityOptions(fsys)
			paths := []string{
				options.KeyPath,
				options.JournalPath,
				freshCodexLeaseStagePath(options.KeyPath),
				freshCodexLeaseStagePath(options.JournalPath),
			}
			values := [][]byte{key, journal, key, journal}
			bits := []int{canonicalKey, canonicalJournal, stagedKey, stagedJournal}
			if shape != 0 {
				if err := fsys.MkdirAll("/state", 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for index, path := range paths {
				if shape&bits[index] != 0 {
					if err := fsys.WriteFile(path, values[index], 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := freshCodexLeaseTestSnapshot(t, fsys, paths)

			err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{})
			if !accepted[shape] {
				if !errors.Is(err, ErrCodexLeaseTrustLost) {
					t.Fatalf("error = %T %v, want trust lost", err, err)
				}
				after := freshCodexLeaseTestSnapshot(t, fsys, paths)
				freshCodexLeaseAssertSnapshotEqual(t, before, after)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range paths[:2] {
				if _, err := fsys.ReadFile(path); err != nil {
					t.Fatalf("canonical authority %s: %v", path, err)
				}
			}
			for _, path := range paths[2:] {
				if _, err := fsys.ReadFile(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stage %s remains: %v", path, err)
				}
			}
			coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
			if err != nil {
				t.Fatal(err)
			}
			if err := coordinator.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInitialiseCodexContinuityAuthorityRequiresOwnerBeforeMutation(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	ownerErr := errors.New("owner unavailable")
	err := InitialiseCodexContinuityAuthority(testCodexContinuityOptions(fsys), testCodexLeaseOwner{err: ownerErr})
	if !errors.Is(err, ErrCodexLeaseWriterUnavailable) || !errors.Is(err, ownerErr) {
		t.Fatalf("error = %T %v, want writer unavailable and owner cause", err, err)
	}
	if _, err := fsys.Stat("/state"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initializer mutated state before owner authority: %v", err)
	}
}

func TestInitialiseCodexContinuityAuthorityRejectsMalformedStageWithoutMutation(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	options := testCodexContinuityOptions(fsys)
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	stageKey := freshCodexLeaseStagePath(options.KeyPath)
	if err := fsys.WriteFile(stageKey, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{})
	if !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("error = %T %v, want trust lost", err, err)
	}
	got, readErr := fsys.ReadFile(stageKey)
	if readErr != nil || string(got) != "not-a-key" {
		t.Fatalf("malformed stage changed: length=%d error=%v", len(got), readErr)
	}
	for _, path := range []string{options.KeyPath, options.JournalPath, freshCodexLeaseStagePath(options.JournalPath)} {
		if _, err := fsys.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected stage created %s: %v", path, err)
		}
	}
}

func TestInitialiseCodexContinuityAuthorityRecoversEveryPublishBoundary(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"stage-key", "stage-journal", "key", "journal"} {
		for _, failure := range []string{"rename", "sync"} {
			target, failure := target, failure
			t.Run(target+"/"+failure, func(t *testing.T) {
				t.Parallel()
				mem := fsutil.NewMemFS()
				fsys := &freshCodexLeaseFaultFS{MemFS: mem, target: target, failure: failure}
				options := testCodexContinuityOptions(fsys)
				firstAt := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
				options.Policy.Now = func() time.Time { return firstAt }
				err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{})
				if err == nil || !fsys.fired {
					t.Fatalf("injected %s/%s error = %v fired=%t", target, failure, err, fsys.fired)
				}
				wantOutcome := fsutil.CommitNotCommitted
				if failure == "sync" {
					wantOutcome = fsutil.CommitIndeterminate
				}
				if got := fsutil.AtomicWriteOutcome(err); got != wantOutcome {
					t.Fatalf("outcome = %v, want %v (error %v)", got, wantOutcome, err)
				}

				retainedKey := freshCodexLeaseTestFirstExisting(t, mem, options.KeyPath, freshCodexLeaseStagePath(options.KeyPath))
				retainedJournal := freshCodexLeaseTestFirstExisting(t, mem, options.JournalPath, freshCodexLeaseStagePath(options.JournalPath))
				retryOptions := testCodexContinuityOptions(mem)
				retryOptions.Policy.Now = func() time.Time { return firstAt.Add(24 * time.Hour) }
				if err := InitialiseCodexContinuityAuthority(retryOptions, testCodexLeaseOwner{}); err != nil {
					t.Fatal(err)
				}
				installedKey, err := mem.ReadFile(options.KeyPath)
				if err != nil {
					t.Fatal(err)
				}
				installedJournal, err := mem.ReadFile(options.JournalPath)
				if err != nil {
					t.Fatal(err)
				}
				if retainedKey != nil && !bytes.Equal(installedKey, retainedKey) {
					t.Fatal("recovery regenerated retained key authority")
				}
				if retainedJournal != nil && !bytes.Equal(installedJournal, retainedJournal) {
					t.Fatal("recovery regenerated retained journal authority")
				}
				var envelope codexLeaseJournalEnvelopeV2
				if err := decodeCodexLeaseV2StrictJSON(installedJournal, &envelope); err != nil {
					t.Fatal(err)
				}
				if retainedJournal != nil && envelope.Cutover.At != firstAt {
					t.Fatalf("retained genesis time = %s, want %s", envelope.Cutover.At, firstAt)
				}
			})
		}
	}
}

func TestInitialiseCodexContinuityAuthorityUsesPrivateModesAndNoIdentifiers(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	options := testCodexContinuityOptions(fsys)
	if err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{}); err != nil {
		t.Fatal(err)
	}
	for path, wantMode := range map[string]os.FileMode{
		filepath.Dir(options.KeyPath): 0o700,
		options.KeyPath:               0o600,
		options.JournalPath:           0o600,
	} {
		info, err := fsys.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("mode for %s = %#o, want %#o", filepath.Base(path), got, wantMode)
		}
	}
	journal, err := fsys.ReadFile(options.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"account@example.invalid", "raw-session-id", "raw-thread-id", "raw-turn-id"} {
		if bytes.Contains(journal, []byte(raw)) {
			t.Fatalf("fresh journal contains raw identifier marker of length %d", len(raw))
		}
	}
}

func TestInitialiseCodexContinuityAuthorityResyncsCleanPairBeforeSuccess(t *testing.T) {
	t.Parallel()
	mem := fsutil.NewMemFS()
	options := testCodexContinuityOptions(mem)
	if err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{}); err != nil {
		t.Fatal(err)
	}
	keyBefore, err := mem.ReadFile(options.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	journalBefore, err := mem.ReadFile(options.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	failing := &freshCodexLeaseFaultFS{MemFS: mem, target: "clean", failure: "sync"}
	failingOptions := testCodexContinuityOptions(failing)
	err = InitialiseCodexContinuityAuthority(failingOptions, testCodexLeaseOwner{})
	if !failing.fired || !errors.Is(err, ErrCodexLeaseTrustLost) || !errors.Is(err, fsutil.ErrCommitIndeterminate) {
		t.Fatalf("clean-pair sync error = %T %v fired=%t", err, err, failing.fired)
	}
	keyAfter, keyErr := mem.ReadFile(options.KeyPath)
	journalAfter, journalErr := mem.ReadFile(options.JournalPath)
	if keyErr != nil || journalErr != nil || !bytes.Equal(keyBefore, keyAfter) || !bytes.Equal(journalBefore, journalAfter) {
		t.Fatalf("failed clean-pair sync changed bytes: key=%v journal=%v", keyErr, journalErr)
	}
	if err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{}); err != nil {
		t.Fatalf("clean-pair resync retry: %v", err)
	}
}

func TestInitialiseCodexContinuityAuthorityRequiresExactGenesis(t *testing.T) {
	t.Parallel()
	seedFS := fsutil.NewMemFS()
	options := testCodexContinuityOptions(seedFS)
	if err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{}); err != nil {
		t.Fatal(err)
	}
	key, err := seedFS.ReadFile(options.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := seedFS.ReadFile(options.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	var generationTwo codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(journal, &generationTwo); err != nil {
		t.Fatal(err)
	}
	generationTwo.Generation = 2
	store := &CodexLeaseStore{key: append([]byte(nil), key...)}
	generationTwoBytes, err := store.marshalV2Envelope(generationTwo)
	clear(store.key)
	if err != nil {
		t.Fatal(err)
	}
	badMAC := append([]byte(nil), journal...)
	macIndex := bytes.Index(badMAC, []byte(`"mac":"`))
	if macIndex < 0 {
		t.Fatal("fresh journal MAC member missing")
	}
	macIndex += len(`"mac":"`)
	if badMAC[macIndex] == 'A' {
		badMAC[macIndex] = 'B'
	} else {
		badMAC[macIndex] = 'A'
	}

	for name, candidate := range map[string][]byte{
		"bad_mac":        badMAC,
		"generation_two": generationTwoBytes,
		"trailing_space": append(append([]byte(nil), journal...), '\n'),
	} {
		name, candidate := name, candidate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fsys := fsutil.NewMemFS()
			testOptions := testCodexContinuityOptions(fsys)
			if err := fsys.MkdirAll("/state", 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile(freshCodexLeaseStagePath(testOptions.KeyPath), key, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile(freshCodexLeaseStagePath(testOptions.JournalPath), candidate, 0o600); err != nil {
				t.Fatal(err)
			}
			err := InitialiseCodexContinuityAuthority(testOptions, testCodexLeaseOwner{})
			if !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("error = %T %v, want trust lost", err, err)
			}
			got, readErr := fsys.ReadFile(freshCodexLeaseStagePath(testOptions.JournalPath))
			if readErr != nil || !bytes.Equal(got, candidate) {
				t.Fatalf("rejected stage changed: length=%d error=%v", len(got), readErr)
			}
			for _, path := range []string{testOptions.KeyPath, testOptions.JournalPath} {
				if _, err := fsys.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected genesis published %s: %v", filepath.Base(path), err)
				}
			}
		})
	}
}

func TestInitialiseCodexContinuityAuthorityRejectsUnsafeStageMode(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	options := testCodexContinuityOptions(fsys)
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	stageKey := freshCodexLeaseStagePath(options.KeyPath)
	if err := fsys.WriteFile(stageKey, make([]byte, codexLeaseHMACKeyBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{})
	if !errors.Is(err, ErrCodexLeaseTrustLost) || !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("error = %T %v, want unsafe trust lost", err, err)
	}
	info, err := fsys.Stat(stageKey)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("rejected stage mode = %#o, want 0644", info.Mode().Perm())
	}
}

func TestInitialiseCodexContinuityAuthorityRejectsStageNameCollisionBeforeOwner(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	owner := &countingCodexLeaseTestOwner{}
	options := testCodexContinuityOptions(fsys)
	options.JournalPath = freshCodexLeaseStagePath(options.KeyPath)
	err := InitialiseCodexContinuityAuthority(options, owner)
	if !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("error = %T %v, want trust lost", err, err)
	}
	if owner.asserts != 0 || owner.begins != 0 {
		t.Fatalf("colliding names consulted owner: asserts=%d begins=%d", owner.asserts, owner.begins)
	}
	if _, err := fsys.Stat("/state"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("colliding names mutated state: %v", err)
	}
}

type freshCodexLeaseTestFile struct {
	exists bool
	data   []byte
}

func freshCodexLeaseTestSnapshot(t *testing.T, fsys *fsutil.MemFS, paths []string) []freshCodexLeaseTestFile {
	t.Helper()
	result := make([]freshCodexLeaseTestFile, len(paths))
	for index, path := range paths {
		data, err := fsys.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result[index] = freshCodexLeaseTestFile{exists: true, data: data}
	}
	return result
}

func freshCodexLeaseAssertSnapshotEqual(t *testing.T, before, after []freshCodexLeaseTestFile) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("snapshot lengths = %d/%d", len(before), len(after))
	}
	for index := range before {
		if before[index].exists != after[index].exists || !bytes.Equal(before[index].data, after[index].data) {
			t.Fatalf("authority entry %d changed after rejected shape", index)
		}
	}
}

func freshCodexLeaseShapeName(shape int) string {
	name := []byte("KJkj")
	for index, bit := range []int{1 << 3, 1 << 2, 1 << 1, 1} {
		if shape&bit == 0 {
			name[index] = '0'
		}
	}
	return string(name)
}

func freshCodexLeaseTestFirstExisting(t *testing.T, fsys *fsutil.MemFS, paths ...string) []byte {
	t.Helper()
	for _, path := range paths {
		data, err := fsys.ReadFile(path)
		if err == nil {
			return data
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return nil
}

type freshCodexLeaseFaultFS struct {
	*fsutil.MemFS
	target     string
	failure    string
	fired      bool
	lastTarget string
}

func (fsys *freshCodexLeaseFaultFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &freshCodexLeaseFaultDirectory{SecureDirectory: directory, fsys: fsys}, nil
}

type freshCodexLeaseFaultDirectory struct {
	fsutil.SecureDirectory
	fsys *freshCodexLeaseFaultFS
}

func (directory *freshCodexLeaseFaultDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *freshCodexLeaseFaultDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	target := freshCodexLeaseRenameTarget(newName)
	if !directory.fsys.fired && directory.fsys.target == target && directory.fsys.failure == "rename" {
		directory.fsys.fired = true
		return fmt.Errorf("injected fresh authority rename failure")
	}
	if err := directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected); err != nil {
		return err
	}
	directory.fsys.lastTarget = target
	return nil
}

func (directory *freshCodexLeaseFaultDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *freshCodexLeaseFaultDirectory) RenameNoReplace(oldName, newName string) error {
	target := freshCodexLeaseRenameTarget(newName)
	if !directory.fsys.fired && directory.fsys.target == target && directory.fsys.failure == "rename" {
		directory.fsys.fired = true
		return fmt.Errorf("injected fresh authority rename failure")
	}
	if err := directory.SecureDirectory.RenameNoReplace(oldName, newName); err != nil {
		return err
	}
	directory.fsys.lastTarget = target
	return nil
}

func (directory *freshCodexLeaseFaultDirectory) Sync() error {
	target := directory.fsys.lastTarget
	directory.fsys.lastTarget = ""
	if !directory.fsys.fired && directory.fsys.failure == "sync" && (directory.fsys.target == target || directory.fsys.target == "clean" && target == "") {
		directory.fsys.fired = true
		return fmt.Errorf("injected fresh authority directory sync failure")
	}
	return directory.SecureDirectory.Sync()
}

func freshCodexLeaseRenameTarget(name string) string {
	switch {
	case strings.HasSuffix(name, ".key"+freshCodexLeaseStageSuffix):
		return "stage-key"
	case strings.HasSuffix(name, ".json"+freshCodexLeaseStageSuffix):
		return "stage-journal"
	case strings.HasSuffix(name, ".key"):
		return "key"
	case strings.HasSuffix(name, ".json"):
		return "journal"
	default:
		return ""
	}
}
