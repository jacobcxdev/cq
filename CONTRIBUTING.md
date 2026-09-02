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
2. Validate current installed Codex against live upstream:
   ```bash
   scripts/validate-codex-release
   ```
   This runs normal WebSocket, repeated HTTP, compaction, and rescue traffic through isolated proxy listeners. It leaves any production listener unchanged and publishes the commit-bound `cq/live-normal-routing` status required by the tag workflow.
3. Tag the commit: `git tag v0.x.y`
4. Push the tag: `git push origin v0.x.y`
5. Run the `Release` workflow against that tag. It builds and validates release
   artifacts, publishes the GitHub release, and publishes the generated
   Homebrew Cask to [`jacobcxdev/homebrew-tap`](https://github.com/jacobcxdev/homebrew-tap).
6. Verify the published package channels and installed transport validation.

## Homebrew Cask Service Lifecycle

The Homebrew Cask owns CQ as one complete installation. Its hooks call
`cq service install --owner=homebrew` after installation and
`cq service uninstall --owner=homebrew` before removal. Users should not run a
manual post-install service command.

- Install: `brew install --cask jacobcxdev/tap/cq`
- Upgrade: `brew upgrade --cask cq`
- Uninstall: `brew uninstall --cask cq`
- Inspect or restart both components: `cq service status --json` or
  `cq service restart`

Direct `cq proxy install|restart|uninstall` LaunchAgent commands remain
available for focused development and repair work, but they are not a complete
Homebrew installation path.

For local development rollouts, never overwrite the running executable in place
with `cp`, `install`, or shell redirection. macOS can kill the mapped process with
`SIGKILL (Code Signature Invalid)` while its inode changes. Build and validate a
new executable under a unique name in the destination directory, preserve the
old executable as a rollback, atomically rename the new executable into place,
then restart and health-check the service. The rename must stay on the same
filesystem; a cross-filesystem move can degrade to a copy.

### Required Secret

The release workflow needs a `HOMEBREW_TAP_TOKEN` repository secret — a GitHub PAT with `repo` scope on `jacobcxdev/homebrew-tap`.
