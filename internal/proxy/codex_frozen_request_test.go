package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexFrozenRequestInspectionPreservesExactPlainReplay(t *testing.T) {
	body := []byte("{\n  \"type\": \"response.create\",\n  \"model\": \"gpt-5.4\",\n  \"client_metadata\": {\"x-codex-turn-metadata\": {\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"turn\",\"request_kind\":\"turn\"}},\n  \"input\": [{\"role\":\"user\",\"content\":\"private prompt\"}]\n}\n")
	original := bytes.Clone(body)
	header := http.Header{
		"Content-Type":      {"application/json"},
		"Accept":            {"text/event-stream"},
		"Authorization":     {"Bearer private"},
		"Cookie":            {"private=cookie"},
		"Content-Length":    {"999"},
		"X-Custom-Secret":   {"private-header"},
		"X-Codex-Window-Id": {"window"},
	}
	inspection, err := InspectCodexNativeRequest(context.Background(), body, header)
	if err != nil {
		t.Fatalf("InspectCodexNativeRequest: %v", err)
	}
	defer inspection.Release()

	protocol, err := inspection.Protocol()
	if err != nil {
		t.Fatalf("Protocol: %v", err)
	}
	if protocol.Model != "gpt-5.4" || protocol.Metadata.Source != CodexTurnMetadataNested || !protocol.Metadata.Strong || protocol.Metadata.Metadata.RequestKind != CodexRequestTurn {
		t.Fatalf("protocol = %#v", protocol)
	}

	choice := RouteChoice{
		AccountKey:      "account",
		RequestedModel:  "gpt-5.4",
		EffectiveModel:  "gpt-5.4",
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
	}
	frozen, err := inspection.Freeze(context.Background(), choice, nil, HeadroomModeCache)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	defer frozen.Release()

	body[0] = 'X'
	header.Set("Accept", "mutated")
	choice.RequiredBuckets[0] = "mutated"

	for attempt := 0; attempt < 3; attempt++ {
		replay, err := frozen.Replay()
		if err != nil {
			t.Fatalf("Replay %d: %v", attempt, err)
		}
		got := readFrozenRequestReplay(t, replay)
		replay.Release()
		if !bytes.Equal(got, original) {
			t.Fatalf("replay %d changed: got %q want %q", attempt, got, original)
		}
	}

	replay, err := frozen.Replay()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Release()
	gotHeader, err := replay.Header()
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader.Get("Content-Type") != "application/json" || gotHeader.Get("Accept") != "text/event-stream" || gotHeader.Get("X-Codex-Window-Id") != "window" {
		t.Fatalf("semantic headers = %q", gotHeader)
	}
	for _, name := range []string{"Authorization", "Cookie", "Content-Length", "X-Custom-Secret"} {
		if value := gotHeader.Get(name); value != "" {
			t.Fatalf("unsafe %s retained: %q", name, value)
		}
	}
	gotChoice, err := frozen.Choice()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotChoice.RequiredBuckets) != 1 || gotChoice.RequiredBuckets[0] != CapacityBucketBase {
		t.Fatalf("choice buckets = %v", gotChoice.RequiredBuckets)
	}
	gotChoice.RequiredBuckets[0] = "mutated-copy"
	again, err := frozen.Choice()
	if err != nil || len(again.RequiredBuckets) != 1 || again.RequiredBuckets[0] != CapacityBucketBase {
		t.Fatalf("choice mutation escaped: choice=%v error=%v", again.RequiredBuckets, err)
	}
}

func TestCodexFrozenRequestZstdInspectionCallsHeadroomOnceWithoutReencoding(t *testing.T) {
	decoded := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private prompt")
	encoded := encodeCodexZstd(t, decoded)
	header := http.Header{"Content-Encoding": {"zstd"}, "Content-Type": {"application/json"}}
	inspection, err := InspectCodexNativeRequest(context.Background(), encoded, header)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	headroom := CodexRequestHeadroomFunc(func(_ context.Context, body []byte, mode HeadroomMode) ([]byte, int, error) {
		calls++
		if mode != HeadroomModeCache || !bytes.Equal(body, decoded) {
			t.Fatalf("headroom input mode/body = %v/%q", mode, body)
		}
		return bytes.Clone(body), 0, nil
	})
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Release()
	if calls != 1 {
		t.Fatalf("headroom calls = %d, want 1", calls)
	}
	for attempt := 0; attempt < 3; attempt++ {
		replay, err := frozen.Replay()
		if err != nil {
			t.Fatal(err)
		}
		got := readFrozenRequestReplay(t, replay)
		replay.Release()
		if !bytes.Equal(got, encoded) {
			t.Fatalf("unchanged zstd replay %d was re-encoded", attempt)
		}
	}
}

