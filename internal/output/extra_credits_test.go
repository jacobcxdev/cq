package output

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
	"github.com/muesli/termenv"
)

func TestCodexExtraCreditsHeaderStates(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	for _, tc := range []struct{ name, fields, glyph, style string }{
		{"available", `"enabled":true,"available":true`, "\U000F13D1", greenStyle.Render("\U000F13D1")},
		{"empty", `"enabled":true,"available":false`, "\U000F0FAF", redStyle.Render("\U000F0FAF")},
		{"unknown", `"enabled":true,"available":null`, "\U000F0FAF", redStyle.Render("\U000F0FAF")},
		{"disabled with credits", `"enabled":false,"available":true`, "\U000F05E4", boldRedStyle.Render("\U000F05E4")},
		{"disabled empty", `"enabled":false,"available":false`, "\U000F05E4", boldRedStyle.Render("\U000F05E4")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result quota.Result
			if err := json.Unmarshal([]byte(`{"status":"ok","email":"account@example.test","extra_credits":{`+tc.fields+`}}`), &result); err != nil {
				t.Fatal(err)
			}
			model := BuildTTYModel(app.Report{Providers: []app.ProviderReport{
				{ID: provider.Codex, Results: []quota.Result{result}},
				{ID: provider.Claude, Results: []quota.Result{result}},
			}}, time.Unix(0, 0))
			header := model.Sections[0].Header
			if want := "   · " + tc.glyph + "    Codex · account@example.test"; stripANSI(header) != want || !strings.Contains(header, tc.style) || tc.style == tc.glyph {
				t.Fatalf("header = %q, want %q with style %q", header, want, tc.style)
			}
			if got := stripANSI(model.Sections[1].Header); got != "  ✻        Claude · account@example.test" {
				t.Fatalf("non-Codex header = %q, want a blank status slot", got)
			}
		})
	}
}
