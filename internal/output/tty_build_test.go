package output

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestBuildTTYModel_SingleProvider(t *testing.T) {
	now := time.Unix(1000, 0)
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Claude,
				Name: "claude",
				Results: []quota.Result{
					{
						Status: quota.StatusOK,
						Email:  "test@example.com",
						Plan:   "max",
						Windows: map[quota.WindowName]quota.Window{
							quota.Window5Hour: {RemainingPct: 75, ResetAtUnix: 10000},
						},
					},
				},
			},
		},
	}

	model := BuildTTYModel(report, now)

	if len(model.Sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(model.Sections))
	}

	sec := model.Sections[0]
	if sec.Separator == "" {
		t.Error("first section should have a separator")
	}
	if sec.Header == "" {
		t.Error("expected non-empty header")
	}
	if len(sec.WindowRows) != 1 {
		t.Fatalf("expected 1 window row, got %d", len(sec.WindowRows))
	}
	row := sec.WindowRows[0]
	if row.Bar == "" {
		t.Error("expected non-empty bar")
	}
	if row.Pct == "" {
		t.Error("expected non-empty pct")
	}
	if row.Reset == "" {
		t.Error("expected non-empty reset")
	}
	if row.PaceDiff == "" {
		t.Error("expected non-empty pace diff")
	}
	if row.Burndown == "" {
		t.Error("expected non-empty burndown")
	}
}

func TestBuildTTYModel_ErrorResult(t *testing.T) {
	now := time.Unix(1000, 0)
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Codex,
				Name: "codex",
				Results: []quota.Result{
					{
						Status: quota.StatusError,
						Error:  &quota.ErrorInfo{Message: "token expired"},
					},
				},
			},
		},
	}

	model := BuildTTYModel(report, now)

	if len(model.Sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(model.Sections))
	}

	sec := model.Sections[0]
	if len(sec.WindowRows) != 0 {
		t.Errorf("expected 0 window rows for error result, got %d", len(sec.WindowRows))
	}
}

func TestBuildTTYModel_WithAggregate(t *testing.T) {
	now := time.Unix(1000, 0)
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Claude,
				Name: "claude",
				Results: []quota.Result{
					{
						Status: quota.StatusOK,
						Plan:   "max",
						Windows: map[quota.WindowName]quota.Window{
							quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 10000},
						},
					},
				},
				Aggregate: &app.AggregateReport{
					Kind: "weighted_pace",
					Summary: aggregate.AccountSummary{
						Count: 2, TotalMulti: 2, Label: "2 x max",
					},
					Windows: map[quota.WindowName]quota.AggregateResult{
						quota.Window5Hour: {
							RemainingPct: 75,
							ExpectedPct:  50,
							PaceDiff:     25,
							Burndown:     3600,
						},
					},
				},
			},
		},
	}

	model := BuildTTYModel(report, now)

	if len(model.Sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(model.Sections))
	}

	sec := model.Sections[0]
	if sec.ThinSep == "" {
		t.Error("expected non-empty thin separator before aggregate")
	}
	if sec.AggHeader == "" {
		t.Error("expected non-empty aggregate header")
	}
	if len(sec.AggRows) != 1 {
		t.Fatalf("expected 1 aggregate row, got %d", len(sec.AggRows))
	}
}

func TestBuildAggRows_OverburnWithoutGapUsesEmDash(t *testing.T) {
	rows := buildAggRows(map[quota.WindowName]quota.AggregateResult{
		quota.Window5Hour: {
			RemainingPct: 20,
			ExpectedPct:  80,
			GaugePos:     0,
		},
	})

	if len(rows) != 1 {
		t.Fatalf("expected 1 aggregate row, got %d", len(rows))
	}

	pace := stripANSI(rows[0].PaceDiff)
	if strings.Contains(pace, "now") {
		t.Fatalf("expected missing gap duration to avoid now, got %q", pace)
	}
	if !strings.Contains(pace, "—") {
		t.Fatalf("expected missing gap duration to render em dash, got %q", pace)
	}

	dry := stripANSI(rows[0].Burndown)
	if strings.Contains(dry, "now") {
		t.Fatalf("expected missing gap start to avoid now, got %q", dry)
	}
	if !strings.Contains(dry, "—") {
		t.Fatalf("expected missing gap start to render em dash, got %q", dry)
	}
}

