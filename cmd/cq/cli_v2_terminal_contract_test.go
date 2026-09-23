package main

import (
	"github.com/charmbracelet/x/ansi"
	"os"
	"path/filepath"
	"testing"
)

// terminalFixtureRegistration records the independent authored contract, including
// stream identity where a scenario has both a receipt and diagnostics.
type terminalFixtureRegistration struct{ Command, Scenario, Stream, File string }

func assertTerminalFixture(t *testing.T, relative string, got string) {
	t.Helper()
	path := filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", relative)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if ansi.Strip(got) != string(want) {
		t.Fatalf("terminal fixture %s differs\nwant:\n%s\ngot:\n%s", relative, want, ansi.Strip(got))
	}
}
