package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
)

type durableFakeFS struct {
	*fakeFS
	modes    map[string]os.FileMode
	failStep string
}

func newDurableFakeFS() *durableFakeFS {
	return &durableFakeFS{fakeFS: newFakeFS(), modes: make(map[string]os.FileMode)}
}

func (f *durableFakeFS) WriteFile(name string, data []byte, mode os.FileMode) error {
	if f.failStep == "write" && bytes.Contains([]byte(name), []byte(".tmp_")) {
		return os.ErrPermission
	}
	if err := f.fakeFS.WriteFile(name, append([]byte(nil), data...), mode); err != nil {
		return err
	}
	f.modes[name] = mode
	return nil
}

func (f *durableFakeFS) Rename(oldpath, newpath string) error {
	if f.failStep == "rename" && bytes.Contains([]byte(oldpath), []byte(".tmp_")) {
		return os.ErrPermission
	}
	if err := f.fakeFS.Rename(oldpath, newpath); err != nil {
		return err
	}
	f.modes[newpath] = f.modes[oldpath]
	delete(f.modes, oldpath)
	return nil
}

func (f *durableFakeFS) Remove(name string) error {
	delete(f.modes, name)
	return f.fakeFS.Remove(name)
}

func (f *durableFakeFS) Chmod(name string, mode os.FileMode) error {
	if f.failStep == "chmod" {
		return os.ErrPermission
	}
	if _, ok := f.files[name]; !ok {
		return os.ErrNotExist
	}
	f.modes[name] = mode
	return nil
}

func (f *durableFakeFS) SyncFile(name string) error {
	if f.failStep == "file sync" {
		return os.ErrPermission
	}
	if _, ok := f.files[name]; !ok {
		return os.ErrNotExist
	}
	return nil
}

func (f *durableFakeFS) SyncDir(string) error {
	if f.failStep == "directory sync" {
		return os.ErrPermission
	}
	return nil
}

type durableFileInfo struct {
	fakeFileInfo
	mode os.FileMode
}

func (i durableFileInfo) Mode() os.FileMode { return i.mode }

func (f *durableFakeFS) Stat(name string) (os.FileInfo, error) {
	if _, ok := f.files[name]; !ok {
		return nil, os.ErrNotExist
	}
	return durableFileInfo{fakeFileInfo: fakeFileInfo{name: name}, mode: f.modes[name]}, nil
}

func testManagedStore(t *testing.T, fs *durableFakeFS) *ManagedStore {
	t.Helper()
	store, err := NewManagedStore(fs)
	if err != nil {
		t.Fatal(err)
	}
	randomData := make([]byte, 4096)
	for i := range randomData {
		randomData[i] = byte(i / 16)
	}
	store.Random = bytes.NewReader(randomData)
	store.EnsureEpoch = func() error { return nil }
	return store
}

func testLoginCredential() LoginCredential {
	return LoginCredential{
		Tokens:    auth.CodexTokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: "id"},
		Claims:    auth.CodexClaims{AccountID: "acct-1", UserID: "user-1", Email: "user@test.com"},
		CreatedAt: time.Unix(100, 0),
	}
}

func TestManagedStoreRoundTripsClosedMetadataAndUnknownFields(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatalf("SaveNew: %v", err)
	}
	record.Document["unknown"] = map[string]any{"keep": true}
	metadataDocument := record.Document["_cq"].(map[string]any)
	metadataDocument["future_field"] = "keep"
	expected := record.Metadata.Revision
	if err := store.Commit(&record, expected); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	loaded, err := store.Load(record.Path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata.Provenance != ProvenanceCQOAuth || loaded.Metadata.RefreshOwnership != RefreshCQOwnedNeverExported || loaded.Metadata.OperationState != OperationReady {
		t.Fatalf("metadata = %+v", loaded.Metadata)
	}
	if _, ok := loaded.Document["unknown"]; !ok {
		t.Fatal("unknown field was lost")
	}
	if got := loaded.Document["_cq"].(map[string]any)["future_field"]; got != "keep" {
		t.Fatalf("unknown metadata field = %#v", got)
	}
	if loaded.Metadata.Generation != 2 {
		t.Fatalf("generation = %d, want 2", loaded.Metadata.Generation)
	}
}

