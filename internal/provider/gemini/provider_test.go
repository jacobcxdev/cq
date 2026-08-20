package gemini

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

type fakeCommandRunner struct {
	path        string
	lookupErr   error
	output      []byte
	runErr      error
	waitForDone bool

	lookupNames     []string
	runPaths        []string
	runArgs         [][]string
	hasDeadline     bool
	deadlineTimeout time.Duration
	contextErr      error
}

func (f *fakeCommandRunner) LookPath(name string) (string, error) {
	f.lookupNames = append(f.lookupNames, name)
	return f.path, f.lookupErr
}

func (f *fakeCommandRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	f.runPaths = append(f.runPaths, path)
	f.runArgs = append(f.runArgs, append([]string(nil), args...))
	if deadline, ok := ctx.Deadline(); ok {
		f.hasDeadline = true
		f.deadlineTimeout = time.Until(deadline)
	}
	if f.waitForDone {
		<-ctx.Done()
		f.contextErr = ctx.Err()
		return nil, ctx.Err()
	}
	return f.output, f.runErr
}

func TestDiscoverAccountsReturnsNoneWithoutCLI(t *testing.T) {
	runner := &fakeCommandRunner{lookupErr: errors.New("not found")}

	accounts, err := newProvider(runner).DiscoverAccounts(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAccounts() error = %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("DiscoverAccounts() count = %d, want 0", len(accounts))
	}
	if len(runner.runPaths) != 0 {
		t.Fatalf("Run() calls = %d, want 0", len(runner.runPaths))
	}
}

func TestDiscoverAccountsReturnsAntigravityCLI(t *testing.T) {
	runner := &fakeCommandRunner{path: "/opt/homebrew/bin/agy"}

	accounts, err := newProvider(runner).DiscoverAccounts(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAccounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("DiscoverAccounts() count = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if account.AccountID != antigravityAccountID || account.Label != "Antigravity CLI" || !account.Active {
		t.Fatalf("account = %#v, want stable active Antigravity identity", account)
	}
	if account.Email != "" {
		t.Fatalf("account email = %q, want empty", account.Email)
	}
	if !reflect.DeepEqual(runner.lookupNames, []string{"agy"}) {
		t.Fatalf("LookPath() names = %q, want [agy]", runner.lookupNames)
	}
	if len(runner.runPaths) != 0 {
		t.Fatalf("Run() calls = %d, want 0", len(runner.runPaths))
	}
}

func TestFetchRunsExactUsageCommand(t *testing.T) {
	runner := &fakeCommandRunner{
		path:   "/opt/homebrew/bin/agy",
		output: usageFixture("SUCCESS", 0, 0, "usage", validGeminiBuckets),
	}

	results, err := newProvider(runner).Fetch(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Fetch() count = %d, want 1", len(results))
	}
	if results[0].AccountID != antigravityAccountID || !results[0].Active {
		t.Fatalf("result identity = %q/%v, want antigravity-cli/true", results[0].AccountID, results[0].Active)
	}
	if !reflect.DeepEqual(runner.runPaths, []string{"/opt/homebrew/bin/agy"}) {
		t.Fatalf("Run() paths = %q, want resolved agy path", runner.runPaths)
	}
	wantArgs := []string{"-p", "/usage", "--output-format", "json", "--print-timeout", "15s"}
	if len(runner.runArgs) != 1 || !reflect.DeepEqual(runner.runArgs[0], wantArgs) {
		t.Fatalf("Run() args = %q, want %q", runner.runArgs, wantArgs)
	}
	if !runner.hasDeadline || runner.deadlineTimeout < 19*time.Second || runner.deadlineTimeout > 20*time.Second {
		t.Fatalf("Run() timeout = %s (present %v), want 20s safety timeout", runner.deadlineTimeout, runner.hasDeadline)
	}
}

func TestFetchReturnsNotConfiguredWithoutCLI(t *testing.T) {
	runner := &fakeCommandRunner{lookupErr: errors.New("not found")}

	result := fetchSingleResult(t, newProvider(runner), context.Background())
	assertResultError(t, result, "not_configured")
	if result.Error.Message != "install and authenticate antigravity-cli" {
		t.Fatalf("error message = %q, want install guidance", result.Error.Message)
	}
	if len(runner.runPaths) != 0 {
		t.Fatalf("Run() calls = %d, want 0", len(runner.runPaths))
	}
}

func TestFetchClassifiesCommandFailureWithoutDetails(t *testing.T) {
	runner := &fakeCommandRunner{
		path:   "/opt/homebrew/bin/agy",
		runErr: errors.New("secret diagnostic details"),
	}

	result := fetchSingleResult(t, newProvider(runner), context.Background())
	assertResultError(t, result, "fetch_error")
	if strings.Contains(result.Error.Message, "secret diagnostic details") {
		t.Fatalf("error message exposed command details: %q", result.Error.Message)
	}
}

func TestFetchClassifiesInvalidOutputWithoutDetails(t *testing.T) {
	raw := `{"secret":"account@example.com"}`
	runner := &fakeCommandRunner{
		path:   "/opt/homebrew/bin/agy",
		output: []byte(raw),
	}

	result := fetchSingleResult(t, newProvider(runner), context.Background())
	assertResultError(t, result, "parse_error")
	if strings.Contains(result.Error.Message, raw) || strings.Contains(result.Error.Message, "account@example.com") {
		t.Fatalf("error message exposed command output: %q", result.Error.Message)
	}
}

func TestFetchPropagatesCallerCancellation(t *testing.T) {
	runner := &fakeCommandRunner{
		path:        "/opt/homebrew/bin/agy",
		waitForDone: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := fetchSingleResult(t, newProvider(runner), ctx)
	assertResultError(t, result, "fetch_error")
	if !errors.Is(runner.contextErr, context.Canceled) {
		t.Fatalf("runner context error = %v, want %v", runner.contextErr, context.Canceled)
	}
}

type quotaProvider interface {
	Fetch(context.Context, time.Time) ([]quota.Result, error)
}

func fetchSingleResult(t *testing.T, provider quotaProvider, ctx context.Context) quota.Result {
	t.Helper()
	results, err := provider.Fetch(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Fetch() count = %d, want 1", len(results))
	}
	return results[0]
}

func assertResultError(t *testing.T, result quota.Result, code string) {
	t.Helper()
	if result.Status != quota.StatusError || result.Error == nil || result.Error.Code != code {
		t.Fatalf("result error = %#v, want code %q", result, code)
	}
}
