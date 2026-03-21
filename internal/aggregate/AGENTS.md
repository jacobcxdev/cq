<!-- Parent: ../AGENTS.md -->

# aggregate

Multi-account weighted pace, sustainability, and burndown computation.

## Key Files

| File | Description |
|------|-------------|
| `aggregate.go` | `Compute`: weighted average across accounts, requires 2+ usable results |
| `sustain.go` | `computeSustainability`: binary search over coverage of quota intervals |
| `label.go` | `BuildLabel`: aggregate display label from plan names and multipliers |
| `types.go` | Type aliases bridging to `quota.AggregateResult` |

## For AI Agents

### Working In This Directory

- `Compute` requires `len(valid) >= 2` accounts; returns nil for single-account providers
- Sustainability uses a sweep-line interval coverage algorithm with binary search refinement
- All arithmetic guards against division by zero (`sumWeight == 0`, `burnDen == 0`)
- `Burndown == 0` in aggregate is ambiguous (exhausted or no data) — callers show em-dash for both
