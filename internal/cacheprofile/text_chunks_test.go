package cacheprofile

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestProfilerPreservedLongTextBeforeEditedTail(t *testing.T) {
	p := profiler(t)
	prefix := strings.Repeat("Private long text 界🔒. ", 900)
	for _, test := range []struct {
		protocol   Protocol
		structured bool
	}{
		{Responses, false}, {Responses, true}, {Messages, false}, {Messages, true}, {ChatCompletions, false}, {ChatCompletions, true}, {Gemini, true},
	} {
		t.Run(string(test.protocol)+map[bool]string{false: "/string", true: "/block"}[test.structured], func(t *testing.T) {
			before := longTextRequest(t, test.protocol, prefix+"original ending", test.structured, nil)
			after := longTextRequest(t, test.protocol, prefix+"edited ending", test.structured, nil)
			a, err := p.Capture(test.protocol, before)
			if err != nil {
				t.Fatal(err)
			}
			b, err := p.Capture(test.protocol, after)
			if err != nil {
				t.Fatal(err)
			}
			got := Compare(a, b)
			if got.PrefixStable || got.ReusableBytes < len(prefix)-4096 || got.PreviousBytes-got.ReusableBytes > 4096+128 {
				t.Fatalf("long edited tail lost stable text: %+v prefix=%d", got, len(prefix))
			}
			encoded, _ := json.Marshal(a)
			if bytes.Contains(encoded, []byte("Private long text")) {
				t.Fatal("text reached persisted snapshot")
			}
			if test.structured {
				changedHeader := longTextRequest(t, test.protocol, prefix+"original ending", true, map[string]any{"future_text_setting": "changed"})
				c, err := p.Capture(test.protocol, changedHeader)
				if err != nil {
					t.Fatal(err)
				}
				if header := Compare(a, c); header.PrefixStable || header.ReusableBytes >= 4096 {
					t.Fatalf("unknown text metadata ignored: %+v", header)
				}
			}
		})
	}
}

func TestProfilerDidNotChunkUnknownTextBlockTypes(t *testing.T) {
	p := profiler(t)
	prefix := strings.Repeat("opaque payload ", 1000)
	a := longTextRequest(t, Responses, prefix+"first", true, map[string]any{"type": "future_opaque_type"})
	b := longTextRequest(t, Responses, prefix+"second", true, map[string]any{"type": "future_opaque_type"})
	first, err := p.Capture(Responses, a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Capture(Responses, b)
	if err != nil {
		t.Fatal(err)
	}
	if got := Compare(first, second); got.PrefixStable || got.ReusableBytes >= 4096 {
		t.Fatalf("unknown block treated as known text: %+v", got)
	}
}

func TestProfilerKeptLongTextBlockBoundary(t *testing.T) {
	p := profiler(t)
	text := strings.Repeat("x", 2*textChunkBytes)
	before, err := p.Capture(Responses, longTextRequest(t, Responses, text, false, nil))
	if err != nil {
		t.Fatal(err)
	}
	after, err := p.Capture(Responses, longTextRequest(t, Responses, text+"appended", false, nil))
	if err != nil {
		t.Fatal(err)
	}
	comparison := Compare(before, after)
	if comparison.PrefixStable || comparison.SharedComponents != len(before.Components)-1 {
		t.Fatalf("old block end silently ignored: %+v", comparison)
	}
}

func TestProfilerBoundedExpandedTextComponents(t *testing.T) {
	p := profiler(t)
	blocks := make([]any, 1100)
	for index := range blocks {
		blocks[index] = map[string]any{"type": "input_text", "text": strings.Repeat("x", textChunkBytes+1)}
	}
	body, err := json.Marshal(map[string]any{"model": "m", "input": []any{map[string]any{"role": "user", "content": blocks}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.Capture(Responses, body)
	if err == nil || snapshot.Complete || len(snapshot.Components) > maxComponents || Compare(snapshot, snapshot).PrefixStable {
		t.Fatalf("expanded text exceeded bounded incomplete contract: err=%v complete=%v components=%d", err, snapshot.Complete, len(snapshot.Components))
	}
}

func longTextRequest(t *testing.T, protocol Protocol, text string, structured bool, extra map[string]any) []byte {
	t.Helper()
	var content any = text
	if structured {
		block := map[string]any{"text": text}
		switch protocol {
		case Responses:
			block["type"] = "input_text"
		case Messages, ChatCompletions:
			block["type"] = "text"
		}
		for key, value := range extra {
			block[key] = value
		}
		content = []any{block}
	}
	request := map[string]any{"model": "model"}
	message := map[string]any{"role": "user"}
	if protocol == Gemini {
		message["parts"] = content
		request["contents"] = []any{message}
	} else {
		message["content"] = content
		if protocol == Responses {
			request["input"] = []any{message}
		} else {
			request["messages"] = []any{message}
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
