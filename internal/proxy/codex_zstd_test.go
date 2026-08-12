package proxy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestCodexZstdDecodeAndReplay(t *testing.T) {
	t.Parallel()
	original := []byte(`{"model":"gpt-5","input":"hello"}`)
	encoded := encodeCodexZstd(t, original)
	got, err := DecodeCodexRequest(encoded, "zstd", DefaultCodexZstdLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Decoded(), original) || !bytes.Equal(got.Replay(), encoded) || got.Encoding() != "zstd" {
		t.Fatalf("decoded/replay mismatch")
	}
	encoded[0] ^= 0xff
	if bytes.Equal(got.Replay(), encoded) {
		t.Fatal("replay aliases caller body")
	}
}

func TestCodexZstdInfersEncodingWhenHeaderOmitted(t *testing.T) {
	t.Parallel()
	original := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	encoded := encodeCodexZstd(t, original)

	got, err := DecodeCodexRequest(encoded, "", codexHTTPZstdLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Decoded(), original) || !bytes.Equal(got.Replay(), encoded) || got.Encoding() != "zstd" {
		t.Fatalf("inferred decoded/replay/encoding mismatch: %q/%t/%q", got.Decoded(), bytes.Equal(got.Replay(), encoded), got.Encoding())
	}
}

func TestCodexHTTPZstdLimitsAcceptBodyOverLegacyLimit(t *testing.T) {
	decoded := codexProtocolRequestBodyAtSize(t, maxRequestBody+1)
	encoded, err := EncodeCodexRequest(decoded, "zstd", codexHTTPZstdLimits())
	if err != nil {
		t.Fatalf("encode over legacy limit: %v", err)
	}
	request, err := DecodeCodexRequest(encoded, "zstd", codexHTTPZstdLimits())
	if err != nil {
		t.Fatalf("decode over legacy limit: %v", err)
	}
	if !bytes.Equal(request.Decoded(), decoded) {
		t.Fatal("decoded request changed")
	}
}

func TestCodexZstdBoundsAndMalformed(t *testing.T) {
	t.Parallel()
	decoded := []byte(strings.Repeat("a", 32<<10))
	encoded := encodeCodexZstd(t, decoded)
	tests := []struct {
		name   string
		body   []byte
		limits CodexZstdLimits
	}{
		{"encoded", encoded, CodexZstdLimits{MaxEncodedBytes: len(encoded) - 1, MaxDecodedBytes: 1 << 20, MaxExpansion: 10000}},
		{"decoded", encoded, CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: len(decoded) - 1, MaxExpansion: 10000}},
		{"ratio", encoded, CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: 1 << 20, MaxExpansion: 2}},
		{"malformed", []byte("not zstd"), DefaultCodexZstdLimits},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeCodexRequest(tc.body, "zstd", tc.limits); err == nil {
				t.Fatal("expected decode error")
			}
		})
	}
}

func TestCodexZstdStreamingBoundsWithoutContentSize(t *testing.T) {
	t.Parallel()
	decodedOversize := bytes.Repeat([]byte("a"), 4<<10)
	excessiveExpansion := bytes.Repeat([]byte("b"), 32<<10)
	expansionFrame := encodeCodexZstdStreaming(t, excessiveExpansion)
	if len(excessiveExpansion) <= len(expansionFrame)*2 {
		t.Fatalf("expansion fixture ratio = %d/%d, want over 2", len(excessiveExpansion), len(expansionFrame))
	}
	tests := []struct {
		name    string
		body    []byte
		limits  CodexZstdLimits
		wantErr string
	}{
		{
			name:    "decoded size",
			body:    encodeCodexZstdStreaming(t, decodedOversize),
			limits:  CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: len(decodedOversize) - 1, MaxExpansion: 10000},
			wantErr: "Codex decoded request exceeds limit",
		},
		{
			name:    "expansion ratio",
			body:    expansionFrame,
			limits:  CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: 1 << 20, MaxExpansion: 2},
			wantErr: "Codex zstd expansion ratio exceeds limit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeCodexRequest(test.body, "zstd", test.limits)
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestCodexZstdDecodeLimitUsesTighterBoundWithoutOverflow(t *testing.T) {
	t.Parallel()
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name         string
		encodedBytes int
		limits       CodexZstdLimits
		wantLimit    int
		wantRatio    bool
	}{
		{
			name:         "expansion ratio",
			encodedBytes: 128,
			limits:       CodexZstdLimits{MaxDecodedBytes: 4096, MaxExpansion: 2},
			wantLimit:    256,
			wantRatio:    true,
		},
		{
			name:         "decoded size",
			encodedBytes: 4096,
			limits:       CodexZstdLimits{MaxDecodedBytes: 1024, MaxExpansion: 2},
			wantLimit:    1024,
		},
		{
			name:         "multiplication overflow",
			encodedBytes: maxInt,
			limits:       CodexZstdLimits{MaxDecodedBytes: maxInt, MaxExpansion: 2},
			wantLimit:    maxInt,
		},
		{
			name:         "large expansion factor",
			encodedBytes: 2,
			limits:       CodexZstdLimits{MaxDecodedBytes: maxInt, MaxExpansion: maxInt},
			wantLimit:    maxInt,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			limit, ratioLimited := codexZstdDecodeLimit(test.encodedBytes, test.limits)
			if limit != test.wantLimit || ratioLimited != test.wantRatio {
				t.Fatalf("limit=%d ratio=%v, want %d/%v", limit, ratioLimited, test.wantLimit, test.wantRatio)
			}
		})
	}
}

