package fsutil

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestSecureFileContentReadSizeHintDoesNotReplaceByteLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 65537)
	for _, hint := range []int64{-1, 0, 1, int64(len(data)), int64(len(data)) + 1, 1 << 40} {
		t.Run(fmt.Sprint(hint), func(t *testing.T) {
			got, err := readSecureFileContent(bytes.NewReader(data), int64(len(data)), hint)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("size hint changed bounded read: %v", err)
			}
			if _, err := readSecureFileContent(bytes.NewReader(data), int64(len(data))-1, hint); !errors.Is(err, ErrSecureFileTooLarge) {
				t.Fatalf("size hint bypassed read limit: %v", err)
			}
		})
	}
}

func TestSecureFileContentReadAllocationBound(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 6<<20)
	allocations := testing.AllocsPerRun(1, func() {
		got, err := readSecureFileContent(bytes.NewReader(data), int64(len(data)), int64(len(data)))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("secure content read failed: %v", err)
		}
	})
	if allocations > 4 {
		t.Fatalf("secure content read allocated %.0f objects, want <= 4", allocations)
	}
}

func BenchmarkSecureFileContentRead(b *testing.B) {
	data := bytes.Repeat([]byte("x"), 6<<20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got, err := readSecureFileContent(bytes.NewReader(data), int64(len(data)), int64(len(data)))
		if err != nil || len(got) != len(data) {
			b.Fatal(err)
		}
	}
}
