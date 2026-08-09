package proxy

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func testPrimerTarget() CodexPrimerTarget {
	return CodexPrimerTarget{
		ResetAt: time.Unix(1786834644, 0), ModelID: "gpt-5.3-codex-spark",
		Windows: []codex.WindowDescriptor{{
			RawLimitName: "backend-scope-secret", WindowName: quota.Window7Day,
			Period: 7 * 24 * time.Hour, ScopeKind: codex.WindowScopeModelFamily,
			Scope: "backend-scope-secret", ResetAt: time.Unix(1786834644, 0),
		}},
	}
}

func TestCodexPrimerStoreHashesIdentityAndClaimsOnce(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target := testPrimerTarget()
	if err := store.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("/state/primer.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"account-secret", "backend-scope-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("journal leaked %q", secret)
		}
	}
	claimed, err := store.Claim("account-secret", target)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	claimed, err = store.Claim("account-secret", target)
	if err != nil || claimed {
		t.Fatalf("second claim = %v, %v", claimed, err)
	}
}

func TestCodexPrimerStoreNeverReplaysAdmittedGenerationAfterRestart(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target := testPrimerTarget()
	if err := store.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.Claim("account-secret", target); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.Mark("account-secret", target, PrimerStateAdmitted, ""); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := restarted.Claim("account-secret", target); err != nil || claimed {
		t.Fatalf("restart claim = %v, %v", claimed, err)
	}
}

func TestCodexPrimerStoreBoundsRejectedGenerationRetries(t *testing.T) {
	store, err := OpenCodexPrimerStore(fsutil.NewMemFS(), "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target := testPrimerTarget()
	if err := store.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		claimed, err := store.Claim("account-secret", target)
		if err != nil || !claimed {
			t.Fatalf("claim %d = %v, %v", attempt, claimed, err)
		}
		if err := store.Mark("account-secret", target, PrimerStateRejected, "pre_admission"); err != nil {
			t.Fatal(err)
		}
	}
	if claimed, err := store.Claim("account-secret", target); err != nil || claimed {
		t.Fatalf("third claim = %v, %v", claimed, err)
	}
	record, found := store.Lookup("account-secret", target)
	if !found || record.Attempts != 2 {
		t.Fatalf("record = %+v, %v", record, found)
	}
}

func TestCodexPrimerStoreBoundsDormantRetriesAcrossSlidingRestart(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	now := testPrimerTarget().ResetAt.Add(-time.Hour)
	target := testPrimerTarget()
	for attempt := 1; attempt <= codexPrimerMaxRejectedAttempts; attempt++ {
		target.ResetAt = target.ResetAt.Add(5 * time.Second)
		for i := range target.Windows {
			target.Windows[i].ResetAt = target.ResetAt
		}
		if err := store.Observe("account-secret", target); err != nil {
			t.Fatal(err)
		}
		if claimed, err := store.ClaimDormant("account-secret", target, now); err != nil || !claimed {
			t.Fatalf("claim %d = %v, %v", attempt, claimed, err)
		}
		if err := store.Mark("account-secret", target, PrimerStateRejected, "dormant_rejected"); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target.ResetAt = target.ResetAt.Add(5 * time.Second)
	for i := range target.Windows {
		target.Windows[i].ResetAt = target.ResetAt
	}
	if err := restarted.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	if claimed, err := restarted.ClaimDormant("account-secret", target, now); err != nil || claimed {
		t.Fatalf("claim after restart = %v, %v", claimed, err)
	}
}

func TestCodexPrimerStoreNeverReplaysSlidingAdmissionAfterRestart(t *testing.T) {
	for _, state := range []PrimerState{PrimerStateAdmitted, PrimerStateAmbiguous} {
		t.Run(string(state), func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
			if err != nil {
				t.Fatal(err)
			}
			now := testPrimerTarget().ResetAt.Add(-time.Hour)
			oldTarget := testPrimerTarget()
			if err := store.Observe("account-secret", oldTarget); err != nil {
				t.Fatal(err)
			}
			if claimed, err := store.ClaimDormant("account-secret", oldTarget, now); err != nil || !claimed {
				t.Fatalf("claim = %v, %v", claimed, err)
			}
			if err := store.Mark("account-secret", oldTarget, state, "dormant_"+string(state)); err != nil {
				t.Fatal(err)
			}
			restarted, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
			if err != nil {
				t.Fatal(err)
			}
			newTarget := oldTarget
			newTarget.ResetAt = oldTarget.ResetAt.Add(5 * time.Second)
			for i := range newTarget.Windows {
				newTarget.Windows[i].ResetAt = newTarget.ResetAt
			}
			if err := restarted.Observe("account-secret", newTarget); err != nil {
				t.Fatal(err)
			}
			if claimed, err := restarted.ClaimDormant("account-secret", newTarget, now); err != nil || claimed {
				t.Fatalf("sliding claim = %v, %v", claimed, err)
			}
		})
	}
}

