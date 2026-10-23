package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCLIV2HookDuplicateRequiredKeys(t *testing.T) {
	var out bytes.Buffer
	err := runProxyCodexStopHook(context.Background(), strings.NewReader(`{"hook_event_name":"Stop","session_id":"first","session_id":"last","turn_id":"u"}`), &out, proxyCodexHookDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"schema_version":2,"found":false}`))}, nil
	})})
	if err == nil || out.Len() != 0 {
		t.Fatalf("duplicate required key accepted: error=%v output=%q", err, out.String())
	}
}
func TestCLIV2HookPoolQuoted(t *testing.T) {
	got := formatCodexTurnReceipt(proxy.CodexTurnReceiptV2{CodexTurnReceiptV1: proxy.CodexTurnReceiptV1{Pool: `Research Ω; "night"`}})
	if !strings.Contains(got, `pool "Research Ω; \"night\""`) {
		t.Fatalf("pool not quoted: %s", got)
	}
}

func v2HookReceipt(pool string) proxy.CodexTurnReceiptLookupV2 {
	return proxy.CodexTurnReceiptLookupV2{SchemaVersion: 2, Found: true, Receipt: &proxy.CodexTurnReceiptV2{CodexTurnReceiptV1: proxy.CodexTurnReceiptV1{State: proxy.CodexTurnReceiptCompleted, Transport: proxy.CodexTurnReceiptTransportHTTP, RequestKind: "turn", CompactionPhase: "not_applicable", RequestLineage: "previous_response_id_absent", RequestedModelClass: "gpt_5_6_sol", RequestedReasoningEffort: "high", Pool: pool, ActualAccountHint: "codex:0123456789ab", RouteReason: proxy.CodexTurnReceiptRouteAffinityReuse}, ShadowComparison: proxy.CodexTurnReceiptShadowSameAccount}}
}
func v2HookEvent(session, turn string) string {
	data, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": session, "turn_id": turn, "last_assistant_message": "cq-fixture-secret-never-output", "extra": map[string]string{"token": "cq-fixture-secret-never-output"}})
	return string(data)
}
func newV2HookFixture(t *testing.T, found bool) *v2Fixture {
	t.Helper()
	f := &v2Fixture{In: strings.NewReader(v2HookEvent(strings.Repeat("Ω", 2048), strings.Repeat("é", 2048))), Secrets: []string{"cq-fixture-secret-never-output", "fixture-local-token"}}
	deps := proxyCodexHookDependencies{LoadConfig: func() (*proxy.Config, error) {
		f.Call("config")
		return &proxy.Config{LocalToken: "fixture-local-token"}, nil
	}, Doer: testDoer(func(request *http.Request) (*http.Response, error) {
		f.Call("network")
		var outbound map[string]string
		if err := json.NewDecoder(request.Body).Decode(&outbound); err != nil {
			t.Fatal(err)
		}
		if len(outbound) != 2 || len(outbound["session_id"]) != 4096 || len(outbound["turn_id"]) != 4096 {
			t.Fatal("forwarded fields or selector sizes differ")
		}
		if request.Header.Get("Authorization") != "Bearer fixture-local-token" {
			t.Fatal("missing local control authentication")
		}
		lookup := v2HookReceipt(`Research Ω; "night"`)
		if !found {
			lookup = proxy.CodexTurnReceiptLookupV2{SchemaVersion: 2}
		}
		data, _ := json.Marshal(lookup)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	f.Lookup = func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Hook(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2HookWithPreparation(ctx, inv, s, func(context.Context) (proxyCodexHookDependencies, error) { return deps, nil })
		}, ok
	}
	return f
}
func init() {
	registerV2Fixture("hook-unicode", func(t *testing.T) *v2Fixture { return newV2HookFixture(t, true) })
	registerV2Fixture("hook-missing-receipt", func(t *testing.T) *v2Fixture { return newV2HookFixture(t, false) })
}
func TestCLIV2HookContract(t *testing.T) {
	for _, scenario := range []string{"hook-unicode", "hook-missing-receipt"} {
		runV2Case(t, v2Case{Name: scenario, Scenario: scenario, Args: []string{"codex", "proxy", "hook", "stop", "--json"}, Command: "codex proxy hook stop", Forbid: []string{"service", "consume", "filesystem-write"}, Calls: map[string]int{"network": 1}})
	}
}
func TestCLIV2HookRawEnvelopeAndAlias(t *testing.T) {
	for _, found := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			for _, mode := range []string{"raw", "json", "false"} {
				t.Run(fmt.Sprint(found, legacy, mode), func(t *testing.T) {
					f := newV2HookFixture(t, found)
					args := []string{"codex", "proxy", "hook", "stop"}
					if legacy {
						args = []string{"proxy", "hook", "codex-stop"}
					}
					if mode == "json" {
						args = append(args, "--json")
					}
					if mode == "false" {
						args = append(args, "--json=false")
					}
					var out, diagnostic bytes.Buffer
					exit := cli.Run(context.Background(), args, &cli.Session{In: f.In, Out: &out, Err: &diagnostic}, f.Lookup)
					for _, secret := range f.Secrets {
						if v2OutputsContainSecret(out.Bytes(), diagnostic.Bytes(), secret) {
							t.Fatal("secret leaked")
						}
					}
					if exit != 0 {
						t.Fatalf("exit=%d stderr=%s", exit, diagnostic.String())
					}
					data := out.Bytes()
					if mode == "json" {
						var envelope struct {
							Data json.RawMessage `json:"data"`
						}
						if err := json.Unmarshal(data, &envelope); err != nil {
							t.Fatal(err)
						}
						data = envelope.Data
					}
					if !found {
						if string(bytes.TrimSpace(data)) != "{}" {
							t.Fatalf("missing receipt: %s", data)
						}
					} else {
						var value map[string]string
						if err := json.Unmarshal(data, &value); err != nil {
							t.Fatal(err)
						}
						want := `CQ route: completed via HTTP; pool "Research Ω; \"night\""; account codex:0123456789ab (actual); Sol/High; warm affinity. Shadow: no-affinity comparison agreed.`
						if len(value) != 1 || value["systemMessage"] != want {
							t.Fatalf("raw protocol: %s", data)
						}
					}
					if mode != "json" && strings.Contains(out.String(), "Deprecated") {
						t.Fatal("stdout deprecation")
					}
					if legacy != strings.Contains(diagnostic.String(), "Deprecated") {
						t.Fatalf("deprecation=%q", diagnostic.String())
					}
				})
			}
		}
	}
}
func TestCLIV2HookInvalidEvents(t *testing.T) {
	valid := v2HookEvent("s", "u")
	cases := map[string]string{"invalid JSON": "{", "array": "[]", "null": "null", "trailing": valid + "{}", "wrong event": strings.Replace(valid, `"Stop"`, `"Other"`, 1), "required null": strings.Replace(valid, `"session_id":"s"`, `"session_id":null`, 1), "required wrong type": strings.Replace(valid, `"turn_id":"u"`, `"turn_id":true`, 1), "missing": `{"hook_event_name":"Stop","session_id":"s"}`, "empty": v2HookEvent("", "u"), "oversize": strings.Repeat(" ", codexStopHookInputMax+1), "invalid UTF8": strings.Replace(valid, "s", string([]byte{0xff}), 1)}
	for _, key := range []string{"hook_event_name", "session_id", "turn_id"} {
		cases["duplicate "+key] = `{"` + key + `":"ignored",` + valid[1:]
	}
	for _, which := range []string{"session", "turn"} {
		for _, suffix := range []string{"a", "\x00", "\x1f", "\x7f"} {
			s, u := strings.Repeat("Ω", 2048), strings.Repeat("é", 2048)
			if which == "session" {
				s += suffix
			} else {
				u += suffix
			}
			cases[fmt.Sprintf("%s suffix %q", which, suffix)] = v2HookEvent(s, u)
		}
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := proxyCodexHookDependencies{LoadConfig: func() (*proxy.Config, error) { t.Fatal("invalid event read config"); return nil, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) { t.Fatal("invalid event network"); return nil, nil })}
			handler := func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2HookWithPreparation(ctx, inv, s, func(context.Context) (proxyCodexHookDependencies, error) { return deps, nil })
			}
			exit := cli.Run(context.Background(), []string{"codex", "proxy", "hook", "stop"}, &cli.Session{In: strings.NewReader(event), Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) { return handler, true })
			if exit != 2 || stdout.Len() != 0 || stderr.String() != "cq: Invalid Codex Stop hook input.\n" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
			}
		})
	}
}

type v2HookUnreadBody struct{ closed bool }

func (*v2HookUnreadBody) Read([]byte) (int, error) { panic("error response body read") }
func (b *v2HookUnreadBody) Close() error           { b.closed = true; return nil }
func TestCLIV2HookOperationalErrors(t *testing.T) {
	for _, test := range []struct {
		name         string
		status, exit int
		code         string
		err          error
	}{
		{"401", 401, 5, "hook_auth_failed", nil}, {"403", 403, 5, "hook_auth_failed", nil}, {"503", 503, 4, "hook_unavailable", nil}, {"network", 0, 4, "hook_unavailable", errors.New("cq-fixture-secret-never-output")}, {"timeout", 0, 7, "hook_timeout", context.DeadlineExceeded}, {"interrupted", 0, 130, "interrupted", context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &v2HookUnreadBody{}
			deps := proxyCodexHookDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: test.status, Body: body}, nil
			})}
			out := handleV2HookWithPreparation(context.Background(), cli.Invocation{}, &cli.Session{In: strings.NewReader(v2HookEvent("s", "u"))}, func(context.Context) (proxyCodexHookDependencies, error) { return deps, nil })
			if out.ExitCode != test.exit || len(out.Errors) != 1 || out.Errors[0].Code != test.code || len(out.Data) != 0 {
				t.Fatalf("outcome: %+v", out)
			}
			if test.status != 0 && !body.closed {
				t.Fatal("body not closed")
			}
		})
	}
}
func TestCLIV2HookPreparationSharesDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := handleV2HookWithPreparation(ctx, cli.Invocation{}, &cli.Session{}, func(work context.Context) (proxyCodexHookDependencies, error) {
		deadline, ok := work.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("missing fixed deadline")
		}
		cancel()
		return proxyCodexHookDependencies{}, errors.New("private error")
	})
	if out.ExitCode != 130 {
		t.Fatalf("preparation cancellation: %+v", out)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	out = handleV2HookWithPreparation(ctx, cli.Invocation{}, &cli.Session{}, func(work context.Context) (proxyCodexHookDependencies, error) {
		<-work.Done()
		return proxyCodexHookDependencies{}, errors.New("private error")
	})
	if out.ExitCode != 7 {
		t.Fatalf("preparation timeout: %+v", out)
	}
}

func TestCLIV2HookPoolControlEscapes(t *testing.T) {
	pool := "Research Ω; \"night\"\\tab\tline\nnext\x00"
	receipt := v2HookReceipt(pool)
	if !validCodexTurnReceiptLookup(receipt) {
		t.Fatal("receipt narrowed arbitrary pool text")
	}
	message := formatCodexTurnReceipt(*receipt.Receipt)
	quoted, _ := json.Marshal(pool)
	if !strings.Contains(message, "pool "+string(quoted)+"; account") || strings.ContainsAny(message, "\x00\n\t") {
		t.Fatalf("unsafe inner pool rendering: %q", message)
	}
	outer, _ := json.Marshal(map[string]string{"systemMessage": message})
	var decoded map[string]string
	if json.Unmarshal(outer, &decoded) != nil || decoded["systemMessage"] != message {
		t.Fatal("outer encoding changed message")
	}
}

func TestCLIV2HookInputBoundaryAndUnknownFields(t *testing.T) {
	base := v2HookEvent("s", "u")
	event := base + strings.Repeat(" ", codexStopHookInputMax-len(base))
	deps := proxyCodexHookDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"schema_version":2,"found":false}`))}, nil
	})}
	for _, input := range []string{event, `{"ignored":"a","ignored":"b",` + base[1:]} {
		var output bytes.Buffer
		if err := runProxyCodexStopHook(context.Background(), strings.NewReader(input), &output, deps); err != nil || output.String() != "{}\n" {
			t.Fatalf("valid ignored input failed: %v", err)
		}
	}
}
func TestCLIV2HookRejectsUnsupportedReceiptEnums(t *testing.T) {
	for _, field := range []string{"state", "transport", "request_kind", "compaction_phase", "request_lineage", "requested_model_class", "requested_reasoning_effort", "route_reason", "shadow_comparison"} {
		t.Run(field, func(t *testing.T) {
			receipt := v2HookReceipt("Research Ω")
			body, _ := json.Marshal(receipt)
			var lookup map[string]any
			_ = json.Unmarshal(body, &lookup)
			lookup["receipt"].(map[string]any)[field] = "cq-fixture-secret-never-output"
			body, _ = json.Marshal(lookup)
			deps := proxyCodexHookDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}
			out := handleV2HookWithPreparation(context.Background(), cli.Invocation{}, &cli.Session{In: strings.NewReader(v2HookEvent("s", "u"))}, func(context.Context) (proxyCodexHookDependencies, error) { return deps, nil })
			if out.ExitCode != 4 || len(out.Data) != 0 {
				t.Fatalf("bad receipt accepted: %+v", out)
			}
		})
	}
}
