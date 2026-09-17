package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

type outputEnvelope struct {
	Schema   int             `json:"schema_version"`
	Command  string          `json:"command"`
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Errors   []Diagnostic    `json:"errors"`
	Warnings []Diagnostic    `json:"warnings"`
}

func decodeOutput(t *testing.T, data string) outputEnvelope {
	t.Helper()
	if !strings.HasSuffix(data, "\n") || strings.ContainsRune(data, '\x1b') {
		t.Fatalf("invalid terminal bytes: %q", data)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &keys); err != nil {
		t.Fatalf("not one JSON document: %v: %q", err, data)
	}
	if len(keys) != 6 {
		t.Fatalf("envelope keys: %v", keys)
	}
	for _, key := range []string{"schema_version", "command", "ok", "data", "errors", "warnings"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	var got outputEnvelope
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != 2 || got.Errors == nil || got.Warnings == nil {
		t.Fatalf("invalid envelope: %+v", got)
	}
	return got
}

func runOutput(t *testing.T, ctx context.Context, args []string, handler Handler) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	session := &Session{In: strings.NewReader(""), Out: &out, Err: &stderr}
	exit := Run(ctx, args, session, func(path string) (Handler, bool) { return handler, handler != nil })
	return exit, out.String(), stderr.String()
}

func TestCLIV2OutputEnvelope(t *testing.T) {
	for _, code := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 130} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var out bytes.Buffer
			outcome := Outcome{ExitCode: code}
			if code != 0 {
				outcome.Errors = []Diagnostic{{"failure", "A failure."}}
			}
			if err := WriteJSON(&out, "codex reset use", outcome); err != nil {
				t.Fatal(err)
			}
			got := decodeOutput(t, out.String())
			if got.Command != "codex reset use" || got.OK != (code == 0) || string(got.Data) != "null" || !reflect.DeepEqual(got.Errors, append([]Diagnostic{}, outcome.Errors...)) {
				t.Fatalf("envelope: %+v", got)
			}
		})
	}
	data, err := EncodeData(struct {
		Items []string `json:"items"`
		Name  string   `json:"name"`
	}{[]string{"b", "a"}, "Research Ω\x1b\n"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WriteJSON(&out, "check", Outcome{Data: data, ExitCode: 8, Errors: []Diagnostic{{"partial", "Partial."}}, Warnings: []Diagnostic{{"first", "First."}, {"second", "Second."}}}); err != nil {
		t.Fatal(err)
	}
	got := decodeOutput(t, out.String())
	if !strings.Contains(string(got.Data), `"items":["b","a"]`) || len(got.Warnings) != 2 || got.Warnings[0].Code != "first" {
		t.Fatalf("order or data: %+v", got)
	}
}

func TestCLIV2OutputPurePresentation(t *testing.T) {
	for _, args := range [][]string{{"--help", "--json"}, {"codex"}, {"proxy", "pin", "--help"}, {"help", "proxy", "policy"}, {"--version", "--json"}, {"version", "--json"}, {"codex", "account", "remove", "--version", "--json"}, {"completion", "bash"}, {"completion", "fish", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, stderr bytes.Buffer
			revision, dirty := strings.Repeat("a", 40), false
			s := &Session{Out: &out, Err: &stderr, In: panicInput{}, BuildInfo: BuildInfo{Version: "1.2.3", Revision: &revision, Dirty: &dirty}}
			exit := Run(context.Background(), args, s, func(string) (Handler, bool) { t.Fatal("presentation looked up a handler"); return nil, false })
			if exit != 0 {
				t.Fatalf("exit %d: %s", exit, stderr.String())
			}
			inv, err := Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case inv.Presentation == "help":
				want, ok := Help(inv.Path)
				if !ok || out.String() != want {
					t.Fatalf("help: %q", out.String())
				}
			case inv.Presentation == "version" || inv.Path == "version":
				got := decodeOutput(t, out.String())
				if got.Command != "version" || !strings.Contains(string(got.Data), `"version":"1.2.3"`) || !strings.Contains(string(got.Data), `"dirty":false`) {
					t.Fatalf("version: %+v", got)
				}
			case inv.JSON:
				got := decodeOutput(t, out.String())
				if got.Command != "completion" || !strings.Contains(string(got.Data), `"shell":"fish"`) {
					t.Fatalf("completion: %+v", got)
				}
			default:
				want, _ := Completion("bash")
				if out.String() != want {
					t.Fatal("changed completion script")
				}
			}
		})
	}
	var out bytes.Buffer
	if exit := Run(context.Background(), []string{"version", "--json"}, &Session{Out: &out, Err: io.Discard}, nil); exit != 0 {
		t.Fatal(exit)
	}
	if got := decodeOutput(t, out.String()); string(got.Data) != `{"version":"dev","revision":null,"dirty":null,"cli_schema_version":2}` {
		t.Fatalf("default provenance: %s", got.Data)
	}
}

