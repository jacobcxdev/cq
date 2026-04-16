package output

import (
	"context"
	"io"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
)

// TTYRenderer renders a Report as styled terminal output.
type TTYRenderer struct {
	W   io.Writer
	Now time.Time
}

func (r *TTYRenderer) Render(_ context.Context, report app.Report) error {
	model := BuildTTYModel(report, r.Now)
	return writeTTY(r.W, model)
}

// errWriter wraps an io.Writer and captures the first error.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) write(s string) {
	if ew.err != nil {
		return
	}
	_, ew.err = io.WriteString(ew.w, s)
}

func writeTTY(w io.Writer, model TTYModel) error {
	ew := &errWriter{w: w}
	for i, section := range model.Sections {
		if section.Separator != "" {
			ew.write(section.Separator)
			ew.write("\n\n")
		} else {
			ew.write("\n")
		}
		ew.write(section.Header)
		ew.write("\n")

		for _, row := range section.WindowRows {
			writeWindowRow(ew, row)
		}

		if section.AggHeader != "" {
			ew.write("\n")
			ew.write(section.ThinSep)
			ew.write("\n\n")
			ew.write(section.AggHeader)
			ew.write("\n")
			for _, row := range section.AggRows {
				writeWindowRow(ew, row)
			}
		}

		ew.write("\n")

		if i == len(model.Sections)-1 {
			ew.write(model.ClosingSeparator)
			ew.write("\n")
		}
	}
	return ew.err
}

func writeWindowRow(ew *errWriter, row TTYWindowRow) {
	if row.Bar == "" && row.Pct == "" && row.Reset == "" && row.PaceDiff == "" && row.Burndown == "" {
		ew.write("\n")
		ew.write(row.Label)
		ew.write("\n")
		return
	}

	ew.write(row.Label)
	ew.write(row.Bar)
	ew.write("  ")
	ew.write(row.Pct)
	ew.write("  ")
	ew.write(row.Reset)
	ew.write("  ")
	ew.write(row.PaceDiff)
	ew.write("  ")
	ew.write(row.Burndown)
	ew.write("\n")
}
