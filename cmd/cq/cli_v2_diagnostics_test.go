package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	registerV2Fixture("lease-no-journal", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		handler, err := (&proxy.Server{Config: &proxy.Config{ClaudeUpstream: "https://example.test"}}).RuntimeHandler()
		if err != nil {
			t.Fatal(err)
		}
		deps := v2DiagnosticsDependencies{Lease: proxyLeaseDependencies{LoadConfig: func() (*proxy.Config, error) {
			f.Call("config-read")
			return &proxy.Config{LocalToken: "synthetic"}, nil
		}, Doer: testDoer(func(r *http.Request) (*http.Response, error) {
			f.Call("control")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w.Result(), nil
		})}}
		f.Lookup = diagnosticsTestLookup(deps)
		return f
	})
	registerV2Fixture("trace-empty", func(t *testing.T) *v2Fixture {
		return &v2Fixture{Lookup: diagnosticsTestLookup(v2DiagnosticsDependencies{Trace: proxyTraceDependencies{Now: time.Now, LoadConfig: func() (*proxy.Config, error) {
			return &proxy.Config{DiagnosticsLog: filepath.Join(t.TempDir(), "trace")}, nil
		}}})}
	})
}
func diagnosticsTestLookup(deps v2DiagnosticsDependencies) cli.Lookup {
	return func(path string) (cli.Handler, bool) {
		if _, ok := lookupV2RoutingDiagnostics(path); !ok {
			return nil, false
		}
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2RoutingDiagnosticsWithDependencies(ctx, inv, s, deps)
		}, true
	}
}
func TestCLIV2RoutingDiagnosticsContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "missing lease authority is explicit", Scenario: "lease-no-journal", Args: []string{"codex", "proxy", "lease", "invalidate", "--json"}, Exit: 4, Command: "codex proxy lease invalidate", Code: "lease_journal_unavailable", Forbid: []string{"credential-activate", "consume"}})
	runV2Case(t, v2Case{Name: "empty trace terminal", Scenario: "trace-empty", Args: []string{"codex", "proxy", "trace", "--json"}, Command: "codex proxy trace", WantJSON: `{"end":{"records":0,"reason":"completed"}}`})
}

