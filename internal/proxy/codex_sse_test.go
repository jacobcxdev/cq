package proxy

import (
	"errors"
	"io"
	"reflect"
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
	valid := []struct {
		name string
		body string
		want *bool
	}{
		{"absent", `data: {"type":"response.completed","response":{}}` + "\n\n", nil},
		{"false", `data: {"type":"response.completed","response":{"end_turn":false}}` + "\n\n", boolPointer(false)},
		{"true", `data: {"type":"response.completed","response":{"end_turn":true}}` + "\n\n", boolPointer(true)},
	}
	for _, tc := range valid {
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

	invalid := []struct {
		name string
		data string
	}{
		{"response absent", `{"type":"response.completed"}`},
		{"response null", `{"type":"response.completed","response":null}`},
		{"response string", `{"type":"response.completed","response":"value"}`},
		{"response number", `{"type":"response.completed","response":1}`},
		{"response boolean", `{"type":"response.completed","response":false}`},
		{"response array", `{"type":"response.completed","response":[]}`},
		{"response end turn null", `{"type":"response.completed","response":{"end_turn":null}}`},
		{"response end turn string", `{"type":"response.completed","response":{"end_turn":"false"}}`},
		{"response end turn number", `{"type":"response.completed","response":{"end_turn":0}}`},
		{"response end turn object", `{"type":"response.completed","response":{"end_turn":{}}}`},
		{"response end turn array", `{"type":"response.completed","response":{"end_turn":[]}}`},
		{"envelope end turn null", `{"type":"response.completed","response":{},"end_turn":null}`},
		{"envelope end turn string", `{"type":"response.completed","response":{},"end_turn":"false"}`},
		{"envelope end turn number", `{"type":"response.completed","response":{},"end_turn":0}`},
		{"envelope end turn object", `{"type":"response.completed","response":{},"end_turn":{}}`},
		{"envelope end turn array", `{"type":"response.completed","response":{},"end_turn":[]}`},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCodexSSE([]byte("data: "+tc.data+"\n\n"), 4096)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Kind != CodexSSEMalformed || got[0].ParseError == nil || got[0].EndTurn != nil {
				t.Fatalf("observations = %#v, want one malformed event without completion authority", got)
			}
		})
	}
}

func TestCodexSSERequiresBlankLineTermination(t *testing.T) {
	t.Parallel()
	complete := []byte("data: {\"type\":\"response.completed\",\"response\":{}}\r\n\r\n")

	for split := 0; split <= len(complete); split++ {
		parser := NewCodexSSEParser(4096)
		first, err := parser.Feed(complete[:split])
		if err != nil {
			t.Fatalf("split %d first Feed: %v", split, err)
		}
		second, err := parser.Feed(complete[split:])
		if err != nil {
			t.Fatalf("split %d second Feed: %v", split, err)
		}
		final, err := parser.Finish()
		if err != nil {
			t.Fatalf("split %d Finish: %v", split, err)
		}
		observations := append(append(first, second...), final...)
		if len(observations) != 1 || observations[0].Kind != CodexSSECompleted {
			t.Fatalf("split %d observations = %#v, want one completed event", split, observations)
		}
	}

	for truncate := 1; truncate < len(complete); truncate++ {
		parser := NewCodexSSEParser(4096)
		observations, err := parser.Feed(complete[:truncate])
		if err != nil {
			t.Fatalf("truncate %d Feed: %v", truncate, err)
		}
		final, err := parser.Finish()
		if err == nil {
			t.Fatalf("truncate %d Finish observations = %#v, error = nil; want truncated stream", truncate, final)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncate %d Finish error = %v, want unexpected EOF", truncate, err)
		}
		if len(observations) != 0 || len(final) != 0 {
			t.Fatalf("truncate %d emitted observations = %#v %#v", truncate, observations, final)
		}
	}

	parser := NewCodexSSEParser(4096)
	multiline := []byte("data: {\"type\":\"response.completed\",\n" +
		"data: \"response\":{}}\n")
	observations, err := parser.Feed(multiline)
	if err != nil {
		t.Fatal(err)
	}
	final, err := parser.Finish()
	if err == nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("multiline Finish observations = %#v, error = %v; want unexpected EOF", final, err)
	}
	if len(observations) != 0 || len(final) != 0 {
		t.Fatalf("multiline emitted observations = %#v %#v", observations, final)
	}
}

func TestCodexSSEMalformedUnknownErrorAndRateLimits(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		`data: {`, "",
		`data: {"type":"future.event"}`, "",
		`data: {"type":"response.created"}`, "",
		`data: {"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`, "",
		`data: {"type":"codex.rate_limits","rate_limits":[]}`, "", "",
	}, "\n")
	got, err := ParseCodexSSE([]byte(body), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0].Kind != CodexSSEMalformed || got[1].Kind != CodexSSEUnknown || got[2].Kind != CodexSSEMalformed || got[3].Kind != CodexSSEError || !got[3].Error.HardUsageLimit || got[4].Kind != CodexSSERateLimits {
		t.Fatalf("observations = %#v", got)
	}
}

func TestCodexSSERejectsDuplicateLifecycleAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "top-level type", data: `{"type":"response.failed","type":"response.completed","response":{}}`},
		{name: "case-variant top-level type", data: `{"Type":"response.failed","type":"response.completed","response":{}}`},
		{name: "top-level response", data: `{"type":"response.completed","response":{"end_turn":false},"response":{"end_turn":true}}`},
		{name: "nested response ID", data: `{"type":"response.created","response":{"id":"first","id":"second"}}`},
		{name: "nested response end turn", data: `{"type":"response.completed","response":{"end_turn":false,"end_turn":true}}`},
		{name: "envelope end turn", data: `{"type":"response.completed","response":{},"end_turn":false,"end_turn":true}`},
		{name: "response and envelope end turn", data: `{"type":"response.completed","response":{"end_turn":false},"end_turn":false}`},
		{name: "case-variant response and envelope end turn", data: `{"type":"response.completed","response":{"End_Turn":false},"end_turn":false}`},
		{name: "metadata headers", data: `{"type":"response.metadata","headers":{"x-codex-turn-state":"first"},"headers":{"x-codex-turn-state":"second"}}`},
		{name: "case-variant turn-state header", data: `{"type":"response.metadata","headers":{"x-codex-turn-state":"first","X-Codex-Turn-State":"second"}}`},
		{name: "Unicode-fold top-level response", data: `{"type":"response.completed","response":null,"re\u017fponse":{}}`},
		{name: "lone Unicode-fold top-level response", data: `{"type":"response.completed","re\u017fponse":{}}`},
		{name: "Unicode-fold metadata headers", data: `{"type":"response.metadata","headers":{"x-codex-turn-state":"first","x-codex-turn-\u017ftate":"second"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCodexSSEData([]byte(tc.data))
			if got.Kind != CodexSSEMalformed || got.ParseError == nil || got.Admits || got.EndTurn != nil {
				t.Fatalf("observation = %#v, want malformed without lifecycle authority", got)
			}
		})
	}
}

func TestCodexSSERejectsInvalidNestedLifecycleAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "created numeric response ID", data: `{"type":"response.created","response":{"id":42}}`},
		{name: "created case-variant numeric response ID", data: `{"type":"response.created","response":{"ID":42}}`},
		{name: "completed null response ID", data: `{"type":"response.completed","response":{"id":null}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCodexSSEData([]byte(tc.data))
			if got.Kind != CodexSSEMalformed || got.ParseError == nil || got.Admits || got.EndTurn != nil {
				t.Fatalf("observation = %#v, want malformed without lifecycle authority", got)
			}
		})
	}
}

func TestCodexSSEClassifiesCurrentNonLifecycleEvents(t *testing.T) {
	t.Parallel()
	eventTypes := []string{
		"keepalive",
		"response.custom_tool_call_input.done",
		"response.content_part.added",
		"response.content_part.done",
		"response.function_call_arguments.done",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.output_text.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.done",
		"responsesapi.websocket_timing",
	}
	for _, eventType := range eventTypes {
		t.Run(eventType, func(t *testing.T) {
			t.Parallel()
			got := classifyCodexSSEData([]byte(`{"type":"` + eventType + `"}`))
			if got.Kind != CodexSSEIgnored {
				t.Fatalf("kind = %q, want %q", got.Kind, CodexSSEIgnored)
			}
		})
	}
}

func TestCodexSSEClassifiesTerminalFailureEvents(t *testing.T) {
	t.Parallel()
	for _, eventType := range []string{"response.failed", "response.incomplete"} {
		t.Run(eventType, func(t *testing.T) {
			t.Parallel()
			got := classifyCodexSSEData([]byte(`{"type":"` + eventType + `","response":{}}`))
			if got.Kind != CodexSSEError {
				t.Fatalf("kind = %q, want %q", got.Kind, CodexSSEError)
			}
		})
	}
}

func TestCodexSSEOversized(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(32)
	if _, err := parser.Feed([]byte("data: " + strings.Repeat("x", 40))); err == nil {
		t.Fatal("expected oversized event error")
	}
}

func TestCodexSSEOversizeIsStickyAndBounded(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(32)
	observations, firstErr := parser.Feed([]byte("data: " + strings.Repeat("x", 256)))
	if firstErr == nil {
		t.Fatal("first Feed error = nil, want oversized event")
	}
	if len(observations) != 0 {
		t.Fatalf("first Feed observations = %#v, want none", observations)
	}
	if retained := codexSSEParserRetainedBytes(parser); retained != 0 || parser.eventSize != 0 {
		t.Errorf("retained state after first error: bytes=%d event_size=%d", retained, parser.eventSize)
	}

	for call := 0; call < 32; call++ {
		observations, err := parser.Feed([]byte(strings.Repeat("y", 256)))
		if err != firstErr {
			t.Fatalf("Feed %d error = %v, want original error %v", call, err, firstErr)
		}
		if len(observations) != 0 {
			t.Fatalf("Feed %d observations = %#v, want none", call, observations)
		}
		if retained := codexSSEParserRetainedBytes(parser); retained != 0 || parser.eventSize != 0 {
			t.Fatalf("retained state after Feed %d: bytes=%d event_size=%d", call, retained, parser.eventSize)
		}
	}

	final, err := parser.Finish()
	if err != firstErr {
		t.Fatalf("Finish error = %v, want original error %v", err, firstErr)
	}
	if len(final) != 0 {
		t.Fatalf("Finish observations = %#v, want none", final)
	}
	if retained := codexSSEParserRetainedBytes(parser); retained != 0 || parser.eventSize != 0 {
		t.Fatalf("retained state after Finish: bytes=%d event_size=%d", retained, parser.eventSize)
	}
}

