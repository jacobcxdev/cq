package codex

import (
	"path/filepath"
	"testing"
)

func TestCQStatePathsDoNotUseProviderHome(t *testing.T) {
	journal := RemovalJournal{StateDir: "/cq/state"}
	if got := journal.path(); got != filepath.Join("/cq/state", "codex_removal.json") {
		t.Fatalf("journal = %q", got)
	}
	if got := DefaultCredentialControlPath("/cq/state"); got != filepath.Join("/cq/state", "credential.sock") {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestRemovalJournalRoundTripAndClear(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	journal := RemovalJournal{FS: fs, StateDir: testCQStateDir(), Store: store}
	plan := RemovalPlan{
		Version: 1, OperationID: "op-1", AccountKey: "acct-opaque",
		Candidates:             []RemovalCandidate{{CandidateID: "cand-1", Revision: "rev-1"}},
		ExpectedSystemRevision: "system-rev", Force: true,
	}
	if err := journal.Save(plan); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, ok, err := journal.Load()
	if err != nil || !ok {
		t.Fatalf("Load = %+v, %v, %v", loaded, ok, err)
	}
	if loaded.OperationID != plan.OperationID || loaded.Candidates[0] != plan.Candidates[0] {
		t.Fatalf("loaded = %+v, want %+v", loaded, plan)
	}
	if err := journal.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok, err := journal.Load(); err != nil || ok {
		t.Fatalf("Load after clear = ok %v, err %v", ok, err)
	}
}

func TestRemovalJournalDirectorySyncFailureRemainsRecoverable(t *testing.T) {
	fs := newDurableFakeFS()
	store := testManagedStore(t, fs)
	journal := RemovalJournal{FS: fs, StateDir: testCQStateDir(), Store: store}
	fs.failStep = "directory sync"
	err := journal.Save(RemovalPlan{Version: 1, OperationID: "op", AccountKey: "acct"})
	if err == nil {
		t.Fatal("Save error = nil")
	}
	fs.failStep = ""
	if _, ok, loadErr := journal.Load(); loadErr != nil || !ok {
		t.Fatalf("committed journal not recoverable: ok %v, err %v", ok, loadErr)
	}
}