type panicInput struct{}

func (panicInput) Read([]byte) (int, error) { panic("unexpected stdin access") }

func TestCLIV2OutputParseFailures(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		path, code string
		exit       int
	}{
		{[]string{"nonsense", "--json"}, "", "unknown_command", 2},
		{[]string{"proxy", "--port=1", "--json"}, "proxy", "unknown_option", 2},
		{[]string{"codex", "account", "remove", "alice", "--dry-run", "--json"}, "codex account remove", "unknown_option", 2},
		{[]string{"operation", "recover", "--operation-id", strings.Repeat("a", 32), "--json"}, "operation recover", "operation_recovery_unavailable", 4},
		{[]string{"operation", "recover", "--help", "--json"}, "operation recover", "operation_recovery_unavailable", 4},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, stderr bytes.Buffer
			exit := Run(context.Background(), tc.args, &Session{Out: &out, Err: &stderr, In: panicInput{}}, func(string) (Handler, bool) { t.Fatal("invalid input reached lookup"); return nil, false })
			if exit != tc.exit {
				t.Fatalf("exit %d, want %d; stderr %q", exit, tc.exit, stderr.String())
			}
			got := decodeOutput(t, out.String())
			if got.Command != tc.path || len(got.Errors) != 1 || got.Errors[0].Code != tc.code || string(got.Data) != "null" {
				t.Fatalf("parse outcome: %+v", got)
			}
		})
	}
}

func TestCLIV2OutputWarningsAndScope(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		warnings []Diagnostic
	}{
		{[]string{"proxy", "status", "--port=29280", "--json"}, []Diagnostic{{"deprecated_alias", "Deprecated syntax; use cq proxy health."}}},
		{[]string{"check", "--refresh", "--json"}, []Diagnostic{{"deprecated_option", "Deprecated option --refresh; use --fresh."}}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			inv, parseErr := Parse(tc.args)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			var seen []Invocation
			handler := func(_ context.Context, in Invocation, _ *Session) Outcome {
				seen = append(seen, in)
				return Outcome{ExitCode: 8, Data: json.RawMessage(`{"changed":true}`), Human: "Changed: true.\n", Errors: []Diagnostic{{"partial", "Incomplete.\nSafe."}}, Warnings: []Diagnostic{{"operational", "Follow up."}}}
			}
			exit, stdout, stderr := runOutput(t, context.Background(), tc.args, handler)
			got := decodeOutput(t, stdout)
			want := append(tc.warnings, Diagnostic{"operational", "Follow up."})
			if exit != 8 || got.Command != inv.Path || !reflect.DeepEqual(got.Warnings, want) {
				t.Fatalf("warnings/scope: %d %+v, want %+v", exit, got, want)
			}
			var expected strings.Builder
			for _, warning := range want {
				fmt.Fprintf(&expected, "cq: warning: %s\n", warning.Message)
			}
			// JSON diagnostics live in the envelope; human diagnostics use stderr.
			if stderr != expected.String() {
				t.Fatalf("JSON stderr %q", stderr)
			}
			humanArgs := append([]string{}, tc.args[:len(tc.args)-1]...)
			exit, stdout, stderr = runOutput(t, context.Background(), humanArgs, handler)
			if exit != 8 || stdout != "Changed: true.\n" || stderr != expected.String()+"cq: Incomplete.\\u000aSafe.\n" {
				t.Fatalf("human output: %d %q %q", exit, stdout, stderr)
			}
			seen[0].JSON, seen[1].JSON = false, false
			delete(seen[0].Options, "json")
			delete(seen[0].Supplied, "json")
			delete(seen[1].Options, "json")
			delete(seen[1].Supplied, "json")
			if !reflect.DeepEqual(seen[0], seen[1]) {
				t.Fatalf("scope changed: %+v / %+v", seen[0], seen[1])
			}
		})
	}
}