func traceTestDependencies(path string, now time.Time) v2DiagnosticsDependencies {
	return v2DiagnosticsDependencies{Trace: proxyTraceDependencies{Now: func() time.Time { return now }, LoadConfig: func() (*proxy.Config, error) {
		return &proxy.Config{DiagnosticsLog: path, PayloadDiagnosticsLog: path + ".payload"}, nil
	}}}
}
func runDiagnosticsTest(t *testing.T, ctx context.Context, deps v2DiagnosticsDependencies, args []string, out io.Writer) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if out == nil {
		out = &stdout
	}
	exit := cli.Run(ctx, args, &cli.Session{Out: out, Err: &stderr}, diagnosticsTestLookup(deps))
	return exit, stdout.String(), stderr.String()
}
func diagnosticsTraceArgs(extra ...string) []string {
	return append([]string{"codex", "proxy", "trace", "--json"}, extra...)
}
func diagnosticsLeaseArgs(extra ...string) []string {
	return append([]string{"codex", "proxy", "lease", "invalidate", "--json"}, extra...)
}
func diagnosticsLines(t *testing.T, text string) []map[string]json.RawMessage {
	t.Helper()
	var lines []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, envelope)
	}
	return lines
}
func assertTraceTerminal(t *testing.T, text string, records, exit int, code, reason string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != records+1 {
		t.Fatalf("lines=%d records=%d: %s", len(lines), records, text)
	}
	for i, line := range lines {
		c := v2Case{Command: "codex proxy trace"}
		if i == records {
			c.Exit = exit
			c.Code = code
			c.WantJSON = fmt.Sprintf(`{"end":{"records":%d,"reason":%q}}`, records, reason)
		}
		if err := validateV2Envelope([]byte(line+"\n"), c); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCLIV2RoutingDiagnosticsTraceFiltersTailAndResource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace")
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	keys := proxy.CodexTraceSessionKeys("session-one")
	var history strings.Builder
	for i, key := range keys {
		fmt.Fprintf(&history, `{"time":%q,"event_type":"codex_trace","trace_id":"wanted","sequence":%d,"session_key":%q}`+"\n", now.Add(-time.Minute).Format(time.RFC3339Nano), i+1, key)
	}
	history.WriteString(`{"time":"2026-09-23T00:58:59.999999999Z","trace_id":"wanted","session_key":"` + keys[0] + `"}` + "\n")
	history.WriteString(`{"time":"2026-09-23T01:00:00Z","trace_id":"other","session_key":"` + keys[0] + `"}` + "\n")
	history.WriteString(`{"time":"2026-09-23T01:00:00Z","trace_id":"wanted","session_key":"nonmatching"}` + "\n")
	history.WriteString("malformed\n")
	if err := os.WriteFile(path+".1", []byte(history.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"session-one", "  codex://threads/session-one  "} {
		for _, tail := range []string{"0", "2"} {
			t.Run(selector+"/"+tail, func(t *testing.T) {
				exit, text, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, now), diagnosticsTraceArgs("--session", selector, "--trace", "wanted", "--since", "1m", "--tail", tail), nil)
				count := len(keys)
				if tail == "2" {
					count = 2
				}
				if exit != 0 {
					t.Fatalf("exit=%d %s", exit, text)
				}
				assertTraceTerminal(t, text, count, 0, "", "completed")
				var data struct {
					Record map[string]json.RawMessage `json:"record"`
				}
				if err := json.Unmarshal(diagnosticsLines(t, text)[0]["data"], &data); err != nil {
					t.Fatal(err)
				}
				if len(data.Record) != 23 || string(data.Record["kind"]) != `"route"` || string(data.Record["attempt"]) != "0" || string(data.Record["connection_id"]) != `""` {
					t.Fatalf("route resource=%s", diagnosticsLines(t, text)[0]["data"])
				}
			})
		}
	}
	// Default tail is 200 and filtering precedes tail across retained segments.
	var many strings.Builder
	for i := 0; i < 205; i++ {
		fmt.Fprint(&many, proxyTraceTestRecord(fmt.Sprint(i)))
	}
	if err := os.WriteFile(path, []byte(many.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, text, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, now), diagnosticsTraceArgs(), nil)
	if exit != 0 {
		t.Fatal(text)
	}
	assertTraceTerminal(t, text, 200, 0, "", "completed")
}

func TestCLIV2RoutingDiagnosticsPayloadRedactionAndPrecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace")
	raw := `{"time":"2026-09-23T02:00:00.000000123+01:00","event_type":"codex_payload","trace_id":"t","headers":{"aUtHoRiZaTiOn":["fixture-secret"],"Proxy-Authorization":["fixture-secret"],"Cookie":["fixture-secret"],"Set-Cookie":["fixture-secret"],"X-API-Key":["fixture-secret"],"API-Key":["fixture-secret"],"X-Request-ID":["keep"]},"body":{"prompt":"preserve arbitrary secret in user text","n":18446744073709551615,"nested":[{"access_token":"fixture-secret","refresh_token":"fixture-secret","id_token":"fixture-secret","api_key":"fixture-secret","password":"fixture-secret","client_secret":"fixture-secret","headers":{"Authorization":"fixture-secret"}}]},"unrecognised":"not public"}` + "\n"
	if err := os.WriteFile(path+".payload", []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, text, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), diagnosticsTraceArgs("--payload"), nil)
	if exit != 0 {
		t.Fatal(text)
	}
	assertTraceTerminal(t, text, 1, 0, "", "completed")
	if strings.Contains(text, "fixture-secret") || strings.Contains(text, "unrecognised") || !strings.Contains(text, "18446744073709551615") || !strings.Contains(text, "preserve arbitrary secret in user text") || !strings.Contains(text, "2026-09-23T01:00:00.000000123Z") {
		t.Fatalf("redaction/resource: %s", text)
	}
	var data struct {
		Record map[string]json.RawMessage `json:"record"`
	}
	_ = json.Unmarshal(diagnosticsLines(t, text)[0]["data"], &data)
	if len(data.Record) != 28 || string(data.Record["frame_index"]) != "0" || string(data.Record["complete"]) != "false" {
		t.Fatalf("payload fields=%d data=%s", len(data.Record), diagnosticsLines(t, text)[0]["data"])
	}
	exit, text, _ = runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), []string{"codex", "proxy", "trace", "--payload"}, nil)
	if exit != 0 || text != "2026-09-23T01:00:00.000000123Z t #0  route_summary\n" {
		t.Fatalf("payload human=%q exit=%d", text, exit)
	}
	// Route selection never reads payload records or enables capture.
	exit, text, _ = runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), diagnosticsTraceArgs(), nil)
	if exit != 0 {
		t.Fatal(text)
	}
	assertTraceTerminal(t, text, 0, 0, "", "completed")
}

