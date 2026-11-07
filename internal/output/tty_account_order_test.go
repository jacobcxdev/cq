package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexDisplayOrderUsesValueThenTierThenReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rows := []quota.Result{
		{Email: "ordinary-50@example.com", Plan: "promax"},
		{Email: "pooled-business@example.com", Plan: "self_serve_business_prolite"},
		{Email: "system@example.com", Plan: "plus", Active: true},
		{Email: "pooled-later@example.com", Plan: "pro"},
		{Email: "pooled-sooner@example.com", Plan: "pro"},
		{Email: "ordinary-20@example.com", Plan: "pro"},
		{Email: "maximum-value@example.com", Plan: "promax"},
		{Email: "unknown-value@example.com", Plan: "promax"},
		{Email: "ordinary-unknown-tier@example.com", Plan: "promax", RateLimitTier: "custom"},
	}
	for i := range rows {
		rows[i].Status = quota.StatusOK
		rows[i].Windows = map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 50, ResetAtUnix: now.Add(time.Duration(i+1) * time.Hour).Unix()}}
	}
	rows[3].Windows[quota.Window7Day] = quota.Window{RemainingPct: 50, ResetAtUnix: now.Add(12 * time.Hour).Unix()}
	report := app.Report{Providers: []app.ProviderReport{{
		ID: provider.Codex, Results: rows,
		DisplayAccountValues: map[int]uint32{0: 0, 1: 10, 2: 0, 3: 10, 4: 10, 5: 0, 6: ^uint32(0), 8: 0},
	}}}
	before, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	model := BuildTTYModel(report, now)
	assertAccountHeaderOrder(t, model, []string{
		"system@example.com", "maximum-value@example.com", "pooled-sooner@example.com",
		"pooled-later@example.com", "pooled-business@example.com", "ordinary-50@example.com",
		"ordinary-20@example.com", "ordinary-unknown-tier@example.com", "unknown-value@example.com",
	})
	after, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("TTY sorting mutated report data or JSON order")
	}
}

func TestAccountDisplayResetUsesLimitingSharedWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	window := func(pct int, hours int64) quota.Window {
		return quota.Window{RemainingPct: pct, ResetAtUnix: now.Add(time.Duration(hours) * time.Hour).Unix()}
	}
	for _, tc := range []struct {
		name string
		a, b map[quota.WindowName]quota.Window
		want []string
	}{
		{"weekly limits despite earlier session reset", map[quota.WindowName]quota.Window{quota.Window5Hour: window(90, 1), quota.Window7Day: window(5, 72)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(5, 24)}, []string{"b@example.com", "a@example.com"}},
		{"session limits", map[quota.WindowName]quota.Window{quota.Window5Hour: window(2, 1), quota.Window7Day: window(80, 72)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(2, 24)}, []string{"a@example.com", "b@example.com"}},
		{"scoped exhaustion does not replace shared limit", map[quota.WindowName]quota.Window{quota.Window7Day: window(80, 72), "5h:special": window(0, 1)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(80, 24)}, []string{"b@example.com", "a@example.com"}},
		{"all exhausted shared limits must reset", map[quota.WindowName]quota.Window{quota.Window5Hour: window(0, 1), quota.Window7Day: window(0, 72)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(0, 24)}, []string{"b@example.com", "a@example.com"}},
		{"missing limiting reset stays unknown", map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 2}, quota.Window7Day: window(90, 72)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(2, 24)}, []string{"b@example.com", "a@example.com"}},
		{"expired reset stays unknown", map[quota.WindowName]quota.Window{quota.Window7Day: window(0, -1)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(0, 24)}, []string{"b@example.com", "a@example.com"}},
		{"scoped-only account", map[quota.WindowName]quota.Window{"5h:special": window(10, 1)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(10, 24)}, []string{"a@example.com", "b@example.com"}},
		{"identity breaks equal reset tie", map[quota.WindowName]quota.Window{quota.Window7Day: window(10, 24)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(10, 24)}, []string{"a@example.com", "b@example.com"}},
		{"equal positive limits use earlier reset", map[quota.WindowName]quota.Window{quota.Window5Hour: window(10, 1), quota.Window7Day: window(10, 72)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(10, 24)}, []string{"a@example.com", "b@example.com"}},
		{"unknown exhausted reset prevents recovery estimate", map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}, quota.Window7Day: window(0, 1)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(0, 24)}, []string{"b@example.com", "a@example.com"}},
		{"no windows stays unknown", nil, map[quota.WindowName]quota.Window{quota.Window7Day: window(10, 24)}, []string{"b@example.com", "a@example.com"}},
		{"invalid percentage does not limit", map[quota.WindowName]quota.Window{quota.Window5Hour: window(-1, 1), quota.Window7Day: window(10, 72)}, map[quota.WindowName]quota.Window{quota.Window7Day: window(10, 24)}, []string{"b@example.com", "a@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []provider.ID{provider.Claude, provider.Codex, provider.Gemini, provider.ZAI, provider.Grok} {
				t.Run(string(id), func(t *testing.T) {
					rows := []quota.Result{{Email: "b@example.com", Plan: "pro", Status: quota.StatusOK, Windows: tc.b}, {Email: "a@example.com", Plan: "pro", Status: quota.StatusOK, Windows: tc.a}}
					model := BuildTTYModel(app.Report{Providers: []app.ProviderReport{{ID: id, Results: rows}}}, now)
					assertAccountHeaderOrder(t, model, tc.want)
				})
			}
		})
	}
}

