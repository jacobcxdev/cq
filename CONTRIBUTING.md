# Contributing

Git strategy and workflow for cq development.

## Branching Model

cq uses trunk-based development on `main`.

- `main` is the only long-lived branch. Every change lands through a short-lived branch and a PR.
- Feature branches live for hours to days, not weeks. Target: < 2 days ideal, > 5 days is a smell — split the work.
- Branches are deleted immediately after merge.

## Branch Naming

Format: `{kind}/{slug}`

- `kind` is one of: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`.
- `slug` is lowercase kebab-case describing the change.

Examples:
- `feat/gemini-provider`
- `fix/oauth-callback-once`
- `refactor/architecture`
- `docs/readme`

## Commit Messages

Format: `type: description`

Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`

- Keep subjects imperative and specific.
- When useful, lead with the package: `fix: auth — validate URL scheme before browser open`.
- Avoid `wip` and `fix stuff`.

## PR Workflow

- Open a PR for every branch into `main`.
- **Squash merge only** on `main` — one bisect point per reviewed change.
- PR title must be the final squash commit subject in conventional-commit form.
- Delete the source branch immediately after merge.
- Self-review: read the diff in the PR view after CI passes before merging.

## CI Gates

All checks must pass before merge:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

## Code Review

- All changes should be reviewed, either by a human or an AI agent
- Critical and high severity issues must be resolved before merge
- Security-sensitive changes (auth, keyring, credential handling) require extra scrutiny

## Releasing

cq uses [semver](https://semver.org/) starting at `v0.1.0`. Bump minor for features, patch for fixes.

To release a new version:

1. Ensure `main` is green (CI passing).
2. Tag the commit: `git tag v0.x.y`
3. Push the tag: `git push origin v0.x.y`
4. GitHub Actions runs GoReleaser, which:
   - Builds cross-platform binaries (darwin/linux amd64+arm64, windows amd64)
   - Creates a GitHub Release with auto-generated changelog
   - Opens a PR against [`jacobcxdev/homebrew-tap`](https://github.com/jacobcxdev/homebrew-tap) to update the formula
5. Merge the Homebrew formula PR.

### Required Secret

The release workflow needs a `HOMEBREW_TAP_TOKEN` repository secret — a GitHub PAT with `repo` scope on `jacobcxdev/homebrew-tap`.