func TestBuildAggRows_OverburnImmediateGapRendersNowAndDuration(t *testing.T) {
	rows := buildAggRows(map[quota.WindowName]quota.AggregateResult{
		quota.Window5Hour: {
			RemainingPct: 20,
			ExpectedPct:  80,
			GaugePos:     0,
			GapStartS:    0,
			GapDurationS: 3600,
		},
	})

	if len(rows) != 1 {
		t.Fatalf("expected 1 aggregate row, got %d", len(rows))
	}

	pace := stripANSI(rows[0].PaceDiff)
	if !strings.Contains(pace, "1h") {
		t.Fatalf("expected immediate gap duration to render formatted duration, got %q", pace)
	}

	dry := stripANSI(rows[0].Burndown)
	if !strings.Contains(dry, "now") {
		t.Fatalf("expected immediate gap start to render now, got %q", dry)
	}
}

func TestBuildTTYModel_MultipleSections(t *testing.T) {
	now := time.Unix(1000, 0)
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Claude,
				Name: "claude",
				Results: []quota.Result{
					{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: 5000},
					}},
				},
			},
			{
				ID:   provider.Codex,
				Name: "codex",
				Results: []quota.Result{
					{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
						quota.WindowPro: {RemainingPct: 90, ResetAtUnix: 80000},
					}},
				},
			},
		},
	}

	model := BuildTTYModel(report, now)

	if len(model.Sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(model.Sections))
	}
	if model.Sections[0].Separator == "" {
		t.Error("first section should have separator")
	}
	if model.Sections[1].Separator == "" {
		t.Error("second section should have separator")
	}
}

func TestBuildTTYModel_MultiResultProviderClosingSeparator(t *testing.T) {
	now := time.Unix(1000, 0)
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Claude,
				Name: "claude",
				Results: []quota.Result{
					{Status: quota.StatusOK, Email: "a@b.com", Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: 5000},
					}},
					{Status: quota.StatusOK, Email: "c@d.com", Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 5000},
					}},
				},
			},
		},
	}

	model := BuildTTYModel(report, now)

	// Two results in one provider → 2 sections (first with separator, second without)
	if len(model.Sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(model.Sections))
	}
	if model.Sections[0].Separator == "" {
		t.Error("first section should have separator")
	}
	if model.Sections[1].Separator != "" {
		t.Error("continuation section should have no separator")
	}
	// ClosingSeparator must always be set regardless of which section is last
	if model.ClosingSeparator == "" {
		t.Error("ClosingSeparator should be set even when last section has no separator")
	}
}

func TestBuildTTYModel_EmptyProviders(t *testing.T) {
	model := BuildTTYModel(app.Report{}, time.Unix(1000, 0))
	if len(model.Sections) != 0 {
		t.Errorf("expected 0 sections, got %d", len(model.Sections))
	}
	if model.ClosingSeparator != "" {
		t.Errorf("expected empty ClosingSeparator for empty report, got %q", model.ClosingSeparator)
	}
}

func TestRenderBar_Width(t *testing.T) {
	bar := renderBar(50, greenStyle, -1)
	// Strip ANSI codes to count visible characters
	stripped := stripANSI(bar)
	if len([]rune(stripped)) != barWidth {
		t.Errorf("bar visible width = %d, want %d (stripped: %q)", len([]rune(stripped)), barWidth, stripped)
	}
}

func TestRenderBar_PaceMarker(t *testing.T) {
	bar := renderBar(50, greenStyle, 50)
	if !strings.Contains(bar, "|") {
		t.Error("expected pace marker '|' in bar")
	}
}

func TestRenderSustainGauge_Width(t *testing.T) {
	gauge := renderSustainGauge(3, false) // on-pace position
	stripped := stripANSI(gauge)
	if len([]rune(stripped)) != gaugeWidth {
		t.Errorf("gauge visible width = %d, want %d", len([]rune(stripped)), gaugeWidth)
	}
}

func TestRenderSustainGauge_Positions(t *testing.T) {
	for pos := 0; pos < gaugeWidth; pos++ {
		gauge := renderSustainGauge(pos, false)
		stripped := stripANSI(gauge)
		if len([]rune(stripped)) != gaugeWidth {
			t.Errorf("pos=%d: visible width = %d, want %d", pos, len([]rune(stripped)), gaugeWidth)
		}
		runes := []rune(stripped)
		if runes[pos] != '\u2502' {
			t.Errorf("pos=%d: expected marker at position %d, got %c", pos, pos, runes[pos])
		}
	}
}

