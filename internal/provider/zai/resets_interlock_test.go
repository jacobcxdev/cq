package zai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestResetExecutorBlockedPendingZCodeUseBeforeManualOrScheduledDispatch(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "scheduled"}[scheduled], func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			writeZCodeResetInteropFixture(t, fsys, "pending", "account")
			client := ResetClient{HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("cross-protocol pending reset reached HTTP")
				return nil, nil
			})}
			executor := resetTestExecutor(fsys, client)
			account := Account{ID: "account", APIKey: "private-key"}
			var result ResetUseResult
			var err error
			if scheduled {
				result, err = executor.UseScheduled(context.Background(), account, "5h", "5h:earlier")
			} else {
				result, err = executor.Use(context.Background(), account, "5h", "5h:earlier", func(ResetCredit) (bool, error) {
					t.Fatal("cross-protocol pending reset requested confirmation")
					return true, nil
				})
			}
			if !errors.Is(err, ErrResetPending) || !result.Dispatched {
				t.Fatal("other reset contract's uncertainty was not preserved")
			}
		})
	}
}

func TestResetExecutorRejectedMalformedOtherResetJournalBeforeHTTP(t *testing.T) {
	for _, body := range []string{`{`, strings.Repeat("x", (16<<10)+1), `{"account_id":"other","state":"pending"}`, `{"schema_version":1,"account_id":"account","operation":"use","window":"5h","request_id":"00000000-0000-4000-8000-000000000001","state":"unexpected","started_at":"2026-11-02T19:00:00Z"}`} {
		fsys := fsutil.NewMemFS()
		if err := fsys.WriteFile(resetUseInteropPath("/state/zai-resets", "account"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := resetTestExecutor(fsys, ResetClient{HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("malformed interop journal reached HTTP")
			return nil, nil
		})})
		if _, err := executor.UseScheduled(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "5h:earlier"); err == nil {
			t.Fatal("malformed other reset journal accepted")
		}
	}
}

func TestResetExecutorCompletedOrRejectedZCodeUseDidNotBlockExactCard(t *testing.T) {
	for _, state := range []string{"completed", "rejected"} {
		t.Run(state, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			writeZCodeResetInteropFixture(t, fsys, state, "account")
			posts := 0
			client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					return resetResponse(200, `{"code":200,"success":true}`), nil
				}
				return resetResponse(200, resetFixture), nil
			})}
			result, err := resetTestExecutor(fsys, client).UseScheduled(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "5h:earlier")
			if err != nil || !result.Accepted || posts != 1 {
				t.Fatal("resolved other reset journal blocked exact reset")
			}
		})
	}
}

func writeZCodeResetInteropFixture(t *testing.T, fsys fsutil.FileSystem, state, accountID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"schema_version": 1, "account_id": accountID, "operation": "use", "window": "5h", "request_id": "00000000-0000-4000-8000-000000000001", "state": state, "started_at": resetNow})
	if err := fsys.WriteFile(resetUseInteropPath("/state/zai-resets", "account"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}
