<!-- Parent: ../AGENTS.md -->

# aggregate

Multi-account weighted pace, sustainability, gauge, and burndown computation.

## Key Files

| File | Description |
|------|-------------|
| `aggregate.go` | `Compute`: weighted average across accounts, requires 2+ usable results |
| `sustain.go` | `computeSustainability`: binary search over coverage intervals (JSON field); `computeGaugeInfo`: correction-deadline gauge for TTY display |
| `label.go` | `BuildLabel`: aggregate display label from plan names and multipliers |
| `types.go` | Type aliases bridging to `quota.AggregateResult` |
| `gauge_test.go` | Tests for `computeGaugeInfo`, `firstGapSpan`, `buildIntervals`, `projectedWasteInfo` |

## For AI Agents

### Working In This Directory

- `Compute` requires `len(valid) >= 2` accounts; returns nil for single-account providers
- `computeSustainability` (raw multiplier `s`) is retained for JSON backward compat; the TTY gauge uses `computeGaugeInfo`
- **Gauge semantics:** left (0-2) = overburn severity (dry spot deadline), center (3) = on pace, right (4-6) = underburn severity (projected waste)
- Gauge thresholds are proportional to window period: severe ≤10%, moderate 10-25%, mild >25%
- Weekly-gated 5h accounts whose 7d reset falls within the 5h horizon contribute coverage intervals starting at the gate offset (not excluded)
- Accounts with `elapsed=0` and `used>0` use a bounded fallback rate (`used/period`) instead of being skipped
- `GaugeInfo` exposes gap start, gap duration, wasted %, and waste deadline for aggregate display columns
- All arithmetic guards against division by zero (`sumWeight == 0`, `burnDen == 0`)
- `Burndown == 0` in aggregate is ambiguous (exhausted or no data) — callers show em-dash for both
