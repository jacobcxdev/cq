package proxy

import (
	"math"
	"testing"
)

func TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping(t *testing.T) {
	got, err := CanonicalJSONV1(map[string]any{
		"\ue000":     "bmp",
		"\U00010000": "supplementary",
		"markup":     "<>&",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"markup\":\"<>&\",\"𐀀\":\"supplementary\",\"\":\"bmp\"}"
	if string(got) != want {
		t.Fatalf("CanonicalJSONV1() = %q, want %q", got, want)
	}
}

func TestCanonicalJSONV1RejectsNonFiniteNumber(t *testing.T) {
	if _, err := CanonicalJSONV1(math.Inf(1)); err == nil {
		t.Fatal("accepted positive infinity")
	}
}

func TestAppendCanonicalJSONUsesECMAScriptNumberSerialisation(t *testing.T) {
	for input, want := range map[string]string{
		"[1.0,1e+09,-0]":             "[1,1000000000,0]",
		"[1e-7,1e-6]":                "[1e-7,0.000001]",
		"[1e20,1e21]":                "[100000000000000000000,1e+21]",
		"[333333333.33333329]":       "[333333333.3333333]",
		"[4.50,2e-3,0.000000000001]": "[4.5,0.002,1e-12]",
		"[5e-324,1e-324]":            "[5e-324,0]",
		"[9007199254740993]":         "[9007199254740992]",
		"[-0.0,-0e+100]":             "[0,0]",
		"[1e30,1e-27]":               "[1e+30,1e-27]",
		"[1.7976931348623157e308]":   "[1.7976931348623157e+308]",
		"[9.999999999999997e-7]":     "[9.999999999999997e-7]",
	} {
		decoded, err := decodeStrictJSON([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		got, err := appendCanonicalJSON(nil, decoded)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("appendCanonicalJSON(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDecodeStrictJSONRejectsAmbiguousOrIncompleteInput(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"a":2}`,
		`{"nested":{"a":1,"a":2}}`,
		`{} []`,
		`{"a":[1`,
		`]`,
	} {
		if _, err := decodeStrictJSON([]byte(input)); err == nil {
			t.Errorf("accepted invalid JSON %q", input)
		}
	}
}

func TestCanonicalJSONV1RejectsInvalidUTF8(t *testing.T) {
	for _, input := range []any{
		"\xff",
		map[string]any{"\xff": "value"},
		map[string]any{"key": []string{"\xff"}},
	} {
		if _, err := CanonicalJSONV1(input); err == nil {
			t.Errorf("accepted invalid UTF-8 in %#v", input)
		}
	}
}
