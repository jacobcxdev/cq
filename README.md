# cq

A CLI tool to check your AI provider quota usage at a glance. Supports **Claude**, **Codex**, and **Gemini**.

## Install

```bash
go install github.com/jacobcxdev/cq/cmd/cq@latest
```

## Usage

```bash
cq                       # Check all providers
cq check claude          # Check specific providers
cq --json                # JSON output
cq --refresh             # Bypass cache
```

### Account Management (Claude)

```bash
cq claude login          # Add account via OAuth
cq claude accounts       # List accounts
cq claude switch EMAIL   # Switch active account
```

## What It Shows

For each provider, cq displays remaining quota as a percentage bar, pace indicator, and burndown estimate for each rate-limit window.

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ● Claude  pro 5x · user@example.com
     5h  ████████████████░░░░ 82%  |  ↑ +12  🕐 3h 12m
     7d  ██████████████████░░ 91%  |  ↑  +5  🕐 4d 2h
  ─────────────────────────────────────────
  ◆ Aggregate
     5h  ████████████████░░░░ 82%  |  ↑ +12  🕐 3h 12m
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CQ_TTL` | `30s` | Cache duration (e.g., `1m`, `5m`) |

## Licence

MIT
