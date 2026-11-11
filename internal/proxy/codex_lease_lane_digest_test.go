package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestCodexJournalLaneDigestPreservesEncoding(t *testing.T) {
	cases := [][3]string{
		{},
		{"session", "thread", "namespace"},
		{"a\x00b", "c", "d"},
		{"a", "b\x00c", "d"},
		{"\xff\xfe", "模型", "\x80\x00"},
		{strings.Repeat("s", 43), strings.Repeat("t", 43), strings.Repeat("n", 43)},
		{strings.Repeat("s", 253), "", ""},
		{strings.Repeat("s", 254), "", ""},
		{strings.Repeat("s", 255), "", ""},
		{strings.Repeat("s", 4096), strings.Repeat("t", 8192), strings.Repeat("n", 16384)},
	}
	for _, values := range cases {
		if got, want := codexJournalLaneDigest(values[0], values[1], values[2]), codexLaneDigestLegacy(values[0], values[1], values[2]); got != want {
			t.Fatalf("lane digest changed for lengths %d/%d/%d: got %q, want %q", len(values[0]), len(values[1]), len(values[2]), got, want)
		}
	}
}

func TestCodexJournalLaneDigestAllocations(t *testing.T) {
	values := [3]string{strings.Repeat("s", 43), strings.Repeat("t", 43), strings.Repeat("n", 43)}
	var result string
	allocations := testing.AllocsPerRun(100, func() {
		result = codexJournalLaneDigest(values[0], values[1], values[2])
	})
	if allocations > 1 {
		t.Fatalf("lane digest allocated %g times, want at most the returned string", allocations)
	}
	if result != codexLaneDigestLegacy(values[0], values[1], values[2]) {
		t.Fatal("lane digest changed")
	}
}

func BenchmarkCodexJournalLaneDigest(b *testing.B) {
	values := [3]string{strings.Repeat("s", 43), strings.Repeat("t", 43), strings.Repeat("n", 43)}
	want := codexLaneDigestLegacy(values[0], values[1], values[2])
	for _, variant := range []struct {
		name string
		run  func(string, string, string) string
	}{
		{"Current", codexJournalLaneDigest},
		{"Legacy", codexLaneDigestLegacy},
	} {
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if got := variant.run(values[0], values[1], values[2]); got != want {
					b.Fatal("lane digest changed")
				}
			}
		})
	}
}

func codexLaneDigestLegacy(session, thread, namespace string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(session))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(thread))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(namespace))
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}
