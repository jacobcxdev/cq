<!-- Parent: ../AGENTS.md -->

# output

TTY and JSON renderers for quota reports.

## Key Files

| File | Description |
|------|-------------|
| `tty_build.go` | `BuildTTYModel`: converts Report → TTYModel with styled sections, bars, gauges |
| `tty_renderer.go` | `TTYRenderer.Render`: writes TTYModel to `io.Writer` via `errWriter` pattern |
| `tty_format.go` | `calcPace`, `fmtDuration` — pure formatting helpers |
| `tty_bar.go` | `renderBar`: percentage bar with pace marker |
| `tty_gauge.go` | `renderSustainGauge`: sustainability gauge visualisation |
| `tty_style.go` | Lipgloss style definitions and `pctStyle` colour thresholds |
| `tty_model.go` | TTYModel, TTYSection, TTYWindowRow structs |
| `json.go` | `JSONRenderer`: standard/pretty JSON with optional ANSI colouring |

## For AI Agents

### Working In This Directory

- `BuildTTYModel` is a pure function (no I/O) — all rendering state is in the returned model
- `errWriter` captures the first write error and short-circuits subsequent writes
- Width calculations use terminal-cell width through `lipgloss.Width`; preserve ANSI, Unicode and wide-character alignment
- `providerDisplayName` handles empty strings and non-ASCII via `utf8.DecodeRuneInString`
- `buildThinSeparator` uses `max(0, width-2)` to prevent negative repeat counts

- Provider, account and pool titles start at the same terminal column. Leave the credit status slot blank when there is no status. Pool headers use the pool name alone, without UUIDs or a `Proxy` prefix.
