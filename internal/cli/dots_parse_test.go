package cli

import "testing"

func TestDotsInheritedRootErrorOrder(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"codex", "dots", "--root", "--bogus"}, "missing_option_value"},
		{[]string{"codex", "dots", "--root", "/one", "--root", "/two", "unknown"}, "duplicate_option"},
	} {
		_, err := Parse(tc.args)
		if err == nil || err.Diagnostic.Code != tc.code {
			t.Errorf("Parse(%q) = %v, want %s", tc.args, err, tc.code)
		}
	}
}
