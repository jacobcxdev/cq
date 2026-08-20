# Gemini Antigravity Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `cq check gemini` read current Gemini quota from authenticated Antigravity CLI without changing provider identity.

**Architecture:** Gemini provider resolves `agy`, executes exact structured `/usage` print command through injected bounded runner, validates zero-turn response, and maps only required Gemini buckets into existing `5h` and `7d` windows. CQ never reads Antigravity credentials or calls retired/private Google quota endpoints.

**Tech Stack:** Go 1.26.1, `os/exec`, `encoding/json`, existing quota domain types, race-enabled Go tests.

**Spec:** `docs/superpowers/specs/2026-08-20-gemini-antigravity-migration.md`

## Global Constraints

- Provider ID stays `gemini`.
- Exact command is `agy -p /usage --output-format json --print-timeout 15s`.
- Accepted command output must report `status=SUCCESS`, `num_turns=0`, `usage.total_tokens=0`, and `command.name=usage`.
- Stdout limit is 1 MiB; command safety timeout is 20 seconds.
- Required buckets are exactly one `gemini-5h` and one `gemini-weekly`; ignore every `3p-*` bucket.
- CQ must not read, refresh, write, log, or persist Antigravity credentials.
- Every Go test command uses `-race -count=1`.

---

## File map

- `internal/provider/gemini/cli.go`: executable discovery, exact command runner interface, bounded production execution.
- `internal/provider/gemini/cli_test.go`: process-boundary tests for output cap and context cancellation.
- `internal/provider/gemini/parser.go`: structured `/usage` response types, safety validation, Gemini bucket mapping.
- `internal/provider/gemini/parser_test.go`: literal response fixtures and parser edge cases.
- `internal/provider/gemini/provider.go`: discovery, timeout, command orchestration, privacy-safe error classification.
- `internal/provider/gemini/provider_test.go`: injected runner tests for exact command, discovery, failures, and cancellation.
- `internal/provider/gemini/refresh.go`: delete retired OAuth and Code Assist HTTP implementation.
- `cmd/cq/main.go`: construct Gemini provider without HTTP client.
- `README.md`, `AGENTS.md`, `internal/provider/AGENTS.md`, `internal/provider/gemini/AGENTS.md`, `internal/fsutil/AGENTS.md`: document current Antigravity authority and remove legacy credential claims.

### Task 1: Parse safe Antigravity quota output

**Files:**
- Replace: `internal/provider/gemini/parser_test.go`
- Replace: `internal/provider/gemini/parser.go`

**Interfaces:**
- Produces: `func parseUsage(data []byte) (quota.Result, error)`
- Produces: constants `antigravityAccountID`, `geminiFiveHourBucketID`, `geminiWeeklyBucketID`

- [ ] **Step 1: Write failing parser tests**

Use literal full-shape fixture matching observed CLI JSON:

```go
func TestParseUsageMapsOnlyGeminiBuckets(t *testing.T) {
	result, err := parseUsage([]byte(`{
		"status":"SUCCESS",
		"num_turns":0,
		"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0},
		"command":{"name":"usage","data":{"groups":[
			{"name":"Gemini Models","buckets":[
				{"id":"gemini-weekly","remaining_fraction":0.734,"reset_time":"2026-08-27T06:56:51Z"},
				{"id":"gemini-5h","remaining_fraction":0.426,"reset_time":"2026-08-20T11:56:51Z"}]},
			{"name":"Claude and GPT models","buckets":[
				{"id":"3p-weekly","remaining_fraction":0.01,"reset_time":"2026-08-27T06:56:51Z"},
				{"id":"3p-5h","remaining_fraction":0.02,"reset_time":"2026-08-20T11:56:51Z"}]}
		]}}}`))
	if err != nil {
		t.Fatalf("parseUsage() error = %v", err)
	}
	if result.AccountID != "antigravity-cli" || !result.Active {
		t.Fatalf("identity = %q/%v", result.AccountID, result.Active)
	}
	if got := result.Windows[quota.Window5Hour].RemainingPct; got != 43 {
		t.Fatalf("5h remaining = %d, want 43", got)
	}
	if got := result.Windows[quota.Window7Day].RemainingPct; got != 73 {
		t.Fatalf("7d remaining = %d, want 73", got)
	}
}
```

Add table cases that reject malformed JSON, non-success status, nonzero turns, nonzero total tokens, wrong command name, missing usage/command objects, missing required bucket, duplicate required bucket, and missing `remaining_fraction`. Add clamping case with `1.4` and `-0.2` expecting `100` and `0`, plus exhausted status.

- [ ] **Step 2: Run parser tests and verify RED**

Run: `go test -race -count=1 ./internal/provider/gemini -run '^TestParseUsage'`

