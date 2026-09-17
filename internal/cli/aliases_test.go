package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type compatibilityCase struct {
	Name          string              `json:"name"`
	Legacy        []string            `json:"legacy"`
	Canonical     []string            `json:"canonical"`
	Reject        bool                `json:"reject"`
	Code          string              `json:"code"`
	Exit          int                 `json:"exit"`
	Path          string              `json:"path"`
	Options       map[string][]string `json:"options"`
	Arguments     map[string][]string `json:"arguments"`
	AbsentOptions []string            `json:"absent_options"`
}
type compatibilityRow struct {
	LegacyPath  string              `json:"legacy_path"`
	Disposition string              `json:"disposition"`
	Cases       []compatibilityCase `json:"cases"`
}

func TestCLIV2AliasCompatibility(t *testing.T) {
	var rows []compatibilityRow
	b, e := os.ReadFile("testdata/compatibility.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &rows); e != nil {
		t.Fatal(e)
	}
	var inventory []compatibilityRow
	b, e = os.ReadFile("../../specs/cli-v2/migration.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &inventory); e != nil {
		t.Fatal(e)
	}
	if len(rows) != 92 || len(inventory) != len(rows) {
		t.Fatal("migration inventory incomplete")
	}
	for i, row := range rows {
		if row.LegacyPath != inventory[i].LegacyPath || row.Disposition != inventory[i].Disposition || len(row.Cases) == 0 {
			t.Fatalf("uncovered migration row %d", i)
		}
		for _, c := range row.Cases {
			t.Run(row.LegacyPath+"/"+c.Name, func(t *testing.T) {
				actual, err := Parse(nativeTestArgs(c.Legacy))
				if c.Reject {
					if err == nil || err.ExitCode != 2 {
						t.Fatalf("malformed alias accepted: %q -> %#v %#v", c.Legacy, actual, err)
					}
					return
				}
				if c.Code != "" {
					if err == nil || err.ExitCode != c.Exit || err.Diagnostic.Code != c.Code || err.Path != c.Path {
						t.Fatalf("retired result: %#v %#v", actual, err)
					}
					return
				}
				want, e := Parse(nativeTestArgs(c.Canonical))
				if err != nil || e != nil {
					t.Fatalf("alias %q: %#v; canonical %q: %#v", c.Legacy, err, c.Canonical, e)
				}
				if c.Path != "" {
					assertParseCase(t, parseCase{Args: c.Legacy, Path: c.Path, Options: c.Options, Arguments: c.Arguments})
				}
				for _, name := range c.AbsentOptions {
					if _, ok := actual.Options[name]; ok {
						t.Errorf("consumed option %s forwarded", name)
					}
				}
				warnings := actual.Warnings
				actual.Warnings = nil
				want.Warnings = nil
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("alias mismatch\nactual %#v\nwant   %#v", actual, want)
				}
				if row.LegacyPath != actual.Path {
					expected := []Diagnostic{{Code: "deprecated_alias", Message: "Deprecated syntax; use cq " + actual.Path + "."}}
					if !reflect.DeepEqual(warnings, expected) {
						t.Fatalf("warnings %#v; want %#v", warnings, expected)
					}
				}
			})
		}
	}
}
func TestCLIV2AliasGroupHelp(t *testing.T) {
	var groups map[string]string
	data, err := os.ReadFile("testdata/compatibility-help.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &groups); err != nil {
		t.Fatal(err)
	}
	for old, target := range groups {
		for _, a := range [][]string{append(strings.Fields(old), "--help"), append([]string{"help"}, strings.Fields(old)...), append([]string{strings.Fields(old)[0], "help"}, strings.Fields(old)[1:]...)} {
			t.Run(strings.Join(a, " "), func(t *testing.T) {
				in, e := Parse(a)
				if e != nil || in.Path != target || in.Presentation != "help" {
					t.Fatalf("help: %#v %#v", in, e)
				}
				text, ok := Help(in.Path)
				want, _ := Help(target)
				if !ok || text != want {
					t.Fatal("help mismatch")
				}
				if old == "proxy policy" && (len(in.Warnings) != 2 || in.Warnings[1].Code != "legacy_group_split") {
					t.Fatalf("split warning: %#v", in.Warnings)
				}
			})
		}
	}
	for _, a := range [][]string{{"proxy", "pin", "--help"}, {"help", "proxy", "pin"}, {"proxy", "help", "pin"}} {
		in, e := Parse(a)
		if e != nil || in.Presentation != "help" || in.Path != "proxy pin" || len(in.Warnings) != 0 {
			t.Fatalf("provider help: %#v %#v", in, e)
		}
		got, ok := Help(in.Path)
		if !ok || got != "Usage: cq <provider> proxy pin <command>\n\nChoose a provider explicitly:\n  cq claude proxy pin --help\n  cq codex proxy pin --help\n" {
			t.Fatal("provider guidance help")
		}
	}
}
func TestCLIV2RetiredRecover(t *testing.T) {
	for _, args := range [][]string{{"operation", "recover", "--help"}, {"operation", "recover", "--operation-id=" + strings.Repeat("a", 32)}} {
		in, e := Parse(args)
		if e == nil || e.ExitCode != 4 || e.Diagnostic.Code != "operation_recovery_unavailable" || len(in.Warnings) != 0 {
			t.Fatalf("recovery: %#v %#v", in, e)
		}
	}
	for _, args := range [][]string{{"operation", "recover"}, {"operation", "recover", "--help", "--operation-id"}, {"operation", "recover", "--operation-id=bad"}} {
		if _, e := Parse(args); e == nil || e.ExitCode != 2 {
			t.Fatalf("malformed retirement: %#v", e)
		}
	}
}
func TestCLIV2AliasWarnings(t *testing.T) {
	in, e := Parse([]string{"check", "--refresh"})
	if e != nil || !reflect.DeepEqual(in.Warnings, []Diagnostic{{Code: "deprecated_option", Message: "Deprecated option --refresh; use --fresh."}}) {
		t.Fatalf("refresh warning %#v %#v", in, e)
	}
	a := []string{"proxy", "candidate", "status", "--instance-state-root=/tmp/state"}
	in, e = Parse(nativeTestArgs(a))
	if e != nil || !reflect.DeepEqual(in.Warnings, []Diagnostic{{Code: "deprecated_option", Message: "Deprecated option --instance-state-root; use --state-dir."}}) {
		t.Fatalf("option warning %#v %#v", in, e)
	}
}

