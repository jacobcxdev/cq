<!-- Parent: ../AGENTS.md -->

# quota

Domain types shared across all providers and the aggregation layer.

## Key Files

| File | Description |
|------|-------------|
| `result.go` | `Result`, `Window`, `ErrorInfo`, `ErrorResult`, `MinRemainingPct`, `StatusFromWindows` |
| `constants.go` | `WindowName` constants, `PeriodFor`, `DefaultResetEpoch`, `Status` enum |
| `time.go` | `ParseResetTime`, `CleanResetTime` — timestamp normalisation |
| `multiplier.go` | `ExtractMultiplier` — parses rate-limit tier strings for numeric multiplier |
| `aggregate.go` | `AggregateResult` — aggregate window data with pace, burndown, sustainability |

## For AI Agents

### Working In This Directory

- `MinRemainingPct()` returns `-1` for empty windows — callers must handle this (not `0`)
- `ErrorResult` always sets `Status: StatusError` — check `IsUsable()` to filter
- Window names are `Window5Hour` and `Window7Day` — these are the only two used across all providers