Expected: compile failure because `parseUsage` and Antigravity constants do not exist.

- [ ] **Step 3: Implement strict parser**

Define pointer-bearing envelope fields so absent objects and fractions cannot look like valid zero values:

```go
const (
	antigravityAccountID   = "antigravity-cli"
	geminiFiveHourBucketID = "gemini-5h"
	geminiWeeklyBucketID   = "gemini-weekly"
)

type usageEnvelope struct {
	Status   string        `json:"status"`
	NumTurns int           `json:"num_turns"`
	Usage    *usageTotals  `json:"usage"`
	Command  *usageCommand `json:"command"`
}
```

Unmarshal once. Validate safety fields before reading buckets. Track required bucket counts, reject counts other than one, round via `math.Round`, clamp via `max`/`min`, parse reset times with `quota.ParseResetTime`, and return status from `quota.StatusFromWindows`.

- [ ] **Step 4: Run parser tests and verify GREEN**

Run: `go test -race -count=1 ./internal/provider/gemini -run '^TestParseUsage'`

Expected: PASS.

- [ ] **Step 5: Commit parser**

```bash
git add internal/provider/gemini/parser.go internal/provider/gemini/parser_test.go
git commit -m "feat: parsed Antigravity quota output" -m $'- Validated zero-turn structured usage responses.\n- Mapped Gemini five-hour and weekly limits.'
```

### Task 2: Add bounded CLI transport

**Files:**
- Create: `internal/provider/gemini/cli.go`
- Create: `internal/provider/gemini/cli_test.go`

**Interfaces:**
- Produces: `type commandRunner interface { LookPath(string) (string, error); Run(context.Context, string, ...string) ([]byte, error) }`
- Produces: `type osCommandRunner struct{}` implementing `commandRunner`
- Produces: constant `maxCLIOutputBytes = 1 << 20`

- [ ] **Step 1: Write failing real-process boundary tests**

Use test binary as helper process. `TestCLIHelperProcess` checks `CQ_TEST_CLI_HELPER=1`, then emits requested byte count or waits for cancellation. Test `osCommandRunner.Run` returns exact small output, rejects `maxCLIOutputBytes+1`, and returns after context cancellation.

```go
func TestOSCommandRunnerRejectsOversizeOutput(t *testing.T) {
	t.Setenv("CQ_TEST_CLI_HELPER", "1")
	t.Setenv("CQ_TEST_CLI_BYTES", strconv.Itoa(maxCLIOutputBytes+1))
	_, err := (osCommandRunner{}).Run(context.Background(), os.Args[0], "-test.run=TestCLIHelperProcess")
	if !errors.Is(err, errCLIOutputTooLarge) {
		t.Fatalf("Run() error = %v, want errCLIOutputTooLarge", err)
	}
}
```

- [ ] **Step 2: Run CLI tests and verify RED**

Run: `go test -race -count=1 ./internal/provider/gemini -run '^(TestOSCommandRunner|TestCLIHelperProcess)'`

Expected: compile failure because runner types and limit do not exist.

- [ ] **Step 3: Implement direct bounded runner**

`LookPath` delegates to `exec.LookPath`. `Run` uses `exec.CommandContext`, sets `Stderr` to `io.Discard`, reads `StdoutPipe` through `io.LimitReader(maxCLIOutputBytes+1)`, kills and waits for child on overflow/read failure, waits normally after EOF, and returns `ctx.Err()` for cancellation/deadline.

No method formats process output into returned errors.

- [ ] **Step 4: Run CLI tests and verify GREEN**

Run: `go test -race -count=1 ./internal/provider/gemini -run '^(TestOSCommandRunner|TestCLIHelperProcess)'`

Expected: PASS without leaked helper processes.

- [ ] **Step 5: Commit transport**

```bash
git add internal/provider/gemini/cli.go internal/provider/gemini/cli_test.go
git commit -m "feat: added bounded quota command" -m $'- Executed quota lookup without a shell.\n- Bounded output and honoured cancellation.'
```

### Task 3: Replace legacy provider orchestration

**Files:**
- Replace: `internal/provider/gemini/provider_test.go`
- Replace: `internal/provider/gemini/provider.go`
- Delete: `internal/provider/gemini/refresh.go`
- Modify: `cmd/cq/main.go:370-377`

**Interfaces:**
- Consumes: `commandRunner`, `parseUsage`, `antigravityAccountID`
- Produces: `func New() *Provider`
- Produces: `func newProvider(commandRunner) *Provider` for package tests

- [ ] **Step 1: Write failing provider tests**

Define fake runner that records executable and arguments while returning full structured fixture. Add tests proving:

