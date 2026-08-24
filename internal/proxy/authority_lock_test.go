package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestLifecycleHolderProofJSONExcludesInMemoryFileID(t *testing.T) {
	proof := LifecycleHolderProof{LockIdentity: fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 1}, DescriptionID: "description", Mode: LifecycleShared}
	withoutFileID, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	proof.LockIdentity.FileID[15] = 9
	withFileID, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(withFileID, withoutFileID) {
		t.Fatalf("FileID changed holder proof JSON: %s != %s", withFileID, withoutFileID)
	}
}

func TestAuthorityLockDowngradesSameDescriptionAndReleases(t *testing.T) {
	backend := &fakeLifecycleBackend{next: 1, identity: fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 1}}
	order := NewAuthorityLockOrder()
	_, directory := newAuthorityFSTestDirectory(t)
	handle, err := AcquireLifecycle(context.Background(), backend, directory, "instance", LifecycleExclusive, order)
	if err != nil {
		t.Fatal(err)
	}
	before := handle.HolderProof()
	if err := handle.DowngradeToShared(); err != nil {
		t.Fatal(err)
	}
	after := handle.HolderProof()
	if before.DescriptionID != after.DescriptionID || after.Mode != LifecycleShared {
		t.Fatalf("downgrade changed description: before=%#v after=%#v", before, after)
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}
	if !backend.descriptions[0].closed {
		t.Fatal("release did not close retained description")
	}
}

func TestAuthorityLockReleaseRequiresStrictLIFO(t *testing.T) {
	backend := &fakeLifecycleBackend{next: 1, identity: fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 1}}
	order := NewAuthorityLockOrder()
	_, directory := newAuthorityFSTestDirectory(t)
	handle, err := AcquireLifecycle(context.Background(), backend, directory, "instance", LifecycleExclusive, order)
	if err != nil {
		t.Fatal(err)
	}
	releaseInner, err := order.Acquire(AuthorityLockMutation)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(); !errors.Is(err, ErrAuthorityLockOrder) {
		t.Fatalf("out-of-order release error = %v", err)
	}
	if backend.descriptions[0].closed || handle.HolderProof().DescriptionID == "" {
		t.Fatal("out-of-order release dropped lifecycle exclusion")
	}
	if err := releaseInner(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}
	if !backend.descriptions[0].closed {
		t.Fatal("ordered recovery did not close lifecycle description")
	}
}

func TestAuthorityLockRequiresDistinctHolderDescriptions(t *testing.T) {
	identity := fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 1}
	first := LifecycleHolderProof{LockIdentity: identity, DescriptionID: "one", Mode: LifecycleShared}
	second := LifecycleHolderProof{LockIdentity: identity, DescriptionID: "two", Mode: LifecycleShared}
	if err := ValidateDistinctLifecycleHolders(first, second); err != nil {
		t.Fatal(err)
	}
	second.DescriptionID = first.DescriptionID
	if err := ValidateDistinctLifecycleHolders(first, second); !errors.Is(err, ErrLifecycleHolderConflict) {
		t.Fatalf("duplicate holder error = %v", err)
	}
}

func TestAuthorityLockRejectsOrderInversion(t *testing.T) {
	order := NewAuthorityLockOrder()
	releaseMutation, err := order.Acquire(AuthorityLockMutation)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = releaseMutation() }()
	if _, err := order.Acquire(AuthorityLockLifecycle); !errors.Is(err, ErrAuthorityLockOrder) {
		t.Fatalf("inversion error = %v", err)
	}
}

