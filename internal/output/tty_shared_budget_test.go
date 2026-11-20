package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestSharedBudgetTTYAlignment(t *testing.T) {
	now := time.Unix(1000, 0)
	openCode := quota.SharedBudgetReport{
		ProviderID: "opencode-go", ProviderName: "OpenCode", Plan: "Go",
		Icon: "\U000F0C15", Email: "member@example.com", ExtraUsage: &quota.ExtraCredits{},
		Windows: []quota.SharedBudgetWindow{
			{Name: "5h", RemainingPct: 0, ResetAtUnix: now.Add(3 * time.Hour).Unix()},
			{Name: "7d", RemainingPct: 0.25},
			{Name: "1mo", RemainingPct: 100},
		},
	}
	model := BuildTTYModel(app.Report{
		Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{{
			Status: quota.StatusOK, Plan: "plus",
			Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 75}},
		}}}},
		SharedBudgetSubscriptions: []quota.SharedBudgetReport{openCode, {
			ProviderID: "test-subscription", ProviderName: "Another", Plan: "Standard", Icon: "界",
		}},
	}, now)
	if len(model.Sections) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(model.Sections))
	}
	codex, shared, unavailable := model.Sections[0], model.Sections[1], model.Sections[2]
	if got := stripANSI(shared.Header); got != "  󰰕 · 󰗤    OpenCode Go · member@example.com" {
		t.Fatalf("unexpected OpenCode header: %q", got)
	}
	column := func(header, name string) int {
		before, _, found := strings.Cut(stripANSI(header), name)
		if !found {
			t.Fatalf("%q missing from header %q", name, header)
		}
		return lipgloss.Width(before)
	}
	if got, want := column(shared.Header, "OpenCode"), column(codex.Header, "Codex"); got != want {
		t.Errorf("provider name starts at cell %d, want %d", got, want)
	}
	if got, want := column(unavailable.Header, "Another"), column(codex.Header, "Codex"); got != want {
		t.Errorf("wide icon shifted provider name to cell %d, want %d", got, want)
	}
	if got := stripANSI(unavailable.Header); !strings.HasSuffix(got, "Another Standard · Allowance unavailable") || strings.Contains(got, "󰗤") || len(unavailable.WindowRows) != 0 {
		t.Errorf("unexpected unavailable section: %#v", unavailable)
	}
	if shared.AggHeader != "" || shared.ProxyHeader != "" || len(shared.WindowRows) != 3 {
		t.Fatalf("unexpected allowance section: %#v", shared)
	}
	var terminal bytes.Buffer
	if err := writeTTY(&terminal, model); err != nil {
		t.Fatal(err)
	}
	base := codex.WindowRows[0]
	for i, row := range shared.WindowRows {
		if got := strings.TrimSpace(stripANSI(row.Label)); got != openCode.Windows[i].Name {
			t.Errorf("window %d: label = %q, want %q", i, got, openCode.Windows[i].Name)
		}
		if lipgloss.Width(row.Label) != lipgloss.Width(base.Label) || lipgloss.Width(row.Bar) != lipgloss.Width(base.Bar) || lipgloss.Width(row.Pct) != lipgloss.Width(base.Pct) || lipgloss.Width(row.Reset) != lipgloss.Width(base.Reset) {
			t.Errorf("window %d: allowance columns do not align with Codex", i)
		}
		if row.PaceDiff != "" || row.Burndown != "" || strings.Contains(row.Bar, "|") {
			t.Errorf("window %d: contains a forecast: %#v", i, row)
		}
		line := stripANSI(row.Label + row.Bar + "  " + row.Pct + "  " + row.Reset)
		if !strings.Contains(stripANSI(terminal.String()), line+"\n") || lipgloss.Width(line) != rowVisibleWidth(row) {
			t.Errorf("window %d: measured row differs from rendered row: %q", i, line)
		}
	}
	if !strings.HasSuffix(stripANSI(shared.WindowRows[0].Pct), "  0%") || !strings.HasSuffix(stripANSI(shared.WindowRows[0].Reset), "3h") {
		t.Errorf("exhausted window lost its percentage or reset: %#v", shared.WindowRows[0])
	}
	if !strings.HasSuffix(stripANSI(shared.WindowRows[1].Pct), "<1%") || !strings.HasSuffix(stripANSI(shared.WindowRows[1].Reset), "—") {
		t.Errorf("low allowance or unknown reset rendered incorrectly: %#v", shared.WindowRows[1])
	}
}

func TestSharedBudgetTTYMultipleAccounts(t *testing.T) {
	model := BuildTTYModel(app.Report{SharedBudgetSubscriptions: []quota.SharedBudgetReport{
		{ProviderID: "opencode-go", ProviderName: "OpenCode", Plan: "Go", Icon: "\U000F0C15", Email: "first@example.test"},
		{ProviderID: "opencode-go", ProviderName: "OpenCode", Plan: "Go", Icon: "\U000F0C15", Email: "second@example.test"},
	}}, time.Unix(1000, 0))
	if len(model.Sections) != 2 || model.Sections[0].Separator == "" || model.Sections[1].Separator != "" ||
		model.Sections[1].AggHeader != "" ||
		!strings.Contains(stripANSI(model.Sections[0].Header), "first@example.test") ||
		!strings.Contains(stripANSI(model.Sections[1].Header), "second@example.test") {
		t.Fatalf("multi-account OpenCode layout = %+v", model.Sections)
	}
}

