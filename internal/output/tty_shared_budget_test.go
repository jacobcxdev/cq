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
