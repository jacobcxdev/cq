package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type parseCase struct {
	ID             string              `json:"id"`
	Args           []string            `json:"args"`
	Path           string              `json:"path"`
	Code           string              `json:"code"`
	Message        string              `json:"message"`
	Exit           int                 `json:"exit"`
	Presentation   string              `json:"presentation"`
	Options        map[string][]string `json:"options"`
	Arguments      map[string][]string `json:"arguments"`
	Supplied       map[string]bool     `json:"supplied"`
	JSON           *bool               `json:"json"`
	LegacySelector string              `json:"legacy_selector"`
}

func readParseCases(t *testing.T, file string) []parseCase {
	t.Helper()
	b, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	var cases []parseCase
	if e = json.Unmarshal(b, &cases); e != nil {
		t.Fatal(e)
	}
	return cases
}
func assertParseCase(t *testing.T, c parseCase) {
	t.Helper()
	in, e := Parse(nativeTestArgs(c.Args))
	if c.Code != "" {
		if e == nil || e.Diagnostic.Code != c.Code || e.Path != c.Path || e.ExitCode != c.Exit {
			t.Fatalf("Parse(%q) = %#v, %#v; want %s %s exit %d", c.Args, in, e, c.Path, c.Code, c.Exit)
		}
		if c.Message != "" && e.Error() != c.Message {
			t.Fatalf("message %q; want %q", e.Error(), c.Message)
		}
		return
	}
	if e != nil {
		t.Fatalf("Parse(%q): %#v", c.Args, e)
	}
	if in.Path != c.Path {
		t.Fatalf("path %q; want %q", in.Path, c.Path)
	}
	if c.Presentation != "" && in.Presentation != c.Presentation {
		t.Fatalf("presentation %q; want %q", in.Presentation, c.Presentation)
	}
	for k, v := range c.Options {
		if !reflect.DeepEqual(in.Options[k], nativeTestArgs(v)) {
			t.Errorf("option %s = %q; want %q", k, in.Options[k], v)
		}
	}
	for k, v := range c.Arguments {
		if !reflect.DeepEqual(in.Arguments[k], nativeTestArgs(v)) {
			t.Errorf("argument %s = %q; want %q", k, in.Arguments[k], v)
		}
	}
	for k, v := range c.Supplied {
		if in.Supplied[k] != v {
			t.Errorf("supplied %s = %v; want %v", k, in.Supplied[k], v)
		}
	}
	if c.JSON != nil && in.JSON != *c.JSON {
		t.Errorf("JSON = %v", in.JSON)
	}
	if in.LegacySelector != c.LegacySelector {
		t.Errorf("legacy selector %q; want %q", in.LegacySelector, c.LegacySelector)
	}
}
func TestCLIV2DuplicateOption(t *testing.T) {
	_, err := Parse([]string{"codex", "proxy", "trace", "--tail", "1", "--tail", "2"})
	if err == nil || err.ExitCode != 2 || err.Path != "codex proxy trace" {
		t.Fatalf("duplicate tail accepted: %#v", err)
	}
}
func TestCLIV2ParseGrammar(t *testing.T) {
	for _, c := range []parseCase{
		{ID: "root", Path: "check", Presentation: "run", Options: map[string][]string{"timeout": {"30s"}, "fresh": {"false"}}, Supplied: map[string]bool{"timeout": false}},
		{ID: "group", Args: []string{"codex", "proxy"}, Path: "codex proxy", Presentation: "help"},
		{ID: "globals-between", Args: []string{"codex", "--json", "proxy", "-h", "reserve", "set"}, Path: "codex proxy reserve set", Presentation: "help"},
		{ID: "help-missing-arity", Args: []string{"proxy", "health", "--port", "--help"}, Path: "proxy health", Code: "missing_option_value", Exit: 2, Message: "Option --port requires PORT."},
		{ID: "help-bad-value", Args: []string{"proxy", "health", "--port=bad", "--help"}, Path: "proxy health", Presentation: "help"},
		{ID: "unknown", Args: []string{"nonsense", "--help"}, Path: "", Code: "unknown_command", Exit: 2},
		{ID: "duplicate-global", Args: []string{"-j", "--json"}, Path: "check", Code: "duplicate_option", Exit: 2},
		{ID: "no-bundles", Args: []string{"-jh"}, Path: "check", Code: "unknown_option", Exit: 2},
		{ID: "local-at-group", Args: []string{"proxy", "--port=1", "health"}, Path: "proxy", Code: "unknown_option", Exit: 2, Message: "Unknown option: --port. Run cq proxy --help."},
		{ID: "sentinel-help", Args: []string{"--", "help", "proxy", "health"}, Path: "proxy health", Presentation: "help"},
		{ID: "sentinel-help-unknown", Args: []string{"--", "help", "nonsense"}, Path: "", Code: "unknown_command", Exit: 2, Message: "Unknown command: nonsense. Run cq help."},
		{ID: "sentinel-help-literal-option", Args: []string{"--", "help", "proxy", "--json"}, Path: "proxy", Code: "unknown_command", Exit: 2},
		{ID: "sentinel-help-account", Args: []string{"--", "codex", "account", "remove", "help"}, Path: "codex account remove", Arguments: map[string][]string{"account": {"help"}}},
		{ID: "sentinel-path", Args: []string{"--", "codex", "account", "remove", "-account"}, Path: "codex account remove", Arguments: map[string][]string{"account": {"-account"}}},
		{ID: "consent-deferred", Args: []string{"proxy", "candidate", "stop", "--state-dir=/does-not-exist"}, Path: "proxy candidate stop", Options: map[string][]string{"confirm-client-stopped": {"false"}}},
		{ID: "session-bytes", Args: []string{"codex", "proxy", "session", "digest", "--session-id=s\n"}, Path: "codex proxy session digest", Options: map[string][]string{"session-id": {"s\n"}}},
		{ID: "invalid-utf8", Args: []string{"codex", "account", "activate", string([]byte{255})}, Path: "codex account activate", Code: "invalid_argument", Exit: 2},
	} {
		t.Run(c.ID, func(t *testing.T) { assertParseCase(t, c) })
	}
}
func TestCLIV2ParseNoEnvironment(t *testing.T) {
	a, e := Parse([]string{"proxy", "health"})
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("HOME", "/missing/parser-must-not-read")
	t.Setenv("CQ_PROXY_PORT", "oops")
	t.Setenv("CODEX_HOME", "/missing")
	b, e := Parse([]string{"proxy", "health"})
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("environment changed parsing: %#v %#v", b, e)
	}
}
func TestCLIV2ParseInputIsNotMutated(t *testing.T) {
	a := []string{"proxy", "reserve", "set", "--window", "7d", "--percent", "2"}
	copyA := append([]string(nil), a...)
	Parse(a)
	if strings.Join(a, "\x00") != strings.Join(copyA, "\x00") {
		t.Fatal("mutated argv")
	}
}