func TestAuthorityLockProductionBackendInitialisesFixedLifecycleFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (fsutil.OSFileSystem{}).OpenSecureDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := NewProductionLifecycleBackend(fsutil.OSFileSystem{}, bytes.NewReader(bytes.Repeat([]byte{0x41}, 64)))
	handle, err := InitialiseLifecycle(context.Background(), backend, directory, "candidate", NewAuthorityLockOrder())
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(root, ".cq-instance-candidate.lifecycle.lock")
	info, err := os.Lstat(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lifecycle file mode = %v", info.Mode())
	}
	before := handle.HolderProof()
	if err := handle.DowngradeToShared(); err != nil {
		t.Fatal(err)
	}
	after := handle.HolderProof()
	if before.DescriptionID != after.DescriptionID || before.LockIdentity != after.LockIdentity || after.Mode != LifecycleShared {
		t.Fatalf("production downgrade changed description: before=%#v after=%#v", before, after)
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityLockProductionAcquireNeverCreatesMissingLifecycleFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := (fsutil.OSFileSystem{}).OpenSecureDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := NewProductionLifecycleBackend(fsutil.OSFileSystem{}, bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)))
	for _, mode := range []LifecycleLockMode{LifecycleShared, LifecycleExclusive} {
		basename := string(mode)
		if _, err := AcquireLifecycle(context.Background(), backend, directory, basename, mode, NewAuthorityLockOrder()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("acquire missing %s error = %v", mode, err)
		}
		if _, err := os.Lstat(filepath.Join(root, ".cq-instance-"+basename+".lifecycle.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ordinary %s acquisition created lifecycle file: %v", mode, err)
		}
	}

	handle, err := InitialiseLifecycle(context.Background(), backend, directory, "existing", NewAuthorityLockOrder())
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := InitialiseLifecycle(context.Background(), backend, directory, "existing", NewAuthorityLockOrder()); !errors.Is(err, os.ErrExist) {
		t.Fatalf("repeat initialisation error = %v", err)
	}
	existing, err := AcquireLifecycle(context.Background(), backend, directory, "existing", LifecycleShared, NewAuthorityLockOrder())
	if err != nil {
		t.Fatal(err)
	}
	if err := existing.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityLockDerivesNameAndRejectsPathGrammar(t *testing.T) {
	backend := &fakeLifecycleBackend{next: 1, identity: fsutil.SecureFileIdentity{Device: 1, Inode: 2, Links: 1}}
	_, directory := newAuthorityFSTestDirectory(t)
	if _, err := AcquireLifecycle(context.Background(), backend, directory, "../foreign.lock", LifecycleExclusive, NewAuthorityLockOrder()); !errors.Is(err, ErrAuthorityPathGrammar) {
		t.Fatalf("arbitrary lock path error = %v", err)
	}
	if len(backend.names) != 0 {
		t.Fatalf("backend opened arbitrary names: %v", backend.names)
	}
	if handle, err := AcquireLifecycle(context.Background(), backend, directory, "instance", LifecycleShared, NewAuthorityLockOrder()); err != nil {
		t.Fatal(err)
	} else {
		defer handle.Release()
	}
	if got := backend.names[0]; got != ".cq-instance-instance.lifecycle.lock" {
		t.Fatalf("backend name = %q", got)
	}
}

type fakeLifecycleBackend struct {
	next         int
	identity     fsutil.SecureFileIdentity
	descriptions []*fakeLifecycleDescription
	names        []string
}

func (backend *fakeLifecycleBackend) AcquireLifecycleDescription(_ context.Context, _ fsutil.SecureDirectory, name string, mode LifecycleLockMode) (LifecycleLockDescription, error) {
	return backend.description(name, mode)
}

func (backend *fakeLifecycleBackend) CreateLifecycleDescription(_ context.Context, _ fsutil.SecureDirectory, name string) (LifecycleLockDescription, error) {
	return backend.description(name, LifecycleExclusive)
}

func (backend *fakeLifecycleBackend) description(name string, mode LifecycleLockMode) (LifecycleLockDescription, error) {
	backend.names = append(backend.names, name)
	description := &fakeLifecycleDescription{id: string(rune('0' + backend.next)), identity: backend.identity, mode: mode}
	backend.next++
	backend.descriptions = append(backend.descriptions, description)
	return description, nil
}

type fakeLifecycleDescription struct {
	id       string
	identity fsutil.SecureFileIdentity
	mode     LifecycleLockMode
	closed   bool
}

func (description *fakeLifecycleDescription) Identity() fsutil.SecureFileIdentity {
	return description.identity
}
func (description *fakeLifecycleDescription) DescriptionID() string   { return description.id }
func (description *fakeLifecycleDescription) Mode() LifecycleLockMode { return description.mode }
func (description *fakeLifecycleDescription) DowngradeToShared() error {
	if description.mode != LifecycleExclusive {
		return ErrLifecycleLockMode
	}
	description.mode = LifecycleShared
	return nil
}
func (description *fakeLifecycleDescription) Close() error { description.closed = true; return nil }
