package proxy

import (
	"bytes"
	"context"
	"slices"
	"testing"
)

func TestInstanceAuthorityStagedActivation(t *testing.T) {
	filesystem, directory := newAuthorityFSTestDirectory(t)
	lock, err := AcquireSelectorCASLock(filesystem, directory, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	publisher := NewAuthorityObjectPublisher(filesystem, bytes.NewReader(bytes.Repeat([]byte{0x63}, 512)), lock)
	store, err := OpenInstanceAuthorityStore(context.Background(), filesystem, directory, publisher, bytes.Repeat([]byte{0x43}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateFeature("instance"); err == nil {
		t.Fatal("feature activated before controller initialisation")
	}
	if err := store.InitialiseController("instance"); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateFeature("instance"); err != nil {
		t.Fatal(err)
	}
	if err := store.Release("instance"); err == nil {
		t.Fatal("released before staged deactivation")
	}
	if err := store.StageRelease("instance"); err != nil {
		t.Fatal(err)
	}
	if err := store.Release("instance"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenInstanceAuthorityStore(context.Background(), filesystem, directory, publisher, bytes.Repeat([]byte{0x43}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Release("instance"); err == nil {
		t.Fatal("terminal release was not recovered")
	}
}

func TestInstanceAuthorityExternalReferenceRowsAreClosed(t *testing.T) {
	want := []string{
		"candidate_authority_terminal_v1",
		"candidate_removal_terminal_v1",
		"receipt_export_terminal_v1",
		"canonical_import_terminal_v1",
		"import_finalisation_terminal_v1",
		"promotion_terminal_v1",
		"release_history_terminal_v1",
		"runtime_stage_history_terminal_v1",
	}
	if got := InstanceAuthorityExternalReferenceKinds(); !slices.Equal(got, want) {
		t.Fatalf("external reference kinds = %v, want %v", got, want)
	}
	for _, kind := range want {
		if !ValidInstanceAuthorityExternalReferenceKind(kind) {
			t.Fatalf("rejected %q", kind)
		}
	}
	if ValidInstanceAuthorityExternalReferenceKind("generic_terminal_v1") {
		t.Fatal("accepted extension external reference kind")
	}
}