func TestAccountDisplayOrderAppliesAcrossProviders(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, id := range []provider.ID{provider.Claude, provider.Codex, provider.Gemini, provider.ZAI, provider.Grok} {
		t.Run(string(id), func(t *testing.T) {
			rows := []quota.Result{
				{Email: "low@example.com", Plan: "same", RateLimitTier: "reported_1x"},
				{Email: "late@example.com", Plan: "same", RateLimitTier: "reported_20x"},
				{Email: "current@example.com", Plan: "same", Active: true},
				{Email: "early@example.com", Plan: "same", RateLimitTier: "reported_20x"},
				{Email: "unknown@example.com", Plan: "same", RateLimitTier: "custom"},
				{Email: "pool@example.com", Plan: "same", RateLimitTier: "reported_1x"},
			}
			for i := range rows {
				rows[i].Status = quota.StatusOK
				rows[i].Windows = map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 50, ResetAtUnix: now.Add(12 * time.Hour).Unix()}}
			}
			rows[3].Windows[quota.Window7Day] = quota.Window{RemainingPct: 50, ResetAtUnix: now.Add(time.Hour).Unix()}
			report := app.Report{Providers: []app.ProviderReport{{ID: id, Results: rows, DisplayAccountValues: map[int]uint32{0: 0, 1: 0, 2: 0, 3: 0, 4: 0, 5: 10}}}}
			before, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			assertAccountHeaderOrder(t, BuildTTYModel(report, now), []string{"current@example.com", "pool@example.com", "early@example.com", "late@example.com", "low@example.com", "unknown@example.com"})
			after, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("TTY sorting changed source report or JSON order")
			}
		})
	}
}

