package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type zaiScheduleCommandHTTP func(*http.Request) (*http.Response, error)

func (f zaiScheduleCommandHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

func zaiScheduleCommandFixture(t *testing.T) (zaiResetScheduleDependencies, *proxy.ZAIResetScheduler, *bytes.Buffer) {
	t.Helper()
	fs := fsutil.NewMemFS()
	now := time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC)
	accounts := func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{{ID: "zai_0123456789abcdef", Label: "primary", APIKey: "schedule-private-key"}}, nil
	}
	client := zaiprov.ResetClient{HTTP: zaiScheduleCommandHTTP(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("schedule controls consumed a card")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":"card","expireTime":"2026-11-02T21:00:00Z","available":true}],"weekResets":[]}}`))}, nil
	})}
	scheduler, err := proxy.NewZAIResetScheduler(fs, "/state", accounts, zaiprov.ResetExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	deps := zaiResetScheduleDependencies{Scheduler: func() (*proxy.ZAIResetScheduler, error) { return scheduler, nil }, Accounts: accounts, Client: client, Now: func() time.Time { return now }, In: strings.NewReader(""), Out: out, ErrOut: io.Discard}
	return deps, scheduler, out
}

func TestZAIResetScheduleCLIAddedExactCardDisabled(t *testing.T) {
	deps, scheduler, out := zaiScheduleCommandFixture(t)
	var cmd ZAIResetScheduleCmd
	cmd.Add.Reference = "primary"
	cmd.Add.At = "2026-11-02T20:30:00Z"
	cmd.Add.Window = "5h"
	if err := runZAIResetScheduleCommand(t.Context(), "add <reference>", cmd, true, deps); err != nil {
		t.Fatal(err)
	}
	state, err := scheduler.Store.Load()
	if err != nil || state.Enabled || len(state.Entries) != 1 || state.Entries[0].AccountID != "zai_0123456789abcdef" || state.Entries[0].CreditID != "5h:card" {
		t.Fatal("exact disabled binding missing")
	}
	if strings.Contains(out.String(), "private-key") {
		t.Fatal("schedule output exposed credentials")
	}
}

func TestZAIResetScheduleCLIEnableRequiresConfirmation(t *testing.T) {
	deps, scheduler, _ := zaiScheduleCommandFixture(t)
	if err := runZAIResetScheduleCommand(t.Context(), "enable", ZAIResetScheduleCmd{}, false, deps); err == nil {
		t.Fatal("empty confirmation enabled schedule")
	}
	state, _ := scheduler.Store.Load()
	if state.Enabled {
		t.Fatal("defaultNo enabled schedule")
	}
	var cmd ZAIResetScheduleCmd
	cmd.Enable.Yes = true
	if err := runZAIResetScheduleCommand(t.Context(), "enable", cmd, false, deps); err != nil {
		t.Fatal(err)
	}
	state, _ = scheduler.Store.Load()
	if !state.Enabled {
		t.Fatal("explicit enable ignored")
	}
}

func TestZAIResetScheduleCLIEnableShowsExactPendingCards(t *testing.T) {
	deps, _, out := zaiScheduleCommandFixture(t)
	var cmd ZAIResetScheduleCmd
	cmd.Add.Reference = "primary"
	cmd.Add.At = "2026-11-02T20:30:00Z"
	cmd.Add.Window = "5h"
	if err := runZAIResetScheduleCommand(t.Context(), "add <reference>", cmd, true, deps); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	deps.ErrOut = out
	if err := runZAIResetScheduleCommand(t.Context(), "enable", ZAIResetScheduleCmd{}, false, deps); err == nil {
		t.Fatal("defaultNo ignored")
	}
	for _, detail := range []string{"zai_0123456789abcdef", "5h:card", "2026-11-02T20:30:00Z", "pending", "[y/N]"} {
		if !strings.Contains(out.String(), detail) {
			t.Fatalf("enable confirmation omitted %s", detail)
		}
	}
}

func TestZAIResetScheduleCLIEnabledAddRequiresConfirmation(t *testing.T) {
	deps, scheduler, _ := zaiScheduleCommandFixture(t)
	_, _ = scheduler.Store.SetEnabled(true)
	var cmd ZAIResetScheduleCmd
	cmd.Add.Reference = "primary"
	cmd.Add.At = "2026-11-02T20:30:00Z"
	cmd.Add.Window = "5h"
	if err := runZAIResetScheduleCommand(t.Context(), "add <reference>", cmd, true, deps); err == nil {
		t.Fatal("live schedule changed without confirmation")
	}
	state, _ := scheduler.Store.Load()
	if len(state.Entries) != 0 {
		t.Fatal("unconfirmed entry stored")
	}
	deps.In = strings.NewReader("yes\n")
	if err := runZAIResetScheduleCommand(t.Context(), "add <reference>", cmd, true, deps); err != nil {
		t.Fatal(err)
	}
}

func TestZAIResetScheduleCLIRejectedInvalidDatesAndAccount(t *testing.T) {
	for _, at := range []string{"tomorrow", "2026-11-02T19:00:00Z", "2026-11-02T21:00:00Z"} {
		deps, scheduler, _ := zaiScheduleCommandFixture(t)
		var cmd ZAIResetScheduleCmd
		cmd.Add.Reference = "primary"
		cmd.Add.At = at
		cmd.Add.Window = "5h"
		if err := runZAIResetScheduleCommand(t.Context(), "add <reference>", cmd, true, deps); err == nil {
			t.Fatal("invalid date accepted")
		}
		state, _ := scheduler.Store.Load()
		if len(state.Entries) != 0 {
			t.Fatal("invalid entry persisted")
		}
	}
	if _, err := zaiScheduleAccount([]zaiprov.Account{{ID: "one", Label: "same"}, {ID: "two", Label: "same"}}, "same"); err == nil {
		t.Fatal("ambiguous label resolved")
	}
}

func TestZAIResetScheduleCLIStatusNeedsNoCredentials(t *testing.T) {
	deps, _, out := zaiScheduleCommandFixture(t)
	deps.Accounts = func() ([]zaiprov.Account, error) { t.Fatal("status read credentials"); return nil, nil }
	if err := runZAIResetScheduleCommand(context.Background(), "status", ZAIResetScheduleCmd{}, true, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"enabled":false`) {
		t.Fatal("disabled status missing")
	}
}
