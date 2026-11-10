package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCodexFrozenStringScanMatchesJSONValidation(t *testing.T) {
	values := [][]byte{[]byte(`""`), []byte(`"plain"`), []byte(`"\\\"\/\b\f\n\r\t"`), []byte(`"\u0041\ud83d\ude00"`), []byte(`"\uD800"`), []byte(`"\uFFFF"`), []byte(`"\u0000"`), []byte(`"\x"`), []byte(`"\uG000"`), []byte(`"\u000G"`)}
	for value := range 256 {
		values = append(values, []byte{'"', byte(value), '"'}, []byte{'"', '\\', byte(value), '"'})
	}
	for _, value := range append([][]byte(nil), values...) {
		for length := range len(value) {
			values = append(values, bytes.Clone(value[:length]))
		}
	}
	for index, value := range values {
		prefix := []byte("   ")
		source := append(prefix, value...)
		scanner := codexFrozenJSONScanner{source: source, pos: len(prefix)}
		start, end, err := scanner.scanString()
		valid := err == nil && end == len(source)
		if valid != json.Valid(value) {
			t.Fatalf("case %d: string %q accepted=%v, JSON validation=%v, error=%v", index, value, valid, json.Valid(value), err)
		}
		if err == nil && (start != len(prefix) || scanner.pos != end || source[end-1] != '"') {
			t.Fatalf("case %d: invalid successful cursor %d/%d/%d", index, start, end, scanner.pos)
		}
		if err != nil && codexFrozenAuthorityFailureCode(err) != CodexFrozenRequestProtocolInvalid {
			t.Fatalf("case %d: string syntax error changed classification: %v", index, err)
		}
	}
}

func TestCodexFrozenStringScanPreservesFailureCursor(t *testing.T) {
	for _, test := range []struct {
		value    string
		position int
	}{
		{value: `false`, position: 0},
		{value: "\"a\x01\"", position: 3},
		{value: `"\q"`, position: 3},
		{value: `"\u00"`, position: 3},
		{value: `"\u00Q0"`, position: 3},
		{value: `"unfinished`, position: 11},
	} {
		scanner := codexFrozenJSONScanner{source: []byte(test.value)}
		_, _, err := scanner.scanString()
		if err == nil || scanner.pos != test.position {
			t.Fatalf("string %q: position %d,error %v; want position%d", test.value, scanner.pos, err, test.position)
		}
	}
}

func TestCodexFrozenStringScanRejectsMalformedLargeInputAndDuplicateAuthority(t *testing.T) {
	metadata := `"client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}}`
	for _, test := range []struct {
		name, extra string
		want        CodexFrozenRequestErrorCode
	}{
		{name: "late control byte", extra: `"input":"` + strings.Repeat("x", 1<<20) + "\x01" + `"`, want: CodexFrozenRequestProtocolInvalid},
		{name: "late invalid escape", extra: `"input":"` + strings.Repeat("x", 1<<20) + `\q"`, want: CodexFrozenRequestProtocolInvalid},
		{name: "escaped duplicate model", extra: `"m\u006fdel":"gpt-5"`, want: CodexFrozenRequestModelAuthority},
		{name: "escaped duplicate metadata", extra: `"client_\u006detadata":null`, want: CodexFrozenRequestMetadataAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5",%s,%s}`, metadata, test.extra))
			inspection, err := InspectCodexNativeRequest(context.Background(), body, nil)
			if inspection != nil {
				inspection.Release()
			}
			assertFrozenRequestErrorCode(t, err, test.want)
		})
	}
}
