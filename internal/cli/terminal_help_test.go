package cli

import (
	"bytes"
	"context"
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
		for _, width := range []int{40, 80, 120} {
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