func TestCodexZstdLargeExpansionFactorDoesNotOverflow(t *testing.T) {
	t.Parallel()
	var decoded, encoded []byte
	for size := 1; size <= 256; size++ {
		candidate := deterministicCodexZstdNoise(size)
		compressed := encodeCodexZstd(t, candidate)
		if len(compressed) > 1 && len(compressed)%2 == 0 {
			decoded, encoded = candidate, compressed
			break
		}
	}
	if encoded == nil {
		t.Fatal("could not construct an even-length zstd frame")
	}
	limits := CodexZstdLimits{
		MaxEncodedBytes: 1 << 20,
		MaxDecodedBytes: 1 << 20,
		MaxExpansion:    int(^uint(0) >> 1),
	}
	request, err := DecodeCodexRequest(encoded, "zstd", limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := request.Decoded(); !bytes.Equal(got, decoded) {
		t.Fatalf("decoded = %x, want %x", got, decoded)
	}
	if _, err := EncodeCodexRequest(decoded, "zstd", limits); err != nil {
		t.Fatal(err)
	}
}

func TestCodexZstdConcatenatedKnownSizeFramesUseAggregateLimit(t *testing.T) {
	t.Parallel()
	const firstDecodedBytes = 64 << 10
	const secondDecodedBytes = 2 << 20
	firstBody := deterministicCodexZstdNoise(firstDecodedBytes)
	first := encodeCodexZstd(t, firstBody)
	second := encodeCodexZstdStreamingWindow(t, bytes.Repeat([]byte("c"), secondDecodedBytes), 1<<20)
	var header zstd.Header
	if err := header.Decode(first); err != nil {
		t.Fatal(err)
	}
	if !header.HasFCS {
		t.Fatal("first concatenated frame must declare its content size")
	}
	firstFrameContentSize := header.FrameContentSize
	if firstFrameContentSize != firstDecodedBytes {
		t.Fatalf("first frame content size = %d, want %d", firstFrameContentSize, firstDecodedBytes)
	}
	if err := header.Decode(second); err != nil {
		t.Fatal(err)
	}
	if header.HasFCS {
		t.Fatal("second concatenated frame must omit its content size")
	}
	encoded := append(bytes.Clone(first), second...)
	limits := CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: 4 << 20, MaxExpansion: 2}
	firstExpansionLimit := uint64(len(first)) * uint64(limits.MaxExpansion)
	if firstFrameContentSize > firstExpansionLimit {
		t.Fatalf("first frame content size = %d, want within independent expansion limit %d", firstFrameContentSize, firstExpansionLimit)
	}
	aggregateExpansionLimit := uint64(len(encoded)) * uint64(limits.MaxExpansion)
	if aggregateExpansionLimit >= uint64(limits.MaxDecodedBytes) {
		t.Fatalf("aggregate expansion limit = %d, want below decoded limit %d", aggregateExpansionLimit, limits.MaxDecodedBytes)
	}
	aggregateDecodedBytes := uint64(firstDecodedBytes + secondDecodedBytes)
	if aggregateDecodedBytes <= aggregateExpansionLimit {
		t.Fatalf("aggregate decoded bytes = %d, want over expansion limit %d", aggregateDecodedBytes, aggregateExpansionLimit)
	}
	if aggregateDecodedBytes > uint64(limits.MaxDecodedBytes) {
		t.Fatalf("aggregate decoded bytes = %d, want within decoded limit %d", aggregateDecodedBytes, limits.MaxDecodedBytes)
	}
	firstRequest, err := DecodeCodexRequest(first, "zstd", limits)
	if err != nil {
		t.Fatalf("decode first frame independently: %v", err)
	}
	firstDecoded := firstRequest.Decoded()
	if !bytes.Equal(firstDecoded, firstBody) {
		t.Fatalf("first frame decoded bytes = %d, want %d", len(firstDecoded), firstDecodedBytes)
	}
	if uint64(len(firstDecoded)) > firstExpansionLimit {
		t.Fatalf("first frame decoded bytes = %d, want within independent expansion limit %d", len(firstDecoded), firstExpansionLimit)
	}
	_, err = DecodeCodexRequest(encoded, "zstd", limits)
	if err == nil || err.Error() != "Codex zstd expansion ratio exceeds limit" {
		t.Fatalf("error = %v, want aggregate expansion limit", err)
	}
}