func TestCLIV2RoutingDiagnosticsHumanEscapingAndUint64(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace")
	raw := `{"time":"2026-09-23T01:00:00Z","trace_id":"t\n\u001b","sequence":18446744073709551615,"transport":"http","outcome":"ok","stage":"s","direction":"in","event_name":"e","account_hint":"a","attempt":2,"upstream_status":201,"status_code":200,"pool":"p\t","close_code":3,"close_reason":"c\r","error_class":"x\u0085","reason":"r\n"}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, text, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), []string{"codex", "proxy", "trace"}, nil)
	want := `2026-09-23T01:00:00Z t\u000a\u001b #18446744073709551615 http route_summary ok s in e a attempt=2 status=201 pool=p\u0009 close=3 close_reason=c\u000d error_class=x\u0085 reason=r\u000a` + "\n"
	if exit != 0 || text != want {
		t.Fatalf("human=%q want=%q exit=%d", text, want, exit)
	}
	exit, text, _ = runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), diagnosticsTraceArgs(), nil)
	if exit != 0 || !strings.Contains(text, `"sequence":18446744073709551615`) || !strings.Contains(text, `"trace_id":"t\n\u001b"`) {
		t.Fatal(text)
	}
}

func TestCLIV2RoutingDiagnosticsPureValidationAndHelp(t *testing.T) {
	for _, args := range [][]string{
		diagnosticsTraceArgs("--tail", "0", "--tail", "2"), diagnosticsTraceArgs("--session", "codex://threads/  "), diagnosticsTraceArgs("--trace", ""), diagnosticsTraceArgs("--since", "0s"), diagnosticsTraceArgs("--tail", "-1"), diagnosticsTraceArgs("--follow", "--follow"), diagnosticsTraceArgs("--payload", "--payload"), diagnosticsTraceArgs("--timeout", "0s"), diagnosticsLeaseArgs("--port", "0"), diagnosticsLeaseArgs("--port", "65536"), diagnosticsLeaseArgs("--timeout", "-1s"),
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			exit := cli.Run(context.Background(), args, &cli.Session{Out: &out, Err: io.Discard}, func(string) (cli.Handler, bool) { t.Fatal("validation reached dependencies"); return nil, false })
			if exit != 2 {
				t.Fatalf("exit=%d %s", exit, out.String())
			}
		})
	}
	for _, path := range []string{"codex proxy trace", "codex proxy lease invalidate", "proxy trace", "proxy leases invalidate"} {
		t.Run(path+" help", func(t *testing.T) {
			var out bytes.Buffer
			exit := cli.Run(context.Background(), append(strings.Fields(path), "--help"), &cli.Session{Out: &out, Err: io.Discard}, func(string) (cli.Handler, bool) { t.Fatal("help read state"); return nil, false })
			canonical := "codex proxy trace"
			if strings.Contains(path, "lease") {
				canonical = "codex proxy lease invalidate"
			}
			want, _ := cli.Help(canonical)
			if exit != 0 || out.String() != want {
				t.Fatalf("help exit=%d", exit)
			}
		})
	}
}

