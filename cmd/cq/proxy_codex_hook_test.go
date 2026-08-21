package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyCodexHookForwardsOnlyCorrelationAndWritesExactReceipt(t *testing.T) {
	t.Parallel()

	var outbound map[string]string
	doer := testDoer(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "http://127.0.0.1:24567"+proxy.RuntimeCodexTurnReceiptPath {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer local-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if bytes.Contains(body, []byte("PRIVATE")) || bytes.Contains(body, []byte("/private/transcript")) {
			t.Fatalf("private hook fields forwarded: %s", body)
		}
		if err := json.Unmarshal(body, &outbound); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		response := proxy.CodexTurnReceiptLookupV1{
			SchemaVersion: 1,
			Found:         true,
			Receipt: &proxy.CodexTurnReceiptV1{
				State:                    proxy.CodexTurnReceiptCompleted,
				Transport:                proxy.CodexTurnReceiptTransportWebSocket,
				RequestKind:              "turn",
				RequestLineage:           "previous_response_id_present",
				RequestedModelClass:      "gpt_5_6_sol",
				RequestedReasoningEffort: "high",
				CompactionPhase:          "not_applicable",
				Pool:                     "protected",
				RouteReason:              proxy.CodexTurnReceiptRouteAffinityReuse,
				PlannedAccountHint:       "codex:fedcba987654",
				ActualAccountHint:        "codex:0123456789ab",
			},
		}
		body, err = json.Marshal(response)
		if err != nil {
			t.Fatalf("encode response: %v", err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	input := strings.NewReader(`{
		"hook_event_name":"Stop",
		"session_id":"session-1",
		"turn_id":"turn-2",
		"transcript_path":"/private/transcript",
		"cwd":"/private/worktree",
		"permission_mode":"never",
		"last_assistant_message":"PRIVATE",
		"stop_hook_active":false
	}`)
	var output bytes.Buffer
	if err := runProxyCodexStopHook(context.Background(), input, &output, proxyCodexHookDependencies{
		LoadConfig: func() (*proxy.Config, error) {
			return &proxy.Config{Port: 24567, LocalToken: "local-secret"}, nil
		},
		Doer: doer,
	}); err != nil {
		t.Fatalf("run hook: %v", err)
	}
	if len(outbound) != 2 || outbound["session_id"] != "session-1" || outbound["turn_id"] != "turn-2" {
		t.Fatalf("outbound = %#v", outbound)
	}
	want := "{\"systemMessage\":\"CQ route: completed via WebSocket; pool protected; account codex:0123456789ab (actual); Sol/High; warm affinity. Shadow comparison: not enabled.\"}\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestProxyCodexHookMissWritesEmptyObject(t *testing.T) {
	t.Parallel()

	doer := testDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"schema_version":1,"found":false}`))}, nil
	})
	var output bytes.Buffer
	err := runProxyCodexStopHook(context.Background(), strings.NewReader(`{"hook_event_name":"Stop","session_id":"session","turn_id":"turn"}`), &output, proxyCodexHookDependencies{
		LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: 24567, LocalToken: "token"}, nil },
		Doer:       doer,
	})
	if err != nil || output.String() != "{}\n" {
		t.Fatalf("miss = %q, %v", output.String(), err)
	}
}

func TestProxyCodexHookRejectsInvalidInputBeforeLookup(t *testing.T) {
	t.Parallel()

	inputs := map[string]string{
		"wrong event":  `{"hook_event_name":"PreToolUse","session_id":"session","turn_id":"turn"}`,
		"missing turn": `{"hook_event_name":"Stop","session_id":"session"}`,
		"control":      "{\"hook_event_name\":\"Stop\",\"session_id\":\"session\\u0000\",\"turn_id\":\"turn\"}",
		"long id":      `{"hook_event_name":"Stop","session_id":"` + strings.Repeat("s", 4097) + `","turn_id":"turn"}`,
		"trailing":     `{"hook_event_name":"Stop","session_id":"session","turn_id":"turn"}{}`,
	}
	for name, input := range inputs {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			called := false
			err := runProxyCodexStopHook(context.Background(), strings.NewReader(input), io.Discard, proxyCodexHookDependencies{
				LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: 24567, LocalToken: "token"}, nil },
				Doer: testDoer(func(*http.Request) (*http.Response, error) {
					called = true
					return nil, errors.New("unexpected")
				}),
			})
			if err == nil || called {
				t.Fatalf("invalid input = error %v, lookup called %t", err, called)
			}
		})
	}
}

func TestProxyCodexHookBoundsInput(t *testing.T) {
	t.Parallel()

	called := false
	err := runProxyCodexStopHook(context.Background(), strings.NewReader(strings.Repeat("x", codexStopHookInputMax+1)), io.Discard, proxyCodexHookDependencies{
		LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{}, nil },
		Doer: testDoer(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("unexpected")
		}),
	})
	if err == nil || called {
		t.Fatalf("oversize input = error %v, lookup called %t", err, called)
	}
}

func TestProxyCodexHookHidesLookupFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		doer testDoer
	}{
		{
			name: "network",
			doer: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("PRIVATE network detail")
			},
		},
		{
			name: "status",
			doer: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("PRIVATE response body"))}, nil
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := runProxyCodexStopHook(context.Background(), strings.NewReader(`{"hook_event_name":"Stop","session_id":"session","turn_id":"turn"}`), io.Discard, proxyCodexHookDependencies{
				LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: 24567, LocalToken: "PRIVATE token"}, nil },
				Doer:       test.doer,
			})
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("lookup error = %v", err)
			}
		})
	}
}