func TestMeasuredSepWidth(t *testing.T) {
	t.Run("empty model returns 0", func(t *testing.T) {
		model := TTYModel{}
		got := measuredSepWidth(model)
		if got != 0 {
			t.Errorf("measuredSepWidth(empty) = %d, want 0", got)
		}
	})

	t.Run("measures widest row", func(t *testing.T) {
		short := TTYWindowRow{Label: "  5h  ", Bar: "bar", Pct: "50%", Reset: "1h", PaceDiff: "+1", Burndown: "2h"}
		long := TTYWindowRow{Label: "  7d  ", Bar: "longbar1234567890", Pct: "100%", Reset: "6d 23h", PaceDiff: "dry 1d 3h", Burndown: "in 1d 9h"}
		model := TTYModel{
			Sections: []TTYSection{
				{WindowRows: []TTYWindowRow{short}, AggRows: []TTYWindowRow{long}},
			},
		}
		got := measuredSepWidth(model)
		want := rowVisibleWidth(long)
		if got != want {
			t.Errorf("measuredSepWidth = %d, want %d (widest row)", got, want)
		}
	})

	t.Run("wide header wins over narrow rows", func(t *testing.T) {
		header := "  ✻   Claude max 20x · very.long.email.address.that.makes.header.wide@example-domain.com"
		narrow := TTYWindowRow{Label: "  5h  ", Bar: "bar", Pct: "50%"}
		model := TTYModel{
			Sections: []TTYSection{
				{Header: header, WindowRows: []TTYWindowRow{narrow}},
			},
		}
		got := measuredSepWidth(model)
		headerW := visibleWidth(header)
		if got != headerW {
			t.Errorf("measuredSepWidth = %d, want %d (header width)", got, headerW)
		}
	})
}

