package output

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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
			header, _ := buildResultBlock(result, provider.Codex, 0, 7)
			if !strings.HasSuffix(stripANSI(header), " · "+tc.glyph) || !strings.Contains(header, tc.style) {
				t.Fatalf("header = %q, want suffix glyph %q with style %q", header, tc.glyph, tc.style)
			}
			if tc.style == tc.glyph {
				t.Fatal("test must exercise ANSI colour/bold")
			}
			header, _ = buildResultBlock(result, provider.Claude, 0, 7)
			if strings.Contains(header, tc.glyph) {
				t.Fatal("credit marker leaked to non-Codex provider")
			}
		})
	}
}