func TestCodexPrimerStoreGenerationIdentityIgnoresSelectedModel(t *testing.T) {
	store, err := OpenCodexPrimerStore(fsutil.NewMemFS(), "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target := testPrimerTarget()
	if err := store.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	target.ModelID = "new-registry-preference"
	if err := store.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	if len(store.Records()) != 1 {
		t.Fatalf("model change created duplicate generation: %+v", store.Records())
	}
}

func TestCodexPrimerStoreBoundsSlidingObservedLineage(t *testing.T) {
	store, err := OpenCodexPrimerStore(fsutil.NewMemFS(), "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target := testPrimerTarget()
	for observation := 0; observation < 3; observation++ {
		target.ResetAt = target.ResetAt.Add(5 * time.Second)
		for i := range target.Windows {
			target.Windows[i].ResetAt = target.ResetAt
		}
		if err := store.Observe("account-secret", target); err != nil {
			t.Fatal(err)
		}
	}
	records := store.Records()
	if len(records) != 1 {
		t.Fatalf("sliding observations created %d records: %+v", len(records), records)
	}
	record, found := store.Lookup("account-secret", target)
	if !found || !record.ResetAt.Equal(target.ResetAt) || record.State != PrimerStateObserved || record.Attempts != 0 {
		t.Fatalf("newest observation = %+v, %v", record, found)
	}
}

func TestCodexPrimerStoreMigratesSlidingObservedLineageAfterRestart(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	base := testPrimerTarget()
	targetAt := func(offset time.Duration) CodexPrimerTarget {
		target := base
		target.ResetAt = base.ResetAt.Add(offset)
		for i := range target.Windows {
			target.Windows[i].ResetAt = target.ResetAt
		}
		return target
	}
	recordFor := func(target CodexPrimerTarget, state PrimerState, attempts int, code string) PrimerRecord {
		accountHash, scopeHash := store.identity("account-secret", target)
		return PrimerRecord{
			AccountHash: accountHash, ScopeHash: scopeHash, WindowHash: store.windowIdentity(target),
			ResetAt: target.ResetAt.UTC(), ModelID: target.ModelID, State: state, Attempts: attempts, ResultCode: code,
		}
	}
	oldObserved := targetAt(time.Minute)
	newObserved := targetAt(2 * time.Minute)
	preserved := []struct {
		target   CodexPrimerTarget
		state    PrimerState
		attempts int
		code     string
	}{
		{targetAt(3 * time.Minute), PrimerStateObserved, 1, "pre_admission"},
		{targetAt(4 * time.Minute), PrimerStateClaimed, 1, "dormant_claimed"},
		{targetAt(5 * time.Minute), PrimerStateAdmitted, 1, "dormant_admitted"},
		{targetAt(6 * time.Minute), PrimerStateAmbiguous, 1, "dormant_ambiguous"},
		{targetAt(7 * time.Minute), PrimerStateVerifying, 1, "dormant_epoch_stability"},
		{targetAt(8 * time.Minute), PrimerStateVerified, 1, "dormant_epoch_stable"},
		{targetAt(9 * time.Minute), PrimerStateRejected, 2, "dormant_rejected"},
		{targetAt(10 * time.Minute), PrimerStateFailed, 1, "dormant_model_incapable"},
	}
	records := []PrimerRecord{
		recordFor(oldObserved, PrimerStateObserved, 0, ""),
		recordFor(newObserved, PrimerStateObserved, 0, ""),
	}
	for _, want := range preserved {
		records = append(records, recordFor(want.target, want.state, want.attempts, want.code))
	}
	if err := store.commitLocked(records); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(restarted.Records()), len(preserved)+1; got != want {
		t.Fatalf("migrated records = %d, want %d: %+v", got, want, restarted.Records())
	}
	if _, found := restarted.Lookup("account-secret", oldObserved); found {
		t.Fatal("older unattempted observation survived migration")
	}
	newest, found := restarted.Lookup("account-secret", newObserved)
	if !found || newest.State != PrimerStateObserved || newest.Attempts != 0 || !newest.ResetAt.Equal(newObserved.ResetAt) {
		t.Fatalf("newest observation = %+v, %v", newest, found)
	}
	for _, want := range preserved {
		record, found := restarted.Lookup("account-secret", want.target)
		if !found || record.State != want.state || record.Attempts != want.attempts || record.ResultCode != want.code {
			t.Fatalf("preserved %s record = %+v, %v", want.state, record, found)
		}
	}
	data, err := fsys.ReadFile("/state/primer.json")
	if err != nil {
		t.Fatal(err)
	}
	var migrated codexPrimerEnvelope
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Version != codexPrimerJournalVersion || migrated.Generation != 2 || len(migrated.Records) != len(preserved)+1 || !restarted.validMAC(migrated) {
		t.Fatalf("persisted migration = %+v", migrated)
	}
}

func TestCodexPrimerStorePreservesCompletedFutureResetGeneration(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	completed := testPrimerTarget()
	if err := store.Observe("account-secret", completed); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.Claim("account-secret", completed); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.Mark("account-secret", completed, PrimerStateVerified, "reset_advanced"); err != nil {
		t.Fatal(err)
	}
	future := completed
	future.ResetAt = completed.ResetAt.Add(7 * 24 * time.Hour)
	for i := range future.Windows {
		future.Windows[i].ResetAt = future.ResetAt
	}
	if err := store.Observe("account-secret", future); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	oldRecord, oldFound := restarted.Lookup("account-secret", completed)
	newRecord, newFound := restarted.Lookup("account-secret", future)
	if !oldFound || oldRecord.State != PrimerStateVerified || !newFound || newRecord.State != PrimerStateObserved || len(restarted.Records()) != 2 {
		t.Fatalf("completed/future records = %+v/%v, %+v/%v", oldRecord, oldFound, newRecord, newFound)
	}
}

func TestCodexPrimerStoreRestoresDormantStabilityVerification(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	dispatched := testPrimerTarget()
	expected := dispatched
	expected.ResetAt = expected.ResetAt.Add(5 * time.Second)
	for i := range expected.Windows {
		expected.Windows[i].ResetAt = expected.ResetAt
	}
	if err := store.Observe("account-secret", dispatched); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.Claim("account-secret", dispatched); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.Mark("account-secret", dispatched, PrimerStateAdmitted, "dormant_admitted"); err != nil {
		t.Fatal(err)
	}
	next := time.Unix(10000, 0)
	if err := store.MarkDormantVerifying("account-secret", dispatched, expected, next); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ReconcileDormant("account-secret", []CodexPrimerTarget{expected}, next, next.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	record, found := restarted.Lookup("account-secret", dispatched)
	if !found || record.State != PrimerStateVerified || record.ResultCode != "dormant_epoch_stable" {
		t.Fatalf("restarted record = %+v, %v", record, found)
	}
}

func TestCodexPrimerStoreRetiresSlidingDormantModelCapability(t *testing.T) {
	store, err := OpenCodexPrimerStore(fsutil.NewMemFS(), "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(12000, 0)
	dispatched := testPrimerTarget()
	dispatched.ModelID = "gpt-5.3-codex-spark"
	dispatched.ResetAt = now.Add(7 * 24 * time.Hour)
	for i := range dispatched.Windows {
		dispatched.Windows[i].ResetAt = dispatched.ResetAt
	}
	if err := store.Observe("account-secret", dispatched); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimDormant("account-secret", dispatched, now); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.Mark("account-secret", dispatched, PrimerStateAdmitted, "dormant_admitted"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDormantVerifying("account-secret", dispatched, dispatched, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	corrected := dispatched
	corrected.ModelID = "gpt-5.4"
	corrected.ResetAt = dispatched.ResetAt.Add(5 * time.Second)
	for i := range corrected.Windows {
		corrected.Windows[i].ResetAt = corrected.ResetAt
	}
	if _, err := store.ReconcileDormant("account-secret", []CodexPrimerTarget{corrected}, now.Add(5*time.Second), now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	record, found := store.Lookup("account-secret", dispatched)
	if !found || record.State != PrimerStateFailed || record.ResultCode != "dormant_model_incapable" {
		t.Fatalf("retired record = %+v, %v", record, found)
	}
	if err := store.Observe("account-secret", corrected); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimDormant("account-secret", corrected, now.Add(5*time.Second)); err != nil || !claimed {
		t.Fatalf("corrected claim = %v, %v", claimed, err)
	}
}

func TestCodexPrimerStoreRetiresLegacyCoalescedDormantTarget(t *testing.T) {
	store, err := OpenCodexPrimerStore(fsutil.NewMemFS(), "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(13000, 0)
	shared := testPrimerTarget()
	shared.Windows[0].RawLimitName = "primary_window"
	shared.Windows[0].WindowName = quota.Window7Day
	shared.Windows[0].ScopeKind = codex.WindowScopeShared
	shared.Windows[0].Scope = ""
	spark := testPrimerTarget()
	combined := shared
	combined.Windows = append(append([]codex.WindowDescriptor(nil), shared.Windows...), spark.Windows...)
	if err := store.Observe("account-secret", combined); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimDormant("account-secret", combined, now); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.Mark("account-secret", combined, PrimerStateAdmitted, "dormant_admitted"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDormantVerifying("account-secret", combined, combined, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileDormant("account-secret", []CodexPrimerTarget{shared, spark}, now, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	record, found := store.Lookup("account-secret", combined)
	if !found || record.State != PrimerStateFailed || record.ResultCode != "dormant_target_split" {
		t.Fatalf("split record = %+v, %v", record, found)
	}
}

func TestCodexPrimerStoreUsesPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenCodexPrimerStore(fsutil.OSFileSystem{}, dir+"/primer.json", dir+"/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Observe("account-secret", testPrimerTarget()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir + "/primer.json", dir + "/primer.key"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
}

func TestCodexPrimerStoreWriteFailureDoesNotAdvanceMemory(t *testing.T) {
	fsys := &failingDurableFS{MemFS: fsutil.NewMemFS()}
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	fsys.failWrite = true
	if err := store.Observe("account-secret", testPrimerTarget()); err == nil {
		t.Fatal("Observe returned no write error")
	}
	if len(store.Records()) != 0 {
		t.Fatalf("records advanced after failed write: %+v", store.Records())
	}
	fsys.failWrite = false
	if err := store.Observe("account-secret", testPrimerTarget()); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadFile("/state/primer.json"); err != nil {
		t.Fatalf("retry did not persist journal: %v", err)
	}
}
