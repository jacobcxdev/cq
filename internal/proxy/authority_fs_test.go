package proxy

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestAuthorityFSCreateWriteSyncPublishOrdering(t *testing.T) {
	filesystem, baseDirectory := newAuthorityFSTestDirectory(t)
	events := make([]string, 0, 12)
	directory := &recordingAuthorityDirectory{SecureDirectory: baseDirectory, events: &events}
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x09}, 32)))
	if _, err := publisher.PublishImmutable(context.Background(), directory, "ordered", []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertAuthorityEventOrder(t, events, "create", "write", "file_sync", "file_close", "rename_no_replace", "directory_sync", "open:ordered")
}

func TestAuthorityFSPartialTemporaryIsNotPublished(t *testing.T) {
	filesystem, baseDirectory := newAuthorityFSTestDirectory(t)
	events := make([]string, 0, 8)
	directory := &recordingAuthorityDirectory{SecureDirectory: baseDirectory, events: &events, failWrite: true}
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x0a}, 32)))
	if _, err := publisher.PublishImmutable(context.Background(), directory, "object", []byte("body"), 0o600); err == nil {
		t.Fatal("partial write succeeded")
	}
	if _, err := filesystem.Stat("/authority/object"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final exists after partial write: %v", err)
	}
	entries, err := baseDirectory.(fsutil.SecureDirectoryReader).ReadDir()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary residue = %v", entries)
	}
	assertAuthorityEventOrder(t, events, "create", "write", "file_close", "remove", "directory_sync")
}

func TestAuthorityFSPublishImmutableAndRejectCollision(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x11}, 64)))

	identity, err := publisher.PublishImmutable(context.Background(), directory, "object.json", []byte("first"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Digest == "" || identity.Size != 5 || identity.File.Links != 1 {
		t.Fatalf("identity = %#v", identity)
	}
	if _, err := publisher.PublishImmutable(context.Background(), directory, "object.json", []byte("second"), 0o600); err == nil {
		t.Fatal("no-replace collision succeeded")
	}
	got, err := filesystem.ReadFile("/authority/object.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("body = %q, want first", got)
	}
}

func TestAuthorityFSReplaceSelectorRequiresExactPrior(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x22}, 64)), lock)
	prior, err := publisher.PublishImmutable(context.Background(), directory, "anchor", []byte("one"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	wrong := prior
	wrong.Digest = "00" + wrong.Digest[2:]
	if _, err := publisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &wrong, []byte("two")); !errors.Is(err, ErrAuthorityPriorMismatch) {
		t.Fatalf("wrong prior error = %v", err)
	}
	if got, _ := filesystem.ReadFile("/authority/anchor"); string(got) != "one" {
		t.Fatalf("selector changed after rejected CAS: %q", got)
	}

	next, err := publisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &prior, []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Digest == prior.Digest {
		t.Fatal("selector identity did not change")
	}
}

func TestAuthorityFSReplaceSelectorRejectsMissingCASCapability(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x23}, 64)))
	prior, err := publisher.PublishImmutable(context.Background(), directory, "anchor", []byte("one"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &prior, []byte("two")); !errors.Is(err, ErrAuthorityCASCapability) {
		t.Fatalf("missing capability error = %v", err)
	}
}

func TestAuthorityFSSelectorCASCapabilityCannotBorrowForeignReacquisition(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x27}, 64)), lock)
	prior, err := publisher.PublishImmutable(context.Background(), directory, "anchor", []byte("one"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &prior, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	foreign, err := directory.OpenExclusiveLock("mutation.lock", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := publisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &prior, []byte("stale")); !errors.Is(err, ErrAuthorityCASCapability) {
		t.Fatalf("foreign-revived capability error = %v", err)
	}
}

func TestAuthorityFSSelectorCASCapabilityRejectsReplacedLockPathWhileHeld(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x28}, 64)), lock)
	prior, err := publisher.PublishImmutable(context.Background(), directory, "anchor", []byte("one"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Remove("mutation.lock"); err != nil {
		t.Fatal(err)
	}
	foreign, err := directory.OpenExclusiveLock("mutation.lock", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := publisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &prior, []byte("two")); !errors.Is(err, ErrAuthorityCASCapability) {
		t.Fatalf("replaced lock path error = %v", err)
	}
	if got, _ := filesystem.ReadFile("/authority/anchor"); string(got) != "one" {
		t.Fatalf("selector changed after lock path replacement: %q", got)
	}
}

func TestAuthorityFSSelectorCASCapabilityRevalidatesAfterTemporaryReopen(t *testing.T) {
	filesystem, baseDirectory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, baseDirectory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x29}, 64)), lock)
	prior, err := publisher.PublishImmutable(context.Background(), baseDirectory, "anchor", []byte("one"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	directory := &replaceLockAfterTemporaryReopenDirectory{SecureDirectory: baseDirectory, lockName: "mutation.lock"}
	defer directory.closeReplacement()
	if _, err := publisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &prior, []byte("two")); !errors.Is(err, ErrAuthorityCASCapability) {
		t.Fatalf("late lock replacement error = %v", err)
	}
	if !directory.replaced {
		t.Fatal("test hook did not replace the lock after temporary reopen")
	}
	if got, _ := filesystem.ReadFile("/authority/anchor"); string(got) != "one" {
		t.Fatalf("selector changed after late lock path replacement: %q", got)
	}
}