func TestCLIV2OutputHumanValues(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	var controls strings.Builder
	var escaped strings.Builder
	for r := rune(0); r <= 0x9f; r++ {
		if r < 0x20 || r >= 0x7f {
			controls.WriteRune(r)
			fmt.Fprintf(&escaped, `\u%04x`, r)
		}
	}
	text := "Research Ω 日本語 " + controls.String()
	if got := HumanValue(text); got != "Research Ω 日本語 "+escaped.String() {
		t.Fatalf("control escaping: %q", got)
	}
	var absent *string
	flag, number := false, 12.5
	for _, tc := range []struct {
		value any
		want  string
	}{{nil, "—"}, {absent, "—"}, {true, "true"}, {&flag, "false"}, {12, "12"}, {&number, "12.5"}, {0.0000001, "0.0000001"}} {
		if got := HumanValue(tc.value); got != tc.want {
			t.Errorf("HumanValue(%v)=%q want %q", tc.value, got, tc.want)
		}
	}
	_, stdout, _ := runOutput(t, context.Background(), []string{"check"}, func(context.Context, Invocation, *Session) Outcome {
		return Outcome{Human: "Name: " + HumanValue("x\ny") + "\nReady: " + HumanValue(true) + "\n"}
	})
	if stdout != "Name: x\\u000ay\nReady: true\n" {
		t.Fatalf("template newlines: %q", stdout)
	}
}

func TestCLIV2OutputAllHelpBeforeLookup(t *testing.T) {
	for path, want := range helpByPath {
		t.Run(path, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := append(strings.Fields(path), "--help", "--json")
			if path == "help" {
				args = []string{"help", "help", "--json"}
			}
			exit := Run(context.Background(), args, &Session{In: panicInput{}, Out: &out, Err: &stderr}, func(string) (Handler, bool) { t.Fatal("help reached lookup"); return nil, false })
			if exit != 0 || out.String() != want || stderr.Len() != 0 {
				t.Fatalf("help differs: %d %q %q", exit, out.String(), stderr.String())
			}
		})
	}
}

func TestCLIV2OutputDeprecationPrecedence(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		stderr string
		json   bool
	}{
		{nativeTestArgs([]string{"proxy", "policy", "initialise", "--state-root=/tmp/state", "--json"}), "cq: warning: Deprecated syntax; use cq proxy state initialise.\n", true},
		{nativeTestArgs([]string{"proxy", "status", "--human", "--instance-state-root=/tmp/state"}), "cq: warning: Deprecated option --human; use --json=false.\ncq: warning: Deprecated option --instance-state-root; use --state-dir.\n", false},
		{[]string{"help", "proxy", "policy"}, "cq: warning: Deprecated syntax; use cq codex proxy.\ncq: warning: Shared state initialisation moved to cq proxy state initialise.\n", false},
	} {
		exit, stdout, stderr := runOutput(t, context.Background(), tc.args, func(context.Context, Invocation, *Session) Outcome { return Outcome{Data: json.RawMessage(`{}`)} })
		if exit != 0 || stderr != tc.stderr {
			t.Fatalf("warning precedence %v: %d %q", tc.args, exit, stderr)
		}
		if tc.json {
			got := decodeOutput(t, stdout)
			if len(got.Warnings) != 1 || got.Warnings[0].Code != "deprecated_alias" {
				t.Fatalf("redundant option warning: %+v", got)
			}
		}
	}
	_, _, stderr := runOutput(t, context.Background(), []string{"check"}, func(context.Context, Invocation, *Session) Outcome {
		return Outcome{ExitCode: 1, Warnings: []Diagnostic{{"warning", "Ω\x1b\u0085"}}, Errors: []Diagnostic{{"failure", "Failed\r\n"}}}
	})
	if stderr != "cq: warning: Ω\\u001b\\u0085\ncq: Failed\\u000d\\u000a\n" {
		t.Fatalf("diagnostic escaping: %q", stderr)
	}
}