func TestCodexFrozenRequestParsesOnlyFrozenReplayHeaders(t *testing.T) {
	valid := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private")
	directMetadata := `{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}`

	t.Run("connection nominated content encoding", func(t *testing.T) {
		encoded := encodeCodexZstd(t, valid)
		inspection, err := InspectCodexNativeRequest(context.Background(), encoded, http.Header{
			"Connection":       {"Content-Encoding"},
			"Content-Encoding": {"zstd"},
		})
		if inspection != nil {
			inspection.Release()
		}
		assertFrozenRequestErrorCode(t, err, CodexFrozenRequestProtocolInvalid)
	})

	t.Run("connection nominated metadata", func(t *testing.T) {
		body := []byte(`{"type":"response.create","model":"gpt-5.4"}`)
		inspection, err := InspectCodexNativeRequest(context.Background(), body, http.Header{
			"Connection":            {"X-Codex-Turn-Metadata"},
			"X-Codex-Turn-Metadata": {directMetadata},
		})
		if inspection != nil {
			inspection.Release()
		}
		assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
	})

	t.Run("connection nominated turn state", func(t *testing.T) {
		encoded := encodeCodexZstd(t, valid)
		inspection, err := InspectCodexNativeRequest(context.Background(), encoded, http.Header{
			"Connection":         {"X-Codex-Turn-State"},
			"Content-Encoding":   {"zstd"},
			"X-Codex-Turn-State": {"private-state"},
		})
		if err != nil {
			t.Fatal(err)
		}
		protocol, err := inspection.Protocol()
		if err != nil {
			t.Fatal(err)
		}
		if protocol.HasTurnState || protocol.TurnState != "" {
			t.Fatalf("nominated turn state influenced protocol: %#v", protocol)
		}
		frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeCache)
		if err != nil {
			t.Fatal(err)
		}
		defer frozen.Release()
		replay, err := frozen.Replay()
		if err != nil {
			t.Fatal(err)
		}
		defer replay.Release()
		if got := readFrozenRequestReplay(t, replay); !bytes.Equal(got, encoded) {
			t.Fatal("unchanged zstd replay bytes differ")
		}
		header, err := replay.Header()
		if err != nil {
			t.Fatal(err)
		}
		if header.Get("Content-Encoding") != "zstd" || header.Get("X-Codex-Turn-State") != "" {
			t.Fatalf("replay semantic headers = %#v", header)
		}
	})
}

func TestCodexFrozenRequestTransformsModelAndHeadroomBeforeSingleZstdEncode(t *testing.T) {
	decoded := frozenRequestBody(codexSparkModel, CodexRequestTurn, "private prompt")
	encoded := encodeCodexZstd(t, decoded)
	header := http.Header{
		"Content-Encoding": {"zstd"},
		"Content-Type":     {"application/json"},
		"Content-Length":   {"1"},
	}
	inspection, err := InspectCodexNativeRequest(context.Background(), encoded, header)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	headroom := CodexRequestHeadroomFunc(func(_ context.Context, body []byte, _ HeadroomMode) ([]byte, int, error) {
		calls++
		if got := extractModel(body); got != codexFallbackModel {
			t.Fatalf("headroom model = %q, want chosen effective model", got)
		}
		return append(bytes.Clone(body), '\n'), 17, nil
	})
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice(codexSparkModel, codexFallbackModel), headroom, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Release()
	if calls != 1 || frozen.HeadroomSavings() != 17 {
		t.Fatalf("headroom calls/savings = %d/%d", calls, frozen.HeadroomSavings())
	}

	var first []byte
	for attempt := 0; attempt < 3; attempt++ {
		replay, err := frozen.Replay()
		if err != nil {
			t.Fatal(err)
		}
		got := readFrozenRequestReplay(t, replay)
		if attempt == 0 {
			first = got
		} else if !bytes.Equal(got, first) {
			t.Fatalf("replay %d differs from first transformed encoding", attempt)
		}
		length, err := replay.ContentLength()
		if err != nil || length != int64(len(got)) {
			t.Fatalf("content length = %d, error %v, want %d", length, err, len(got))
		}
		gotHeader, err := replay.Header()
		if err != nil {
			t.Fatal(err)
		}
		if gotHeader.Get("Content-Encoding") != "zstd" || gotHeader.Get("Content-Length") != "" {
			t.Fatalf("framing headers = %q", gotHeader)
		}
		replay.Release()
	}
	if bytes.Equal(first, encoded) {
		t.Fatal("transformed request retained original encoding")
	}
	prepared, err := DecodeCodexRequest(first, "zstd", codexTransportRewriteLimits())
	if err != nil {
		t.Fatalf("decode transformed request: %v", err)
	}
	if got := extractModel(prepared.Decoded()); got != codexFallbackModel {
		t.Fatalf("transformed model = %q", got)
	}
	if !bytes.HasSuffix(prepared.Decoded(), []byte("\n")) {
		t.Fatal("headroom transform missing")
	}
}

