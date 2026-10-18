//go:build darwin

package dots

import (
	"encoding/binary"
	"slices"
	"testing"
)

func TestDarwinArgumentsPreserveSpacesAndEmptyValues(t *testing.T) {
	data := make([]byte, 4)
	binary.NativeEndian.PutUint32(data, 4)
	data = append(data, []byte("/path/Original app\x00\x00/path/Original app\x00__dots\x00\x00/root with spaces\x00SECRET_ENV=ignored\x00")...)
	got, err := parseDarwinProcessArguments(data)
	want := []string{"/path/Original app", "__dots", "", "/root with spaces"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("arguments %#v, %v", got, err)
	}
	for _, invalid := range [][]byte{nil, data[:3], data[:8], {0, 0, 0, 0, 0}} {
		if _, err := parseDarwinProcessArguments(invalid); err == nil {
			t.Fatal("malformed process arguments accepted")
		}
	}
}
