package codex

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func resetSelectionCredits(now time.Time) []ResetCredit {
	expiresFirst := now.Add(time.Hour)
	expiresSecond := now.Add(2 * time.Hour)
	return []ResetCredit{
		{ID: "never", ResetType: ResetTypeCodexRateLimits, Status: ResetCreditAvailable, GrantedAt: now.Add(-3 * time.Hour)},
		{ID: "second", ResetType: ResetTypeCodexRateLimits, Status: ResetCreditAvailable, GrantedAt: now.Add(-2 * time.Hour), ExpiresAt: &expiresSecond},
		{ID: "first", ResetType: ResetTypeCodexRateLimits, Status: ResetCreditAvailable, GrantedAt: now.Add(-time.Hour), ExpiresAt: &expiresFirst},
	}
}

func TestResetIdempotencyKeyDeterministicAndScoped(t *testing.T) {
	first := ResetIdempotencyKey("account-a", "credit-1")
	if first != ResetIdempotencyKey("account-a", "credit-1") {
		t.Fatal("same tuple produced different key")
	}
	if first == ResetIdempotencyKey("account-b", "credit-1") || first == ResetIdempotencyKey("account-a", "credit-2") {
		t.Fatal("different tuple reused key")
	}
	if !strings.HasPrefix(first, "cq-reset-v1-") || len(first) != len("cq-reset-v1-")+64 {
		t.Fatalf("key = %q", first)
	}
}

func TestSelectResetCreditDefaultsToOldestPending(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	pending := []ResetAttempt{
		{CreditID: "later", StartedAt: now.Add(time.Minute), IdempotencyKey: "later-key"},
		{CreditID: "first", StartedAt: now, IdempotencyKey: "first-key"},
	}
	got, err := SelectResetCredit(now, resetSelectionCredits(now), pending, "")
	if err != nil || !got.Resume || got.Credit.ID != "first" || got.Attempt == nil || got.Attempt.IdempotencyKey != "first-key" {
		t.Fatalf("selection = %+v, %v", got, err)
	}
}

func TestSelectResetCreditExplicitPendingRules(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	pending := []ResetAttempt{{CreditID: "first", StartedAt: now, IdempotencyKey: "first-key"}}
	got, err := SelectResetCredit(now, resetSelectionCredits(now), pending, "first")
	if err != nil || !got.Resume || got.Credit.ID != "first" {
		t.Fatalf("matching pending = %+v, %v", got, err)
	}
	if _, err := SelectResetCredit(now, resetSelectionCredits(now), pending, "second"); err == nil {
		t.Fatal("different explicit credit bypassed pending attempt")
	}
}

func TestSelectResetCreditDefaultsToNextExpiry(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	got, err := SelectResetCredit(now, resetSelectionCredits(now), nil, "")
	if err != nil || got.Resume || got.Credit.ID != "first" {
		t.Fatalf("selection = %+v, %v", got, err)
	}
	explicit, err := SelectResetCredit(now, resetSelectionCredits(now), nil, "second")
	if err != nil || explicit.Credit.ID != "second" {
		t.Fatalf("explicit selection = %+v, %v", explicit, err)
	}
}

func TestSelectResetCreditRejectsUnavailableCredits(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	expiredAt := now
	tests := []ResetCredit{
		{ID: "expired", ResetType: ResetTypeCodexRateLimits, Status: ResetCreditAvailable, ExpiresAt: &expiredAt},
		{ID: "unsupported", ResetType: "other", Status: ResetCreditAvailable},
		{ID: "redeemed", ResetType: ResetTypeCodexRateLimits, Status: ResetCreditRedeemed},
		{ID: "unknown", ResetType: ResetTypeCodexRateLimits, Status: "unknown"},
	}
	for _, credit := range tests {
		if _, err := SelectResetCredit(now, []ResetCredit{credit}, nil, credit.ID); err == nil {
			t.Errorf("credit %q was selectable", credit.ID)
		}
	}
}

func TestSelectResetCreditResumesMissingPendingCredit(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	pending := []ResetAttempt{{CreditID: "missing", StartedAt: now, IdempotencyKey: "pending-key"}}
	got, err := SelectResetCredit(now, nil, pending, "")
	if err != nil || !got.Resume || got.Credit.ID != "missing" || got.Attempt == nil {
		t.Fatalf("selection = %+v, %v", got, err)
	}
}

func newResetAttemptStore(t *testing.T) (*ResetAttemptStore, *fsutil.MemFS) {
	t.Helper()
	fs := fsutil.NewMemFS()
	store, err := NewResetAttemptStore(fs, "/cache/cq")
	if err != nil {
		t.Fatal(err)
	}
	return store, fs
}

func TestResetAttemptEnsurePersistsPrivateImmutableRecord(t *testing.T) {
	store, fs := newResetAttemptStore(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	attempt, err := store.Ensure("account-key", "credit-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.IdempotencyKey != ResetIdempotencyKey("account-key", "credit-1") {
		t.Fatalf("attempt = %+v", attempt)
	}
	entries, err := fs.ReadDir(store.Dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if strings.Contains(entries[0].Name(), "account-key") || strings.Contains(entries[0].Name(), "credit-1") {
		t.Fatalf("raw identifier leaked into filename %q", entries[0].Name())
	}
	info, _ := fs.Stat(filepath.Join(store.Dir, entries[0].Name()))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
	dirInfo, _ := fs.Stat(store.Dir)
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o", dirInfo.Mode().Perm())
	}
	pending, err := store.Pending("account-key")
	if err != nil || len(pending) != 1 || pending[0] != attempt {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

func TestResetAttemptEnsureReusesConcurrentAttempt(t *testing.T) {
	store, _ := newResetAttemptStore(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	results := make(chan struct {
		attempt ResetAttempt
		err     error
	}, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			attempt, err := store.Ensure("account-key", "credit-1", now)
			results <- struct {
				attempt ResetAttempt
				err     error
			}{attempt: attempt, err: err}
		}()
	}
	start.Done()
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.attempt != b.attempt {
		t.Fatalf("attempts = %+v/%+v errors=%v/%v", a.attempt, b.attempt, a.err, b.err)
	}
}

func TestResetAttemptPendingFailsClosedOnMalformedRecord(t *testing.T) {
	store, fs := newResetAttemptStore(t)
	_ = fs.WriteFile(filepath.Join(store.Dir, "malformed.json"), []byte(`{"version":1}`), 0o600)
	if _, err := store.Pending("account-key"); err == nil {
		t.Fatal("malformed record was ignored")
	}
}

func TestResetAttemptRemoveTargetsExactDigest(t *testing.T) {
	store, _ := newResetAttemptStore(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	_, _ = store.Ensure("account-key", "credit-1", now)
	_, _ = store.Ensure("account-key", "credit-2", now.Add(time.Second))
	if err := store.Remove("account-key", "credit-1"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending("account-key")
	if err != nil || len(pending) != 1 || pending[0].CreditID != "credit-2" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

func TestResetAttemptJSONContainsOnlyAttemptSchema(t *testing.T) {
	store, fs := newResetAttemptStore(t)
	_, _ = store.Ensure("opaque-account", "credit-1", time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	entries, _ := fs.ReadDir(store.Dir)
	data, _ := fs.ReadFile(filepath.Join(store.Dir, entries[0].Name()))
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	want := map[string]bool{"version": true, "account_key": true, "credit_id": true, "idempotency_key": true, "started_at": true}
	if len(fields) != len(want) {
		t.Fatalf("fields = %v", fields)
	}
	for field := range fields {
		if !want[field] {
			t.Fatalf("unexpected field %q", field)
		}
	}
}