func TestInspectCodexNativeRequestEnforcesTypedAuthority(t *testing.T) {
	valid := frozenRequestBody("gpt-5.4", CodexRequestTurn, "input")
	tests := []struct {
		name    string
		body    []byte
		header  http.Header
		want    CodexFrozenRequestErrorCode
		private string
	}{
		{name: "unsupported encoding", body: valid, header: http.Header{"Content-Encoding": {"gzip"}}, want: CodexFrozenRequestUnsupportedEncoding},
		{name: "duplicate model", body: []byte(`{"type":"response.create","model":"private-a","model":"private-b","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}}}`), want: CodexFrozenRequestModelAuthority, private: "private-a"},
		{name: "non-string model", body: []byte(`{"type":"response.create","model":42,"client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}}}`), want: CodexFrozenRequestModelAuthority},
		{name: "missing model", body: []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}}}`), want: CodexFrozenRequestModelAuthority},
		{name: "duplicate nested metadata", body: []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","session_id":"private-session","thread_id":"t","turn_id":"u","request_kind":"turn"}}}`), want: CodexFrozenRequestMetadataAuthority, private: "private-session"},
		{name: "missing metadata", body: []byte(`{"type":"response.create","model":"gpt-5.4"}`), want: CodexFrozenRequestMetadataAuthority},
		{name: "duplicate direct metadata header", body: []byte(`{"type":"response.create","model":"gpt-5.4"}`), header: http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}`, `{"session_id":"private","thread_id":"t","turn_id":"u","request_kind":"turn"}`}}, want: CodexFrozenRequestMetadataAuthority, private: "private"},
		{name: "oversized direct metadata despite nested authority", body: valid, header: http.Header{"X-Codex-Turn-Metadata": {`{"padding":"` + strings.Repeat("x", codexTurnMetadataMaxBytes) + `"}`}}, want: CodexFrozenRequestMetadataAuthority},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), test.body, test.header)
			if inspection != nil {
				inspection.Release()
			}
			assertFrozenRequestErrorCode(t, err, test.want)
			if test.private != "" && strings.Contains(err.Error(), test.private) {
				t.Fatalf("error leaked private request material: %q", err)
			}
		})
	}
}

func TestInspectCodexNativeRequestCapturesBoundedTurnStateAuthority(t *testing.T) {
	body := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private")
	header := http.Header{"X-Codex-Turn-State": {"state-one"}}
	inspection, err := InspectCodexNativeRequest(context.Background(), body, header)
	if err != nil {
		t.Fatal(err)
	}
	protocol, err := inspection.Protocol()
	if err != nil {
		t.Fatal(err)
	}
	if !protocol.HasTurnState || protocol.TurnState != "state-one" {
		t.Fatalf("inspection turn state = %#v", protocol)
	}
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Release()
	protocol, err = frozen.Protocol()
	if err != nil || !protocol.HasTurnState || protocol.TurnState != "state-one" {
		t.Fatalf("frozen turn state = %#v, error %v", protocol, err)
	}

	for _, test := range []struct {
		name   string
		header http.Header
	}{
		{name: "oversized", header: http.Header{"X-Codex-Turn-State": {strings.Repeat("x", codexTurnMetadataMaxBytes+1)}}},
		{name: "conflicting case variants", header: http.Header{"X-Codex-Turn-State": {"state-one"}, "x-codex-turn-state": {"state-two"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), body, test.header)
			if inspection != nil {
				inspection.Release()
			}
			assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
		})
	}
}

func TestInspectCodexNativeRequestReconcilesStrongMetadataSources(t *testing.T) {
	nested := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private")
	same := `{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}`
	matching, err := InspectCodexNativeRequest(context.Background(), nested, http.Header{"X-Codex-Turn-Metadata": {same}})
	if err != nil {
		t.Fatalf("matching nested/header metadata: %v", err)
	}
	matching.Release()

	conflictingHeader := `{"session_id":"session","thread_id":"other","turn_id":"turn","request_kind":"turn"}`
	if inspection, err := InspectCodexNativeRequest(context.Background(), nested, http.Header{"X-Codex-Turn-Metadata": {conflictingHeader}}); err == nil {
		inspection.Release()
		t.Fatal("conflicting nested/header metadata accepted")
	} else {
		assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
	}

	matchingFlat := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn","x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"input":[]}`)
	inspection, err := InspectCodexNativeRequest(context.Background(), matchingFlat, nil)
	if err != nil {
		t.Fatalf("matching nested/flat metadata: %v", err)
	}
	inspection.Release()
	conflictingFlat := bytes.Replace(matchingFlat, []byte(`"thread_id":"thread","turn_id":"turn","request_kind":"turn","x-codex`), []byte(`"thread_id":"other","turn_id":"turn","request_kind":"turn","x-codex`), 1)
	if inspection, err := InspectCodexNativeRequest(context.Background(), conflictingFlat, nil); err == nil {
		inspection.Release()
		t.Fatal("conflicting nested/flat metadata accepted")
	} else {
		assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
	}

	partialFlat := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","x-codex-window-id":"window","x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","window_id":"window","request_kind":"turn"}},"input":[]}`)
	partialHeader := `{"session_id":"session","thread_id":"thread","turn_id":"turn","window_id":"window","request_kind":"turn"}`
	inspection, err = InspectCodexNativeRequest(context.Background(), partialFlat, http.Header{"X-Codex-Turn-Metadata": {partialHeader}})
	if err != nil {
		t.Fatalf("matching partial flat mirror: %v", err)
	}
	inspection.Release()
	conflictingPartial := bytes.Replace(partialFlat, []byte(`"session_id":"session"`), []byte(`"session_id":"other"`), 1)
	if inspection, err := InspectCodexNativeRequest(context.Background(), conflictingPartial, http.Header{"X-Codex-Turn-Metadata": {partialHeader}}); err == nil {
		inspection.Release()
		t.Fatal("conflicting partial flat mirror accepted")
	} else {
		assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
	}
}

