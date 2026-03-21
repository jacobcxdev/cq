# cq

A CLI tool to check your AI provider quota usage at a glance. Supports **Claude**, **Codex**, and **Gemini**.

## Install

```bash
brew install jacobcxdev/tap/cq
```

Or with Go:

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

For each provider, cq displays remaining quota as a percentage bar, pace indicator, and burndown estimate for each rate-limit window. Requires a [Nerd Font](https://www.nerdfonts.com/) for icons to render correctly.

```
————————————————————————————————————————————————————————————————————————

  ✻   Claude max 20x · alice@example.com
          5h  ╌╌╌|╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌  󰪟   3%  󰦖 44m      󰓅  -12  󰅒 7m
          7d  ━━━━━╌╌╌╌╌╌|╌╌╌╌╌╌╌╌  󰪟  27%  󰦖 3d 21h   󰓅  -29  󰅒 1d 3h

  ✻   Claude max 20x · bob@example.com
          5h  ━━━━━━━━━━━━━━━━━━━━  󰪟 100%  󰦖 —        󰓅    —  󰅒 —
          7d  |╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌  󰪟   0%  󰦖 6h 44m   󰓅   -4  󰅒 now

  ----------------------------------------------------------------------

  ✻   Claude 2 × max 20x = 40x
          5h  ╌╌╌|╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌  󰪟   3%  󰊚 │──────  󰓅  -12  󰅒 7m
          7d  ━━╌╌╌╌|╌╌╌╌╌╌╌╌╌╌╌╌╌  󰪟  14%  󰊚 ───│───  󰓅  -16  󰅒 1d 3h

————————————————————————————————————————————————————————————————————————

      Codex plus · alice@example.com
          5h  ━━━━━━━━━━━|━━━━╌╌╌╌  󰪟  82%  󰦖 2h 54m   󰓅  +24  󰅒 9h 30m
          7d  ━━━━╌╌╌╌╌╌╌╌|╌╌╌╌╌╌╌  󰪟  21%  󰦖 4d 10h   󰓅  -43  󰅒 16h 16m

————————————————————————————————————————————————————————————————————————

     Gemini paid · alice@example.com
       quota  ━━━━━━━━━━━━━━━━━━━|  󰪟 100%  󰦖 1d       󰓅   +0  󰅒 —

————————————————————————————————————————————————————————————————————————
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CQ_TTL` | `30s` | Cache duration (e.g., `1m`, `5m`) |

## Licence

MIT