func TestAccountDisplayOrderUsesProviderSpecificTiers(t *testing.T) {
	for _, tc := range []struct {
		id   provider.ID
		rows []quota.Result
		want []string
	}{
		{provider.Claude, []quota.Result{
			{AccountID: "pro", Plan: "pro", RateLimitTier: "default_claude_max_1x"},
			{AccountID: "max5", Plan: "max", RateLimitTier: "default_claude_max_5x"},
			{AccountID: "max20", Plan: "max", RateLimitTier: "default_claude_max_20x"},
			{AccountID: "unknown", Plan: "max", RateLimitTier: "unfamiliar"},
		}, []string{"max20", "max5", "pro", "unknown"}},
		{provider.Claude, []quota.Result{
			{AccountID: "z", Tier: "PRO"}, {AccountID: "a", Plan: "pro"},
			{AccountID: "max", Tier: "max"},
		}, []string{"max", "a", "z"}},
		{provider.ZAI, []quota.Result{
			{AccountID: "start", Plan: "ZCode Start"}, {AccountID: "pro", Plan: "Pro"},
			{AccountID: "lite", Plan: "Lite"}, {AccountID: "max", Plan: "Max"},
		}, []string{"lite", "max", "pro", "start"}},
		{provider.Grok, []quota.Result{
			{AccountID: "pro", Plan: "SuperGrokPro"}, {AccountID: "base", Plan: "SuperGrok"},
			{AccountID: "heavy", Plan: "SuperGrok Heavy"},
		}, []string{"base", "heavy", "pro"}},
		{provider.Gemini, []quota.Result{{AccountID: "only", Tier: "Antigravity", Active: true}}, []string{"only"}},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			got := accountDisplayOrder(app.ProviderReport{ID: tc.id, Results: tc.rows}, 1_800_000_000)
			for i, want := range tc.want {
				if got[i].AccountID != want {
					t.Errorf("row %d = %q, want %q", i, got[i].AccountID, want)
				}
			}
		})
	}
}

func TestCodexDisplayOrderGroupsEqualCapacityTiersAndIdentity(t *testing.T) {
	rows := []quota.Result{
		{AccountID: "z", Email: "same@example.com", Plan: "plus"},
		{AccountID: "a", Email: "SAME@example.com", Plan: "plus"},
		{AccountID: "business", Email: "z@example.com", Plan: "team"},
	}
	ordered := accountDisplayOrder(app.ProviderReport{ID: provider.Codex, Results: rows}, 1_800_000_000)
	for i, want := range []string{"business", "a", "z"} {
		if ordered[i].AccountID != want {
			t.Errorf("row %d account = %q, want %q", i, ordered[i].AccountID, want)
		}
	}
}

func TestCodexDisplayOrderUsesFreshSystemIdentityForCachedRows(t *testing.T) {
	rows := []quota.Result{
		{Email: "old@example.com", Plan: "promax", Active: true},
		{Email: "current@example.com", Plan: "plus"},
	}
	report := app.Report{Providers: []app.ProviderReport{{
		ID: provider.Codex, Results: rows,
		DisplayActiveAccounts: map[int]bool{0: false, 1: true},
	}}}
	before, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountHeaderOrder(t, BuildTTYModel(report, time.Unix(1_800_000_000, 0)), []string{"current@example.com", "old@example.com"})
	after, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("fresh display identity changed cached report JSON")
	}
}

func TestCodexDisplayResetPreservesUpstreamPrecision(t *testing.T) {
	precise := 0.4
	result := quota.Result{Windows: map[quota.WindowName]quota.Window{
		quota.Window5Hour: {RemainingPct: 0, RemainingPctExact: &precise, ResetAtUnix: 1_800_003_600},
		quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: 1_800_086_400},
	}}
	if got := accountDisplayReset(result, 1_800_000_000); got != 1_800_086_400 {
		t.Fatalf("limiting reset = %d, want exhausted weekly reset", got)
	}
	result.Windows[quota.Window7Day] = quota.Window{RemainingPct: 1, ResetAtUnix: 1_800_086_400}
	if got := accountDisplayReset(result, 1_800_000_000); got != 1_800_003_600 {
		t.Fatalf("limiting reset = %d, want precise session reset", got)
	}
}

func assertAccountHeaderOrder(t *testing.T, model TTYModel, emails []string) {
	t.Helper()
	if len(model.Sections) != len(emails) {
		t.Fatalf("sections = %d, want %d", len(model.Sections), len(emails))
	}
	for i, email := range emails {
		if !strings.Contains(model.Sections[i].Header, email) {
			t.Errorf("section %d = %q, want account %s", i, model.Sections[i].Header, email)
		}
	}
}