func TestCLIV2OutputStreamedHumanError(t *testing.T) {
	exit, stdout, stderr := runOutput(t, context.Background(), []string{"proxy", "serve"}, func(context.Context, Invocation, *Session) Outcome {
		return Outcome{ExitCode: 1, Streamed: true, Errors: []Diagnostic{{"failed", "Runtime failed."}}}
	})
	if exit != 1 || stdout != "" || stderr != "cq: Runtime failed.\n" {
		t.Fatalf("stream diagnostics lost: %d %q %q", exit, stdout, stderr)
	}
}

func TestCLIV2InterruptBeforeExecutionAndStreamTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	exit := Run(ctx, []string{"check", "--json"}, &Session{Out: &out, Err: io.Discard}, func(string) (Handler, bool) { t.Fatal("cancelled invocation reached lookup"); return nil, false })
	if exit != 130 || decodeOutput(t, out.String()).OK {
		t.Fatalf("pre-cancel: %d %s", exit, out.String())
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	exit, stdout, _ := runOutput(t, ctx, []string{"proxy", "serve", "--json"}, func(_ context.Context, inv Invocation, s *Session) Outcome {
		cancel()
		outcome := Outcome{ExitCode: 130, Data: json.RawMessage(`{"event":"stopped","reason":"interrupted"}`), Errors: []Diagnostic{{"interrupted", "Operation interrupted; inspect state before retrying."}}}
		if err := WriteJSON(s.Out, inv.Path, outcome); err != nil {
			t.Fatal(err)
		}
		outcome.Streamed = true
		return outcome
	})
	if exit != 130 || decodeOutput(t, stdout).OK {
		t.Fatalf("stream interrupt: %d %s", exit, stdout)
	}
}

type failingOutput struct {
	left, calls int
	bytes       bytes.Buffer
	short       bool
}

func (w *failingOutput) Write(p []byte) (int, error) {
	w.calls++
	n := min(len(p), w.left)
	w.left -= n
	w.bytes.Write(p[:n])
	if n < len(p) {
		if w.short {
			return n, nil
		}
		return n, errors.New("private writer detail")
	}
	return n, nil
}

func TestCLIV2OutputFailuresDoNotRetry(t *testing.T) {
	for _, limit := range []int{0, 1, 20} {
		for _, short := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", limit, short), func(t *testing.T) {
				w := &failingOutput{left: limit, short: short}
				calls := 0
				var stderr bytes.Buffer
				exit := Run(context.Background(), []string{"codex", "reset", "use", "alice", "--yes", "--json"}, &Session{Out: w, Err: &stderr}, func(string) (Handler, bool) {
					return func(context.Context, Invocation, *Session) Outcome {
						calls++
						return Outcome{Data: json.RawMessage(`{"outcome":"reset"}`)}
					}, true
				})
				if exit != 1 || calls != 1 || w.calls != 1 {
					t.Fatalf("failure replayed: exit=%d mutations=%d writes=%d", exit, calls, w.calls)
				}
				if strings.Contains(stderr.String(), "private writer detail") {
					t.Fatal("leaked writer error")
				}
			})
		}
	}
	if _, err := EncodeData(math.NaN()); err == nil {
		t.Fatal("NaN encode succeeded")
	}
	if _, err := EncodeData(make(chan int)); err == nil {
		t.Fatal("unsupported encode succeeded")
	}
	for _, bad := range []json.RawMessage{json.RawMessage(`{"unfinished":`), json.RawMessage(`{} {}`)} {
		var out bytes.Buffer
		if err := WriteJSON(&out, "check", Outcome{Data: bad}); err == nil || out.Len() != 0 {
			t.Fatalf("invalid resource wrote output: %q %v", out.String(), err)
		}
		exit, stdout, _ := runOutput(t, context.Background(), []string{"check", "--json"}, func(context.Context, Invocation, *Session) Outcome { return Outcome{Data: bad} })
		if exit != 1 || stdout != "" {
			t.Fatalf("encoding failure retried: %d %q", exit, stdout)
		}
	}
	for _, args := range [][]string{{"--help"}, {"completion", "bash"}, {"check"}} {
		w := &failingOutput{}
		exit := Run(context.Background(), args, &Session{Out: w, Err: io.Discard}, func(string) (Handler, bool) {
			return func(context.Context, Invocation, *Session) Outcome { return Outcome{Human: "Written.\n"} }, true
		})
		if exit != 1 || w.calls != 1 {
			t.Fatalf("human/presentation failure: %v %d %d", args, exit, w.calls)
		}
	}
}

