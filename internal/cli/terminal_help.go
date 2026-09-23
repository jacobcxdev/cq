package cli

import "strings"

type helpLine struct {
	Indent int
	Spans  []Span
}

// terminalHelp renders approved, build-time help without consulting user state.
func terminalHelp(path string, width int, colour bool) (string, bool) {
	lines, ok := authoredHelpByPath[path]
	if !ok {
		return "", false
	}
	options := TerminalOptions{Width: width, Colour: colour}
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(TerminalLine(options, line.Indent, line.Spans...))
	}
	return out.String(), true
}