func TestAuthorityFSSelectorCASCapabilityHasOneGatePerDescription(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	clone := *lock
	if !clone.sharesDescription(lock) {
		t.Fatal("copied capability acquired an independent serialisation gate")
	}
	if _, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock"); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("duplicate capability error = %v", err)
	}
}

func TestAuthorityFSReplaceSelectorLinearisesStaleWriters(t *testing.T) {
	filesystem, baseDirectory := newAuthorityFSTestDirectory(t)
	guard := newObservedSelectorCASGuard()
	initial := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x24}, 32)), guard)
	prior, err := initial.PublishImmutable(context.Background(), baseDirectory, "anchor", []byte("one"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	renameReached := make(chan struct{})
	allowRename := make(chan struct{})
	directory := &blockingRenameDirectory{SecureDirectory: baseDirectory, reached: renameReached, allow: allowRename}
	firstPublisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x25}, 32)), guard)
	secondPublisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x26}, 32)), guard)
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, err := firstPublisher.ReplaceSelectorExactPrior(context.Background(), directory, "anchor", &prior, []byte("two"))
		firstResult <- err
	}()
	<-guard.attempted
	<-guard.entered
	<-renameReached
	go func() {
		_, err := secondPublisher.ReplaceSelectorExactPrior(context.Background(), baseDirectory, "anchor", &prior, []byte("stale"))
		secondResult <- err
	}()
	<-guard.attempted
	select {
	case <-guard.entered:
		t.Fatal("stale writer entered while first CAS remained active")
	default:
	}
	close(allowRename)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	<-guard.entered
	if err := <-secondResult; !errors.Is(err, ErrAuthorityPriorMismatch) {
		t.Fatalf("stale writer error = %v", err)
	}
	if got, _ := filesystem.ReadFile("/authority/anchor"); string(got) != "two" {
		t.Fatalf("selector = %q, want first successor", got)
	}
}

func TestAuthorityFSRejectsTraversalModeAndHardLinks(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x33}, 64)))
	for _, name := range []string{"../escape", "a/b", ".", ""} {
		if _, err := publisher.PublishImmutable(context.Background(), directory, name, []byte("x"), 0o600); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	if _, err := publisher.PublishImmutable(context.Background(), directory, "bad-mode", []byte("x"), 0o644); err == nil {
		t.Fatal("non-owner-only mode accepted")
	}
	wrongOwnerPublisher := NewAuthorityObjectPublisher(wrongOwnerInspector{SecurePathInspector: filesystem}, bytes.NewReader(bytes.Repeat([]byte{0x34}, 32)))
	if _, err := wrongOwnerPublisher.PublishImmutable(context.Background(), directory, "wrong-owner", []byte("x"), 0o600); err == nil {
		t.Fatal("foreign-owner authority directory accepted")
	}

	realRoot := t.TempDir()
	if err := os.Chmod(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(realRoot, "source")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(realRoot, "linked")); err != nil {
		t.Fatal(err)
	}
	realDirectory, err := (fsutil.OSFileSystem{}).OpenSecureDirectory(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer realDirectory.Close()
	realLock, err := AcquireSelectorCASLock(fsutil.OSFileSystem{}, realDirectory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer realLock.Close()
	realPublisher := NewAuthorityObjectPublisher(fsutil.OSFileSystem{}, bytes.NewReader(bytes.Repeat([]byte{0x44}, 32)), realLock)
	prior := StableObjectIdentity{File: fileIdentity(t, fsutil.OSFileSystem{}, source), Size: 1, Digest: mustAuthorityDigest(t, []byte("x"))}
	if _, err := realPublisher.ReplaceSelectorExactPrior(context.Background(), realDirectory, "linked", &prior, []byte("y")); err == nil {
		t.Fatal("hard-linked prior accepted")
	}
}

type observedSelectorCASGuard struct {
	mu        sync.Mutex
	attempted chan struct{}
	entered   chan struct{}
}

func (*observedSelectorCASGuard) selectorCASCapability() {}

func (*observedSelectorCASGuard) validateSelectorCAS(fsutil.SecureDirectory) error { return nil }

func newObservedSelectorCASGuard() *observedSelectorCASGuard {
	return &observedSelectorCASGuard{attempted: make(chan struct{}, 2), entered: make(chan struct{}, 2)}
}

func (guard *observedSelectorCASGuard) AcquireSelectorCAS(context.Context, fsutil.SecurePathInspector, fsutil.SecureDirectory) (func() error, error) {
	guard.attempted <- struct{}{}
	guard.mu.Lock()
	guard.entered <- struct{}{}
	return func() error { guard.mu.Unlock(); return nil }, nil
}

type blockingRenameDirectory struct {
	fsutil.SecureDirectory
	reached chan<- struct{}
	allow   <-chan struct{}
}

type replaceLockAfterTemporaryReopenDirectory struct {
	fsutil.SecureDirectory
	lockName    string
	once        sync.Once
	replaced    bool
	replacement fsutil.ExclusiveLock
}

func (directory *replaceLockAfterTemporaryReopenDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	opened, err := directory.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	var replaceErr error
	if strings.HasPrefix(name, ".anchor.") && strings.HasSuffix(name, ".tmp") {
		directory.once.Do(func() {
			if replaceErr = directory.SecureDirectory.Remove(directory.lockName); replaceErr != nil {
				return
			}
			directory.replacement, replaceErr = directory.SecureDirectory.OpenExclusiveLock(directory.lockName, 0o600)
			directory.replaced = replaceErr == nil
		})
	}
	if replaceErr != nil {
		_ = opened.Close()
		return nil, replaceErr
	}
	return opened, nil
}

func (directory *replaceLockAfterTemporaryReopenDirectory) closeReplacement() {
	if directory.replacement != nil {
		_ = directory.replacement.Close()
	}
}

func (directory *blockingRenameDirectory) Rename(oldName, newName string) error {
	directory.reached <- struct{}{}
	<-directory.allow
	return directory.SecureDirectory.Rename(oldName, newName)
}

type wrongOwnerInspector struct {
	fsutil.SecurePathInspector
}

func (wrongOwnerInspector) EffectiveUID() uint64 { return 999 }

func TestAuthorityFSCancelledBeforeCreate(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x55}, 32)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := publisher.PublishImmutable(ctx, directory, "object", []byte("body"), 0o600); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := filesystem.Stat("/authority/object"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("object exists after cancelled publish: %v", err)
	}
}