func TestInspectCodexNativeRequestParsesFlatCompactionObject(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"compaction","compaction":{"phase":"standalone_turn"}}}`)
	inspection, err := InspectCodexNativeRequest(context.Background(), body, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Release()
	protocol, err := inspection.Protocol()
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Metadata.Source != CodexTurnMetadataFlat || protocol.Metadata.Metadata.RequestKind != CodexRequestCompaction || protocol.Metadata.Metadata.CompactionPhase != CodexCompactionStandalone {
		t.Fatalf("flat compaction protocol = %#v", protocol)
	}
}

func TestInspectCodexNativeRequestRejectsRepeatedlyEncodedMetadata(t *testing.T) {
	encoded := `{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}`
	depth := 0
	for {
		wrapped, err := json.Marshal(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(wrapped) > codexTurnMetadataMaxBytes {
			break
		}
		encoded = string(wrapped)
		depth++
	}
	if depth < 2 {
		t.Fatalf("fixture depth = %d", depth)
	}
	body := []byte(`{"type":"response.create","model":"gpt-5.4"}`)
	inspection, err := InspectCodexNativeRequest(context.Background(), body, http.Header{"X-Codex-Turn-Metadata": {encoded}})
	if inspection != nil {
		inspection.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
}

func TestInspectCodexNativeRequestAllowsLargeNestedMetadataOnly(t *testing.T) {
	padding := strings.Repeat("x", codexTurnMetadataMaxBytes+1)
	metadata := `{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn","padding":"` + padding + `"}`
	nestedBody := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":` + metadata + `}}`)
	inspection, err := InspectCodexNativeRequest(context.Background(), nestedBody, nil)
	if err != nil {
		t.Fatalf("large nested metadata: %v", err)
	}
	inspection.Release()

	for _, test := range []struct {
		name   string
		header http.Header
	}{
		{name: "direct metadata", header: http.Header{"X-Codex-Turn-Metadata": {metadata}}},
		{name: "turn state", header: http.Header{"X-Codex-Turn-State": {padding}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), test.header)
			if inspection != nil {
				inspection.Release()
			}
			assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
		})
	}
}

func TestInspectCodexNativeRequestUnwrapsMetadataStringOnce(t *testing.T) {
	metadata := `{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}`
	wrappedBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := string(wrappedBytes)
	tests := []struct {
		name       string
		body       []byte
		header     http.Header
		wantSource CodexTurnMetadataSource
	}{
		{
			name:       "nested",
			body:       []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":` + wrapped + `}}`),
			wantSource: CodexTurnMetadataNested,
		},
		{
			name:       "direct",
			body:       []byte(`{"type":"response.create","model":"gpt-5.4"}`),
			header:     http.Header{"X-Codex-Turn-Metadata": {wrapped}},
			wantSource: CodexTurnMetadataHeader,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), test.body, test.header)
			if err != nil {
				t.Fatal(err)
			}
			defer inspection.Release()
			protocol, err := inspection.Protocol()
			if err != nil {
				t.Fatal(err)
			}
			if protocol.Metadata.Source != test.wantSource {
				t.Fatalf("metadata source = %q, want %q", protocol.Metadata.Source, test.wantSource)
			}
		})
	}

	doubleWrapped, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	doubleBody := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":` + string(doubleWrapped) + `}}`)
	inspection, err := InspectCodexNativeRequest(context.Background(), doubleBody, nil)
	if inspection != nil {
		inspection.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
}

func TestInspectCodexNativeRequestRejectsAmbiguousAuthorityForms(t *testing.T) {
	metadata := `"client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}}`
	for _, test := range []struct {
		name string
		body string
		want CodexFrozenRequestErrorCode
	}{
		{name: "case variant root model", body: `{"model":"gpt-5.4","MODEL":"gpt-5.4",` + metadata + `}`, want: CodexFrozenRequestModelAuthority},
		{name: "escaped root model", body: `{"model":"gpt-5.4","\u006dodel":"gpt-5.4",` + metadata + `}`, want: CodexFrozenRequestModelAuthority},
		{name: "escaped non-string model", body: `{"\u006dodel":42,` + metadata + `}`, want: CodexFrozenRequestModelAuthority},
		{name: "root and params model", body: `{"model":"gpt-5.4","params":{"model":"gpt-5.4"},` + metadata + `}`, want: CodexFrozenRequestModelAuthority},
		{name: "duplicate params container", body: `{"params":{"model":"gpt-5.4"},"PARAMS":null,` + metadata + `}`, want: CodexFrozenRequestProtocolInvalid},
		{name: "duplicate client metadata", body: `{"model":"gpt-5.4",` + metadata + `,"CLIENT_METADATA":null}`, want: CodexFrozenRequestMetadataAuthority},
		{name: "escaped invalid client metadata", body: `{"model":"gpt-5.4","\u0063lient_metadata":[]}`, want: CodexFrozenRequestMetadataAuthority},
		{name: "case variant nested turn", body: `{"model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","TURN_ID":"u","request_kind":"turn"}}}`, want: CodexFrozenRequestMetadataAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), []byte(test.body), nil)
			if inspection != nil {
				inspection.Release()
			}
			assertFrozenRequestErrorCode(t, err, test.want)
		})
	}
}

func TestInspectCodexNativeRequestFindsOnlyExactEncryptedContentKeys(t *testing.T) {
	for _, test := range []struct {
		name string
		item string
		want bool
	}{
		{name: "exact", item: `{"encrypted_content":null}`, want: true},
		{name: "escaped exact", item: `{"\u0065ncrypted_content":null}`, want: true},
		{name: "case variant", item: `{"Encrypted_content":null}`},
		{name: "text value", item: `{"content":"encrypted_content"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}},"input":[` + test.item + `]}`)
			inspection, err := InspectCodexNativeRequest(context.Background(), body, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer inspection.Release()
			protocol, err := inspection.Protocol()
			if err != nil {
				t.Fatal(err)
			}
			if protocol.HasEncryptedState != test.want {
				t.Fatalf("HasEncryptedState = %v, want %v", protocol.HasEncryptedState, test.want)
			}
		})
	}
}

