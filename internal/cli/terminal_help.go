package cli

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// terminalHelp is a presentation of the same catalogue as reference help.
// It performs no environment, credential or service access.
func terminalHelp(path string, width int, colour bool) (string, bool) {
	page, ok := terminalHelpByPath[path]
	if !ok {
		return "", false
	}
	width = max(20, min(width, 100))
	var out strings.Builder
	paint := func(text, code string) string {
		if !colour {
			return text
		}
		return "\x1b[" + code + "m" + text + "\x1b[0m"
	}
	text := func(value string, indent int, code string) {
		for _, line := range strings.Split(ansi.Wrap(value, width-indent, ""), "\n") {
			out.WriteString(strings.Repeat(" ", indent) + paint(strings.TrimRight(line, " "), code) + "\n")
		}
	}
	heading := func(title string) { out.WriteByte('\n'); text(title, 0, "1") }
	rows := func(entries []helpEntry) {
		labelWidth := 0
		for _, entry := range entries {
			labelWidth = max(labelWidth, ansi.StringWidth(entry.Label))
		}
		for _, entry := range entries {
			if labelWidth+4+24 <= width && entry.Description != "" {
				lines := strings.Split(ansi.Wrap(entry.Description, width-labelWidth-4, ""), "\n")
				out.WriteString("  " + paint(entry.Label, "36") + strings.Repeat(" ", labelWidth-ansi.StringWidth(entry.Label)+2) + lines[0] + "\n")
				for _, line := range lines[1:] {
					out.WriteString(strings.Repeat(" ", labelWidth+4) + line + "\n")
				}
			} else {
				text(entry.Label, 2, "36")
				if entry.Description != "" {
					text(entry.Description, 4, "0")
				}
			}
			for _, detail := range entry.Details {
				text(detail, 4, "2")
			}
		}
	}
	title := "cq"
	if path != "" {
		title += " " + path
	}
	text(title, 0, "1;36")
	text(page.Summary, 0, "0")
	heading("Usage")
	text(strings.TrimPrefix(page.Usage, "Usage: "), 2, "36")
	if path != "" {
		for _, paragraph := range page.Description {
			if paragraph == page.Summary || strings.HasPrefix(paragraph, "Choose a subcommand below.") {
				continue
			}
			out.WriteByte('\n')
			text(paragraph, 0, "0")
		}
	}
	if path == "" {
		for _, group := range []struct {
			title string
			names []string
		}{
			{"Quotas", []string{"check"}}, {"Providers", []string{"claude", "codex", "gemini"}},
			{"Operations", []string{"proxy", "service", "auth", "models"}}, {"Utilities", []string{"completion", "help", "version"}},
		} {
			var entries []helpEntry
			for _, name := range group.names {
				for _, entry := range page.Commands {
					if entry.Label == name {
						entries = append(entries, entry)
					}
				}
			}
			if len(entries) > 0 {
				heading(group.title)
				rows(entries)
			}
		}
	} else if len(page.Commands) > 0 {
		heading("Commands")
		rows(page.Commands)
	}
	if len(page.Related) > 0 {
		heading("Related commands")
		rows(page.Related)
	}
	if len(page.Arguments) > 0 {
		heading("Arguments")
		rows(page.Arguments)
	}
	if len(page.Options) > 0 {
		heading("Options")
		rows(page.Options)
	}
	heading("Global options")
	rows([]helpEntry{{Label: "-h, --help", Description: "Show command help."}, {Label: "-j, --json", Description: "Emit machine-readable output; help remains text."}, {Label: "-v, --version", Description: "Show version."}})
	if len(page.Examples) > 0 {
		heading("Examples")
		for _, entry := range page.Examples {
			text(entry.Label, 2, "36")
			if entry.Description != "" {
				text(entry.Description, 4, "2")
			}
		}
	}
	out.WriteByte('\n')
	return out.String(), true
}