func TestCLIV2ParsePrecedenceAndInspection(t *testing.T) {
	cases := []parseCase{
		{ID: "earlier-value", Args: []string{"proxy", "health", "--port=bad", "--oops"}, Path: "proxy health", Code: "invalid_argument", Exit: 2},
		{ID: "earlier-option", Args: []string{"proxy", "health", "--oops", "--port=bad"}, Path: "proxy health", Code: "unknown_option", Exit: 2},
		{ID: "help-skips-value-only", Args: []string{"proxy", "health", "--port=bad", "--help", "--oops"}, Path: "proxy health", Code: "unknown_option", Exit: 2},
		{ID: "help-version-conflict", Args: []string{"--help", "--version"}, Path: "", Code: "conflicting_options", Exit: 2},
		{ID: "version-bypasses-required", Args: []string{"codex", "account", "remove", "--version"}, Path: "codex account remove", Presentation: "version"},
		{ID: "version-bypasses-value", Args: []string{"proxy", "health", "--port=bad", "--version"}, Path: "proxy health", Presentation: "version"},
		{ID: "version-still-arity", Args: []string{"proxy", "health", "--port", "--version"}, Path: "proxy health", Code: "missing_option_value", Exit: 2},
		{ID: "false-help", Args: []string{"codex", "account", "remove", "--help=false"}, Path: "codex account remove", Code: "missing_argument", Exit: 2},
		{ID: "false-version", Args: []string{"codex", "account", "remove", "--version=false"}, Path: "codex account remove", Code: "missing_argument", Exit: 2},
		{ID: "false-bool-positional", Args: []string{"check", "--fresh", "false"}, Path: "check", Code: "invalid_argument", Exit: 2},
		{ID: "dash-value-equals", Args: []string{"codex", "proxy", "session", "digest", "--session-id=-id"}, Path: "codex proxy session digest", Options: map[string][]string{"session-id": {"-id"}}},
		{ID: "dash-value-separate", Args: []string{"codex", "proxy", "session", "digest", "--session-id", "-id"}, Path: "codex proxy session digest", Code: "routing_invalid_argument", Exit: 2},
		{ID: "empty-value-help", Args: []string{"proxy", "health", "--port=", "--help"}, Path: "proxy health", Presentation: "help"},
		{ID: "sentinel-default", Args: []string{"--"}, Path: "check", Presentation: "run"},
		{ID: "sentinel-no-slots", Args: []string{"version", "--", "--help"}, Path: "version", Code: "unexpected_argument", Exit: 2},
		{ID: "globals-not-consumed", Args: []string{"codex", "account", "activate", "--json", "alice@example.com"}, Path: "codex account activate", Arguments: map[string][]string{"account": {"alice@example.com"}}},
		{ID: "global-before-value", Args: []string{"proxy", "health", "--port", "--json", "123"}, Path: "proxy health", Code: "missing_option_value", Exit: 2},
		{ID: "refresh-root-before-command", Args: []string{"--refresh", "check", "codex"}, Path: "check", Options: map[string][]string{"fresh": {"true"}}, Arguments: map[string][]string{"providers": {"codex"}}},
		{ID: "refresh-group", Args: []string{"proxy", "--refresh", "health"}, Path: "proxy health", Code: "invalid_argument", Exit: 2, Message: "--refresh is only supported by cq check; use --fresh."},
		{ID: "bare-root-help", Args: []string{"--help"}, Path: "", Presentation: "help"},
		{ID: "global-duplicate-first", Args: []string{"proxy", "health", "--json", "-j", "--port=bad"}, Path: "proxy health", Code: "duplicate_option", Exit: 2},
		{ID: "normalised-window", Args: []string{"codex", "proxy", "reserve", "set", "--window= 7D_FOO ", "--percent=2"}, Path: "codex proxy reserve set", Options: map[string][]string{"window": {"7d-foo"}}},
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) { assertParseCase(t, c) })
	}
}
func TestCLIV2ParseRepeatableOrder(t *testing.T) {
	in, e := Parse([]string{"codex", "proxy", "pool", "set", "池 Café", "--account=second", "--account=first", "--account=second"})
	if e != nil || !reflect.DeepEqual(in.Options["account"], []string{"second", "first", "second"}) {
		t.Fatalf("parser resolved accounts or reordered input: %#v %#v", in, e)
	}
	// Two selectors can identify the same account only after inventory resolution.
}
func TestCLIV2ParsePreservesLiteralValues(t *testing.T) {
	for _, v := range []string{"$HOME/request.json", "~/request.json", "*.json", "a b.json"} {
		in, e := Parse([]string{"codex", "proxy", "fixture", "create", "--input=" + v, "--output=result.json"})
		if e != nil || in.Options["input"][0] != v {
			t.Fatalf("expanded literal %q: %#v %#v", v, in, e)
		}
	}
}

