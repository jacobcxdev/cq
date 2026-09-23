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

func TestDiagnosticPartialFixture(t *testing.T) {
	var out, stderr bytes.Buffer
	outcome := Outcome{ExitCode: 8, Human: "known result\n", Errors: []Diagnostic{{"check_partial", "Some quota results could not be refreshed."}}, Warnings: []Diagnostic{{"quota_stale_fallback", "Cached quota used after a failed fetch for codex."}}}
	exit := Run(context.Background(), []string{"check"}, &Session{Out: &out, Err: &stderr, ErrTerminal: TerminalOptions{Width: 100, Colour: true}}, func(string) (Handler, bool) {
		return func(context.Context, Invocation, *Session) Outcome { return outcome }, true
	})
	want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", "text", "cq-check", "04-partial-with-stale-data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if exit != 8 || out.String() != "known result\n" || ansi.Strip(stderr.String()) != string(want) {
		t.Fatalf("exit %d stdout %q stderr %q", exit, out.String(), stderr.String())
	}
	for _, token := range []string{"\x1b[33mWarning\x1b[0m", "\x1b[36mcheck_partial\x1b[0m", "\x1b[36mquota_stale_fallback\x1b[0m"} {
		if !strings.Contains(stderr.String(), token) {
			t.Errorf("missing role %q", token)
		}
	}
}

func TestDiagnosticStreamsAndPureBuiltins(t *testing.T) {
	for _, outColour := range []bool{false, true} {
		for _, errColour := range []bool{false, true} {
			for _, args := range [][]string{{"version"}, {"version", "--verbose"}, {"help", "--help"}, {"--help"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}} {
				var out, stderr bytes.Buffer
				exit := Run(context.Background(), args, &Session{Out: &out, Err: &stderr, OutTerminal: TerminalOptions{Width: 100, Colour: outColour}, ErrTerminal: TerminalOptions{Width: 100, Colour: errColour}}, func(string) (Handler, bool) { t.Fatal("pure dispatch consulted handlers"); return nil, false })
				if len(args) > 1 && args[1] == "--verbose" {
					if exit != 2 || strings.Contains(stderr.String(), "\x1b[") != errColour {
						t.Fatalf("stderr colour %q", stderr.String())
					}
					continue
				}
				if exit != 0 {
					t.Fatal(exit)
				}
				if args[0] == "completion" {
					want, _ := Completion(args[1])
					if out.String() != want {
						t.Fatal("completion bytes changed")
					}
					continue
				}
				if strings.Contains(out.String(), "\x1b[") != outColour {
					t.Fatalf("stdout colour %q", out.String())
				}
			}
		}
	}
}

func TestVersionAuthoredFixtures(t *testing.T) {
	revision := "0123456789abcdef0123456789abcdef01234567"
	dirty := false
	for _, tc := range []struct {
		info BuildInfo
		file string
	}{{BuildInfo{Version: "1.0.0", Revision: &revision, Dirty: &dirty}, "02-success.txt"}, {BuildInfo{}, "03-unknown-provenance.txt"}} {
		var out bytes.Buffer
		if Run(context.Background(), []string{"version"}, &Session{Out: &out, Err: &bytes.Buffer{}, BuildInfo: tc.info, OutTerminal: TerminalOptions{Width: 100, Colour: true}}, nil) != 0 {
			t.Fatal("version failed")
		}
		want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", "text", "cq-version", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if ansi.Strip(out.String()) != string(want) {
			t.Fatalf("version got %q want %q", ansi.Strip(out.String()), want)
		}
	}
}

func TestRawHookTerminalPresentation(t *testing.T) {
	var out bytes.Buffer
	exit := Run(context.Background(), []string{"codex", "proxy", "hook", "stop"}, &Session{Out: &out, Err: &bytes.Buffer{}, OutTerminal: TerminalOptions{Width: 100, Colour: true}}, func(string) (Handler, bool) {
		return func(context.Context, Invocation, *Session) Outcome {
			return Outcome{Data: json.RawMessage(`{"decision":"allow"}`), Terminal: func(TerminalOptions) string { t.Fatal("raw hook styled"); return "" }}
		}, true
	})
	if exit != 0 || out.String() != "{\"decision\":\"allow\"}\n" {
		t.Fatalf("raw hook changed: %d %q", exit, out.String())
	}
}

