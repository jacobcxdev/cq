package cli

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// Role gives trusted structure a fixed presentation style. Span text is data.
type Role uint8

const (
	Body Role = iota
	Heading
	Identity
	Quantity
	TimeValue
	Confirmed
	Attention
	Failed
	Secondary
)

type Span struct {
	Text string
	Role Role
}

type Field struct {
	Label string
	Value []Span
}

type TerminalOptions struct {
	Width    int // Zero means redirected/non-terminal.
	Colour   bool
	Now      time.Time
	Location *time.Location
}

type TerminalRenderer func(TerminalOptions) string

var roleSGR = [...]string{"", "1", "36", "1;34", "35", "32", "33", "31", "2"}

func terminalText(text string) string {
	var out strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || r == '\u061c' || r == '\u200e' || r == '\u200f' || r == '\u202a' || r == '\u202b' || r == '\u202c' || r == '\u202d' || r == '\u202e' || r == '\u2066' || r == '\u2067' || r == '\u2068' || r == '\u2069' {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func paintSpan(options TerminalOptions, span Span) string {
	value := terminalText(span.Text)
	if !options.Colour || options.Width <= 0 || span.Role >= Role(len(roleSGR)) || roleSGR[span.Role] == "" || value == "" {
		return value
	}
	return "\x1b[" + roleSGR[span.Role] + "m" + value + "\x1b[0m"
}

func bodyWidth(options TerminalOptions) int {
	if options.Width <= 0 {
		return 0
	}
	return min(options.Width, 100)
}

func TerminalLine(options TerminalOptions, indent int, spans ...Span) string {
	indent = max(indent, 0)
	var content strings.Builder
	for _, span := range spans {
		content.WriteString(paintSpan(options, span))
	}
	value := content.String()
	width := bodyWidth(options)
	if width > 0 {
		if indent >= width {
			indent = 0
		}
		for {
			wrapped := ansi.Wrap(content.String(), width-indent, "")
			overflow := 0
			for _, line := range strings.Split(wrapped, "\n") {
				overflow = max(overflow, ansi.StringWidth(line)-(width-indent))
			}
			value = wrapped
			if overflow == 0 || indent == 0 {
				break
			}
			indent = max(0, indent-overflow)
		}
	}
	prefix := strings.Repeat(" ", indent)
	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix) + "\n"
}

func TerminalFields(options TerminalOptions, indent int, fields []Field) string {
	var out strings.Builder
	for _, field := range fields {
		spans := make([]Span, 0, len(field.Value)+1)
		spans = append(spans, Span{Text: field.Label + ": "})
		spans = append(spans, field.Value...)
		out.WriteString(TerminalLine(options, indent, spans...))
	}
	return out.String()
}

// TerminalTable renders columns only when the complete header and every cell fit.
// The caller owns the empty-state message.
func TerminalTable(options TerminalOptions, headers []string, rows [][]Span) string {
	if len(rows) == 0 {
		return ""
	}
	widths := make([]int, len(headers))
	mismatched := false
	for i, header := range headers {
		widths[i] = ansi.StringWidth(terminalText(header))
	}
	for _, row := range rows {
		mismatched = mismatched || len(row) != len(headers)
		for i := range headers {
			if i < len(row) {
				widths[i] = max(widths[i], ansi.StringWidth(terminalText(row[i].Text)))
			}
		}
	}
	total := 2 * max(0, len(headers)-1)
	for _, width := range widths {
		total += width
	}
	if bodyWidth(options) == 0 || total > bodyWidth(options) || mismatched {
		var out strings.Builder
		for _, row := range rows {
			fields := make([]Field, 0, len(row))
			for i, cell := range row {
				label := fmt.Sprintf("Column %d", i+1)
				if i < len(headers) {
					label = headers[i]
				}
				fields = append(fields, Field{Label: label, Value: []Span{cell}})
			}
			out.WriteString(TerminalFields(options, 0, fields))
		}
		return out.String()
	}
	var out strings.Builder
	writeRow := func(cells []Span) {
		for i, cell := range cells {
			if i > 0 {
				out.WriteString("  ")
			}
			out.WriteString(paintSpan(options, cell))
			if i < len(headers)-1 {
				out.WriteString(strings.Repeat(" ", max(0, widths[i]-ansi.StringWidth(terminalText(cell.Text)))))
			}
		}
		out.WriteByte('\n')
	}
	heading := make([]Span, len(headers))
	for i, header := range headers {
		heading[i] = Span{Text: header, Role: Heading}
	}
	writeRow(heading)
	for _, row := range rows {
		writeRow(row)
	}
	return out.String()
}