func TestCLIV2RoutingDiagnosticsLeaseControlReceiptsAndSchema(t *testing.T) {
	for _, tc := range []struct {
		name, receipt, code string
		status, exit        int
	}{
		{"writer", "lease_journal_unavailable", "lease_journal_unavailable", 503, 4}, {"io", "routing_io_failed", "routing_io_failed", 503, 1}, {"conflict", "routing_conflict", "routing_conflict", 503, 6}, {"stale", "", "routing_conflict", 409, 6}, {"missing", "", "routing_control_unavailable", 503, 4}, {"unknown", "private-secret", "routing_control_unavailable", 503, 4}, {"auth precedence", "routing_io_failed", "routing_auth_failed", 401, 5}, {"forbidden precedence", "lease_journal_unavailable", "routing_auth_failed", 403, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := newReserveErrorBody(false)
			deps := v2DiagnosticsDependencies{Lease: proxyLeaseDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{http.CanonicalHeaderKey(proxy.CodexLeaseErrorHeader): []string{tc.receipt}}, Body: body}, nil
			})}}
			exit, text, _ := runDiagnosticsTest(t, context.Background(), deps, diagnosticsLeaseArgs(), nil)
			if err := validateV2Envelope([]byte(text), v2Case{Command: "codex proxy lease invalidate", Exit: tc.exit, Code: tc.code, WantJSON: "null"}); err != nil || exit != tc.exit {
				t.Fatalf("exit=%d error=%v", exit, err)
			}
			if body.reads != 0 || body.closes != 1 {
				t.Fatalf("reads=%d closes=%d", body.reads, body.closes)
			}
		})
	}
	for _, body := range []string{`null`, `{}`, `{"invalidated_leases":null,"journal_generation":1}`, `{"invalidated_leases":0,"journal_generation":null}`, `{"invalidated_leases":-1,"journal_generation":1}`, `{"invalidated_leases":0,"journal_generation":-1}`, `{"invalidated_leases":0,"journal_generation":18446744073709551616}`, `{"invalidated_leases":0,"journal_generation":1,"extra":1}`, `{"invalidated_leases":0,"journal_generation":1} {}`} {
		t.Run(body, func(t *testing.T) {
			deps := diagnosticsLeaseResponse(body)
			exit, text, _ := runDiagnosticsTest(t, context.Background(), deps, diagnosticsLeaseArgs(), nil)
			if exit != 1 || !strings.Contains(text, "routing_io_failed") {
				t.Fatalf("accepted malformed response: %s", text)
			}
		})
	}
	deps := diagnosticsLeaseResponse(`{"invalidated_leases":3,"journal_generation":18446744073709551615}`)
	exit, text, _ := runDiagnosticsTest(t, context.Background(), deps, []string{"codex", "proxy", "lease", "invalidate"}, nil)
	if exit != 0 || text != "Invalidated leases: 3\nJournal generation: 18446744073709551615\n" {
		t.Fatal(text)
	}
	exit, text, _ = runDiagnosticsTest(t, context.Background(), deps, []string{"proxy", "leases", "invalidate", "--json"}, nil)
	if exit != 0 || !strings.Contains(text, `"journal_generation":18446744073709551615`) {
		t.Fatal(text)
	}
}
func diagnosticsLeaseResponse(body string) v2DiagnosticsDependencies {
	return v2DiagnosticsDependencies{Lease: proxyLeaseDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
}

// The manual clock advances only at an explicit synchronous test boundary.
type diagnosticsDeadlineContext struct{ context.Context }

func (c diagnosticsDeadlineContext) Value(key any) any {
	return context.WithoutCancel(c.Context).Value(key)
}
func (c diagnosticsDeadlineContext) AfterFunc(fn func()) func() bool {
	return context.AfterFunc(c.Context, fn)
}
func (c diagnosticsDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

type diagnosticsHookWriter struct {
	io.Writer
	after  func()
	calls  int
	failAt int
	short  bool
}

func (w *diagnosticsHookWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		if w.short {
			return len(p) - 1, nil
		}
		return 0, errors.New("synthetic write failure")
	}
	n, err := w.Writer.Write(p)
	if w.after != nil {
		w.after()
	}
	return n, err
}

func TestCLIV2RoutingDiagnosticsTraceCancellation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trace")
			if err := os.WriteFile(path, []byte(proxyTraceTestRecord("one")), 0o600); err != nil {
				t.Fatal(err)
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ctx context.Context = parent
			if timeout {
				ctx = diagnosticsDeadlineContext{parent}
			}
			now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
			deps := traceTestDependencies(path, now)
			deps.Trace.Now = func() time.Time { return now }
			var output bytes.Buffer
			writer := &diagnosticsHookWriter{Writer: &output, after: func() { now = now.Add(time.Hour); cancel() }}
			exit, _, _ := runDiagnosticsTest(t, ctx, deps, diagnosticsTraceArgs("--follow"), writer)
			expected, code, reason := 130, "trace_interrupted", "interrupted"
			if timeout {
				expected, code, reason = 7, "trace_timeout", "timeout"
			}
			if exit != expected {
				t.Fatalf("exit=%d: %s", exit, output.String())
			}
			assertTraceTerminal(t, output.String(), 1, expected, code, reason)
		})
	}
}
func TestCLIV2RoutingDiagnosticsBudgetIncludesPreparation(t *testing.T) {
	for _, trace := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("trace=%t/cancel=%t", trace, cancelled), func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				var ctx context.Context = parent
				if !cancelled {
					ctx = diagnosticsDeadlineContext{parent}
				}
				args := diagnosticsLeaseArgs("--timeout", "1h")
				if trace {
					args = diagnosticsTraceArgs("--timeout", "1h")
				}
				inv, err := cli.Parse(args)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				outcome := handleV2RoutingDiagnosticsWithPreparation(ctx, inv, &cli.Session{Out: &out, Err: io.Discard}, time.Now, func(work context.Context) (v2DiagnosticsDependencies, error) {
					if _, ok := work.Deadline(); !ok {
						t.Fatal("budget absent during preparation")
					}
					cancel()
					<-work.Done()
					return v2DiagnosticsDependencies{}, errors.New("preparation failure loses to cancellation")
				})
				expected := 7
				if cancelled {
					expected = 130
				}
				if outcome.ExitCode != expected {
					t.Fatalf("outcome=%+v", outcome)
				}
				if trace {
					code, reason := "trace_timeout", "timeout"
					if cancelled {
						code, reason = "trace_interrupted", "interrupted"
					}
					assertTraceTerminal(t, out.String(), 0, expected, code, reason)
				}
			})
		}
	}
	// The real timer also bounds follow without resetting after preparation.
	path := filepath.Join(t.TempDir(), "trace")
	exit, text, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), diagnosticsTraceArgs("--follow", "--timeout", "1ms"), nil)
	if exit != 7 {
		t.Fatal(text)
	}
	assertTraceTerminal(t, text, 0, 7, "trace_timeout", "timeout")
}