func TestTerminalLineWrapOverflow(t *testing.T) {
	for _, colour := range []bool{false, true} {
		options := TerminalOptions{Width: 20, Colour: colour}
		got := TerminalLine(options, 0, Span{Text: "A raw selector requires authenticated live control. An existing --digest is validated and", Role: Identity})
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > 20 {
				t.Errorf("overflow: %q", line)
			}
		}
		if got := TerminalLine(options, 0, Span{Text: "Prompt: "}); got != "Prompt: \n" {
			t.Fatalf("prompt spacing changed: %q", got)
		}
	}
}

func TestDiagnosticParseFixtures(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		file, path string
	}{{[]string{"inspect"}, "cq-root-help/02-failure.txt", ""}, {[]string{"auth", "inspect"}, "cq-auth/02-failure.txt", "auth"}, {[]string{"version", "--verbose"}, "cq-version/04-failure.txt", "version"}} {
		var out, stderr bytes.Buffer
		if Run(context.Background(), tc.args, &Session{Out: &out, Err: &stderr, ErrTerminal: TerminalOptions{Width: 100, Colour: true}}, func(string) (Handler, bool) { t.Fatal("invalid syntax consulted handlers"); return nil, false }) != 2 {
			t.Fatal("wrong parse exit")
		}
		want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", "text", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if ansi.Strip(stderr.String()) != string(want) {
			t.Errorf("%s: got %q want %q", tc.file, ansi.Strip(stderr.String()), want)
		}
		_, parseErr := Parse(tc.args)
		if parseErr.Path != tc.path {
			t.Fatalf("canonical error path changed: %q", parseErr.Path)
		}
		for _, machine := range []bool{false, true} {
			out.Reset()
			stderr.Reset()
			args := append([]string{}, tc.args...)
			if machine {
				args = append(args, "--json")
			}
			Run(context.Background(), args, &Session{Out: &out, Err: &stderr}, nil)
			if machine {
				var envelope struct{ Command string }
				if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Command != tc.path || stderr.Len() != 0 {
					t.Fatal("machine diagnostic changed")
				}
			} else if stderr.String() != "cq: "+parseErr.Diagnostic.Message+"\n" {
				t.Fatal("plain diagnostic changed")
			}
		}
	}
}

func TestDiagnosticHostileCausesAndDeprecation(t *testing.T) {
	outcome := Outcome{ExitCode: 130, Errors: []Diagnostic{interruptedDiagnostic, {"failed@example.test", "Not restored\x1b[32m\nunknown"}}, Warnings: []Diagnostic{{"deprecated_option", "Deprecated option --refresh; use --fresh."}, {"specific_warning", "Specific cause."}}}
	for _, width := range []int{20, 40, 80, 120} {
		options := TerminalOptions{Width: width, Colour: true}
		got := terminalDiagnostics("check", outcome, options)
		plain := terminalDiagnostics("check", outcome, TerminalOptions{Width: width})
		if ansi.Strip(got) != plain {
			t.Fatal("colour changed diagnostics")
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("wide diagnostic: %q", line)
			}
		}
		if strings.Contains(got, "\x1b[32m") || !strings.Contains(got, "Deprecation:") {
			t.Fatalf("unsafe or lost cause: %q", got)
		}
	}
	got := terminalDiagnostics("check", outcome, TerminalOptions{Width: 100, Colour: true})
	for _, token := range []string{"\x1b[31mError\x1b[0m", "\x1b[36mfailed@example.test\x1b[0m", "specific_warning", "inspect state before retrying", "\\u001b"} {
		if !strings.Contains(got, token) {
			t.Errorf("missing %q in %q", token, got)
		}
	}
}

