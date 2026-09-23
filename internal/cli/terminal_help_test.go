package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTerminalHelpPresentation(t *testing.T) {
	for _, path := range []string{"", "service install", "codex proxy reserve set"} {
		t.Run(path, func(t *testing.T) {
			var out bytes.Buffer
			session := &Session{Out: &out, Err: &bytes.Buffer{}, OutTerminal: TerminalOptions{Width: 80, Colour: true}}
			args := append([]string{"help"}, strings.Fields(path)...)
			exit := Run(context.Background(), args, session, func(string) (Handler, bool) { t.Fatal("help accessed handlers"); return nil, false })
			if exit != 0 {
				t.Fatalf("exit %d", exit)
			}
			got := out.String()
			if !strings.Contains(got, "\x1b[") || strings.Contains(got, "Required: false") || strings.Contains(got, "Repeatable: false") {
				t.Fatalf("unformatted help: %s", got)
			}
			if !strings.Contains(got, "Usage") || !strings.Contains(got, "Examples") {
				t.Fatalf("missing help sections: %s", got)
			}
			for _, line := range strings.Split(got, "\n") {
				if ansi.StringWidth(line) > 80 {
					t.Errorf("wide line: %q", line)
				}
			}
		})
	}
}

func TestHelpPlainReferencePreserved(t *testing.T) {
	var out bytes.Buffer
	session := &Session{Out: &out, Err: &bytes.Buffer{}}
	if Run(context.Background(), []string{"help", "service", "install"}, session, nil) != 0 {
		t.Fatal("help failed")
	}
	want, _ := Help("service install")
	if out.String() != want {
		t.Fatal("redirected reference help changed")
	}
}

func TestTerminalHelpAllPagesAndWidths(t *testing.T) {
	for path := range terminalHelpByPath {
		for _, width := range []int{20, 40, 80, 120} {
			plain, ok := terminalHelp(path, width, false)
			if !ok || strings.Contains(plain, "\x1b") {
				t.Fatalf("bad plain help %q", path)
			}
			coloured, _ := terminalHelp(path, width, true)
			if ansi.Strip(coloured) != plain {
				t.Fatalf("colour changed content %q", path)
			}
			for _, line := range strings.Split(plain, "\n") {
				if ansi.StringWidth(line) > min(width, 100) {
					t.Fatalf("%q width %d: %q", path, width, line)
				}
			}
		}
	}
}

func TestTerminalHelpKeepsConstraints(t *testing.T) {
	got, _ := terminalHelp("codex proxy reserve set", 80, false)
	for _, want := range []string{"--window", "--percent", "Examples"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	root, _ := terminalHelp("", 80, false)
	for _, want := range []string{"Quotas", "Providers", "Operations", "Utilities"} {
		if !strings.Contains(root, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestAuthoredHelpFixtures(t *testing.T) {
	var index struct {
		Fixtures []struct{ Scenario, Invocation, File string }
	}
	base := filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples")
	data, err := os.ReadFile(filepath.Join(base, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, fixture := range index.Fixtures {
		if fixture.Scenario != "Help" {
			continue
		}
		count++
		t.Run(fixture.Invocation, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(base, fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Fields(fixture.Invocation)[1:]
			short := append([]string{}, args...)
			short[len(short)-1] = "-h"
			help := append([]string{"help"}, args[:len(args)-1]...)
			for _, argv := range [][]string{args, short, help} {
				var out bytes.Buffer
				session := &Session{Out: &out, Err: &bytes.Buffer{}, OutTerminal: TerminalOptions{Width: 100, Colour: true}}
				exit := Run(context.Background(), argv, session, func(string) (Handler, bool) { t.Fatal("pure help consulted handlers"); return nil, false })
				if exit != 0 || ansi.Strip(out.String()) != string(want) {
					t.Fatalf("fixture %s args %v mismatch\n%s", fixture.File, argv, ansi.Strip(out.String()))
				}
			}
		})
	}
	if count != 127 {
		t.Fatalf("covered %d help pages", count)
	}
}
