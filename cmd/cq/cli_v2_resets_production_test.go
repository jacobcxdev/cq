package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/compat"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/httputil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func resetProductionFixture(t *testing.T) (*codexprov.ManagedStore, codexprov.ManagedRecord, codexprov.RemovalJournal) {
	t.Helper()
	root, err := os.MkdirTemp(v2FixtureTempRoot(), "cqr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	fs := resetProductionFS{home: root}
	store := &codexprov.ManagedStore{FS: fs, Home: root, Random: rand.Reader, EnsureEpoch: func() error {
		return compat.EnsureEpoch(fs, filepath.Join(root, "state", "compatibility-epoch"), compat.CurrentEpoch)
	}}
	token := fakeRefreshCodexJWT("synthetic@example.test", "account", "user", time.Now().Add(time.Hour))
	record, err := store.SaveNew(codexprov.LoginCredential{Tokens: auth.CodexTokenResponse{AccessToken: token, IDToken: token, RefreshToken: "synthetic-refresh"}, Claims: auth.CodexClaims{AccountID: "account", UserID: "user", Email: "synthetic@example.test"}, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return store, record, codexprov.RemovalJournal{FS: store.FS, Store: store, StateDir: filepath.Join(root, "state")}
}
func resetProductionResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func resetProductionUsage() *http.Response {
	return resetProductionResponse(200, `{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_at":2000000000},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":2000000000}}}`)
}
func TestCLIV2ResetProductionRefreshAuthority(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, mode := range []string{"pending-before", "pending-after-inventory", "success", "cancel", "no-authority"} {
			t.Run(map[bool]string{false: "local", true: "remote"}[remote]+"/"+mode, func(t *testing.T) {
				store, record, journal := resetProductionFixture(t)
				plan := codexprov.RemovalPlan{Version: 1, OperationID: "synthetic-pending", AccountKey: record.Metadata.AccountKey, Candidates: []codexprov.RemovalCandidate{{CandidateID: record.Metadata.CandidateID, Revision: record.Metadata.Revision}}}
				var exchanges, refreshedUsage atomic.Int32
				var once sync.Once
				entered := make(chan struct{})
				cancelled := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
				defer releaseWorker()
				client := testDoer(func(req *http.Request) (*http.Response, error) {
					switch req.URL.Path {
					case "/backend-api/wham/rate-limit-reset-credits":
						return resetProductionResponse(200, `{"credits":[],"available_count":0}`), nil
					case "/backend-api/wham/usage":
						if req.Header.Get("Authorization") == "Bearer refreshed-synthetic" {
							refreshedUsage.Add(1)
							return resetProductionUsage(), nil
						}
						if mode == "pending-after-inventory" {
							once.Do(func() {
								if err := journal.Save(plan); err != nil {
									panic(err)
								}
							})
						}
						return resetProductionResponse(401, `{}`), nil
					case "/oauth/token":
						exchanges.Add(1)
						if mode == "cancel" {
							close(entered)
							select {
							case <-req.Context().Done():
								close(cancelled)
								return nil, req.Context().Err()
							case <-release:
								return nil, context.Canceled
							}
						}
						body, _ := json.Marshal(auth.CodexTokenResponse{AccessToken: "refreshed-synthetic", RefreshToken: "next-synthetic", IDToken: record.Credential.IDToken, ExpiresIn: 3600})
						return resetProductionResponse(200, string(body)), nil
					default:
						panic("unexpected fake HTTP route")
					}
				})
				var owner *codexprov.CredentialControl
				if mode != "no-authority" {
					coordinator, err := codexprov.NewCredentialCoordinator(store, journal.StateDir)
					if err != nil {
						t.Fatal(err)
					}
					coordinator.RefreshMutations = resetProductionRecorder{}
					coordinator.CredentialOwner = resetProductionRecorder{}
					coordinator.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
						return auth.RefreshCodexToken(ctx, client, token)
					}
					owner, err = codexprov.OpenCredentialControl(codexprov.DefaultCredentialControlPath(journal.StateDir), coordinator)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { releaseWorker(); owner.Close() }()
				} else if remote {
					var err error
					owner, err = openResetProductionControl(context.Background(), store, client)
					if err != nil {
						t.Fatal(err)
					}
					defer owner.Close()
				}

				if mode == "pending-before" {
					if err := journal.Save(plan); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				factory := func(ctx context.Context, recommend bool) (v2ResetDependencies, error) {
					if !remote && owner != nil {
						return newV2ResetDependenciesWithControlAt(recommend, store.FS, client, owner, store.Home, filepath.Join(store.Home, "cache"))
					}
					return resetProductionDependencies(ctx, recommend, store, client)
				}
				completed := make(chan cli.Outcome, 1)
				go func() {
					completed <- handleV2ResetInspectionWithFactory(ctx, v2ResetInvocation("codex reset recommend"), nil, factory)
				}()
				if mode == "cancel" {
					select {
					case <-entered:
						cancel()
					case <-time.After(3 * time.Second):
						t.Fatal("usage refresh never started")
					}
				}
				var out cli.Outcome
				select {
				case out = <-completed:
				case <-time.After(3 * time.Second):
					t.Fatal("recommendation did not finish")
				}
				if mode == "cancel" {
					select {
					case <-cancelled:
					case <-time.After(time.Second):
						t.Error("usage refresh did not receive command cancellation")
					}
					if out.ExitCode != 130 {
						t.Errorf("exit=%d", out.ExitCode)
					}
					return
				}
				if mode == "no-authority" {
					if exchanges.Load() != 0 || refreshedUsage.Load() != 0 || out.ExitCode != 8 {
						t.Errorf("unprovisioned authority refreshed: exchanges=%d exit=%d", exchanges.Load(), out.ExitCode)
					}
					return
				}
				if mode == "success" {
					updated, err := store.Load(record.Path)
					if err != nil || updated.Credential.AccessToken != "refreshed-synthetic" || updated.Metadata.Revision == record.Metadata.Revision || exchanges.Load() != 1 || refreshedUsage.Load() != 1 || out.ExitCode != 0 {
						t.Errorf("refresh failed: loaded=%v exchanges=%d refreshed usage=%d exit=%d", err, exchanges.Load(), refreshedUsage.Load(), out.ExitCode)
					}
				} else {
					if exchanges.Load() != 0 {
						t.Errorf("pending removal dispatched %d exchanges", exchanges.Load())
					}
					if _, err := store.Load(record.Path); err != nil {
						t.Errorf("pending removal deleted credential: %v", err)
					}
					if _, exists, err := journal.Load(); err != nil || !exists {
						t.Errorf("pending removal recovered: exists=%t error=%v", exists, err)
					}
				}
			})
		}
	}
}

type resetProductionUsageWait struct {
	app.CodexResetUsage
	done chan struct{}
}

func (u resetProductionUsageWait) Fetch(ctx context.Context, now time.Time) ([]quota.Result, error) {
	defer close(u.done)
	return u.CodexResetUsage.Fetch(ctx, now)
}

type resetProductionHistoryWait struct {
	app.CodexResetHistory
	done chan struct{}
}

func (h resetProductionHistoryWait) UpdateAndGetEstimates(ctx context.Context, rows map[string][]quota.Result, now int64) (history.BurnRates, history.RateEstimates, error) {
	defer close(h.done)
	return h.CodexResetHistory.UpdateAndGetEstimates(ctx, rows, now)
}

type resetProductionHistoryFS struct {
	fsutil.DurableFileSystem
	entered, release chan struct{}
}

func (f resetProductionHistoryFS) ReadFile(path string) ([]byte, error) {
	if strings.HasSuffix(path, "burn_state_v2.json") {
		close(f.entered)
		<-f.release
		return []byte(`{"sensitive-synthetic":!}`), nil
	}
	return f.DurableFileSystem.ReadFile(path)
}
func TestCLIV2ResetProductionDiagnostics(t *testing.T) {
	for _, mode := range []string{"panic", "late-panic", "timeout-panic", "history", "late-history", "timeout-history"} {
		t.Run(mode, func(t *testing.T) {
			store, _, _ := resetProductionFixture(t)
			process, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer process.Close()
			previous := os.Stderr
			os.Stderr = process
			defer func() { os.Stderr = previous }()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			late := strings.HasPrefix(mode, "late-") || strings.HasPrefix(mode, "timeout-")
			client := testDoer(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/usage") {
					if strings.Contains(mode, "panic") {
						close(entered)
						if late {
							<-release
						}
						panic("sensitive-synthetic-panic")
					}
					return resetProductionUsage(), nil
				}
				return resetProductionResponse(200, `{"credits":[],"available_count":0}`), nil
			})
			factory := func(ctx context.Context, recommend bool) (v2ResetDependencies, error) {
				var deps v2ResetDependencies
				var err error
				if strings.Contains(mode, "history") {
					control, openErr := openResetProductionControl(ctx, store, client)
					if openErr != nil {
						return deps, openErr
					}
					deps, err = newV2ResetDependenciesWithControlAt(recommend, resetProductionHistoryFS{store.FS, entered, release}, client, control, store.Home, filepath.Join(store.Home, "cache"))
				} else {
					deps, err = resetProductionDependencies(ctx, recommend, store, client)
				}
				if err != nil {
					return deps, err
				}
				if strings.Contains(mode, "panic") {
					deps.app.Usage = resetProductionUsageWait{deps.app.Usage, done}
				} else {
					deps.app.History = resetProductionHistoryWait{deps.app.History, done}
					if !late {
						close(release)
					}
				}
				return deps, nil
			}
			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			if strings.HasPrefix(mode, "timeout-") {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			finished := make(chan int, 1)
			go func() {
				finished <- cli.Run(ctx, []string{"codex", "reset", "recommend", "--json"}, &cli.Session{Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) {
					return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
						return handleV2ResetInspectionWithFactory(ctx, inv, s, factory)
					}, true
				})
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("production diagnostic path not entered")
			}
			if strings.HasPrefix(mode, "late-") {
				cancel()
			}
			var exit int
			select {
			case exit = <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("command did not finish")
			}
			before := stdout.String() + stderr.String()
			if late {
				close(release)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("late worker did not finish")
			}
			raw, err := os.ReadFile(process.Name())
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) != 0 {
				t.Errorf("raw process stderr escaped safe diagnostics (%d bytes)", len(raw))
			}
			if before != stdout.String()+stderr.String() || strings.Contains(before, "sensitive-synthetic") || strings.Contains(before, "goroutine") {
				t.Error("unsafe or late session output")
			}
			expectedExit := 0
			if strings.Contains(mode, "panic") {
				expectedExit = 8
			}
			if strings.HasPrefix(mode, "late-") {
				expectedExit = 130
			}
			if strings.HasPrefix(mode, "timeout-") {
				expectedExit = 7
			}
			if exit != expectedExit {
				t.Errorf("exit=%d want %d", exit, expectedExit)
			}
			if !late && !strings.Contains(before, map[bool]string{true: "codex_fetch_panic", false: "history_decode_failed"}[strings.Contains(mode, "panic")]) {
				t.Error("missing safe diagnostic")
			}
		})
	}
}

