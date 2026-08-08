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

func TestCodexZstdRejectsKnownOversizeBeforeDecoder(t *testing.T) {
	encoded := encodeCodexZstd(t, []byte(strings.Repeat("b", 64<<10)))
	allocations := testing.AllocsPerRun(100, func() {
		_, _ = DecodeCodexRequest(encoded, "zstd", CodexZstdLimits{MaxEncodedBytes: 1 << 20, MaxDecodedBytes: 1024, MaxExpansion: 1000})
	})
	if allocations > 5 {
		t.Fatalf("oversize rejection allocations = %v, want <= 5", allocations)
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