func TestManagedStoreCrashSteps(t *testing.T) {
	for _, step := range []string{"write", "chmod", "file sync", "rename", "directory sync"} {
		t.Run(step, func(t *testing.T) {
			fs := newDurableFakeFS()
			store := testManagedStore(t, fs)
			fs.failStep = step
			_, err := store.SaveNew(testLoginCredential())
			if err == nil {
				t.Fatal("SaveNew error = nil")
			}
			var commitErr *CommitError
			if !errors.As(err, &commitErr) || commitErr.Step != step {
				t.Fatalf("error = %v, want CommitError step %q", err, step)
			}
			if step == "directory sync" && !commitErr.Committed {
				t.Fatal("directory sync failure must report committed-but-uncertain")
			}
		})
	}
}

func TestManagedStoreRejectsStaleRevision(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(&record, Revision("stale")); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Commit error = %v, want ErrStaleRevision", err)
	}
}

func TestManagedStoreRejectsBadPermissions(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	fs.modes[record.Path] = 0o644
	if _, err := store.Load(record.Path); err == nil {
		t.Fatal("Load error = nil, want permission rejection")
	}
}

func TestManagedStoreUnknownEnumSuspendsRefresh(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	record, err := store.SaveNew(testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(fs.files[record.Path], &doc)
	metadataData, _ := json.Marshal(doc["_cq"])
	var metadata ManagedMetadata
	_ = json.Unmarshal(metadataData, &metadata)
	metadata.OperationState = OperationState("future_state")
	metadata.Revision = managedRecordRevision(doc, record.Credential, metadata)
	doc["_cq"] = metadata
	data, _ := json.Marshal(doc)
	fs.files[record.Path] = data
	loaded, err := store.Load(record.Path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.RefreshSuspended {
		t.Fatal("unknown enum did not suspend refresh")
	}
}

func TestManagedStoreRejectsCorruptMetadata(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	path := "/fake/home/.codex/accounts/corrupt.auth.json"
	fs.files[path] = []byte(`{"tokens":{"access_token":"synthetic"},"_cq":"broken"}`)
	fs.modes[path] = 0o600
	if _, err := store.Load(path); err == nil {
		t.Fatal("Load error = nil, want corrupt metadata rejection")
	}
}

func TestManagedStoreLoadsLegacyWithoutRewrite(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	path := "/fake/home/.codex/accounts/legacy.auth.json"
	before := []byte(`{"tokens":{"access_token":"synthetic"},"unknown":true}`)
	fs.files[path] = before
	fs.modes[path] = 0o600
	loaded, err := store.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.RefreshSuspended || loaded.Metadata.Provenance != ProvenanceLegacyUnknown {
		t.Fatalf("loaded = %+v", loaded)
	}
	if got := string(fs.files[path]); got != string(before) {
		t.Fatal("legacy load rewrote file")
	}
}

func TestManagedStoreAdvancesCompatibilityEpochBeforeCommit(t *testing.T) {
	fs := newDurableFakeFS()
	store, err := NewManagedStore(fs)
	if err != nil {
		t.Fatal(err)
	}
	randomData := make([]byte, 4096)
	for i := range randomData {
		randomData[i] = byte(i / 16)
	}
	store.Random = bytes.NewReader(randomData)
	if _, err := store.SaveNew(testLoginCredential()); err != nil {
		t.Fatal(err)
	}
	data := fs.files["/fake/home/.config/cq/state/compatibility_epoch"]
	if string(data) != "2\n" {
		t.Fatalf("compatibility epoch = %q, want 2", data)
	}
}
