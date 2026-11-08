package cacheprofile

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func BenchmarkProfilerLongPrefix(b *testing.B) {
	p, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		b.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"model":        "synthetic-model",
		"instructions": strings.Repeat("s", 1<<20),
		"input":        []any{map[string]any{"role": "user", "content": strings.Repeat("h", 3<<20)}},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Capture(Responses, body); err != nil {
			b.Fatal(err)
		}
	}
}

func profiler(t *testing.T) *Profiler {
	t.Helper()
	p, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func capture(t *testing.T, p *Profiler, protocol Protocol, body string) Snapshot {
	t.Helper()
	s, err := p.Capture(protocol, []byte(body))
	if err != nil || !s.Complete {
		t.Fatalf("capture incomplete: %v %+v", err, s)
	}
	return s
}

func TestProfilerRequiredPrivateKeyAndCopiedIt(t *testing.T) {
	for _, size := range []int{0, 1, 31, 33} {
		if _, err := New(make([]byte, size)); err == nil {
			t.Fatalf("accepted %d-byte key", size)
		}
	}
	key := bytes.Repeat([]byte{3}, 32)
	p, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	before := capture(t, p, Responses, `{"model":"private-model","input":"hello"}`)
	key[0] = 4
	after := capture(t, p, Responses, `{"model":"private-model","input":"hello"}`)
	if !Compare(before, after).PrefixStable {
		t.Fatal("retained caller's mutable key")
	}
	other, _ := New(key)
	different := capture(t, other, Responses, `{"model":"private-model","input":"hello"}`)
	if before.ModelHash == different.ModelHash || before.Components[0].Hash == different.Components[0].Hash {
		t.Fatal("content fingerprint not keyed")
	}
	encoded, _ := json.Marshal(before)
	for _, secret := range []string{"private-model", "hello"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("snapshot leaked request content")
		}
	}
}

func TestProfilerCanonicalisedObjectsButPreservedArrayOrder(t *testing.T) {
	p := profiler(t)
	a := capture(t, p, Responses, `{"model":"m","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"private_tool","parameters":{"type":"object","properties":{"z":{"type":"number"},"a":{"type":"string"}}}}],"input":[{"role":"user","content":[{"type":"input_text","text":"private_text"}]}]}`)
	b := capture(t, p, Responses, `{"input":[{"content":[{"text":"private_text","type":"input_text"}],"role":"user"}],"tools":[{"parameters":{"properties":{"a":{"type":"string"},"z":{"type":"number"}},"type":"object"},"name":"private_tool","type":"function"}],"reasoning":{"effort":"high"},"model":"m"}`)
	if got := Compare(a, b); !got.PrefixStable || got.SharedComponents != len(a.Components) {
		t.Fatalf("object key order changed prefix: %+v", got)
	}
	encoded, _ := json.Marshal(a)
	for _, secret := range []string{"private_tool", "private_text", "effort", "properties"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("serialized private semantic content: %s", secret)
		}
	}
	first := capture(t, p, Messages, `{"model":"m","tools":[{"name":"one"},{"name":"two"}],"messages":[{"role":"user","content":"hi"}]}`)
	swapped := capture(t, p, Messages, `{"model":"m","tools":[{"name":"two"},{"name":"one"}],"messages":[{"role":"user","content":"hi"}]}`)
	if got := Compare(first, swapped); got.PrefixStable || got.ChangedComponent != "tool" {
		t.Fatalf("tool array reorder lost: %+v", got)
	}
}

func TestProfilerIgnoredTransportFieldsButKeptInferenceSettings(t *testing.T) {
	p := profiler(t)
	base := capture(t, p, Responses, `{"model":"m","input":"hello","stream":true,"metadata":{"request":"first"},"prompt_cache_key":"first"}`)
	same := capture(t, p, Responses, `{"model":"m","input":"hello","stream":false,"metadata":{"request":"second"},"prompt_cache_key":"second"}`)
	if !Compare(base, same).PrefixStable {
		t.Fatal("transport metadata changed semantic prefix")
	}
	changed := capture(t, p, Responses, `{"model":"m","reasoning":{"effort":"max"},"input":"hello"}`)
	if got := Compare(base, changed); got.PrefixStable || got.ChangedComponent != "config" {
		t.Fatalf("reasoning change omitted: %+v", got)
	}
	changed = capture(t, p, Responses, `{"model":"m2","input":"hello"}`)
	if got := Compare(base, changed); got.PrefixStable || got.ChangedComponent != "model" || got.ReusableBytes != 0 {
		t.Fatalf("model change omitted: %+v", got)
	}
}