// Hermetic lifecycle recorders exercise the real refresh exchange and store.
type resetProductionRecorder struct{}

func (resetProductionRecorder) SelectRefreshMutation(op string, _ codexprov.CandidateRef, _ codexprov.Revision, _ codexprov.RefreshMutationCapacity) (codexprov.RefreshMutationSelection, error) {
	return codexprov.RefreshMutationSelection{ReservationDigest: op + "-reservation", CapacityLeaseDigest: op + "-lease"}, nil
}
func (resetProductionRecorder) CompleteRefreshMutation(op, receipt string) (string, error) {
	return op + "-terminal", nil
}
func (resetProductionRecorder) PublishCommit(op, reservation, lease string) (string, error) {
	return op + "-commit", nil
}
func (resetProductionRecorder) PublishReceipt(op, commit string) (string, error) {
	return op + "-receipt", nil
}
func (h resetProductionHistoryWait) UpdateAndGetEstimatesObserved(ctx context.Context, rows map[string][]quota.Result, now int64, warning func(string, string)) (history.BurnRates, history.RateEstimates, error) {
	defer close(h.done)
	if observed, ok := h.CodexResetHistory.(interface {
		UpdateAndGetEstimatesObserved(context.Context, map[string][]quota.Result, int64, func(string, string)) (history.BurnRates, history.RateEstimates, error)
	}); ok {
		return observed.UpdateAndGetEstimatesObserved(ctx, rows, now, warning)
	}
	return h.CodexResetHistory.UpdateAndGetEstimates(ctx, rows, now)
}