func TestAccountDiagnosticAttentionRoles(t *testing.T) {
	for _, code := range []string{"account_timeout", "gemini_account_timeout", "account_login_postcheck_partial", "auth_interrupted"} {
		t.Run(code, func(t *testing.T) {
			outcome := Outcome{ExitCode: 7, Errors: []Diagnostic{{Code: code, Message: "Inspect state before retrying."}}}
			got := terminalDiagnostics("auth refresh", outcome, TerminalOptions{Width: 100, Colour: true})
			if !strings.HasPrefix(got, "\x1b[33mWarning\x1b[0m · Inspect state before retrying.\n") {
				t.Fatalf("indeterminate diagnostic must be amber: %q", got)
			}
			if strings.Contains(got, "\x1b[31m") {
				t.Fatalf("indeterminate diagnostic marked failed: %q", got)
			}
		})
	}
}

func TestHelpTypedDefaultRoles(t *testing.T) {
	for _, tc := range []struct{ path, value, code string }{{"check", "30s", "35"}, {"codex proxy trace", "200", "1;34"}} {
		got, _ := terminalHelp(tc.path, 100, true)
		if !strings.Contains(got, "\x1b["+tc.code+"m"+tc.value+"\x1b[0m") {
			t.Errorf("missing typed default role for %s", tc.path)
		}
	}
	got, _ := terminalHelp("codex proxy trace", 100, true)
	if strings.Contains(got, "\x1b[1;34m0\x1b[0m") {
		t.Fatal("untyped prose numeral coloured")
	}
}

func TestHelpRelatedCommandDescriptionRoles(t *testing.T) {
	got, ok := terminalHelp("auth", 100, true)
	if !ok {
		t.Fatal("missing auth help")
	}
	want := "\x1b[36mcq claude account\x1b[0m   Add, inspect, select and remove Claude accounts."
	if !strings.Contains(got, want) {
		t.Fatalf("related command and description roles were not split: %q", got)
	}
	plain, _ := terminalHelp("auth", 100, false)
	if ansi.Strip(got) != plain {
		t.Fatal("related command styling changed visible text")
	}
}

func TestDiagnosticAuthInterruptionRetainsMachineCauses(t *testing.T) {
	for _, mode := range []string{"terminal", "plain", "json"} {
		t.Run(mode, func(t *testing.T) {
			var out, stderr bytes.Buffer
			s := &Session{Out: &out, Err: &stderr}
			args := []string{"auth", "refresh", "codex"}
			if mode == "terminal" {
				s.ErrTerminal = TerminalOptions{Width: 100, Colour: true}
			}
			if mode == "json" {
				args = append(args, "--json")
			}
			exit := Run(context.Background(), args, s, func(string) (Handler, bool) {
				return func(context.Context, Invocation, *Session) Outcome {
					return Outcome{ExitCode: 130, Errors: []Diagnostic{{Code: "auth_interrupted", Message: "Credential refresh interrupted."}}, Warnings: []Diagnostic{{Code: "auth_refresh_outcome_unknown", Message: "Inspect credentials before retrying."}}}
				}, true
			})
			if exit != 130 {
				t.Fatal(exit)
			}
			if mode == "terminal" {
				got := ansi.Strip(stderr.String())
				if !strings.HasPrefix(got, "Warning · Credential refresh interrupted.\n  Code: auth_interrupted\n") || strings.Contains(got, "Code: interrupted") || !strings.Contains(got, "Cause: auth_refresh_outcome_unknown") {
					t.Fatal(got)
				}
			} else if mode == "json" {
				if !strings.Contains(out.String(), `"code":"interrupted"`) || !strings.Contains(out.String(), `"code":"auth_interrupted"`) {
					t.Fatal(out.String())
				}
			} else if !strings.Contains(stderr.String(), "Operation interrupted; inspect state before retrying.") || !strings.Contains(stderr.String(), "Credential refresh interrupted.") {
				t.Fatal(stderr.String())
			}
		})
	}
}
