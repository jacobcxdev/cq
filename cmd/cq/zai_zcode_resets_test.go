package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

type zcodeResetTestDoer func(*http.Request) (*http.Response, error)

func (f zcodeResetTestDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func zcodeResetTestResponse(value string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(value))}
}

func zcodeResetTestAccount(id, label string) zaiprov.Account {
	return zaiprov.Account{ID: id, Label: label, Transport: "zcode", OAuth: &zcode.LoginSession{UserID: id, ZCodeJWT: "zcode-" + id, BusinessJWT: "business-" + id, DeviceMid: "device-" + id}}
}

func zcodeResetTestStatus(available bool, now time.Time) string {
	resets := []map[string]int64{}
	if available {
		resets = append(resets, map[string]int64{"expire_at": now.Add(time.Hour).UnixMilli()})
	}
	data, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"available_five_hour_resets": resets, "available_week_resets": resets, "latest_five_hour_reset_history": nil, "latest_week_reset_history": nil, "has_unread_history": false}})
	return string(data)
}

func zcodeResetTestDeps(t *testing.T, doer zcodeResetTestDoer) (zcodeResetDependencies, *bytes.Buffer, *fsutil.MemFS) {
	t.Helper()
	output := &bytes.Buffer{}
	fsys := fsutil.NewMemFS()
	return zcodeResetDependencies{Accounts: func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{zcodeResetTestAccount("account-one", "primary"), zcodeResetTestAccount("account-two", "secondary")}, nil
	}, Client: zcode.Client{HTTP: doer, Version: "3.17.2"}, FS: fsys, StateDir: func() (string, error) { return "/state/reset", nil }, In: strings.NewReader("n\n"), Out: output, ErrOut: io.Discard, Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, RequestID: func() string { return "00000000-0000-4000-8000-000000000001" }}, output, fsys
}

func TestZCodeResetUseDidNotDispatchWithoutConfirmation(t *testing.T) {
	posts := 0
	deps, _, fsys := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			posts++
		}
		return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
	})
	err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h"}}, false, deps)
	if err == nil || posts != 0 {
		t.Fatal("unconfirmed use dispatched")
	}
	if _, err := fsys.ReadFile(zcodeResetAttemptPath("/state/reset", "account-one", "use")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfirmed action wrote intent")
	}
}

func TestZCodeResetUseRecheckedAvailabilityBeforeDurableDispatch(t *testing.T) {
	statuses, posts, keys := 0, 0, 0
	deps, _, _ := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			posts++
			return zcodeResetTestResponse(`{"code":0,"data":{"used":true}}`), nil
		}
		statuses++
		return zcodeResetTestResponse(zcodeResetTestStatus(statuses == 1, time.Unix(1_800_000_000, 0))), nil
	})
	deps.RequestID = func() string { keys++; return "00000000-0000-4000-8000-000000000001" }
	err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}, false, deps)
	if err == nil || statuses != 2 || posts != 0 || keys != 0 {
		t.Fatal("stale availability consumed a card or generated an intent")
	}
}