func TestCLIV2ParseExactCommandTokens(t *testing.T) {
	for _, a := range [][]string{{" proxy", "health"}, {"proxy", "health "}, {"CHECK"}, {"Check"}, {"proxy", "Health"}, {"cod"}} {
		if _, e := Parse(a); e == nil || e.Diagnostic.Code != "unknown_command" {
			t.Fatalf("inexact command accepted: %q %#v", a, e)
		}
	}
	assertParseCase(t, parseCase{Args: []string{"help", "help"}, Path: "help", Presentation: "help"})
	assertParseCase(t, parseCase{Args: []string{"help", "check", "codex"}, Path: "check", Code: "unknown_command", Exit: 2})
}
func TestCLIV2ParseConcurrentIsolation(t *testing.T) {
	for i := 0; i < 16; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			in, e := Parse([]string{"proxy", "health"})
			if e != nil {
				t.Fatal(e)
			}
			in.Options["timeout"][0] = "mutated"
			again, e := Parse([]string{"proxy", "health"})
			if e != nil || again.Options["timeout"][0] != "5s" {
				t.Fatalf("mutable defaults leaked: %#v %#v", again, e)
			}
		})
	}
}

// Fixture paths name hypothetical files. Use native lexical separators on
// Windows without reading the filesystem or changing the invalid-path cases.
func nativeTestArgs(values []string) []string {
	if runtime.GOOS != "windows" {
		return values
	}
	result := append([]string(nil), values...)
	for i, value := range result {
		prefix := ""
		if strings.HasPrefix(value, "--") {
			if name, tail, ok := strings.Cut(value, "="); ok {
				prefix = name + "="
				value = tail
			}
		}
		if strings.HasPrefix(value, "/") {
			value = "C:" + strings.ReplaceAll(value, "/", `\`)
		}
		result[i] = prefix + value
	}
	return result
}