func TestHeaderVisibleWidth(t *testing.T) {
	tests := []struct {
		name string
		r    quota.Result
		id   provider.ID
		want int
	}{
		{
			name: "no plan no email",
			r:    quota.Result{},
			id:   provider.Claude,
			want: 12,
		},
		{
			name: "with plan only",
			r:    quota.Result{Plan: "max"},
			id:   provider.Claude,
			want: 12 + 1 + len("max"), // " max"
		},
		{
			name: "plan with multiplier",
			r:    quota.Result{Plan: "max", RateLimitTier: "default_claude_max_20x"},
			id:   provider.Claude,
			want: 12 + 1 + len("max") + len(" 20x"),
		},
		{
			name: "plan with email",
			r:    quota.Result{Plan: "max", Email: "a@b.com"},
			id:   provider.Claude,
			want: 12 + 1 + len("max") + 3 + len("a@b.com"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := headerVisibleWidth(tt.r, tt.id)
			if got != tt.want {
				t.Errorf("headerVisibleWidth() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestProviderIcon(t *testing.T) {
	tests := []struct {
		id   provider.ID
		want string
	}{
		{provider.Claude, "\u273B"},
		{provider.Codex, "\uF120"},
		{provider.Gemini, "\uF51B"},
		{provider.ID("unknown"), "\u25CF"},
		{provider.ID(""), "\u25CF"},
	}
	for _, tt := range tests {
		got := providerIcon(tt.id)
		if got != tt.want {
			t.Errorf("providerIcon(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestProviderDisplayName(t *testing.T) {
	tests := []struct {
		id   provider.ID
		want string
	}{
		{provider.Claude, "Claude"},
		{provider.Codex, "Codex"},
		{provider.Gemini, "Gemini"},
		{provider.ID(""), ""},
	}
	for _, tt := range tests {
		got := providerDisplayName(tt.id)
		if got != tt.want {
			t.Errorf("providerDisplayName(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestRenderBar_ClampAbove100(t *testing.T) {
	// renderBar(150, ...) must not panic and must produce a bar of barWidth visible chars.
	bar := renderBar(150, greenStyle, -1)
	stripped := stripANSI(bar)
	if len([]rune(stripped)) != barWidth {
		t.Errorf("renderBar(150) visible width = %d, want %d", len([]rune(stripped)), barWidth)
	}
	// All characters should be filled (█ / ━ style), not empty (╌ style).
	// The bar at 100% has no dim characters — verify no ╌ present.
	if strings.Contains(stripped, "\u254C") {
		t.Error("renderBar(150) should be fully filled but contains dim char ╌")
	}
}

func TestRenderBar_ClampBelowZero(t *testing.T) {
	// renderBar(-10, ...) must not panic and must produce a bar of barWidth visible chars.
	bar := renderBar(-10, greenStyle, -1)
	stripped := stripANSI(bar)
	if len([]rune(stripped)) != barWidth {
		t.Errorf("renderBar(-10) visible width = %d, want %d", len([]rune(stripped)), barWidth)
	}
	// The bar at 0% should be all empty characters — no filled chars (━).
	if strings.Contains(stripped, "\u2501") {
		t.Error("renderBar(-10) should be empty but contains filled char ━")
	}
}

func TestRenderSustainGauge_Unknown(t *testing.T) {
	// renderSustainGauge(-1, false) must not panic and must not contain a
	// marker character — pos -1 means "unknown", so no marker is placed.
	gauge := renderSustainGauge(-1, false)
	stripped := stripANSI(gauge)
	if strings.Contains(stripped, "\u2502") {
		t.Errorf("renderSustainGauge(-1) should have no marker, got %q", stripped)
	}
	if len([]rune(stripped)) != gaugeWidth {
		t.Errorf("renderSustainGauge(-1) visible width = %d, want %d", len([]rune(stripped)), gaugeWidth)
	}
}

func TestRenderBarMarkerOverlapsFilled(t *testing.T) {
	// When pct equals expectedPct, the marker should replace a filled cell.
	bar := renderBar(50, lipgloss.Style{}, 50)
	stripped := stripANSI(bar)
	if len([]rune(stripped)) != barWidth {
		t.Errorf("renderBar(50, 50) visible width = %d, want %d", len([]rune(stripped)), barWidth)
	}
	// The marker "|" must appear exactly once.
	count := strings.Count(stripped, "|")
	if count != 1 {
		t.Errorf("renderBar(50, 50) marker count = %d, want 1; bar = %q", count, stripped)
	}
}

func TestRenderBarMarkerAt100Pct(t *testing.T) {
	// When expectedPct is 100, the marker is clamped to the last position.
	bar := renderBar(100, lipgloss.Style{}, 100)
	stripped := stripANSI(bar)
	if len([]rune(stripped)) != barWidth {
		t.Errorf("renderBar(100, 100) visible width = %d, want %d", len([]rune(stripped)), barWidth)
	}
	runes := []rune(stripped)
	if runes[barWidth-1] != '|' {
		t.Errorf("renderBar(100, 100) last char = %q, want '|'", runes[barWidth-1])
	}
}

func TestOrderedWindowKeys_DynamicWindowOrder(t *testing.T) {
	windows := map[quota.WindowName]quota.Window{
		quota.Window5Hour:                                 {},
		quota.Window7Day:                                  {},
		quota.WindowName("7d:sonnet"):                    {},
		quota.WindowName("5h:gpt-5.3-codex-spark"):       {},
		quota.WindowName("7d:gpt-5.3-codex-spark"):       {},
		quota.WindowPro:                                   {},
	}

	got := orderedWindowKeys(windows)
	want := []quota.WindowName{
		quota.Window5Hour,
		quota.Window7Day,
		quota.WindowName("7d:sonnet"),
		quota.WindowName("5h:gpt-5.3-codex-spark"),
		quota.WindowName("7d:gpt-5.3-codex-spark"),
		quota.WindowPro,
	}

	if len(got) != len(want) {
		t.Fatalf("orderedWindowKeys length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderedWindowKeys[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildTTYModel_ModelScopedWindowsUseBucketHeaders(t *testing.T) {
	now := time.Unix(1000, 0)
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Codex,
				Name: "codex",
				Results: []quota.Result{
					{
						Status: quota.StatusOK,
						Plan:   "plus",
						Windows: map[quota.WindowName]quota.Window{
							quota.Window5Hour:                           {RemainingPct: 75, ResetAtUnix: 10000},
							quota.Window7Day:                            {RemainingPct: 90, ResetAtUnix: 20000},
							quota.WindowName("7d:sonnet"):              {RemainingPct: 70, ResetAtUnix: 30000},
							quota.WindowName("5h:gpt-5.3-codex-spark"): {RemainingPct: 65, ResetAtUnix: 11000},
							quota.WindowName("7d:gpt-5.3-codex-spark"): {RemainingPct: 85, ResetAtUnix: 21000},
						},
					},
				},
			},
		},
	}

	model := BuildTTYModel(report, now)
	sec := model.Sections[0]

	labels := make([]string, 0, len(sec.WindowRows))
	for _, row := range sec.WindowRows {
		labels = append(labels, strings.TrimSpace(stripANSI(row.Label)))
	}

	want := []string{"5h", "7d", "sonnet", "7d", "gpt-5.3-codex-spark", "5h", "7d"}
	if len(labels) != len(want) {
		t.Fatalf("window row count = %d, want %d (%v)", len(labels), len(want), labels)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Fatalf("labels[%d] = %q, want %q (all=%v)", i, labels[i], want[i], labels)
		}
	}

	if strings.Contains(sec.WindowRows[2].Label, "\x1b[3m") || strings.Contains(sec.WindowRows[4].Label, "\x1b[3m") {
		t.Fatalf("bucket header labels should not be italic: %q / %q", sec.WindowRows[2].Label, sec.WindowRows[4].Label)
	}
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !((s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= 'a' && s[j] <= 'z')) {
					j++
				}
				if j < len(s) {
					j++ // skip the final letter
				}
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