func TestCodexZstdRejectsKnownOversizeBeforeDecoder(t *testing.T) {
	encoded := encodeCodexZstd(t, []byte(strings.Repeat("b", 64<<10)))
	allocations := testing.AllocsPerRun(100, func() {
		_, _ = DecodeCodexRequest(encoded, "zstd", CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: 1024, MaxExpansion: 1000})
	})
	if allocations > 5 {
		t.Fatalf("oversize rejection allocations = %v, want <= 5", allocations)
	}
}

func TestCodexZstdEncodeDeterministicAndBounded(t *testing.T) {
	t.Parallel()
	decoded := []byte(`{"model":"gpt-5.3-codex","input":[{"type":"message","content":"hello"}]}`)
	first, err := EncodeCodexRequest(decoded, " zstd ", DefaultCodexZstdLimits)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeCodexRequest(decoded, "ZSTD", DefaultCodexZstdLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("deterministic encodes differ")
	}
	roundTrip, err := DecodeCodexRequest(first, "zstd", DefaultCodexZstdLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip.Decoded(), decoded) {
		t.Fatal("encoded request does not round-trip")
	}

	identity, err := EncodeCodexRequest(decoded, "identity", DefaultCodexZstdLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identity, decoded) {
		t.Fatal("identity encode changed bytes")
	}
	identity[0] ^= 0xff
	if bytes.Equal(identity, decoded) {
		t.Fatal("identity encode aliases caller bytes")
	}

	tests := []struct {
		name     string
		body     []byte
		encoding string
		limits   CodexZstdLimits
	}{
		{name: "invalid limits", body: decoded, encoding: "zstd", limits: CodexZstdLimits{MaxEncodedBytes: -1}},
		{name: "unsupported encoding", body: decoded, encoding: "gzip", limits: DefaultCodexZstdLimits},
		{name: "decoded over limit", body: decoded, encoding: "zstd", limits: CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: len(decoded) - 1, MaxExpansion: 128}},
		{name: "encoded output over limit", body: decoded, encoding: "zstd", limits: CodexZstdLimits{MaxEncodedBytes: 1, MaxDecodedBytes: 1 << 20, MaxExpansion: 128}},
		{name: "expansion over limit", body: bytes.Repeat([]byte("a"), 32<<10), encoding: "zstd", limits: CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: 1 << 20, MaxExpansion: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := EncodeCodexRequest(tc.body, tc.encoding, tc.limits); err == nil {
				t.Fatal("expected encode error")
			}
		})
	}
}

func encodeCodexZstd(t *testing.T, body []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(body, nil)
}

func encodeCodexZstdStreaming(t *testing.T, body []byte) []byte {
	t.Helper()
	return encodeCodexZstdStreamingWindow(t, body, 1<<10)
}

func encodeCodexZstdStreamingWindow(t *testing.T, body []byte, windowSize int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	encoder, err := zstd.NewWriter(&encoded,
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(windowSize),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	frame := bytes.Clone(encoded.Bytes())
	var header zstd.Header
	if err := header.Decode(frame); err != nil {
		t.Fatalf("decode streaming zstd header: %v", err)
	}
	if header.HasFCS {
		t.Fatal("streaming zstd fixture unexpectedly includes frame content size")
	}
	return frame
}

func deterministicCodexZstdNoise(size int) []byte {
	noise := make([]byte, size)
	state := uint64(0x9e3779b97f4a7c15)
	for index := range noise {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		noise[index] = byte(state >> 56)
	}
	return noise
}