func TestInspectCodexNativeRequestRejectsInvalidOuterJSONShape(t *testing.T) {
	valid := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private")
	deep := append([]byte(`{"model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}},"input":`), bytes.Repeat([]byte{'['}, 513)...)
	deep = append(deep, '0')
	deep = append(deep, bytes.Repeat([]byte{']'}, 513)...)
	deep = append(deep, '}')
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "array", body: []byte(`[]`)},
		{name: "null", body: []byte(`null`)},
		{name: "trailing", body: append(bytes.Clone(valid), []byte(` {}`)...)},
		{name: "depth", body: deep},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), test.body, nil)
			if inspection != nil {
				inspection.Release()
			}
			assertFrozenRequestErrorCode(t, err, CodexFrozenRequestProtocolInvalid)
		})
	}
}

func TestCodexFrozenRequestRewritesParamsModelAuthority(t *testing.T) {
	body := []byte(`{"type":"response.create","params":{"model":"` + codexSparkModel + `"},"client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"input":[]}`)
	inspection, err := InspectCodexNativeRequest(context.Background(), body, nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice(codexSparkModel, codexFallbackModel), nil, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Release()
	replay, err := frozen.Replay()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Release()
	prepared := readFrozenRequestReplay(t, replay)
	var parsed struct {
		Params struct {
			Model string `json:"model"`
		} `json:"params"`
	}
	if err := json.Unmarshal(prepared, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Params.Model != codexFallbackModel {
		t.Fatalf("params model = %q", parsed.Params.Model)
	}
}

func TestCodexFrozenRequestAcceptsBodyOverLegacyLimit(t *testing.T) {
	body := codexProtocolRequestBodyAtSize(t, maxRequestBody+1)
	inspection, err := InspectCodexNativeRequest(context.Background(), body, nil)
	if err != nil {
		t.Fatalf("inspection over legacy limit: %v", err)
	}
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5", "gpt-5"), nil, HeadroomModeCache)
	if err != nil {
		t.Fatalf("freeze over legacy limit: %v", err)
	}
	defer frozen.Release()
	replay, err := frozen.Replay()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Release()
	if length, err := replay.ContentLength(); err != nil || length != int64(len(body)) {
		t.Fatalf("length = %d, error %v", length, err)
	}
}

func TestCodexFrozenRequestRejectsChoiceMismatchBeforeReplay(t *testing.T) {
	tests := []struct {
		name     string
		choice   RouteChoice
		headroom CodexRequestHeadroom
		want     CodexFrozenRequestErrorCode
	}{
		{name: "requested model mismatch", choice: frozenRequestChoice("gpt-other", "gpt-other"), want: CodexFrozenRequestChoiceInvalid},
		{name: "missing effective model", choice: frozenRequestChoice("gpt-5.4", ""), want: CodexFrozenRequestChoiceInvalid},
		{name: "missing buckets", choice: RouteChoice{AccountKey: "account", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"}, want: CodexFrozenRequestChoiceInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
			if err != nil {
				t.Fatal(err)
			}
			frozen, err := inspection.Freeze(context.Background(), test.choice, test.headroom, HeadroomModeCache)
			if frozen != nil {
				frozen.Release()
			}
			assertFrozenRequestErrorCode(t, err, test.want)
			if _, replayErr := inspection.Protocol(); !errors.Is(replayErr, ErrCodexFrozenRequestReleased) {
				t.Fatalf("failed freeze retained inspection: %v", replayErr)
			}
		})
	}
}

func TestCodexFrozenRequestAcceptsLargeHeadroomTransform(t *testing.T) {
	source := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private")
	transformed := make([]byte, 0, maxRequestBody+1)
	transformed = append(transformed, source[:len(source)-1]...)
	transformed = append(transformed, bytes.Repeat([]byte{' '}, maxRequestBody+1-len(source))...)
	transformed = append(transformed, '}')
	wantTransformed := bytes.Clone(transformed)
	inspection, err := InspectCodexNativeRequest(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), CodexRequestHeadroomFunc(func(context.Context, []byte, HeadroomMode) ([]byte, int, error) {
		return transformed, 1, nil
	}), HeadroomModeCache)
	if err != nil {
		t.Fatalf("large transform rejected: %v", err)
	}
	defer frozen.Release()
	replay, err := frozen.Replay()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Release()
	got, err := replay.DecodedBody()
	if err != nil || !bytes.Equal(got, wantTransformed) {
		t.Fatalf("transformed replay = %d bytes, %v; want %d bytes", len(got), err, len(wantTransformed))
	}
}

func TestCodexFrozenRequestRejectsEffectiveModelBucketMismatch(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody(codexSparkModel, CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	choice := frozenRequestChoice(codexSparkModel, codexSparkModel)
	choice.RequiredBuckets = []CapacityBucket{CapacityBucketBase}
	frozen, err := inspection.Freeze(context.Background(), choice, nil, HeadroomModeCache)
	if frozen != nil {
		frozen.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestChoiceInvalid)
}

func TestCodexFrozenRequestRejectsTransformedAuthorityChanges(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		want CodexFrozenRequestErrorCode
	}{
		{name: "effective model", body: frozenRequestBody("gpt-other", CodexRequestTurn, "private transformed"), want: CodexFrozenRequestModelAuthority},
		{name: "missing metadata", body: []byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`), want: CodexFrozenRequestMetadataAuthority},
		{name: "turn identity", body: bytes.Replace(frozenRequestBody("gpt-5.4", CodexRequestTurn, "private transformed"), []byte(`"turn_id":"turn"`), []byte(`"turn_id":"other"`), 1), want: CodexFrozenRequestMetadataAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), http.Header{"X-Codex-Turn-State": {"state-one"}})
			if err != nil {
				t.Fatal(err)
			}
			returned := bytes.Clone(test.body)
			headroom := CodexRequestHeadroomFunc(func(context.Context, []byte, HeadroomMode) ([]byte, int, error) {
				return returned, 1, nil
			})
			frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeToken)
			if frozen != nil {
				frozen.Release()
			}
			assertFrozenRequestErrorCode(t, err, test.want)
			if !allZero(returned) {
				t.Fatal("rejected transformed bytes were not cleared")
			}
		})
	}
}

func TestCodexFrozenRequestRejectsTransformedPreviousAuthorityShape(t *testing.T) {
	metadata := `"client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}`
	tests := []struct {
		name        string
		source      []byte
		transformed []byte
	}{
		{
			name:        "root moved to params",
			source:      []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"response",` + metadata + `}`),
			transformed: []byte(`{"type":"response.create","model":"gpt-5.4","params":{"previous_response_id":"response"},` + metadata + `}`),
		},
		{
			name:        "matching dual reduced to root",
			source:      []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"response","params":{"previous_response_id":"response"},` + metadata + `}`),
			transformed: []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"response",` + metadata + `}`),
		},
		{
			name:        "explicit null removed",
			source:      []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":null,` + metadata + `}`),
			transformed: []byte(`{"type":"response.create","model":"gpt-5.4",` + metadata + `}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCodexNativeRequest(context.Background(), test.source, nil)
			if err != nil {
				t.Fatal(err)
			}
			headroom := CodexRequestHeadroomFunc(func(context.Context, []byte, HeadroomMode) ([]byte, int, error) {
				return bytes.Clone(test.transformed), 0, nil
			})
			frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
			if frozen != nil {
				frozen.Release()
			}
			assertFrozenRequestErrorCode(t, err, CodexFrozenRequestProtocolInvalid)
		})
	}
}

func TestCodexFrozenRequestRejectsTransformedMetadataSourceRemoval(t *testing.T) {
	metadata := `{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}`
	source := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn","x-codex-turn-metadata":` + metadata + `}}`)
	inspection, err := InspectCodexNativeRequest(context.Background(), source, http.Header{"X-Codex-Turn-Metadata": {metadata}})
	if err != nil {
		t.Fatal(err)
	}
	transformed := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private transformed")
	headroom := CodexRequestHeadroomFunc(func(context.Context, []byte, HeadroomMode) ([]byte, int, error) {
		return bytes.Clone(transformed), 0, nil
	})
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
	if frozen != nil {
		frozen.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestMetadataAuthority)
}

func TestCodexFrozenRequestPreCanceledWorkDoesNotTransform(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inspection, err := InspectCodexNativeRequest(ctx, frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if inspection != nil {
		inspection.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestCanceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("inspection cancellation identity = %v", err)
	}

	inspection, err = InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ownedEncoded := inspection.state.encoded
	ownedDecoded := inspection.state.decoded
	calls := 0
	headroom := CodexRequestHeadroomFunc(func(context.Context, []byte, HeadroomMode) ([]byte, int, error) {
		calls++
		return nil, 0, nil
	})
	frozen, err := inspection.Freeze(ctx, frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
	if frozen != nil {
		frozen.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestCanceled)
	if calls != 0 {
		t.Fatalf("pre-canceled Headroom calls = %d", calls)
	}
	if !allZero(ownedEncoded) || !allZero(ownedDecoded) || inspection.state.encoded != nil || inspection.state.decoded != nil || inspection.state.headers != nil {
		t.Fatal("pre-canceled Freeze retained consumed ownership")
	}
}

func TestCodexFrozenRequestCancellationStopsBeforeDispatchAndClearsTransform(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var returned []byte
	headroom := CodexRequestHeadroomFunc(func(context.Context, []byte, HeadroomMode) ([]byte, int, error) {
		returned = []byte(`{"model":"gpt-5.4","input":"private transformed"}`)
		cancel()
		return returned, 1, nil
	})
	frozen, err := inspection.Freeze(ctx, frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
	if frozen != nil {
		frozen.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestCanceled)
	if !errors.Is(err, context.Canceled) || errors.Unwrap(err) != nil {
		t.Fatalf("cancellation identity/chain = %v/%v", errors.Is(err, context.Canceled), errors.Unwrap(err))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause = %v", err)
	}
	if !allZero(returned) {
		t.Fatalf("canceled transformed bytes not cleared: %q", returned)
	}
	if _, err := inspection.Protocol(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("canceled inspection remained available: %v", err)
	}
}

func TestCodexFrozenRequestBridgeAdapterPropagatesDispatchedCancellation(t *testing.T) {
	dispatched := make(chan []byte, 1)
	bridge := headroomTestBridge(t, func(request []byte) headroomTestAction {
		dispatched <- bytes.Clone(request)
		return headroomTestAction{}
	})
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		request *CodexFrozenRequest
		err     error
	}, 1)
	go func() {
		request, freezeErr := inspection.Freeze(ctx, frozenRequestChoice("gpt-5.4", "gpt-5.4"), NewCodexRequestHeadroomAdapter(bridge), HeadroomModeCache)
		result <- struct {
			request *CodexFrozenRequest
			err     error
		}{request: request, err: freezeErr}
	}()

	select {
	case request := <-dispatched:
		var parsed headroomResponsesRequest
		if err := json.Unmarshal(request, &parsed); err != nil {
			t.Fatalf("parse dispatched request: %v", err)
		}
		if parsed.Instructions != nil {
			t.Fatal("cache adapter passed frozen instructions")
		}
	case <-time.After(time.Second):
		t.Fatal("preparer adapter did not dispatch")
	}
	cancel()
	select {
	case got := <-result:
		if got.request != nil {
			got.request.Release()
			t.Fatal("canceled preparation returned a frozen request")
		}
		assertFrozenRequestErrorCode(t, got.err, CodexFrozenRequestCanceled)
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancellation cause = %v", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatched cancellation did not unblock preparation")
	}
}

func TestCodexFrozenRequestBridgeAdapterFailsOpenWithoutPrivateError(t *testing.T) {
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		return headroomTestAction{Exit: true}
	})
	body := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private-headroom-input")
	adapter := NewCodexRequestHeadroomAdapter(bridge)
	got, saved, err := adapter.CompressResponses(context.Background(), body, HeadroomModeToken)
	if err != nil {
		t.Fatalf("bridge failure was not fail-open: %v", err)
	}
	if saved != 0 || !bytes.Equal(got, body) {
		t.Fatalf("fail-open result = %d bytes/%d saved, want %d/0", len(got), saved, len(body))
	}
}

