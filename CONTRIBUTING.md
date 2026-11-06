# Contributing

CQ combines quota reporting, account management and local request routing. Read
[the architecture guide](docs/architecture.md) for package ownership and runtime
contracts, and [the release guide](docs/releases.md) for packaging and delivery.

## Development setup

Install Git and the Go version declared in [go.mod](go.mod), currently **1.26.1**.
Race tests also require a working C toolchain supported by Go on the host.
Clone the repository, then run:

```sh
go mod download
go build ./...
go vet ./...
go test -race -count=1 ./...
```

For a development executable with a predictable location:

```sh
go build -o ./cq ./cmd/cq
./cq --help
```

`go run ./cmd/cq` and `./cq check` read real local account state and may contact
providers. Use injected HTTP/filesystem/credential fakes and temporary roots for
tests. Building and testing does not install services or activate native routing.
Do not replace a running installed executable with the development build.

Source builds report a development version and omit the release-linked Gemini
OAuth client secret. Do not add a runtime secret fallback. Official packages
provide that release input. Portable archives and `go install` are development
or portable binaries; a complete supported installation also owns both per-user
background components.

For native macOS changes, install Xcode with Swift 6 tools and run:

```sh
swift test --package-path native/macos-network --sanitize thread
```

The Swift package requires macOS 13 and uses Swift 5 language mode. Signed app
bundles require the separate Xcode project and authorised signing environment;
see [the native README](native/macos-network/README.md). Cross-builds check Go
compilation only; they do not qualify service managers, MSI packages, native
admission or installed traffic on another platform.

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

- Sign every commit. Use a conventional prefix and a specific past-tense subject under 50 characters, for example `fix: corrected callback validation`.
- Follow the subject with a blank line and a concise description of significant changes; use unordered lists and British English.
- Keep code references in backticks. Avoid `wip` and `fix stuff`.

## PR Workflow

- Open a PR for every branch into `main`.
- **Squash merge only** on `main` — one bisect point per reviewed change.
- PR title must be the final squash commit subject in conventional-commit form.
- Delete the source branch immediately after merge.
- Self-review: read the diff in the PR view after CI passes before merging.

## CI Gates

Every pull request into `main` runs the required **Build, vet and race tests** check on Linux ARM64, using the Go version declared in `go.mod`. Superseded runs are cancelled. The branch must be up to date with `main` before merging.

The **CI** workflow runs on pushes to `main` and can also be dispatched manually.
Its native-companion and package jobs run independently; their outputs feed
separate macOS Codex/Cask and Homebrew lifecycle jobs. Linux and Windows each
have AMD64 and ARM64 jobs. Linux runs the full race suite and requires its native
transport/confinement gates to pass. Windows runs builds, vet and MSI lifecycle
validation on both architectures, with selected race tests on AMD64.

The shared setup action installs Go from `go.mod` and caches downloaded modules
separately from compiler outputs. Keys include runner image, OS, architecture,
resolved Go version and dependency files; compiler caches also distinguish jobs
and source. A module-cache miss downloads dependencies before saving the cache.
Workflows use the upgraded action versions with Node24 or later runtimes; new
action examples must not reintroduce Node20. See the actual
[PR workflow](.github/workflows/pr.yml), [CI workflow](.github/workflows/ci.yml)
and [setup action](.github/actions/setup-go/action.yml) for exact gates.

Release publication is a separate manually dispatched **Release** workflow. Its
platform, installed-client and package gates are described in the release guide.

All checks must pass before merge:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
bash .github/scripts/test-release-source.sh
bash .github/scripts/test-close-superseded-winget-prs.sh
```

## Code Review

- All changes should be reviewed, either by a human or an AI agent
- Critical and high severity issues must be resolved before merge
- Security-sensitive changes (auth, keyring, credential handling) require extra scrutiny

## Releasing

Follow [the release guide](docs/releases.md). It covers sequential versions,
reviewed current-main provenance, the live commit-bound routing status, signed
tags, draft/package checks, native companion pins, publication and deployed
validation. The [native release guide](docs/native-network-releases.md) owns the
separate signing, notarisation and immutable native-asset procedure.

## Homebrew Cask Service Lifecycle

The Homebrew Cask owns CQ as one complete installation. Its installer artifacts call
`cq service install --owner=homebrew` after installation and
`cq service uninstall --owner=homebrew` before removal. Users should not run a
manual post-install service command.

An upgrade keeps the new package installed while admitted turns finish. If the
runtime cannot switch immediately, the existing refresh service retries the
retained candidate automatically. `cq --version` reports the installed package;
`cq service status --json` distinguishes the active and pending runtime versions.
Do not require users to repeat the upgrade or quit Codex. Runtime activation and
background account discovery must preserve the running desktop app and respect
an intentional quit. Verification and genuine activation failures still fail
explicitly and retain the previous healthy runtime.

- Install: `brew install --cask jacobcxdev/tap/cq`
- Upgrade: `brew upgrade --cask cq`
- Uninstall: `brew uninstall --cask cq`
- Inspect or restart both components: `cq service status --json` or
  `cq service restart`

On macOS, the proxy uses launchd's `Interactive` process policy because it serves
user requests. Scheduled refresh uses `Standard` so background throttling does not
delay installation health checks. Installations created
before this fix need one `cq service restart` after upgrading to reload the proxy
job's policy. A normal executable upgrade preserves launchd's cached policy.
The explicit service restart drains and reloads the proxy, retaining its pinned
executable; it can wait for active connections to finish.

Direct `cq proxy install|restart|uninstall` LaunchAgent commands remain
available for focused development and repair work, but they are not a complete
Homebrew installation path.

The release lifecycle gate uses the published previous Cask and executable.
It verifies the actual previous uninstall hook against the candidate install
hook, followed by transport and uninstall checks.

For local development rollouts, never overwrite the running executable in place
with `cp`, `install`, or shell redirection. macOS can kill the mapped process with
`SIGKILL (Code Signature Invalid)` while its inode changes. Build and validate a
new executable under a unique name in the destination directory, preserve the
old executable as a rollback, atomically rename the new executable into place,
then restart and health-check the service. The rename must stay on the same
filesystem; a cross-filesystem move can degrade to a copy.

## Documentation and local work

Keep user guidance in `README.md` and maintained `docs/` pages. Keep package
ownership rules in the applicable `AGENTS.md`; `CLAUDE.md` imports the root rules.
Implementation plans and review evidence remain local and ignored. Transfer
current product and maintainer contracts into published guides before
untracking a plan. Build and verification tools must use current source and
tests without depending on local planning or historical review documents.

Use normal reviewed commits for repository hygiene. Preserve local planning
copies when removing them from tracking; ignore rules alone do not untrack
files. Verify links and a clean checkout so ignored local files cannot conceal
missing build or test inputs.