func TestSharedBudgetTTYDisplaysAllowanceAggregate(t *testing.T) {
	now := time.Unix(1000, 0)
	account := func(windows ...quota.SharedBudgetWindow) quota.SharedBudgetReport {
		return quota.SharedBudgetReport{
			ProviderID: "opencode-go", ProviderName: "OpenCode", Plan: "Go",
			Icon: "\U000F0C15", Windows: windows,
		}
	}
	model := BuildTTYModel(app.Report{
		SharedBudgetSubscriptions: []quota.SharedBudgetReport{
			account(quota.SharedBudgetWindow{Name: "5h", RemainingPct: 100, ResetAtUnix: 1300}),
			account(), account(), account(),
			{ProviderID: "another", ProviderName: "Another"},
		},
		SharedBudgetAggregates: map[string]app.SharedBudgetAggregateReport{
			"opencode-go": {
				AccountCount: 4,
				Windows: []app.SharedBudgetAggregateWindow{
					{Name: "5h", MeanRemainingPct: 43.33, ReportedAccounts: 3, NonZeroAccounts: 2, NextResetAtUnix: now.Add(2*time.Hour + 27*time.Minute).Unix()},
					{Name: "7d", MeanRemainingPct: 0.25, ReportedAccounts: 2, NonZeroAccounts: 1, NextResetAtUnix: now.Add(45 * time.Minute).Unix()},
					{Name: "1mo", MeanRemainingPct: 50, ReportedAccounts: 4, NonZeroAccounts: 3},
				},
			},
		},
	}, now)

	for _, i := range []int{0, 1, 2, 4} {
		if model.Sections[i].AggHeader != "" {
			t.Fatalf("unexpected aggregate on section %d", i)
		}
	}
	summary := model.Sections[3]
	if got := stripANSI(summary.AggHeader); got != "  󰰕        OpenCode 4 accounts · mean allowance" || summary.ThinSep == "" {
		t.Fatalf("aggregate header = %q", got)
	}

	var terminal bytes.Buffer
	if err := writeTTY(&terminal, model); err != nil {
		t.Fatal(err)
	}
	wants := []struct{ name, pct, reset string }{
		{"5h", "43%", "󰦖  2h 27m"},
		{"7d", "<1%", "󰦖     45m"},
		{"1mo", "50%", "󰦖       —"},
	}
	if len(summary.AggRows) != len(wants) {
		t.Fatalf("aggregate row count = %d, want %d", len(summary.AggRows), len(wants))
	}
	base := model.Sections[0].WindowRows[0]
	for i, want := range wants {
		row := summary.AggRows[i]
		if strings.TrimSpace(stripANSI(row.Label)) != want.name || !strings.HasSuffix(stripANSI(row.Pct), want.pct) ||
			stripANSI(row.Reset) != want.reset || row.PaceDiff != "" || row.Burndown != "" || strings.Contains(row.Bar, "|") {
			t.Errorf("unexpected aggregate row for %s: %#v", want.name, row)
		}
		if visibleWidth(row.Label+row.Bar+row.Pct+row.Reset) != visibleWidth(base.Label+base.Bar+base.Pct+base.Reset) {
			t.Errorf("aggregate columns did not align for %s", want.name)
		}
		line := stripANSI(row.Label + row.Bar + "  " + row.Pct + "  " + row.Reset)
		if !strings.Contains(stripANSI(terminal.String()), line+"\n") || rowVisibleWidth(row) != visibleWidth(line) {
			t.Errorf("aggregate row for %s did not render with measured width: %q", want.name, line)
		}
	}
}

func TestSharedBudgetJSONKeepsExactAllowance(t *testing.T) {
	var out bytes.Buffer
	renderer := &JSONRenderer{W: &out}
	report := app.Report{SharedBudgetSubscriptions: []quota.SharedBudgetReport{{
		ProviderID: "opencode-go", ProviderName: "OpenCode", Plan: "Go",
		Email: "member@example.com", Icon: "\U000F0C15", ExtraUsage: &quota.ExtraCredits{},
		Windows: []quota.SharedBudgetWindow{
			{Name: "5h", RemainingPct: 0, ResetAtUnix: 1010},
			{Name: "7d", RemainingPct: 84.75},
		},
	}, {ProviderID: "test-subscription", ProviderName: "Another"}}}
	if err := renderer.Render(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Subscriptions []map[string]json.RawMessage `json:"shared_budget_subscriptions"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Subscriptions) != 2 {
		t.Fatalf("expected two subscriptions: %s", out.String())
	}
	first, second := parsed.Subscriptions[0], parsed.Subscriptions[1]
	if got := string(first["windows"]); got != `[{"name":"5h","remaining_pct":0,"reset_at_unix":1010},{"name":"7d","remaining_pct":84.75}]` {
		t.Errorf("unexpected allowance windows: %s", got)
	}
	if first["extra_usage"] == nil || first["icon"] != nil || second["windows"] != nil || second["extra_usage"] != nil {
		t.Errorf("unexpected subscription fields: %s", out.String())
	}
}
