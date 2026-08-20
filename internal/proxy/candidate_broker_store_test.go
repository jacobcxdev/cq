package proxy

import (
	"bytes"
	"context"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCandidateBrokerStoreRequiresDurableSourceBeforeJournal(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsutil.EnsureSecureDirectory(fsys, "/broker"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/broker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	store, err := OpenCandidateBrokerStore(context.Background(), fsys, directory, NewAuthorityObjectPublisher(fsys, bytes.NewReader(bytes.Repeat([]byte{0x45}, 4096))), bytes.Repeat([]byte{0x46}, 32), CandidateBrokerCaps{Runs: 3, RecordsPerRun: 16})
	if err != nil {
		t.Fatal(err)
	}
	record := CandidateBrokerRecordV1{SchemaVersion: 1, RunID: "run-a", SourceDigest: "missing", Kind: "synthetic_result", PayloadDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if _, err := store.AppendJournal(context.Background(), record); err == nil {
		t.Fatal("journal accepted before source")
	}
	source := CandidateValidationSourceV1{SchemaVersion: 1, RunID: "run-a", Kind: CandidateSourceIngress, CatalogueDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	identity, err := store.PublishSource(context.Background(), source)
	if err != nil {
		t.Fatalf("PublishSource: %v", err)
	}
	dependent := CandidateValidationSourceV1{SchemaVersion: 1, RunID: "run-a", Kind: CandidateSourceMaterialised, Dependencies: []string{identity.Digest}, CatalogueDigest: source.CatalogueDigest}
	if _, err := store.PublishSource(context.Background(), dependent); err != nil {
		t.Fatalf("PublishSource(dependent): %v", err)
	}
	record.SourceDigest = identity.Digest
	position, err := store.AppendJournal(context.Background(), record)
	if err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	seal := CandidateBrokerJournalSealV1{SchemaVersion: 1, RunID: "run-a", Count: 1, HeadDigest: position.Digest}
	if err := store.VerifySealedRun(context.Background(), seal); err != nil {
		t.Fatalf("VerifySealedRun: %v", err)
	}

	reopened, err := OpenCandidateBrokerStore(context.Background(), fsys, directory, store.publisher, store.key[:], store.caps)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := reopened.VerifySealedRun(context.Background(), seal); err != nil {
		t.Fatalf("reopened VerifySealedRun: %v", err)
	}
}