func TestCodexFrozenRequestInfersHeaderlessZstdBeforeHeadroom(t *testing.T) {
	body := frozenRequestBody("gpt-5.4", CodexRequestTurn, "private-zstd-input")
	encoded := encodeCodexZstd(t, body)
	inspection, err := InspectCodexNativeRequest(context.Background(), encoded, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	headroom := CodexRequestHeadroomFunc(func(_ context.Context, got []byte, _ HeadroomMode) ([]byte, int, error) {
		if !bytes.Equal(got, body) {
			t.Fatalf("Headroom input = %q, want decoded JSON", got)
		}
		return got, 0, nil
	})
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Release()
	replay, err := frozen.Replay()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Release()
	gotHeader, err := replay.Header()
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader.Get("Content-Encoding") != "zstd" {
		t.Fatalf("replay Content-Encoding = %q, want zstd", gotHeader.Get("Content-Encoding"))
	}
}

func TestCodexFrozenRequestHeadroomFailureIsTypedAndClearsReturnedBytes(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var returned []byte
	calls := 0
	headroom := CodexRequestHeadroomFunc(func(context.Context, []byte, HeadroomMode) ([]byte, int, error) {
		calls++
		returned = []byte(`{"model":"gpt-5.4","input":"private transformed"}`)
		return returned, 0, errors.New("private headroom failure")
	})
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
	if frozen != nil {
		frozen.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestTransformFailed)
	if calls != 1 {
		t.Fatalf("headroom calls = %d, want 1", calls)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("typed error leaked cause: %q", err)
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("typed error exposed private cause: %v", errors.Unwrap(err))
	}
	if !allZero(returned) {
		t.Fatalf("failed transform bytes not cleared: %q", returned)
	}
}

func TestCodexFrozenRequestErrorsHidePrivateChainsAndPreserveDeadline(t *testing.T) {
	privateEncoding := "private-encoding"
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), http.Header{
		"Content-Encoding": {privateEncoding},
	})
	if inspection != nil {
		inspection.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestUnsupportedEncoding)
	if errors.Unwrap(err) != nil || strings.Contains(err.Error(), privateEncoding) {
		t.Fatalf("unsupported encoding exposed private chain: %v", err)
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	inspection, err = InspectCodexNativeRequest(ctx, frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if inspection != nil {
		inspection.Release()
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestCanceled)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Unwrap(err) != nil {
		t.Fatalf("deadline identity/chain = %v/%v", errors.Is(err, context.DeadlineExceeded), errors.Unwrap(err))
	}
}

func TestCodexFrozenRequestInspectionCopiesShareSingleUseState(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	copyOfInspection := *inspection
	frozen, err := copyOfInspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeCache)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Release()
	duplicate, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeCache)
	if duplicate != nil {
		duplicate.Release()
		t.Fatal("copied inspection froze twice")
	}
	assertFrozenRequestErrorCode(t, err, CodexFrozenRequestErrorCode("consumed"))
	if !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("consumed error identity = %v", err)
	}
}

