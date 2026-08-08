package proxy

import (
	"strings"
	"testing"
)

func TestCodexSSESplitCoalescedCRLFAndMultiline(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(4096)
	chunks := []string{
		"data: {\"type\":\"response.metadata\",\r\n",
		"data: \"headers\":{\"X-Codex-Turn-State\":\"state-a\"}}\r\n\r\ndata: {\"type\":\"response.created\",\"response\":{}}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n",
	}
	var got []CodexSSEObservation
	for _, chunk := range chunks {
		observations, err := parser.Feed([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, observations...)
	}
	if len(got) != 4 || got[0].Kind != CodexSSEMetadata || got[0].TurnState != "state-a" || got[1].Kind != CodexSSECreated || !got[1].Admits || got[2].Kind != CodexSSEDelta || got[3].Kind != CodexSSEDone {
		t.Fatalf("observations = %#v", got)
	}
}

func TestCodexSSECompletedEndTurnTriState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want *bool
	}{
		{"absent", `data: {"type":"response.completed","response":{}}` + "\n\n", nil},
		{"false", `data: {"type":"response.completed","response":{"end_turn":false}}` + "\n\n", boolPointer(false)},
		{"true", `data: {"type":"response.completed","response":{"end_turn":true}}` + "\n\n", boolPointer(true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCodexSSE([]byte(tc.body), 4096)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Kind != CodexSSECompleted || !equalBoolPointers(got[0].EndTurn, tc.want) {
				t.Fatalf("observations = %#v", got)
			}
		})
	}
}

func TestCodexSSEMalformedUnknownErrorAndRateLimits(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		`data: {`, "",
		`data: {"type":"future.event"}`, "",
		`data: {"type":"response.created"}`, "",
		`data: {"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`, "",
		`data: {"type":"codex.rate_limits","rate_limits":[]}`, "",
	}, "\n")
	got, err := ParseCodexSSE([]byte(body), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0].Kind != CodexSSEMalformed || got[1].Kind != CodexSSEUnknown || got[2].Kind != CodexSSEMalformed || got[3].Kind != CodexSSEError || !got[3].Error.HardUsageLimit || got[4].Kind != CodexSSERateLimits {
		t.Fatalf("observations = %#v", got)
	}
}

func TestCodexSSEOversized(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(32)
	if _, err := parser.Feed([]byte("data: " + strings.Repeat("x", 40))); err == nil {
		t.Fatal("expected oversized event error")
	}
}

func TestCodexSSELargeChunkWithBoundedEvents(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(64)
	chunk := strings.Repeat("data: {\"type\":\"future.event\"}\n\n", 10)
	got, err := parser.Feed([]byte(chunk))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("observations = %d, want 10", len(got))
	}
}

func boolPointer(value bool) *bool { return &value }

func equalBoolPointers(left, right *bool) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