func newAuthorityFSTestDirectory(t *testing.T) (*fsutil.MemFS, fsutil.SecureDirectory) {
	t.Helper()
	filesystem := fsutil.NewMemFS()
	if err := filesystem.MkdirAll("/authority", 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := filesystem.OpenSecureDirectory("/authority")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	return filesystem, directory
}

func fileIdentity(t *testing.T, inspector fsutil.SecurePathInspector, path string) fsutil.SecureFileIdentity {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok {
		t.Fatal("file identity unavailable")
	}
	return identity
}

func mustAuthorityDigest(t *testing.T, body []byte) string {
	t.Helper()
	digest, err := FramedSHA256Hex(authorityObjectIdentityDomain, body)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

type recordingAuthorityDirectory struct {
	fsutil.SecureDirectory
	events    *[]string
	failWrite bool
}

func (directory *recordingAuthorityDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	*directory.events = append(*directory.events, "open:"+name)
	return directory.SecureDirectory.OpenNoFollow(name)
}

func (directory *recordingAuthorityDirectory) CreateExclusive(name string, mode fs.FileMode) (fsutil.DurableFile, error) {
	*directory.events = append(*directory.events, "create")
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	return &recordingAuthorityFile{DurableFile: file, events: directory.events, failWrite: directory.failWrite}, nil
}

func (directory *recordingAuthorityDirectory) Rename(oldName, newName string) error {
	*directory.events = append(*directory.events, "rename")
	return directory.SecureDirectory.Rename(oldName, newName)
}

func (directory *recordingAuthorityDirectory) RenameNoReplace(oldName, newName string) error {
	*directory.events = append(*directory.events, "rename_no_replace")
	return directory.SecureDirectory.RenameNoReplace(oldName, newName)
}

func (directory *recordingAuthorityDirectory) Remove(name string) error {
	*directory.events = append(*directory.events, "remove")
	return directory.SecureDirectory.Remove(name)
}

func (directory *recordingAuthorityDirectory) Sync() error {
	*directory.events = append(*directory.events, "directory_sync")
	return directory.SecureDirectory.Sync()
}

type recordingAuthorityFile struct {
	fsutil.DurableFile
	events    *[]string
	failWrite bool
}

func (file *recordingAuthorityFile) Write(body []byte) (int, error) {
	*file.events = append(*file.events, "write")
	if file.failWrite {
		if len(body) == 0 {
			return 0, errors.New("injected partial write")
		}
		written, err := file.DurableFile.Write(body[:1])
		if err != nil {
			return written, err
		}
		return written, errors.New("injected partial write")
	}
	return file.DurableFile.Write(body)
}

func (file *recordingAuthorityFile) Sync() error {
	*file.events = append(*file.events, "file_sync")
	return file.DurableFile.Sync()
}

func (file *recordingAuthorityFile) Close() error {
	*file.events = append(*file.events, "file_close")
	return file.DurableFile.Close()
}

func (file *recordingAuthorityFile) Stat() (os.FileInfo, error) {
	return file.DurableFile.(fsutil.DurableFileInspector).Stat()
}

func assertAuthorityEventOrder(t *testing.T, events []string, expected ...string) {
	t.Helper()
	next := 0
	for _, event := range events {
		if next < len(expected) && event == expected[next] {
			next++
		}
	}
	if next != len(expected) {
		t.Fatalf("events = %v, missing ordered suffix %v", events, expected[next:])
	}
}