func TestCodexFrozenRequestCopiesShareReleaseState(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeCache)
	if err != nil {
		t.Fatal(err)
	}
	copyOfFrozen := *frozen
	copyOfFrozen.Release()
	frozen.Release()
	if _, err := frozen.Protocol(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("original Protocol after copied release = %v", err)
	}
	if _, err := copyOfFrozen.Replay(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) {
		t.Fatalf("copied Replay after release = %v", err)
	}
}

func TestCodexFrozenRequestInspectionCopiesRaceOnOneConsumption(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	copyOfInspection := *inspection
	handles := []*CodexFrozenRequestInspection{inspection, &copyOfInspection}
	start := make(chan struct{})
	results := make(chan struct {
		request *CodexFrozenRequest
		err     error
	}, len(handles))
	headroomCalls := make(chan struct{}, len(handles))
	headroom := CodexRequestHeadroomFunc(func(_ context.Context, body []byte, _ HeadroomMode) ([]byte, int, error) {
		headroomCalls <- struct{}{}
		return bytes.Clone(body), 0, nil
	})
	for _, handle := range handles {
		go func(candidate *CodexFrozenRequestInspection) {
			<-start
			request, freezeErr := candidate.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
			results <- struct {
				request *CodexFrozenRequest
				err     error
			}{request: request, err: freezeErr}
		}(handle)
	}
	close(start)
	successes := 0
	consumed := 0
	for range handles {
		result := <-results
		if result.err == nil {
			successes++
			result.request.Release()
			continue
		}
		assertFrozenRequestErrorCode(t, result.err, CodexFrozenRequestErrorCode("consumed"))
		consumed++
	}
	if successes != 1 || consumed != 1 || len(headroomCalls) != 1 {
		t.Fatalf("success/consumed/headroom = %d/%d/%d", successes, consumed, len(headroomCalls))
	}
}