func TestCLIV2AliasCannotWidenComponent(t *testing.T) {
	for _, path := range []string{"agent install", "agent uninstall", "proxy install", "proxy restart", "proxy uninstall"} {
		for _, component := range []string{"all", "proxy", "token-refresh"} {
			a := append(strings.Fields(path), "--component="+component)
			_, e := Parse(a)
			if e == nil || e.Diagnostic.Code != "duplicate_option" {
				t.Fatalf("component override accepted: %q %#v", a, e)
			}
		}
	}
}
func TestCLIV2AliasWarningOrder(t *testing.T) {
	in, e := Parse(nativeTestArgs([]string{"proxy", "status", "--human", "--instance-state-root=/tmp/state"}))
	want := []Diagnostic{{Code: "deprecated_option", Message: "Deprecated option --human; use --json=false."}, {Code: "deprecated_option", Message: "Deprecated option --instance-state-root; use --state-dir."}}
	if e != nil || !reflect.DeepEqual(in.Warnings, want) {
		t.Fatalf("warnings not in argv order: %#v %#v", in.Warnings, e)
	}
	in, e = Parse([]string{"check", "--refresh", "--refresh"})
	if e == nil || len(in.Warnings) != 1 {
		t.Fatalf("duplicate spelling warning not deduplicated: %#v %#v", in, e)
	}
}
func TestCLIV2AliasCanonicalErrorPath(t *testing.T) {
	_, e := Parse([]string{"proxy", "reserve", "--oops", "set"})
	if e == nil || e.Path != "codex proxy reserve" || e.Error() != "Unknown option: --oops. Run cq codex proxy reserve --help." {
		t.Fatalf("noncanonical error path: %#v", e)
	}
}