func TestCLIV2RoutingDiagnosticsTraceWriterFailures(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		for _, short := range []bool{false, true} {
			for _, failAt := range []int{1, 2} {
				if !jsonMode && failAt == 2 {
					continue
				}
				t.Run(fmt.Sprintf("json=%t/short=%t/at=%d", jsonMode, short, failAt), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "trace")
					if err := os.WriteFile(path, []byte(proxyTraceTestRecord("one")), 0o600); err != nil {
						t.Fatal(err)
					}
					var out bytes.Buffer
					writer := &diagnosticsHookWriter{Writer: &out, failAt: failAt, short: short}
					args := []string{"codex", "proxy", "trace"}
					if jsonMode {
						args = append(args, "--json")
					}
					exit, _, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), args, writer)
					if exit != 1 || writer.calls != failAt {
						t.Fatalf("exit=%d writes=%d want=%d", exit, writer.calls, failAt)
					}
				})
			}
		}
	}
}

func TestCLIV2RoutingDiagnosticsTraceOperationalFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config *proxy.Config
		err    error
		exit   int
		code   string
	}{
		{"missing config", nil, errors.New("synthetic"), 1, "routing_io_failed"}, {"not configured", &proxy.Config{}, nil, 4, "trace_not_configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := v2DiagnosticsDependencies{Trace: proxyTraceDependencies{LoadConfig: func() (*proxy.Config, error) { return tc.config, tc.err }}}
			exit, text, _ := runDiagnosticsTest(t, context.Background(), deps, diagnosticsTraceArgs(), nil)
			if exit != tc.exit {
				t.Fatal(text)
			}
			if err := validateV2Envelope([]byte(text), v2Case{Command: "codex proxy trace", Exit: tc.exit, Code: tc.code, WantJSON: "null"}); err != nil {
				t.Fatal(err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "trace")
	if err := os.WriteFile(path+".1", []byte(`{"trace_id":"partial"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(proxyTraceTestRecord("current")), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, text, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), diagnosticsTraceArgs("--follow"), nil)
	if exit != 8 {
		t.Fatal(text)
	}
	if err := validateV2Envelope([]byte(text), v2Case{Command: "codex proxy trace", Exit: 8, Code: "trace_history_gap", WantJSON: "null"}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIV2RoutingDiagnosticsSignalHelper(t *testing.T) {
	if os.Getenv("CQ_T16_SIGNAL_HELPER") != "1" {
		return
	}
	code := cli.Run(context.Background(), diagnosticsTraceArgs("--follow", "--timeout", "10s"), &cli.Session{Out: os.Stdout, Err: os.Stderr}, lookupV2RoutingDiagnostics)
	os.Exit(code)
}
func TestCLIV2RoutingDiagnosticsSignal(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config", "cq")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "trace")
	if err := os.WriteFile(path, []byte(proxyTraceTestRecord("signal")), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(proxy.Config{DiagnosticsLog: path, LocalToken: "synthetic-signal"})
	if err := os.WriteFile(filepath.Join(config, "proxy.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIV2RoutingDiagnosticsSignalHelper$")
	cmd.Env = append(os.Environ(), "CQ_T16_SIGNAL_HELPER=1", "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_STATE_HOME="+filepath.Join(root, "state"))
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(pipe)
	first, err := reader.ReadString('\n')
	if err != nil {
		_ = cmd.Wait()
		t.Fatalf("first event: %v %s", err, stderr.String())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 130 {
		t.Fatalf("signal exit: %v stdout=%s stderr=%s", err, first+string(rest), stderr.String())
	}
	assertTraceTerminal(t, first+string(rest), 1, 130, "trace_interrupted", "interrupted")
}

func TestCLIV2RoutingDiagnosticsProductionMissingTokenAndNoCreation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	inv, err := cli.Parse(diagnosticsLeaseArgs())
	if err != nil {
		t.Fatal(err)
	}
	out := handleV2RoutingDiagnostics(context.Background(), inv, nil)
	if out.ExitCode != 1 || out.Errors[0].Code != "routing_io_failed" {
		t.Fatalf("missing config=%+v", out)
	}
	if _, err := os.Stat(filepath.Join(root, "cq")); !os.IsNotExist(err) {
		t.Fatalf("created config directory: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cq", "proxy.json"), []byte(`{"port":19280}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out = handleV2RoutingDiagnostics(context.Background(), inv, nil)
	if out.ExitCode != 5 || out.Errors[0].Code != "routing_auth_failed" {
		t.Fatalf("missing token=%+v", out)
	}
}
func TestCLIV2RoutingDiagnosticsLeaseAuthorityAndPorts(t *testing.T) {
	for _, tc := range []struct{ configured, explicit, want int }{{0, 0, 19280}, {19300, 0, 19300}, {19300, 19301, 19301}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			deps := diagnosticsLeaseResponse(`{"invalidated_leases":0,"journal_generation":42}`)
			deps.Lease.LoadConfig = func() (*proxy.Config, error) { return &proxy.Config{Port: tc.configured, LocalToken: "synthetic"}, nil }
			doer := deps.Lease.Doer
			deps.Lease.Doer = testDoer(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.String() != fmt.Sprintf("http://127.0.0.1:%d%s", tc.want, proxy.RuntimeCodexLeaseInvalidationPath) || r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Fatal("wrong control authority")
				}
				return doer.Do(r)
			})
			args := diagnosticsLeaseArgs()
			if tc.explicit != 0 {
				args = append(args, "--port", fmt.Sprint(tc.explicit))
			}
			exit, text, _ := runDiagnosticsTest(t, context.Background(), deps, args, nil)
			if exit != 0 {
				t.Fatal(text)
			}
		})
	}
	for _, tc := range []struct {
		name string
		load func() (*proxy.Config, error)
		doer testDoer
		exit int
		code string
	}{
		{"missing token", func() (*proxy.Config, error) { return &proxy.Config{}, nil }, func(*http.Request) (*http.Response, error) { t.Fatal("missing token sent request"); return nil, nil }, 5, "routing_auth_failed"},
		{"unreachable", func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic unavailable") }, 4, "routing_control_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit, text, _ := runDiagnosticsTest(t, context.Background(), v2DiagnosticsDependencies{Lease: proxyLeaseDependencies{LoadConfig: tc.load, Doer: tc.doer}}, diagnosticsLeaseArgs(), nil)
			if exit != tc.exit || !strings.Contains(text, tc.code) {
				t.Fatal(text)
			}
		})
	}
}