func TestCLIV2OutputStreamAndHook(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		exit, stdout, _ := runOutput(t, context.Background(), []string{"proxy", "serve", "--json"}, func(_ context.Context, inv Invocation, s *Session) Outcome {
			if err := WriteJSON(s.Out, inv.Path, Outcome{Data: json.RawMessage(`{"event":"ready"}`)}); err != nil {
				t.Fatal(err)
			}
			last := Outcome{Data: json.RawMessage(`{"event":"stopped"}`)}
			if terminal {
				if err := WriteJSON(s.Out, inv.Path, last); err != nil {
					t.Fatal(err)
				}
				last.Streamed = true
			}
			return last
		})
		if exit != 0 || strings.Count(stdout, "\n") != 2 {
			t.Fatalf("terminal ownership: %d %q", exit, stdout)
		}
	}
	for _, structured := range []bool{false, true} {
		args := []string{"codex", "proxy", "hook", "stop"}
		if structured {
			args = append(args, "--json")
		}
		exit, stdout, _ := runOutput(t, context.Background(), args, func(context.Context, Invocation, *Session) Outcome {
			return Outcome{Data: json.RawMessage(`{"systemMessage":"Research Ω; \"night\"\nline"}`), Human: "must not use a hand-built protocol string"}
		})
		if exit != 0 {
			t.Fatal(exit)
		}
		if structured {
			if decodeOutput(t, stdout).Command != "codex proxy hook stop" {
				t.Fatal(stdout)
			}
		} else if stdout != "{\"systemMessage\":\"Research Ω; \\\"night\\\"\\nline\"}\n" {
			t.Fatalf("hook encoding: %q", stdout)
		}
	}
	for _, failedRecord := range []int{1, 2} {
		w := &failRecordOutput{fail: failedRecord}
		operations := 0
		ctx, cancel := context.WithCancel(context.Background())
		exit := Run(ctx, []string{"proxy", "serve", "--json"}, &Session{Out: w, Err: io.Discard}, func(string) (Handler, bool) {
			return func(_ context.Context, inv Invocation, s *Session) Outcome {
				operations++
				for _, event := range []string{`{"event":"ready"}`, `{"event":"stopped"}`} {
					if err := WriteJSON(s.Out, inv.Path, Outcome{Data: json.RawMessage(event)}); err != nil {
						cancel() // an interrupt must not turn output failure into a retry
						return Outcome{ExitCode: 1, Streamed: true}
					}
				}
				return Outcome{Streamed: true}
			}, true
		})
		cancel()
		if exit != 1 || w.calls != failedRecord || operations != 1 {
			t.Fatalf("stream write retried: record=%d exit=%d writes=%d operations=%d", failedRecord, exit, w.calls, operations)
		}
	}
}

type failRecordOutput struct{ fail, calls int }

func (w *failRecordOutput) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.fail {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestCLIV2InterruptPreservesKnownOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	exit, stdout, _ := runOutput(t, ctx, []string{"codex", "reset", "use", "alice", "--yes", "--json"}, func(context.Context, Invocation, *Session) Outcome {
		calls++
		cancel()
		return Outcome{Data: json.RawMessage(`{"outcome":"reset","windows_reset":2}`), Warnings: []Diagnostic{{"cleanup", "Cleanup incomplete."}}}
	})
	got := decodeOutput(t, stdout)
	if exit != 130 || got.OK || calls != 1 || len(got.Errors) != 1 || got.Errors[0] != (Diagnostic{"interrupted", "Operation interrupted; inspect state before retrying."}) || string(got.Data) != `{"outcome":"reset","windows_reset":2}` || len(got.Warnings) != 1 {
		t.Fatalf("interrupted known receipt: %d %+v", exit, got)
	}
}

func TestCLIV2OutputMissingHandler(t *testing.T) {
	exit, stdout, _ := runOutput(t, context.Background(), []string{"check", "--json"}, nil)
	got := decodeOutput(t, stdout)
	if exit != 1 || len(got.Errors) != 1 || got.Errors[0].Code != "internal_error" || string(got.Data) != "null" {
		t.Fatalf("missing registration: %d %+v", exit, got)
	}
}
