package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	if count != 139 {
		t.Fatalf("covered %d help pages", count)
	}
}

func TestAuthoredGroupFailureFixtures(t *testing.T) {
	base := filepath.Join("..", "..")
	var manifest struct {
		Fixtures []struct{ Command, Scenario, Stream, File, Task string }
	}
	manifestBytes, err := os.ReadFile(filepath.Join(base, "docs", "superpowers", "plans", "terminal-presentation-coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct {
			Command, Scenario, Stream, Invocation, File string
			Exit                                        json.RawMessage
		}
	}
	examples := filepath.Join(base, "specs", "cli-v2", "presentation", "examples")
	indexBytes, err := os.ReadFile(filepath.Join(examples, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	byFile := make(map[string]struct {
		Command, Scenario, Stream, Invocation, File string
		Exit                                        json.RawMessage
	}, len(index.Fixtures))
	for _, fixture := range index.Fixtures {
		byFile[fixture.File] = fixture
	}
	count := 0
	for _, row := range manifest.Fixtures {
		if row.Task != "T03" || row.Scenario != "Failure" {
			continue
		}
		count++
		t.Run(row.Command, func(t *testing.T) {
			fixture, ok := byFile[row.File]
			if !ok || fixture.Command != row.Command || fixture.Scenario != row.Scenario || fixture.Stream != "stderr" {
				t.Fatalf("bad group fixture registration: %+v", row)
			}
			want, err := os.ReadFile(filepath.Join(examples, fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			wantExit, err := strconv.Atoi(string(fixture.Exit))
			if err != nil {
				t.Fatal(err)
			}
			for _, colour := range []bool{false, true} {
				var out, stderr bytes.Buffer
				args := strings.Fields(fixture.Invocation)[1:]
				exit := Run(context.Background(), args, &Session{Out: &out, Err: &stderr, ErrTerminal: TerminalOptions{Width: 100, Colour: colour}}, func(string) (Handler, bool) {
					t.Fatal("group syntax failure consulted handlers")
					return nil, false
				})
				if exit != wantExit || out.Len() != 0 || ansi.Strip(stderr.String()) != string(want) {
					t.Fatalf("fixture %s exit %d stdout %q stderr %q", fixture.File, exit, &out, ansi.Strip(stderr.String()))
				}
			}
		})
	}
	if count != 42 {
		t.Fatalf("covered %d group failure fixtures", count)
	}
}

func TestAuthoredPureCatalogueFixtures(t *testing.T) {
	base := filepath.Join("..", "..")
	var manifest struct {
		Fixtures []struct{ Command, Scenario, Stream, File, Task string }
	}
	manifestBytes, err := os.ReadFile(filepath.Join(base, "docs", "superpowers", "plans", "terminal-presentation-coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct {
			Command, Scenario, Stream, Invocation, File string
			Exit                                        json.RawMessage
		}
	}
	examples := filepath.Join(base, "specs", "cli-v2", "presentation", "examples")
	indexBytes, err := os.ReadFile(filepath.Join(examples, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	byFile := make(map[string]int, len(index.Fixtures))
	for i, fixture := range index.Fixtures {
		byFile[fixture.File] = i
	}
	count := 0
	for _, row := range manifest.Fixtures {
		if row.Task != "T03" {
			continue
		}
		count++
		t.Run(row.File, func(t *testing.T) {
			at, ok := byFile[row.File]
			if !ok {
				t.Fatal("fixture missing from index")
			}
			fixture := index.Fixtures[at]
			if fixture.Command != row.Command || fixture.Scenario != row.Scenario || fixture.Stream != row.Stream {
				t.Fatal("fixture metadata differs from assignment")
			}
			exitWant, err := strconv.Atoi(string(fixture.Exit))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(examples, fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			info := BuildInfo{}
			switch row.Scenario {
			case "Help", "Failure":
			case "Success":
				if row.Command == "cq version" {
					revision, clean := "0123456789abcdef0123456789abcdef01234567", false
					info = BuildInfo{Version: "1.0.0", Revision: &revision, Dirty: &clean}
				} else if row.Command != "cq help" {
					t.Fatal("unhandled pure success scenario")
				}
			case "Unknown provenance":
				if row.Command != "cq version" {
					t.Fatal("unhandled provenance scenario")
				}
			case "Success — bash raw protocol", "Success — zsh raw protocol", "Success — fish raw protocol":
				if row.Command != "cq completion" {
					t.Fatal("unhandled raw protocol scenario")
				}
			default:
				t.Fatalf("unhandled pure fixture scenario: %s", row.Scenario)
			}
			for _, colour := range []bool{false, true} {
				var stdout, stderr bytes.Buffer
				options := TerminalOptions{Width: 100, Colour: colour}
				args := strings.Fields(fixture.Invocation)[1:]
				exit := Run(context.Background(), args, &Session{Out: &stdout, Err: &stderr, OutTerminal: options, ErrTerminal: options, BuildInfo: info}, func(string) (Handler, bool) {
					t.Fatal("pure catalogue fixture consulted handlers")
					return nil, false
				})
				if exit != exitWant {
					t.Fatalf("exit %d want %d", exit, exitWant)
				}
				got := stdout.String()
				other := stderr.String()
				if row.Stream == "stderr" {
					got, other = other, got
				}
				if row.Stream != "stdout" && row.Stream != "stderr" || other != "" {
					t.Fatalf("unexpected stream: stdout=%q stderr=%q", &stdout, &stderr)
				}
				if strings.HasPrefix(row.Scenario, "Success —") {
					if got != string(want) {
						t.Fatalf("raw bytes differ: %s", row.File)
					}
				} else if ansi.Strip(got) != string(want) {
					t.Fatalf("terminal fixture differs: %s\ngot:\n%s", row.File, ansi.Strip(got))
				}
			}
		})
	}
	if count != 187 {
		t.Fatalf("covered %d pure catalogue fixtures", count)
	}
}
