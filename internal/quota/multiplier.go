package quota

import (
	"regexp"
	"strconv"
)

var multiplierRe = regexp.MustCompile(`_(\d+)x$`)

// ExtractMultiplier parses rateLimitTier like "default_claude_max_20x" -> 20.
// Returns 1 for tiers with no suffix, empty strings, or n <= 0 (unconfigured
// or zero-capacity tier; treat as 1x).
func ExtractMultiplier(tier string) int {
	n, ok := ParseMultiplier(tier)
	if !ok {
		return 1
	}
	return n
}

// ParseMultiplier returns a positive numeric tier multiplier and whether it is
// known. Unlike ExtractMultiplier, absent or invalid allowances have no weight.
func ParseMultiplier(tier string) (int, bool) {
	m := multiplierRe.FindStringSubmatch(tier)
	if len(m) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
