package quota

import "testing"

func TestParseResetTime(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"", 0},
		{"2026-03-19T12:00:00Z", 1773921600},
		{"2026-03-19T12:00:00.123456Z", 1773921600},
		{"2026-03-19T12:00:00+00:00", 1773921600},
	}
	for _, tt := range tests {
		if got := ParseResetTime(tt.input); got != tt.want {
			t.Errorf("ParseResetTime(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestCleanResetTime(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"", ""},
		{"2026-03-19T12:00:00Z", "2026-03-19T12:00:00Z"},
		{"2026-03-19T12:00:00.123456Z", "2026-03-19T12:00:00Z"},
		{"2026-03-19T12:00:00+00:00", "2026-03-19T12:00:00Z"},
		// non-UTC offset — converted to UTC
		{"2026-03-19T12:00:00+05:30", "2026-03-19T06:30:00Z"},
		// fractional seconds with non-UTC offset — converted to UTC
		{"2026-03-19T12:00:00.999+05:30", "2026-03-19T06:30:00Z"},
	}
	for _, tt := range tests {
		if got := CleanResetTime(tt.input); got != tt.want {
			t.Errorf("CleanResetTime(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseResetTimeUTC(t *testing.T) {
	// "2026-03-19T12:00:00Z" is valid RFC3339 input parsed on the primary path.
	got := ParseResetTime("2026-03-19T12:00:00Z")
	want := int64(1773921600)
	if got != want {
		t.Errorf("ParseResetTime(UTC) = %d, want %d", got, want)
	}
}

func TestParseResetTimeInvalid(t *testing.T) {
	if got := ParseResetTime("not-a-time"); got != 0 {
		t.Errorf("ParseResetTime(invalid) = %d, want 0", got)
	}
}
