package quota

import "time"

// ParseResetTime parses an RFC3339 timestamp string and returns a Unix epoch.
// Returns 0 on empty input or parse failure.
func ParseResetTime(s string) int64 {
	if s == "" {
		return 0
	}
	s = CleanResetTime(s)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// CleanResetTime normalises a reset-time string to plain RFC3339 in UTC
// (no sub-second precision, offset expressed as Z). Non-UTC offsets are
// converted to UTC rather than blindly appending Z.
func CleanResetTime(s string) string {
	if s == "" {
		return ""
	}
	// Try parsing with multiple layouts, most specific first.
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	// Fallback: return as-is if nothing parses.
	return s
}
