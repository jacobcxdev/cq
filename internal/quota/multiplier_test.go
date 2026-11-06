package quota

import "testing"

func TestParseMultiplierDistinguishesUnknownFromBaseline(t *testing.T) {
	for _, tt := range []struct {
		tier  string
		want  int
		known bool
	}{
		{"tier_20x", 20, true},
		{"tier_1x", 1, true},
		{"tier_0001x", 1, true},
		{"", 0, false},
		{"custom", 0, false},
		{"tier_0x", 0, false},
		{"tier_-1x", 0, false},
		{"tier_999999999999999999999999999999x", 0, false},
	} {
		t.Run(tt.tier, func(t *testing.T) {
			got, known := ParseMultiplier(tt.tier)
			if got != tt.want || known != tt.known {
				t.Fatalf("multiplier/known = %d/%t, want %d/%t", got, known, tt.want, tt.known)
			}
		})
	}
}

func TestExtractMultiplier(t *testing.T) {
	tests := []struct {
		tier string
		want int
	}{
		{"default_claude_max_20x", 20},
		{"default_claude_pro_1x", 1},
		{"default_claude_free_0x", 1}, // n <= 0 treated as 1
		{"default_claude_max", 1},     // no suffix
		{"", 1},                       // empty string
		{"tier_5x", 5},
		{"something_100x", 100},
	}
	for _, tt := range tests {
		t.Run(tt.tier, func(t *testing.T) {
			got := ExtractMultiplier(tt.tier)
			if got != tt.want {
				t.Errorf("ExtractMultiplier(%q) = %d, want %d", tt.tier, got, tt.want)
			}
		})
	}
}