func TestCLIV2RoutingDiagnosticsFollowRotationOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace")
	if err := os.WriteFile(path, []byte(proxyTraceTestRecord("initial")), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	events := 0
	writer := &diagnosticsHookWriter{Writer: &out, after: func() {
		events++
		switch events {
		case 1:
			rotateProxyTraceTestLog(t, path, proxyTraceTestRecord("one"))
			rotateProxyTraceTestLog(t, path, proxyTraceTestRecord("two"))
		case 3:
			cancel()
		}
	}}
	exit, _, _ := runDiagnosticsTest(t, ctx, traceTestDependencies(path, time.Now()), diagnosticsTraceArgs("--follow"), writer)
	if exit != 130 {
		t.Fatalf("exit=%d %s", exit, out.String())
	}
	assertTraceTerminal(t, out.String(), 3, 130, "trace_interrupted", "interrupted")
	for i, name := range []string{"initial", "one", "two"} {
		if !strings.Contains(string(diagnosticsLines(t, out.String())[i]["data"]), `"trace_id":"trace:`+name+`"`) {
			t.Fatal(out.String())
		}
	}
}
func TestCLIV2RoutingDiagnosticsLeaseDeadlineNeverStartsLateMutation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := diagnosticsDeadlineContext{parent}
	inv, err := cli.Parse(diagnosticsLeaseArgs())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	out := handleV2RoutingDiagnosticsWithPreparation(ctx, inv, nil, time.Now, func(work context.Context) (v2DiagnosticsDependencies, error) {
		return v2DiagnosticsDependencies{Lease: proxyLeaseDependencies{LoadConfig: func() (*proxy.Config, error) {
			cancel()
			<-work.Done()
			return &proxy.Config{LocalToken: "synthetic"}, nil
		}, Doer: testDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("must not run") })}}, nil
	})
	if out.ExitCode != 7 || called {
		t.Fatalf("outcome=%+v mutation=%t", out, called)
	}
}

