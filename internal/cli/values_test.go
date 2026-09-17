package cli

import (
	"strings"
	"testing"
)

func TestCLIV2ValueCorpus(t *testing.T) {
	for _, c := range readParseCases(t, "testdata/parser-cases.json") {
		t.Run(c.ID, func(t *testing.T) { assertParseCase(t, c) })
	}
}
func TestCLIV2ValueTimestamp(t *testing.T) {
	for _, v := range []string{"2026-09-17T12:00:00Z", "2026-09-17T12:00:00.123456789Z"} {
		if _, e := parseTimestamp(v); e != nil {
			t.Errorf("%s: %v", v, e)
		}
	}
	for _, v := range []string{"2026-09-17T12:00:00+00:00", "2026-09-17t12:00:00Z", "2026-09-17T12:00:00.1234567890Z", "2026-13-17T12:00:00Z", "2026-09-17T12:00:00,1Z"} {
		if _, e := parseTimestamp(v); e == nil {
			t.Errorf("accepted %s", v)
		}
	}
}
func TestCLIV2ValueStrictJSON(t *testing.T) {
	for _, v := range []string{`{"a":{"x":1,"x":2}}`, `{"a":1,"a":2}`, `{} {}`, "\ufeff{}", string([]byte{255}), `[1,]`} {
		var dst any
		if strictJSON(v, &dst) == nil {
			t.Errorf("accepted %q", v)
		}
	}
	var dst any
	if e := strictJSON(`{"a":[1,{"x":2}]}`, &dst); e != nil {
		t.Fatal(e)
	}
}
func TestCLIV2ValueNoNUL(t *testing.T) {
	for _, a := range [][]string{{"codex", "account", "activate", "x\x00y"}, {"codex", "proxy", "session", "digest", "--session-id=x\x00y"}, {"codex", "proxy", "pool", "set", "x\x00y", "--account=a"}} {
		if _, e := Parse(a); e == nil {
			t.Fatalf("accepted NUL in %q", a)
		}
	}
}
func TestCLIV2ValueClaudeSelector(t *testing.T) {
	id := "12345678-1234-1234-1234-123456789abc"
	for _, path := range []string{"claude account activate", "claude account remove", "claude proxy pin set"} {
		if _, e := Parse(append(strings.Fields(path), id)); e == nil {
			t.Errorf("canonical UUID accepted on %s", path)
		}
	}
	in, e := Parse([]string{"proxy", "pin", "claude", id})
	if e != nil || in.LegacySelector != "claude_uuid" {
		t.Fatalf("legacy UUID: %#v %#v", in, e)
	}
	in, e = Parse([]string{"proxy", "pin", "claude", "alice@example.com"})
	if e != nil || in.LegacySelector != "" {
		t.Fatalf("email annotated: %#v %#v", in, e)
	}
}

func TestCLIV2ValueFixtureMetadataByteLimit(t *testing.T) {
	base := `{"request_kind":"memory"}`
	for _, n := range []int{65536, 65537} {
		value := base + strings.Repeat(" ", n-len(base))
		_, e := Parse([]string{"codex", "proxy", "fixture", "create", "--input=x", "--output=y", "--metadata-json=" + value})
		if (e == nil) != (n == 65536) {
			t.Fatalf("metadata length %d: %#v", n, e)
		}
	}
}
func TestCLIV2ValueLegacyMetadata(t *testing.T) {
	legacy := `{"request_kind":"compaction","session_id":"s","thread_id":"t","turn_id":"u","compaction":{"phase":"mid_turn"}}`
	in, e := Parse([]string{"codex", "validate", "capture", "--input=x", "--output=y", "--metadata=" + legacy})
	if e != nil || !validFixtureMetadata(in.Options["metadata-json"][0]) {
		t.Fatalf("legacy metadata: %#v %#v", in, e)
	}
	_, e = Parse([]string{"codex", "proxy", "fixture", "create", "--input=x", "--output=y", "--metadata-json=" + legacy})
	if e == nil || e.Diagnostic.Code != "fixture_input_invalid" {
		t.Fatalf("legacy metadata accepted canonically: %#v", e)
	}
	huge := legacy + strings.Repeat(" ", 65537-len(legacy))
	_, e = Parse([]string{"codex", "validate", "capture", "--input=x", "--output=y", "--metadata=" + huge})
	if e == nil {
		t.Fatal("normalisation bypassed raw input size")
	}
}

func TestCLIV2ValueNormalisationCannotRepairUTF8(t *testing.T) {
	_, e := Parse([]string{"codex", "proxy", "reserve", "set", "--window=" + string([]byte{255}), "--percent=2"})
	if e == nil || e.Diagnostic.Code != "routing_invalid_argument" {
		t.Fatalf("normalisation repaired invalid UTF-8: %#v", e)
	}
}
