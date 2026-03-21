package output

// TTYModel is the structured display model for terminal output.
// It is a pure data structure — no I/O — built by BuildTTYModel.
type TTYModel struct {
	Sections         []TTYSection
	ClosingSeparator string // trailing separator after last section
}

// TTYSection represents one provider block (header + window rows + optional aggregate).
type TTYSection struct {
	Separator  string         // thick separator line (em-dash)
	Header     string         // "  ✻  Claude max 20x · user@email.com"
	WindowRows []TTYWindowRow // per-window rows
	ThinSep    string         // thin separator before aggregate (empty if none)
	AggHeader  string         // aggregate header (empty if none)
	AggRows    []TTYWindowRow // aggregate rows
}

// TTYWindowRow holds the pre-formatted fields for a single window line.
type TTYWindowRow struct {
	Label    string // "   5h" or "   7d"
	Bar      string // 20-char bar with pace marker
	Pct      string // "󰪟  42%"
	Reset    string // "󰦖 4h 30m"
	PaceDiff string // "󰓅   +3"
	Burndown string // "󰅒 6h 12m"
}