func TestCodexSSEPreservesCompletedObservationBeforeLaterOversize(t *testing.T) {
	t.Parallel()
	completed := []byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	oversized := []byte("data: " + strings.Repeat("x", 65))

	tests := []struct {
		name   string
		chunks [][]byte
	}{
		{name: "same feed", chunks: [][]byte{append(append([]byte(nil), completed...), oversized...)}},
		{name: "split feeds", chunks: [][]byte{completed, oversized}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parser := NewCodexSSEParser(64)
			var got []CodexSSEObservation
			var firstErr error
			for _, chunk := range tc.chunks {
				observations, err := parser.Feed(chunk)
				got = append(got, observations...)
				if err != nil {
					firstErr = err
					break
				}
			}
			if firstErr == nil {
				t.Fatal("Feed error = nil, want oversized event")
			}
			if len(got) != 1 || got[0].Kind != CodexSSECompleted {
				t.Fatalf("observations = %#v, want completed event before sticky error", got)
			}
			if observations, err := parser.Feed([]byte("data: ignored\n\n")); err != firstErr || len(observations) != 0 {
				t.Fatalf("sticky Feed = %#v, %v; want no observations and %v", observations, err, firstErr)
			}
		})
	}
}

func TestCodexSSEManyEmptyDataLinesRetainBoundedState(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(codexSSEDefaultMaxEventBytes)
	line := "data:\n"
	chunk := []byte(strings.Repeat(line, codexSSEDefaultMaxEventBytes/len(line)))
	observations, err := parser.Feed(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 0 {
		t.Fatalf("observations = %#v, want none before blank-line termination", observations)
	}
	if retained := codexSSEParserRetainedBytes(parser); retained > codexSSEDefaultMaxEventBytes {
		t.Fatalf("retained bytes = %d, want <= %d", retained, codexSSEDefaultMaxEventBytes)
	}
}

func TestCodexSSEMixedDataAndPartialLineRetainBoundedState(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(codexSSEDefaultMaxEventBytes)
	chunk := []byte("data: " + strings.Repeat("a", 100_000) + "\n" +
		"data: " + strings.Repeat("b", 50_000) + "\n" +
		strings.Repeat("c", 100_000))
	observations, err := parser.Feed(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 0 {
		t.Fatalf("observations = %#v, want none before blank-line termination", observations)
	}
	if retained := codexSSEParserRetainedBytes(parser); retained > codexSSEDefaultMaxEventBytes {
		t.Fatalf("retained bytes = %d, want <= %d", retained, codexSSEDefaultMaxEventBytes)
	}
}

func TestCodexSSERetainedCapacityNeverExceedsEventLimit(t *testing.T) {
	t.Parallel()
	const maxEvent = 4096
	parser := NewCodexSSEParser(maxEvent)
	stream := []byte("data: " + strings.Repeat("a", 1800) + "\n" +
		"data: " + strings.Repeat("b", 900) + "\n" +
		strings.Repeat("c", 1000))
	for offset := 0; offset < len(stream); offset++ {
		if _, err := parser.Feed(stream[offset : offset+1]); err != nil {
			t.Fatalf("Feed byte %d: %v", offset, err)
		}
		if retained := codexSSEParserRetainedBytes(parser); retained > maxEvent {
			t.Fatalf("Feed byte %d retained capacity = %d, want <= %d", offset, retained, maxEvent)
		}
	}
}

func TestCodexSSETerminatedCommentOnlyEventResetsLimit(t *testing.T) {
	t.Parallel()
	parser := NewCodexSSEParser(16)
	observations, err := parser.Feed([]byte(strings.Repeat(": ping\n\n", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 0 {
		t.Fatalf("observations = %#v, want none", observations)
	}
	final, err := parser.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 0 {
		t.Fatalf("Finish observations = %#v, want none", final)
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

func codexSSEParserRetainedBytes(parser *CodexSSEParser) int {
	value := reflect.ValueOf(parser).Elem()
	retained := 0
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		if field.Kind() != reflect.Slice {
			continue
		}
		retained += field.Cap() * int(field.Type().Elem().Size())
		if field.Type().Elem().Kind() != reflect.Slice {
			continue
		}
		for element := 0; element < field.Len(); element++ {
			inner := field.Index(element)
			retained += inner.Cap() * int(inner.Type().Elem().Size())
		}
	}
	return retained
}
