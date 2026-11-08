package cacheprofile

import "testing"

func TestUsageNormalisedProtocolCountersWithoutInventingCacheHits(t *testing.T) {
	cases := []struct {
		name, body string
		want       Usage
		ok         bool
	}{
		{"responses", `{"usage":{"input_tokens":100,"output_tokens":9,"input_tokens_details":{"cached_tokens":80}}}`, Usage{100, 80, 0, 9, true}, true},
		{"responses event", `{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":9,"total_tokens":109,"input_tokens_details":{"cached_tokens":0}}}}`, Usage{100, 0, 0, 9, true}, true},
		{"anthropic", `{"usage":{"input_tokens":5,"cache_read_input_tokens":80,"cache_creation_input_tokens":15,"output_tokens":9}}`, Usage{100, 80, 15, 9, true}, true},
		{"anthropic event", `{"message":{"usage":{"input_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":0}}}`, Usage{5, 0, 0, 0, true}, true},
		{"chat", `{"usage":{"prompt_tokens":100,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":80}}}`, Usage{100, 80, 0, 9, true}, true},
		{"gemini", `{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":80,"candidatesTokenCount":9,"thoughtsTokenCount":3,"totalTokenCount":112}}`, Usage{100, 80, 0, 12, true}, true},
		{"zero known", `{"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}`, Usage{0, 0, 0, 0, true}, true},
		{"cache absent", `{"usage":{"input_tokens":100,"output_tokens":9}}`, Usage{100, 0, 0, 9, false}, true},
		{"chat cache absent", `{"usage":{"prompt_tokens":100,"completion_tokens":9}}`, Usage{100, 0, 0, 9, false}, true},
		{"gemini cache absent", `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":9}}`, Usage{100, 0, 0, 9, false}, true},
		{"none", `{"data":{}}`, Usage{}, false},
		{"negative", `{"usage":{"input_tokens":-1,"output_tokens":9}}`, Usage{}, false},
		{"cache greater than input", `{"usage":{"input_tokens":10,"output_tokens":9,"input_tokens_details":{"cached_tokens":11}}}`, Usage{}, false},
		{"negative cache", `{"usage":{"input_tokens":10,"output_tokens":9,"input_tokens_details":{"cached_tokens":-1}}}`, Usage{}, false},
		{"fractional", `{"usage":{"input_tokens":1.5,"output_tokens":9}}`, Usage{}, false},
		{"wrong type", `{"usage":{"input_tokens":"100","output_tokens":9}}`, Usage{}, false},
		{"missing input", `{"usage":{"output_tokens":9}}`, Usage{}, false},
		{"missing output", `{"usage":{"input_tokens":100}}`, Usage{}, false},
		{"bad total", `{"usage":{"input_tokens":100,"output_tokens":9,"total_tokens":10}}`, Usage{}, false},
		{"overflow", `{"usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1,"cache_creation_input_tokens":0,"output_tokens":1}}`, Usage{}, false},
		{"duplicate key", `{"usage":{"input_tokens":100,"input_tokens":1,"output_tokens":9}}`, Usage{}, false},
		{"two authorities", `{"usage":{"input_tokens":100,"output_tokens":9},"response":{"usage":{"input_tokens":100,"output_tokens":9}}}`, Usage{}, false},
		{"two protocol counters", `{"usage":{"input_tokens":100,"output_tokens":9,"prompt_tokens":10,"completion_tokens":3}}`, Usage{}, false},
		{"malformed cache details", `{"usage":{"input_tokens":100,"output_tokens":9,"input_tokens_details":"bad"}}`, Usage{}, false},
		{"invalid reasoning count", `{"usage":{"input_tokens":100,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":10},"input_tokens_details":{"cached_tokens":0}}}`, Usage{}, false},
		{"negative reasoning count", `{"usage":{"input_tokens":100,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":-1},"input_tokens_details":{"cached_tokens":0}}}`, Usage{}, false},
		{"mixed Gemini counts", `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":9,"input_tokens":100,"output_tokens":9}}`, Usage{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UsageFromJSON([]byte(tc.body))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("got %+v,%t want %+v,%t", got, ok, tc.want, tc.ok)
			}
		})
	}
}
