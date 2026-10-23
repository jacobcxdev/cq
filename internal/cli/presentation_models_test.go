package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTerminalModelsCloneDiagnosticBreak(t *testing.T) {
	message := "Native clone source codex/missing-code was not found; run cq models refresh and cq models list --provider codex."
	for _, width := range []int{20, 40, 80, 100, 120} {
		got := terminalDiagnostics("models overlay add", Outcome{Errors: []Diagnostic{{Code: "models_clone_not_found", Message: message}}}, TerminalOptions{Width: width, Colour: true})
		if width >= 100 {
			want := "Error · Native clone source codex/missing-code was not found;\n  run cq models refresh and cq models list --provider codex.\n  Code: models_clone_not_found\n  Command: cq models overlay add\n"
			if ansi.Strip(got) != want {
				t.Fatalf("width %d: %q", width, ansi.Strip(got))
			}
		}
		if !strings.Contains(got, "\x1b[36mcq models") || !strings.Contains(got, "\x1b[31mError") {
			t.Fatal("diagnostic roles", got)
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d: %q", width, line)
			}
		}
	}
	other := terminalDiagnostics("models overlay add", Outcome{Errors: []Diagnostic{{Code: "models_store_failed", Message: message}}}, TerminalOptions{Width: 100})
	if strings.Contains(other, "\n  run ") || !strings.Contains(other, "found; run cq models refresh") {
		t.Fatal("other diagnostic changed", other)
	}
}
