package proxy

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestCodexFixtureStrictMetadata(t *testing.T) {
	for _, metadata := range []string{
		`{"request_kind":"memory","request_kind":"memory"}`,
		`{"request_kind":"memory","extra":1}`,
		`{"request_kind":"memory","session_id":null}`,
		`{"request_kind":"memory","compaction":"pre_turn"}`,
		`{"request_kind":"memory"} {}`,
	} {
		t.Run(metadata, func(t *testing.T) {
			if _, err := BuildSanitisedCodexFixture([]byte(`{"input":[]}`), "", metadata, time.Now()); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestCodexFixtureBounds(t *testing.T) {
	for _, size := range []int{(2 << 20) - 1, 2 << 20, (2 << 20) + 1} {
		t.Run(fmt.Sprintf("encoded-%d", size), func(t *testing.T) {
			body := fixtureSizedBody(size)
			_, err := BuildSanitisedCodexFixture(body, "identity", `{"request_kind":"memory"}`, time.Now())
			if (err == nil) != (size <= 2<<20) {
				t.Fatalf("size=%d err=%v", size, err)
			}
		})
	}
	for _, size := range []int{(8 << 20) - 1, 8 << 20, (8 << 20) + 1} {
		t.Run(fmt.Sprintf("decoded-%d", size), func(t *testing.T) {
			body := fixturePaddedZstd(t, fixtureSizedBody(size), 100000)
			_, err := BuildSanitisedCodexFixture(body, "zstd", `{"request_kind":"memory"}`, time.Now())
			if (err == nil) != (size <= 8<<20) {
				t.Fatalf("size=%d err=%v", size, err)
			}
		})
	}
	for _, size := range []int{128000 - 1, 128000, 128000 + 1} {
		t.Run(fmt.Sprintf("ratio-%d", size), func(t *testing.T) {
			body := fixturePaddedZstd(t, fixtureSizedBody(size), 1000)
			_, err := BuildSanitisedCodexFixture(body, "zstd", `{"request_kind":"memory"}`, time.Now())
			if (err == nil) != (size <= 128000) {
				t.Fatalf("size=%d err=%v", size, err)
			}
		})
	}
}
func fixtureSizedBody(size int) []byte {
	prefix := `{"input":[],"padding":"`
	suffix := `"}`
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}
func fixturePaddedZstd(t *testing.T, body []byte, size int) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	encoded := encoder.EncodeAll(body, nil)
	padding := size - len(encoded) - 8
	if padding < 0 {
		t.Fatal("invalid fixture padding")
	}
	frame := make([]byte, 8+padding)
	binary.LittleEndian.PutUint32(frame, 0x184d2a50)
	binary.LittleEndian.PutUint32(frame[4:], uint32(padding))
	return append(encoded, frame...)
}

func TestCodexFixtureRepresentationsAndUTF8(t *testing.T) {
	turn := `{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}`
	memory := `{"request_kind":"memory"}`
	for _, test := range []struct {
		name, body, explicit, source string
		valid                        bool
	}{
		{"flat", `{"client_metadata":` + turn + `}`, "", "flat", true},
		{"nested", `{"client_metadata":{"x-codex-turn-metadata":` + turn + `}}`, "", "nested", true},
		{"explicit", `{"input":[]}`, turn, "header", true},
		{"same explicit", `{"client_metadata":` + turn + `}`, turn, "header", true},
		{"conflict", `{"client_metadata":` + turn + `}`, memory, "", false},
		{"nested flat conflict", `{"client_metadata":{"request_kind":"memory","x-codex-turn-metadata":` + turn + `}}`, "", "", false},
		{"null hidden", `{"client_metadata":null}`, memory, "", false},
		{"empty hidden", `{"client_metadata":{}}`, memory, "", false},
		{"malformed hidden", `{"client_metadata":{"request_kind":null}}`, memory, "", false},
		{"body UTF8", `{"padding":"` + string([]byte{255}) + `"}`, memory, "", false},
		{"metadata UTF8", `{}`, `{"request_kind":"memory","session_id":"` + string([]byte{255}) + `"}`, "", false},
		{"nested duplicate", `{"client_metadata":{"x-codex-turn-metadata":{"request_kind":"memory","request_kind":"memory"}}}`, "", "", false},
		{"explicit object phase", `{}`, `{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"compaction","compaction":{"phase":"mid_turn"}}`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, err := BuildSanitisedCodexFixture([]byte(test.body), "auto", test.explicit, time.Now())
			if (err == nil) != test.valid {
				t.Fatalf("err=%v", err)
			}
			if test.valid && string(fixture.MetadataSource) != test.source {
				t.Fatalf("source=%s", fixture.MetadataSource)
			}
		})
	}
	for _, size := range []int{4095, 4096, 4097} {
		metadata := `{"request_kind":"memory","session_id":"` + strings.Repeat("x", size) + `"}`
		_, err := BuildSanitisedCodexFixture([]byte(`{}`), "identity", metadata, time.Now())
		if (err == nil) != (size <= 4096) {
			t.Fatalf("identifier=%d err=%v", size, err)
		}
	}
	for _, size := range []int{65535, 65536, 65537} {
		metadata := `{"request_kind":"memory"}`
		metadata += strings.Repeat(" ", size-len(metadata))
		_, err := BuildSanitisedCodexFixture([]byte(`{}`), "identity", metadata, time.Now())
		if (err == nil) != (size <= 65536) {
			t.Fatalf("metadata=%d err=%v", size, err)
		}
	}
}

func TestCodexFixtureAtomicNoClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	const count = 12
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			defer func() {
				if recover() != nil {
					results <- errors.New("writer panic")
				}
			}()
			results <- WriteSanitisedCodexFixture(path, SanitisedCodexFixture{SchemaVersion: 1})
		}()
	}
	successes := 0
	for i := 0; i < count; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful writers=%d", successes)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatal("partial fixture published")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary output leaked")
	}
}