func TestProfilerPreservedAppendOnlyMessagesAndContentBlocks(t *testing.T) {
	p := profiler(t)
	cases := []struct {
		protocol      Protocol
		before, after string
	}{
		{Responses, `{"model":"m","tools":[{"name":"t"}],"instructions":"base","input":[{"role":"user","content":[{"type":"input_text","text":"first"}]}]}`, `{"model":"m","tools":[{"name":"t"}],"instructions":"base","input":[{"role":"user","content":[{"type":"input_text","text":"first"},{"type":"input_text","text":"appended"}]}]}`},
		{Messages, `{"model":"m","system":"base","messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`, `{"model":"m","system":"base","messages":[{"role":"user","content":[{"type":"text","text":"first"}]},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`},
		{ChatCompletions, `{"model":"m","messages":[{"role":"system","content":"base"},{"role":"user","content":"first"}]}`, `{"model":"m","messages":[{"role":"system","content":"base"},{"role":"user","content":"first"},{"role":"system","content":"runtime"},{"role":"user","content":"next"}]}`},
		{Gemini, `{"model":"m","systemInstruction":{"parts":[{"text":"base"}]},"contents":[{"role":"user","parts":[{"text":"first"}]}]}`, `{"model":"m","systemInstruction":{"parts":[{"text":"base"}]},"contents":[{"role":"user","parts":[{"text":"first"},{"text":"appended"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(string(tc.protocol), func(t *testing.T) {
			before := capture(t, p, tc.protocol, tc.before)
			after := capture(t, p, tc.protocol, tc.after)
			got := Compare(before, after)
			if !got.PrefixStable || got.SharedComponents != len(before.Components) || got.ReusableBytes != got.PreviousBytes || got.PreviousBytes <= 0 {
				t.Fatalf("append lost existing prefix: %+v", got)
			}
		})
	}
}

func TestProfilerFoundHoistedRuntimeInstructionRegression(t *testing.T) {
	p := profiler(t)
	tools := `[{"name":"stable_tool","input_schema":{"type":"object"}}]`
	history := `[{"role":"user","content":"` + strings.Repeat("large stable task history ", 100) + `"},{"role":"assistant","content":"work in progress"}]`
	before := capture(t, p, Messages, `{"model":"m","tools":`+tools+`,"system":[{"type":"text","text":"stable base"}],"messages":`+history+`}`)
	hoisted := capture(t, p, Messages, `{"model":"m","tools":`+tools+`,"system":[{"type":"text","text":"stable base"},{"type":"text","text":"new runtime instruction"}],"messages":`+history+`}`)
	appended := capture(t, p, Messages, `{"model":"m","tools":`+tools+`,"system":[{"type":"text","text":"stable base"}],"messages":`+strings.TrimSuffix(history, `]`)+`,{"role":"user","content":[{"type":"text","text":"new runtime instruction"}]}]}`)
	old, new := Compare(before, hoisted), Compare(before, appended)
	if old.PrefixStable || old.ReusableBytes >= new.ReusableBytes || !new.PrefixStable || new.ReusableBytes != new.PreviousBytes {
		t.Fatalf("hoisting not detected: old=%+v append=%+v", old, new)
	}
}

func TestProfilerRefusedIncompleteOrAmbiguousRequests(t *testing.T) {
	p := profiler(t)
	cases := []struct {
		protocol Protocol
		body     string
	}{
		{"unsupported", `{"model":"m","input":"hi"}`},
		{Responses, `{"model":"m","model":"other","input":"hi"}`},
		{Responses, `{"model":"m","input":"hi"} trailing`},
		{Responses, `{"model":"m","previous_response_id":"opaque","input":"hi"}`},
		{Responses, `{"model":"m","input":[{"type":"item_reference","id":"opaque"}]}`},
		{Messages, `{"model":"m","messages":"bad"}`},
		{Responses, `{"input":"unknown model"}`},
		{Gemini, `{"contents":[{"role":"user","parts":[{"text":"URL-only model"}]}]}`},
		{Messages, `{"model":"m","system":"` + strings.Repeat("x", 10<<20) + `","messages":[]}`},
		{Responses, `{"model":"m","input":[{"role":"user","content":[` + strings.Repeat(`{"type":"input_text","text":"x"},`, 4096) + `{"type":"input_text","text":"last"}]}]}`},
	}
	for _, tc := range cases {
		s, _ := p.Capture(tc.protocol, []byte(tc.body))
		if s.Complete {
			t.Fatalf("unsupported request claimed complete: protocol=%s size=%d", tc.protocol, len(tc.body))
		}
		if Compare(s, s).PrefixStable {
			t.Fatal("incomplete snapshot claimed stable")
		}
	}
}

func TestProfilerPreservedToolReferencesAndMeaningfulUnknownSettings(t *testing.T) {
	p := profiler(t)
	a := capture(t, p, Responses, `{"model":"m","future_generation_setting":"one","input":[{"type":"function_call_output","call_id":"one","output":"hi"}]}`)
	b := capture(t, p, Responses, `{"model":"m","future_generation_setting":"two","input":[{"type":"function_call_output","call_id":"one","output":"hi"}]}`)
	if Compare(a, b).PrefixStable {
		t.Fatal("unknown semantic setting silently dropped")
	}
	b = capture(t, p, Responses, `{"model":"m","future_generation_setting":"one","input":[{"type":"function_call_output","call_id":"two","output":"hi"}]}`)
	if Compare(a, b).PrefixStable {
		t.Fatal("meaningful tool-call reference silently dropped")
	}
}

func TestProfilerPreservedProtocolSpecificUnknownFields(t *testing.T) {
	p := profiler(t)
	a := capture(t, p, Gemini, `{"model":"m","instructions":"future semantic one","contents":[{"parts":[{"text":"hi"}]}]}`)
	b := capture(t, p, Gemini, `{"model":"m","instructions":"future semantic two","contents":[{"parts":[{"text":"hi"}]}]}`)
	if Compare(a, b).PrefixStable {
		t.Fatal("uninterpreted field silently omitted")
	}
}

func TestProfilerFlattenedNamespaceToolDeclarations(t *testing.T) {
	p := profiler(t)
	a := capture(t, p, Responses, `{"model":"m","tools":[{"type":"namespace","name":"functions","tools":[{"name":"one"}]}],"input":[]}`)
	b := capture(t, p, Responses, `{"model":"m","tools":[{"type":"namespace","name":"functions","tools":[{"name":"one"},{"name":"two"}]}],"input":[]}`)
	if got := Compare(a, b); !got.PrefixStable || got.ReusableBytes != got.PreviousBytes {
		t.Fatalf("appended namespace declaration invalidated old tools: %+v", got)
	}
	a = capture(t, p, Gemini, `{"model":"m","tools":[{"functionDeclarations":[{"name":"one"}]}],"contents":[]}`)
	b = capture(t, p, Gemini, `{"model":"m","tools":[{"functionDeclarations":[{"name":"one"},{"name":"two"}]}],"contents":[]}`)
	if !Compare(a, b).PrefixStable {
		t.Fatal("appended Gemini declaration invalidated old tools")
	}
}

func TestProfilerIgnoredOnlyTopLevelMessagesCacheControls(t *testing.T) {
	p := profiler(t)
	a := capture(t, p, Messages, `{"model":"m","tools":[{"name":"one","cache_control":{"type":"ephemeral"},"input_schema":{"properties":{"cache_control":{"enum":["semantic one"]}}}}],"system":[{"type":"text","text":"base","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]}]}`)
	b := capture(t, p, Messages, `{"model":"m","tools":[{"name":"one","input_schema":{"properties":{"cache_control":{"enum":["semantic one"]}}}}],"system":[{"type":"text","text":"base"}],"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`)
	if !Compare(a, b).PrefixStable {
		t.Fatal("cache policy treated as inference content")
	}
	c := capture(t, p, Messages, `{"model":"m","tools":[{"name":"one","input_schema":{"properties":{"cache_control":{"enum":["semantic two"]}}}}],"system":[{"type":"text","text":"base"}],"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`)
	if Compare(b, c).PrefixStable {
		t.Fatal("schema property with cache-control name was dropped")
	}
}