// Override the native home query as well as all CQ roots, including on Windows.
type resetProductionFS struct {
	fsutil.OSFileSystem
	home string
}

func (f resetProductionFS) UserHomeDir() (string, error) { return f.home, nil }
func openResetProductionControl(ctx context.Context, store *codexprov.ManagedStore, client httputil.Doer) (*codexprov.CredentialControl, error) {
	state := filepath.Join(store.Home, "state")
	coordinator, err := codexprov.NewCredentialCoordinator(store, state)
	if err != nil {
		return nil, err
	}
	coordinator.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
		return auth.RefreshCodexToken(ctx, client, token)
	}
	return codexprov.OpenCredentialControlPrepared(ctx, codexprov.DefaultCredentialControlPath(state), coordinator, func(ctx context.Context, _ *codexprov.CredentialCoordinator, cap codexprov.CredentialOwnerCapability) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return cap.AssertOwner()
	})
}
func resetProductionDependencies(ctx context.Context, recommend bool, store *codexprov.ManagedStore, client httputil.Doer) (v2ResetDependencies, error) {
	control, err := openResetProductionControl(ctx, store, client)
	if err != nil {
		return v2ResetDependencies{}, err
	}
	return newV2ResetDependenciesWithControlAt(recommend, store.FS, client, control, store.Home, filepath.Join(store.Home, "cache"))
}