func TestZCodeResetRetryRetainedUUIDAndAcceptedReceipt(t *testing.T) {
	posts, generated := 0, 0
	var sent []string
	var fsys *fsutil.MemFS
	deps, output, mem := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
		}
		posts++
		var body struct {
			Key  string `json:"idempotency_key"`
			Type string `json:"reset_type"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Type != "FIVE_HOUR" {
			t.Fatal("wrong reset contract")
		}
		sent = append(sent, body.Key)
		data, err := fsys.ReadFile(zcodeResetAttemptPath("/state/reset", "account-two", "use"))
		var attempt zcodeResetAttempt
		if err != nil || json.Unmarshal(data, &attempt) != nil || attempt.RequestID != body.Key || attempt.State != "pending" {
			t.Fatal("POST preceded durable intent")
		}
		if r.Header.Get("Authorization") != "Bearer zcode-account-two" || r.Header.Get("X-Bigmodel-Authorization") != "business-account-two" || r.Header.Get("X-Device-Mid") != "device-account-two" {
			t.Fatal("wrong account selected")
		}
		if posts == 1 {
			return nil, errors.New("private-upstream-secret")
		}
		return zcodeResetTestResponse(`{"code":0,"data":{"used":true}}`), nil
	})
	fsys = mem
	deps.RequestID = func() string {
		generated++
		if generated == 1 {
			return "00000000-0000-4000-8000-000000000001"
		}
		return "00000000-0000-4000-8000-000000000002"
	}
	options := ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "secondary", Window: "5h", Yes: true}}
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, true, deps); err == nil || strings.Contains(err.Error(), "private-upstream-secret") {
		t.Fatal("ambiguous error lost or leaked")
	}
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, true, deps); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || generated != 1 || len(sent) != 2 || sent[0] != sent[1] {
		t.Fatal("retry changed request identity")
	}
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, true, deps); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || !strings.Contains(output.String(), `"already_accepted":true`) {
		t.Fatal("accepted receipt dispatched another reset")
	}
	options.Use.New = true
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, true, deps); err != nil {
		t.Fatal(err)
	}
	if posts != 3 || generated != 2 || sent[2] == sent[1] {
		t.Fatal("explicit new action did not create a new UUID")
	}
}

func TestZCodeResetNewCouldNotReplaceUnresolvedAttempt(t *testing.T) {
	posts := 0
	deps, _, _ := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
		}
		posts++
		return nil, errors.New("network failure")
	})
	options := ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}
	_ = runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps)
	options.Use.New = true
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps); err == nil || posts != 1 {
		t.Fatal("new action replaced uncertain dispatch")
	}
}

func TestZCodeResetIssueWasExplicitAndSeparateFromUse(t *testing.T) {
	posts := 0
	deps, output, _ := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/coding-plan/reset/opportunity" {
			t.Fatal("issuance called another endpoint")
		}
		posts++
		if r.Header.Get("Authorization") != "Bearer zcode-account-one" {
			t.Fatal("issuance changed account")
		}
		return zcodeResetTestResponse(`{"code":3301,"data":{"granted":false,"next_try_at":1800000600000}}`), nil
	})
	options := ZAIZCodeResetsCmd{Issue: ZAIZCodeResetIssueCmd{Reference: "primary"}}
	if err := runZAIZCodeResetsCommand(context.Background(), "issue <reference>", options, true, deps); err != nil {
		t.Fatal(err)
	}
	if err := runZAIZCodeResetsCommand(context.Background(), "issue <reference>", options, true, deps); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || !strings.Contains(output.String(), `"granted":false`) || !strings.Contains(output.String(), `"next_try_at"`) {
		t.Fatal("issuance lost server cooldown or replayed")
	}
}

func TestZCodeResetRejectedMissingAmbiguousAndAPIKeyAccounts(t *testing.T) {
	deps, _, _ := zcodeResetTestDeps(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid account made request"); return nil, nil })
	deps.Accounts = func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{zcodeResetTestAccount("one", "duplicate"), zcodeResetTestAccount("two", "duplicate"), {ID: "native", Label: "native", APIKey: "private-key"}}, nil
	}
	for _, reference := range []string{"missing", "duplicate", "native"} {
		err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: reference, Window: "5h", Yes: true}}, false, deps)
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("invalid reset account accepted or leaked")
		}
	}
}

func TestZCodeResetHeldAccountLockThroughSelectionConfirmationAndDispatch(t *testing.T) {
	var fsys *fsutil.MemFS
	checked := 0
	assertLocked := func() {
		lock, err := fsutil.AcquireExclusiveLock(fsys, zcodeResetAttemptPath("/state/reset", "account-one", "")+".lock")
		if err == nil {
			_ = lock.Close()
			t.Fatal("reset account lock not held")
		}
		checked++
	}
	deps, _, mem := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		assertLocked()
		if r.Method == "GET" {
			return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
		}
		return zcodeResetTestResponse(`{"code":0,"data":{"used":true}}`), nil
	})
	fsys = mem
	reads := 0
	deps.Accounts = func() ([]zaiprov.Account, error) {
		reads++
		if reads > 1 {
			assertLocked()
		}
		return []zaiprov.Account{zcodeResetTestAccount("account-one", "primary")}, nil
	}
	deps.In = &zcodeResetConfirmReader{Reader: strings.NewReader("yes\n"), Before: assertLocked}
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h"}}, false, deps); err != nil {
		t.Fatal(err)
	}
	if checked < 6 {
		t.Fatal("lock was not checked across all mutation boundaries")
	}
}

type zcodeResetConfirmReader struct {
	io.Reader
	Before func()
}

func (r *zcodeResetConfirmReader) Read(p []byte) (int, error) {
	if r.Before != nil {
		r.Before()
		r.Before = nil
	}
	return r.Reader.Read(p)
}

func TestZCodeResetWeeklyConfirmationAndNewActionWereExplicit(t *testing.T) {
	posts := 0
	deps, _, _ := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
		}
		posts++
		var body struct {
			Type string `json:"reset_type"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Type != "WEEK" {
			t.Fatal("weekly reset sent another type")
		}
		return zcodeResetTestResponse(`{"code":0,"data":{"used":true}}`), nil
	})
	prompt := &bytes.Buffer{}
	deps.In = strings.NewReader("yes\n")
	deps.ErrOut = prompt
	options := ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "7d"}}
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt.String(), "weekly and 5-hour") || strings.Contains(prompt.String(), "%!") {
		t.Fatal("weekly confirmation omitted affected quotas")
	}
	options.Use.New = true
	deps.In = strings.NewReader("no\n")
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps); err == nil || posts != 1 {
		t.Fatal("new action skipped explicit confirmation")
	}
}

