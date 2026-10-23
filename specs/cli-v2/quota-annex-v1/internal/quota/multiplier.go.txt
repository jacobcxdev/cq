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
	m := multiplierRe.FindStringSubmatch(tier)
	if len(m) < 2 {
		return 1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 1
	}
	return n
}
