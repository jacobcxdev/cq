package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
)

type zaiScheduleHTTP func(*http.Request) (*http.Response, error)

func (f zaiScheduleHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

type zaiScheduleFixture struct {
	fs        *fsutil.MemFS
	scheduler *ZAIResetScheduler
	account   zaiprov.Account
	credit    zaiprov.ResetCredit
	now       time.Time
	posts     int
	gets      int
	postErr   bool
	postPanic bool
	getErr    bool
	empty     bool
}

func newZAIScheduleFixture(t *testing.T) *zaiScheduleFixture {
	t.Helper()
	f := &zaiScheduleFixture{fs: fsutil.NewMemFS(), now: time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC), account: zaiprov.Account{ID: "zai_0123456789abcdef", APIKey: "schedule-private-key"}}
	expiry := f.now.Add(time.Hour)
	f.credit = zaiprov.ResetCredit{ID: "5h:123", Window: "5h", Available: true, ExpiresAt: &expiry}
	client := zaiprov.ResetClient{HTTP: zaiScheduleHTTP(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "schedule-private-key" {
			t.Fatal("wrong exact account")
		}
		if r.Method == http.MethodPost {
			f.posts++
			if f.postPanic {
				panic("schedule-private-key")
			}
			if f.postErr {
				return nil, errors.New("schedule-private-key")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"success":true}`))}, nil
		}
		f.gets++
		if f.getErr {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
		}
		body := `{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":123,"available":true,"expireTime":"2026-11-02T21:00:00Z","grantType":"ACTIVITY"}],"weekResets":[]}}`
		if f.empty {
			body = `{"code":200,"success":true,"data":{"fiveHourResets":[],"weekResets":[]}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	executor := zaiprov.ResetExecutor{FS: f.fs, StateDir: "/state/zai-reset-attempts", Client: client, Now: func() time.Time { return f.now }, RequestID: func() string { return "fixed-scheduled-request" }}
	var err error
	f.scheduler, err = NewZAIResetScheduler(f.fs, "/state", func() ([]zaiprov.Account, error) { return []zaiprov.Account{f.account}, nil }, executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.scheduler.Store.Add(f.account.ID, f.credit, f.now.Add(time.Minute), f.now, false); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestZAIResetScheduleDisabledByDefault(t *testing.T) {
	f := newZAIScheduleFixture(t)
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	state, err := f.scheduler.Store.Load()
	if err != nil || state.Enabled || state.Entries[0].State != "pending" || f.posts != 0 || f.gets != 0 {
		t.Fatal("disabled schedule executed or changed")
	}
}

func TestZAIResetScheduleExecutedExactCardOnceAcrossRestart(t *testing.T) {
	f := newZAIScheduleFixture(t)
	if _, err := f.scheduler.Store.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil || f.posts != 0 {
		t.Fatal("future entry executed")
	}
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 || f.scheduler.Health().State.Entries[0].State != "accepted" {
		t.Fatal("due exact card was not accepted")
	}
	restarted, err := NewZAIResetScheduler(f.fs, "/state", f.scheduler.Accounts, f.scheduler.Executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RunOnce(t.Context(), f.now.Add(time.Minute)); err != nil || f.posts != 1 {
		t.Fatal("restart replayed accepted entry")
	}
}

func TestZAIResetScheduleAmbiguousAttemptNeverAutomaticallyReplayed(t *testing.T) {
	f := newZAIScheduleFixture(t)
	f.postErr = true
	_, _ = f.scheduler.Store.SetEnabled(true)
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err == nil || strings.Contains(err.Error(), "private-key") {
		t.Fatal("ambiguous error missing or leaked credentials")
	}
	if f.posts != 1 || f.scheduler.Health().State.Entries[0].State != "ambiguous" {
		t.Fatal("ambiguous outcome lost")
	}
	if err := f.scheduler.RunOnce(t.Context(), f.now.Add(time.Minute)); err != nil || f.posts != 1 {
		t.Fatal("ambiguous attempt replayed")
	}
	data, _ := f.fs.ReadFile("/state/zai-reset-schedule.json")
	if strings.Contains(string(data), "private-key") {
		t.Fatal("schedule leaked credentials")
	}
}

func TestZAIResetSchedulePostPanicDoesNotBecomeSafeRetry(t *testing.T) {
	f := newZAIScheduleFixture(t)
	_, _ = f.scheduler.Store.SetEnabled(true)
	f.postPanic = true
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err == nil || strings.Contains(err.Error(), "private-key") {
		t.Fatal("panic missing or leaked")
	}
	if f.scheduler.Health().State.Entries[0].State != "ambiguous" {
		t.Fatal("post panic treated as pre-dispatch failure")
	}
	_ = f.scheduler.RunOnce(t.Context(), f.now.Add(6*time.Minute))
	if f.posts != 1 {
		t.Fatal("post panic replayed")
	}
}

func TestZAIResetScheduleStaleRunningClaimNeverReplayed(t *testing.T) {
	f := newZAIScheduleFixture(t)
	_, err := f.scheduler.Store.change(func(state *ZAIResetScheduleState) error {
		state.Enabled = true
		state.Entries[0].State = "running"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	if f.posts != 0 || f.gets != 0 || f.scheduler.Health().State.Entries[0].State != "ambiguous" {
		t.Fatal("stale claim retried")
	}
}

func TestZAIResetScheduleMissingAccountAndExpiredCardNeverSent(t *testing.T) {
	for _, mode := range []string{"account", "expiry", "inventory"} {
		t.Run(mode, func(t *testing.T) {
			f := newZAIScheduleFixture(t)
			_, _ = f.scheduler.Store.SetEnabled(true)
			f.now = f.now.Add(2 * time.Minute)
			switch mode {
			case "account":
				f.scheduler.Accounts = func() ([]zaiprov.Account, error) { return nil, nil }
			case "expiry":
				f.now = f.now.Add(2 * time.Hour)
			case "inventory":
				f.empty = true
			}
			_ = f.scheduler.RunOnce(t.Context(), f.now)
			if f.posts != 0 {
				t.Fatal("missing or expired exact card sent")
			}
		})
	}
}

type zaiScheduleWriteFailureFS struct{ *fsutil.MemFS }

func (f zaiScheduleWriteFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	dir, err := f.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return zaiScheduleWriteFailureDirectory{dir}, nil
}

type zaiScheduleWriteFailureDirectory struct{ fsutil.SecureDirectory }

func (zaiScheduleWriteFailureDirectory) CreateExclusive(string, os.FileMode) (fsutil.DurableFile, error) {
	return nil, errors.New("write denied")
}

func TestZAIResetScheduleClaimWriteFailureNeverSent(t *testing.T) {
	f := newZAIScheduleFixture(t)
	_, _ = f.scheduler.Store.SetEnabled(true)
	f.scheduler.Store.FS = zaiScheduleWriteFailureFS{f.fs}
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err == nil || err.Error() != "save Z.ai reset schedule" || f.posts != 0 {
		t.Fatal("failed claim dispatched")
	}
}

func TestZAIResetScheduleOverlapLockDefersWithoutRequests(t *testing.T) {
	f := newZAIScheduleFixture(t)
	_, _ = f.scheduler.Store.SetEnabled(true)
	lock, err := fsutil.AcquireExclusiveLock(f.fs, "/state/zai-reset-schedule.json.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil || f.posts != 0 || f.gets != 0 {
		t.Fatal("busy scheduler dispatched")
	}
}

func TestZAIResetScheduleInventoryFailureRecoversWithoutUncertainReplay(t *testing.T) {
	f := newZAIScheduleFixture(t)
	_, _ = f.scheduler.Store.SetEnabled(true)
	f.getErr = true
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err == nil {
		t.Fatal("inventory failure hidden")
	}
	state, _ := f.scheduler.Store.Load()
	if f.posts != 0 || state.Entries[0].State != "pending" || state.Entries[0].NextAttemptAt == nil {
		t.Fatal("safe pre-dispatch failure terminalised")
	}
	gets := f.gets
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil || f.gets != gets {
		t.Fatal("deferred request retried without backoff")
	}
	f.getErr = false
	f.now = f.now.Add(6 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil || f.posts != 1 {
		t.Fatal("pre-dispatch failure did not recover")
	}
}

func TestZAIResetScheduleManualAccountLockDefersThenRecovers(t *testing.T) {
	f := newZAIScheduleFixture(t)
	_, _ = f.scheduler.Store.SetEnabled(true)
	if err := fsutil.EnsureSecureDirectory(f.fs, "/state/zai-reset-attempts"); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/state/zai-reset-attempts/%x.json.lock", sha256.Sum256([]byte(f.account.ID)))
	lock, err := fsutil.AcquireExclusiveLock(f.fs, path)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err == nil || f.posts != 0 {
		t.Fatal("held manual lock not deferred")
	}
	state, _ := f.scheduler.Store.Load()
	if state.Entries[0].State != "pending" {
		t.Fatal("held manual lock terminalised schedule")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(6 * time.Minute)
	if err := f.scheduler.RunOnce(t.Context(), f.now); err != nil || f.posts != 1 {
		t.Fatal("released manual lock did not recover")
	}
}

func TestZAIResetScheduleControlsRetainClaims(t *testing.T) {
	f := newZAIScheduleFixture(t)
	state, _ := f.scheduler.Store.Load()
	if _, err := f.scheduler.Store.Add(f.account.ID, f.credit, f.now.Add(2*time.Minute), f.now, false); err == nil {
		t.Fatal("duplicate card added")
	}
	if _, err := f.scheduler.Store.Remove(state.Entries[0].ID); err != nil {
		t.Fatal(err)
	}
	state, _ = f.scheduler.Store.Load()
	if len(state.Entries) != 1 || state.Entries[0].State != "cancelled" {
		t.Fatal("cancelled history lost")
	}
	_, _ = f.scheduler.Store.change(func(state *ZAIResetScheduleState) error {
		state.Enabled = true
		state.Entries[0].State = "running"
		return nil
	})
	state, err := f.scheduler.Store.Cancel()
	if err != nil || state.Enabled || state.Entries[0].State != "running" {
		t.Fatal("cancel removed running claim")
	}
}

func TestZAIResetScheduleRejectedInvalidInput(t *testing.T) {
	f := newZAIScheduleFixture(t)
	for _, at := range []time.Time{f.now, f.now.Add(-time.Hour), *f.credit.ExpiresAt} {
		if _, err := f.scheduler.Store.Add(f.account.ID, f.credit, at, f.now, false); err == nil {
			t.Fatal("invalid schedule accepted")
		}
	}
	_, _ = f.scheduler.Store.SetEnabled(true)
	credit := f.credit
	credit.ID = "5h:456"
	if _, err := f.scheduler.Store.Add(f.account.ID, credit, f.now.Add(2*time.Minute), f.now, false); err == nil {
		t.Fatal("enabled schedule mutation lacked confirmation")
	}
}

func TestZAIResetScheduleEnableBoundToReviewedEntries(t *testing.T) {
	f := newZAIScheduleFixture(t)
	reviewed, err := f.scheduler.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	credit := f.credit
	credit.ID = "5h:456"
	if _, err := f.scheduler.Store.Add(f.account.ID, credit, f.now.Add(2*time.Minute), f.now, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.scheduler.Store.EnableReviewed(reviewed); err == nil {
		t.Fatal("unreviewed extra card enabled")
	}
	state, _ := f.scheduler.Store.Load()
	if state.Enabled {
		t.Fatal("changed schedule enabled")
	}
}

func TestZAIResetScheduleNeverResumesAmbiguousManualOperation(t *testing.T) {
	f := newZAIScheduleFixture(t)
	f.postErr = true
	_, err := f.scheduler.Executor.Use(t.Context(), f.account, "5h", f.credit.ID, func(zaiprov.ResetCredit) (bool, error) { return true, nil })
	if err == nil || f.posts != 1 {
		t.Fatal("manual ambiguous fixture not created")
	}
	f.postErr = false
	_, _ = f.scheduler.Store.SetEnabled(true)
	f.now = f.now.Add(2 * time.Minute)
	_ = f.scheduler.RunOnce(t.Context(), f.now)
	if f.posts != 1 {
		t.Fatal("scheduler resumed ambiguous manual POST")
	}
	state, _ := f.scheduler.Store.Load()
	if state.Entries[0].State != "ambiguous" {
		t.Fatal("pending manual operation not retained as unresolved")
	}
}

func TestZAIResetSchedulerRunHonoursCancellation(t *testing.T) {
	f := newZAIScheduleFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.scheduler.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("run ignored cancellation")
	}
}