func TestZCodeResetRetryRejectionDidNotEraseEarlierUncertainty(t *testing.T) {
	posts := 0
	deps, _, _ := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
		}
		posts++
		if posts == 1 {
			return nil, errors.New("network failure")
		}
		response := zcodeResetTestResponse(`{}`)
		response.StatusCode = http.StatusForbidden
		return response, nil
	})
	options := ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}
	_ = runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps)
	_ = runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps)
	options.Use.New = true
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps); err == nil || posts != 2 {
		t.Fatal("retry rejection erased unresolved original outcome")
	}
}

func TestZCodeResetRecoveredLostReceiptAfterOnlyCardWasConsumed(t *testing.T) {
	posts, statuses, generated := 0, 0, 0
	var keys []string
	deps, _, _ := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			statuses++
			return zcodeResetTestResponse(zcodeResetTestStatus(statuses <= 2, time.Unix(1_800_000_000, 0))), nil
		}
		posts++
		var body struct {
			Key string `json:"idempotency_key"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatal("invalid reset body")
		}
		keys = append(keys, body.Key)
		if posts == 1 {
			return nil, errors.New("server consumed card but connection closed before receipt")
		}
		return zcodeResetTestResponse(`{"code":0,"data":{"used":true}}`), nil
	})
	deps.RequestID = func() string { generated++; return "00000000-0000-4000-8000-000000000001" }
	options := ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps); err == nil {
		t.Fatal("lost receipt was not retained as pending")
	}
	options.Use.Yes = false
	deps.In = strings.NewReader("yes\n")
	prompt := &bytes.Buffer{}
	deps.ErrOut = prompt
	if err := runZAIZCodeResetsCommand(context.Background(), "use <reference>", options, false, deps); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || statuses != 4 || generated != 1 || len(keys) != 2 || keys[0] != keys[1] || !strings.Contains(prompt.String(), "original request ID") {
		t.Fatal("idempotent receipt recovery changed action or skipped confirmation/status")
	}
}

func TestZCodeResetBlockedPendingExactCardUseAndRejectedMalformedJournal(t *testing.T) {
	valid := `{"account_id":"account-one","credit_id":"5h:exact","window":"5h","request_id":"stable-exact","state":"pending","started_at":"2026-11-02T19:00:00Z"}`
	for _, body := range []string{valid, `{`, strings.Repeat("x", (16<<10)+1), strings.Replace(valid, `"account-one"`, `"other-account"`, 1), strings.Replace(valid, `"pending"`, `"unexpected"`, 1)} {
		deps, _, fsys := zcodeResetTestDeps(t, func(*http.Request) (*http.Response, error) {
			t.Fatal("other contract journal reached HTTP")
			return nil, nil
		})
		if err := fsys.WriteFile(zcodeResetAttemptPath("/state/reset", "account-one", ""), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runZAIZCodeResetsCommand(t.Context(), "use <reference>", ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}, true, deps)
		if err == nil || body == valid && !errors.Is(err, zaiprov.ErrResetPending) {
			t.Fatal("pending or malformed exact-card journal did not block window reset")
		}
		if _, err := fsys.ReadFile(zcodeResetAttemptPath("/state/reset", "account-one", "use")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("blocked reset wrote a new window-use intent")
		}
	}
}

func TestZCodeResetResolvedExactCardUseDidNotBlockWindowUse(t *testing.T) {
	for _, state := range []string{"completed", "rejected", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			posts := 0
			deps, _, fsys := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					return zcodeResetTestResponse(`{"code":0,"data":{"used":true}}`), nil
				}
				return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
			})
			body, _ := json.Marshal(map[string]any{"account_id": "account-one", "credit_id": "5h:exact", "window": "5h", "request_id": "stable-exact", "state": state, "started_at": deps.Now()})
			if err := fsys.WriteFile(zcodeResetAttemptPath("/state/reset", "account-one", ""), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runZAIZCodeResetsCommand(t.Context(), "use <reference>", ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}, true, deps); err != nil || posts != 1 {
				t.Fatal("resolved exact-card attempt blocked ZCode window use")
			}
		})
	}
}

func TestZCodeResetOpportunityWasNotBlockedByPendingCardUse(t *testing.T) {
	posts := 0
	deps, _, fsys := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		posts++
		if r.Method != "POST" || r.URL.Path != "/api/v1/coding-plan/reset/opportunity" {
			t.Fatal("eligibility dispatched a card-use request")
		}
		return zcodeResetTestResponse(`{"code":3301,"data":{"granted":false,"next_try_at":1800000600000}}`), nil
	})
	body := []byte(`{"account_id":"account-one","credit_id":"5h:exact","window":"5h","request_id":"stable-exact","state":"pending","started_at":"2026-11-02T19:00:00Z"}`)
	if err := fsys.WriteFile(zcodeResetAttemptPath("/state/reset", "account-one", ""), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runZAIZCodeResetsCommand(t.Context(), "issue <reference>", ZAIZCodeResetsCmd{Issue: ZAIZCodeResetIssueCmd{Reference: "primary"}}, true, deps); err != nil || posts != 1 {
		t.Fatal("pending card use blocked explicit eligibility")
	}
}

func TestZCodeResetAndExactCardExecutorContendedForSameAccountLock(t *testing.T) {
	deps, _, fsys := zcodeResetTestDeps(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("locked ZCode reset reached HTTP")
		return nil, nil
	})
	if err := fsutil.EnsureSecureDirectory(fsys, "/state/reset"); err != nil {
		t.Fatal(err)
	}
	path := zaiprov.ResetAccountLockPath("/state/reset", "account-one")
	if path != zcodeResetAttemptPath("/state/reset", "account-one", "")+".lock" {
		t.Fatal("reset contracts used different lock names")
	}
	lock, err := fsutil.AcquireExclusiveLock(fsys, path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := runZAIZCodeResetsCommand(t.Context(), "use <reference>", ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}, true, deps); err == nil {
		t.Fatal("window-use contract ignored shared lock")
	}
	executor := zaiprov.ResetExecutor{FS: fsys, StateDir: "/state/reset", Now: deps.Now, RequestID: deps.RequestID}
	if _, err := executor.UseScheduled(t.Context(), zcodeResetTestAccount("account-one", "primary"), "5h", "5h:exact"); err == nil {
		t.Fatal("exact-card contract ignored shared lock")
	}
}

func TestZCodeResetDefaultStateDirectoryMatchedExactCardExecutor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	windowDir, err := defaultZCodeCommandDependencies().Resets.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	exactDir, err := defaultZAIResetsDependencies().StateDir()
	if err != nil || windowDir != exactDir || filepath.Base(windowDir) != "zai-reset-attempts" {
		t.Fatal("default reset contracts used different directories")
	}
}