func TestCodexFrozenRequestCopiesReleaseConcurrently(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeCache)
	if err != nil {
		t.Fatal(err)
	}
	copyOfFrozen := *frozen
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	for _, handle := range []*CodexFrozenRequest{frozen, &copyOfFrozen} {
		go func(candidate *CodexFrozenRequest) {
			<-start
			candidate.Release()
			done <- struct{}{}
		}(handle)
	}
	close(start)
	<-done
	<-done
	if _, err := frozen.Choice(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("Choice after concurrent copied release = %v", err)
	}
}

func TestCodexFrozenRequestConcurrentFreezeConsumesInspectionOnce(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan struct {
		request *CodexFrozenRequest
		err     error
	}, 2)
	headroomCalls := make(chan struct{}, 2)
	headroom := CodexRequestHeadroomFunc(func(_ context.Context, body []byte, _ HeadroomMode) ([]byte, int, error) {
		headroomCalls <- struct{}{}
		return bytes.Clone(body), 0, nil
	})
	for range 2 {
		go func() {
			<-start
			request, freezeErr := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), headroom, HeadroomModeCache)
			results <- struct {
				request *CodexFrozenRequest
				err     error
			}{request: request, err: freezeErr}
		}()
	}
	close(start)
	successes := 0
	released := 0
	for range 2 {
		result := <-results
		if result.err == nil {
			successes++
			result.request.Release()
		} else if errors.Is(result.err, ErrCodexFrozenRequestReleased) {
			released++
		} else {
			t.Fatalf("unexpected Freeze error: %v", result.err)
		}
	}
	if successes != 1 || released != 1 || len(headroomCalls) != 1 {
		t.Fatalf("success/released/headroom = %d/%d/%d", successes, released, len(headroomCalls))
	}
}

func TestCodexFrozenRequestInspectionReleaseClearsOwnedBytes(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestTurn, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ownedEncoded := inspection.state.encoded
	ownedDecoded := inspection.state.decoded
	inspection.Release()
	inspection.Release()
	if !allZero(ownedEncoded) || !allZero(ownedDecoded) {
		t.Fatal("inspection release did not clear owned bytes")
	}
	if _, err := inspection.Protocol(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("Protocol after inspection release error = %v", err)
	}
}

func TestCodexFrozenRequestReleaseClearsOwnedStateAndFreezeIsSingleUse(t *testing.T) {
	inspection, err := InspectCodexNativeRequest(context.Background(), frozenRequestBody("gpt-5.4", CodexRequestCompaction, "private"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ownedEncoded := inspection.state.encoded
	ownedDecoded := inspection.state.decoded
	frozen, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeCache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspection.Freeze(context.Background(), frozenRequestChoice("gpt-5.4", "gpt-5.4"), nil, HeadroomModeCache); !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("second Freeze error = %v", err)
	}
	if inspection.state.encoded != nil || inspection.state.decoded != nil || inspection.state.headers != nil {
		t.Fatalf("consumed inspection retained state: %#v", inspection)
	}
	// Unchanged preparation transfers decoded ownership into envelope rather than
	// clearing it before final request release.
	if allZero(ownedEncoded) || allZero(ownedDecoded) {
		t.Fatal("transferred request bytes cleared before frozen release")
	}
	ownedEnvelopeEncoded := frozen.state.envelope.encoded
	ownedEnvelopeDecoded := frozen.state.envelope.decoded
	frozen.Release()
	frozen.Release()
	if !allZero(ownedEnvelopeEncoded) || !allZero(ownedEnvelopeDecoded) {
		t.Fatal("frozen request release did not clear owned bytes")
	}
	if _, err := frozen.Protocol(); !errors.Is(err, ErrCodexFrozenRequestReleased) {
		t.Fatalf("Protocol after release error = %v", err)
	}
	if _, err := frozen.Replay(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) {
		t.Fatalf("Replay after release error = %v", err)
	}
}

func frozenRequestBody(model string, kind CodexRequestKind, private string) []byte {
	phase := ""
	if kind == CodexRequestCompaction {
		phase = `,"compaction":"standalone_turn"`
	}
	return []byte(`{"type":"response.create","model":"` + model + `","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"` + string(kind) + `"` + phase + `}},"input":[{"role":"user","content":"` + private + `"}]}`)
}

func frozenRequestChoice(requested, effective string) RouteChoice {
	return RouteChoice{
		AccountKey:      codex.AccountKey("account"),
		RequestedModel:  requested,
		EffectiveModel:  effective,
		RequiredBuckets: []CapacityBucket{CapacityBucketForModel(effective)},
	}
}

func readFrozenRequestReplay(t *testing.T, replay *CodexRequestReplay) []byte {
	t.Helper()
	body, err := replay.Body()
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close replay body = %v/%v", readErr, closeErr)
	}
	return got
}

func assertFrozenRequestErrorCode(t *testing.T, err error, want CodexFrozenRequestErrorCode) {
	t.Helper()
	var typed *CodexFrozenRequestError
	if !errors.As(err, &typed) || typed.Code != want {
		t.Fatalf("error = %#v, want frozen request code %q", err, want)
	}
}