- `DiscoverAccounts` returns no accounts when `agy` lookup fails.
- Discovery returns stable ID, label, and active state when lookup succeeds, without calling `Run`.
- `Fetch` invokes resolved path with exact arguments `-p`, `/usage`, `--output-format`, `json`, `--print-timeout`, `15s`.
- Success returns parser result with `active: true`.
- Missing executable returns `not_configured`.
- command error and timeout return privacy-safe `fetch_error` without raw fake stderr text.
- parser failure returns privacy-safe `parse_error` without raw output.
- caller cancellation reaches injected runner context.

- [ ] **Step 2: Run provider tests and verify RED**

Run: `go test -race -count=1 ./internal/provider/gemini -run '^(TestDiscoverAccounts|TestFetch)'`

Expected: compile failures because `New` signature and injected runner design do not exist.

- [ ] **Step 3: Implement provider orchestration**

```go
var usageArgs = []string{"-p", "/usage", "--output-format", "json", "--print-timeout", "15s"}

func New() *Provider { return newProvider(osCommandRunner{}) }

func (p *Provider) Fetch(ctx context.Context, _ time.Time) ([]quota.Result, error) {
	path, err := p.runner.LookPath("agy")
	if err != nil {
		return []quota.Result{quota.ErrorResult("not_configured", "install and authenticate antigravity-cli", 0)}, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	data, err := p.runner.Run(runCtx, path, usageArgs...)
	if err != nil {
		return []quota.Result{quota.ErrorResult("fetch_error", "antigravity-cli usage failed", 0)}, nil
	}
	result, err := parseUsage(data)
	if err != nil {
		return []quota.Result{quota.ErrorResult("parse_error", "invalid antigravity-cli usage output", 0)}, nil
	}
	return []quota.Result{result}, nil
}
```

Delete all filesystem credential reads, token refresh, HTTP tier/quota calls, panic wrapper tied to old request, and retired helpers. Change command wiring to `geminiprov.New()`.

- [ ] **Step 4: Run provider package tests and verify GREEN**

Run: `go test -race -count=1 ./internal/provider/gemini`

Expected: PASS.

- [ ] **Step 5: Run command package regression tests**

Run: `go test -race -count=1 ./cmd/cq`

Expected: PASS and no stale constructor callers.

- [ ] **Step 6: Commit provider swap**

```bash
git add internal/provider/gemini/provider.go internal/provider/gemini/provider_test.go internal/provider/gemini/refresh.go cmd/cq/main.go
git commit -m "fix: migrated Gemini quota source" -m $'- Delegated quota retrieval to authenticated CLI.\n- Removed legacy credentials and retired API calls.'
```

### Task 4: Update architecture documentation and verify complete migration

**Files:**
- Modify: `README.md`
- Modify: `AGENTS.md`
- Modify: `internal/provider/AGENTS.md`
- Modify: `internal/provider/gemini/AGENTS.md`
- Modify: `internal/fsutil/AGENTS.md`

**Interfaces:**
- Consumes: completed provider behavior from Tasks 1-3
- Produces: current operator and contributor guidance

- [ ] **Step 1: Update documentation**

Document `agy` installation/authentication prerequisite, preserved `gemini` command identity, and external account ownership. Remove claims about `oauth_creds.json`, token refresh, Code Assist endpoints, Gemini filesystem injection, and universal provider HTTP clients.

- [ ] **Step 2: Scan for stale implementation references**

Run:

```bash
rg -n 'oauth_creds|cloudcode-pa|loadCodeAssist|retrieveUserQuota|geminiClientID|refreshAccessToken' internal/provider/gemini README.md AGENTS.md internal/provider/AGENTS.md internal/fsutil/AGENTS.md
```

Expected: no matches.

- [ ] **Step 3: Run full verification**

```bash
go test -race -count=1 ./...
go vet ./...
go build ./...
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 4: Run live isolated acceptance**

```bash
acceptance_bin="$(mktemp -d)/cq-antigravity"
go build -o "$acceptance_bin" ./cmd/cq
"$acceptance_bin" check gemini --json --refresh
```

Validate JSON with `jq`: provider key is `gemini`; status is `ok` or `exhausted`; `account_id` is `antigravity-cli`; windows contain `5h` and `7d`; no error row or HTTP 403 exists.

- [ ] **Step 5: Review diff against spec**

Check every compatibility, credential, command-safety, mapping, error, and non-goal requirement. Confirm only scoped files changed and no credential/token value entered Git history.

- [ ] **Step 6: Commit documentation**

```bash
git add README.md AGENTS.md internal/provider/AGENTS.md internal/provider/gemini/AGENTS.md internal/fsutil/AGENTS.md
git commit -m "docs: updated Gemini provider guidance" -m $'- Documented external quota and account authority.\n- Removed retired credential workflow references.'
```