func init() {
	registerV2Fixture("trace-timeout", func(t *testing.T) *v2Fixture {
		path := filepath.Join(t.TempDir(), "trace")
		if err := os.WriteFile(path, []byte(proxyTraceTestRecord("before-timeout")), 0o600); err != nil {
			t.Fatal(err)
		}
		f := &v2Fixture{}
		f.Lookup = func(pathName string) (cli.Handler, bool) {
			if _, ok := lookupV2RoutingDiagnostics(pathName); !ok {
				return nil, false
			}
			return func(parent context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
				parent, cancel := context.WithCancel(parent)
				defer cancel()
				ctx := diagnosticsDeadlineContext{parent}
				now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
				deps := traceTestDependencies(path, now)
				deps.Trace.Now = func() time.Time { return now }
				copy := *session
				copy.Out = &diagnosticsHookWriter{Writer: session.Out, after: func() { now = now.Add(time.Hour); cancel() }}
				return handleV2RoutingDiagnosticsWithDependencies(ctx, inv, &copy, deps)
			}, true
		}
		return f
	})
}
func TestCLIV2RoutingDiagnosticsTimeoutFixture(t *testing.T) {
	constructor, err := findV2Fixture("trace-timeout")
	if err != nil {
		t.Fatal(err)
	}
	fixture := constructor(t)
	var out bytes.Buffer
	exit := cli.Run(context.Background(), diagnosticsTraceArgs("--follow", "--timeout", "1h"), &cli.Session{Out: &out, Err: io.Discard}, fixture.Lookup)
	if exit != 7 {
		t.Fatalf("exit=%d %s", exit, out.String())
	}
	assertTraceTerminal(t, out.String(), 1, 7, "trace_timeout", "timeout")
}
func TestCLIV2RoutingDiagnosticsProductionRefusesRedirect(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect outside selected authority") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	var port int
	if _, err := fmt.Sscanf(source.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(proxy.Config{Port: port, LocalToken: "synthetic-redirect"})
	if err := os.WriteFile(filepath.Join(root, "cq", "proxy.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := cli.Parse(diagnosticsLeaseArgs())
	if err != nil {
		t.Fatal(err)
	}
	out := handleV2RoutingDiagnostics(context.Background(), inv, nil)
	if out.ExitCode != 4 || out.Errors[0].Code != "routing_control_unavailable" {
		t.Fatalf("redirect outcome=%+v", out)
	}
}

func TestCLIV2RoutingDiagnosticsMalformedResourcesSkippedBeforeTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace")
	route := proxyTraceTestRecord("valid") + `{"time":"2026-09-23T01:00:00Z","trace_id":"bad","attempt":-1}` + "\n"
	payload := `{"time":"2026-09-23T01:00:00Z","event_type":"codex_payload","trace_id":"valid"}` + "\n" + `{"time":"2026-09-23T01:00:00Z","event_type":"codex_payload","trace_id":"bad","body_bytes":-1}` + "\n" + `{"time":"2026-09-23T01:00:00Z","event_type":"codex_payload","trace_id":"bad-shape","headers":[]}` + "\n"
	for _, tc := range []struct {
		path, content string
		payload       bool
	}{{path, route, false}, {path + ".payload", payload, true}} {
		t.Run(fmt.Sprint(tc.payload), func(t *testing.T) {
			if err := os.WriteFile(tc.path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			args := diagnosticsTraceArgs("--tail", "1")
			if tc.payload {
				args = append(args, "--payload")
			}
			exit, text, _ := runDiagnosticsTest(t, context.Background(), traceTestDependencies(path, time.Now()), args, nil)
			if exit != 0 {
				t.Fatal(text)
			}
			assertTraceTerminal(t, text, 1, 0, "", "completed")
			if strings.Contains(text, `"trace_id":"bad`) {
				t.Fatal(text)
			}
		})
	}
}
