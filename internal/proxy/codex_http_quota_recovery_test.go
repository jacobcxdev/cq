package proxy

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestNormalProxyTransportHTTPFreshQuotaFailureProbesHistoricalExclusion(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		wantStatus int
	}{
		{name: "historical account recovered", scenario: normalTransportGateHTTPHardLimit, wantStatus: http.StatusOK},
		{name: "historical account still exhausted", scenario: normalTransportGateHTTPAllHardLimit, wantStatus: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateCodexCallerHarness(t, test.scenario)
			metadata := CodexTurnMetadata{SessionID: "historical-quota-session", ThreadID: "historical-quota-thread", TurnID: "historical-quota-predecessor", RequestKind: CodexRequestTurn}
			harness.httpPlanner.PinnedAccountKey = codexInstalledHTTPValidationAccountB
			prepared, err := harness.httpPlanner.Build(context.Background(), CodexHTTPRequestPlanInput{
				Encoded: normalTransportGateHTTPBody(t, metadata),
			})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Lifecycle.AccountKey() != codexInstalledHTTPValidationAccountB {
				t.Fatalf("seed account = %s, want B", prepared.Lifecycle.AccountKey())
			}
			marked, err := prepared.Lifecycle.MarkDispatchedContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := marked.RecordQuotaExhaustedContext(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			prepared.Frozen.Release()
			harness.httpPlanner.PinnedAccountKey = ""
			harness.httpPlanner.DefaultAccountKey = ""
			frozenDispatchObserveCapacity(t, harness.httpPlanner.Capacity, codexInstalledHTTPValidationDefault, CapacityBucketBase, 0, time.Now())

			metadata.TurnID = "historical-quota-successor"
			status, body := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
			if status != test.wantStatus {
				t.Fatalf("historical quota recovery = %d %q, want %d", status, body, test.wantStatus)
			}
			if test.wantStatus == http.StatusOK && (bytes.Contains(body, []byte("usage_limit_reached")) || !bytes.Contains(body, []byte(`"type":"response.completed"`))) {
				t.Fatalf("successful recovery leaked rejection or lacked completion: %q", body)
			}
			receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
			if len(receipts) != 2 || receipts[0].accountID != "validation-upstream-a" || receipts[0].status != http.StatusTooManyRequests || receipts[1].accountID != "validation-upstream-b" || receipts[1].status != test.wantStatus || receipts[0].payload != receipts[1].payload {
				t.Fatalf("receipts = %#v, want A/429 then exactly one byte-identical B probe with status %d", receipts, test.wantStatus)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportHTTPAdmittedQuotaFailureWithOnlyHistoricalAlternate(t *testing.T) {
	harness := newNormalTransportGateCodexCallerHarness(t, normalTransportGateHTTPSuccess)
	harness.backend.httpTurnState = true
	metadata := CodexTurnMetadata{SessionID: "historical-admitted-session", ThreadID: "historical-admitted-thread", TurnID: "historical-admitted-predecessor", RequestKind: CodexRequestTurn}
	seedHTTPQuotaExclusion(t, harness, metadata, codexInstalledHTTPValidationAccountB)
	harness.httpPlanner.DefaultAccountKey = ""
	frozenDispatchObserveCapacity(t, harness.httpPlanner.Capacity, codexInstalledHTTPValidationDefault, CapacityBucketBase, 0, time.Now())
	metadata.TurnID = "historical-admitted-current"
	encoded := normalTransportGateHTTPBody(t, metadata)
	if status, body := normalTransportGateHTTPCall(t, harness, encoded); status != http.StatusOK {
		t.Fatalf("seed A = %d %q", status, body)
	}
	harness.backend.scenario = normalTransportGateHTTPHardLimit
	status, body := normalTransportGateHTTPCall(t, harness, encoded, http.Header{"X-Codex-Turn-State": {"state-validation-upstream-a"}})
	if status != http.StatusOK {
		t.Fatalf("admitted historical recovery = %d %q, want B/200", status, body)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 3 || receipts[0].accountID != "validation-upstream-a" || receipts[1].status != http.StatusTooManyRequests || receipts[2].accountID != "validation-upstream-b" || receipts[2].status != http.StatusOK || receipts[1].payload != receipts[2].payload || receipts[2].turnState != "" {
		t.Fatalf("receipts = %#v, want A/200, A/429, identical B/200 without old turn state", receipts)
	}
	harness.backend.assertNoFailure(t)
}

func seedHTTPQuotaExclusion(t *testing.T, harness *normalTransportGateHarness, metadata CodexTurnMetadata, account codex.AccountKey) {
	t.Helper()
	harness.httpPlanner.PinnedAccountKey = account
	defer func() { harness.httpPlanner.PinnedAccountKey = "" }()
	prepared, err := harness.httpPlanner.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: normalTransportGateHTTPBody(t, metadata)})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Frozen.Release()
	if prepared.Lifecycle.AccountKey() != account {
		t.Fatalf("seed account = %s, want %s", prepared.Lifecycle.AccountKey(), account)
	}
	marked, err := prepared.Lifecycle.MarkDispatchedContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marked.RecordQuotaExhaustedContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestNormalProxyTransportHTTPAdmittedQuotaRecoveryAfterOrdinaryAlternate(t *testing.T) {
	harness := newNormalTransportGateCodexCallerHarness(t, normalTransportGateHTTPSuccess)
	metadata := CodexTurnMetadata{SessionID: "historical-budget-session", ThreadID: "historical-budget-thread", TurnID: "historical-budget-predecessor", RequestKind: CodexRequestTurn}
	seedHTTPQuotaExclusion(t, harness, metadata, codexInstalledHTTPValidationDefault)
	harness.httpPlanner.DefaultAccountKey = ""
	metadata.TurnID = "historical-budget-current"
	encoded := normalTransportGateHTTPBody(t, metadata)
	if status, body := normalTransportGateHTTPCall(t, harness, encoded); status != http.StatusOK {
		t.Fatalf("seed A = %d %q", status, body)
	}
	harness.backend.httpHardLimitAccounts = map[string]bool{"validation-upstream-a": true, "validation-upstream-b": true}
	status, body := normalTransportGateHTTPCall(t, harness, encoded)
	if status != http.StatusOK {
		t.Fatalf("ordinary then historical recovery = %d %q, want C/200", status, body)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 4 || receipts[0].accountID != "validation-upstream-a" || receipts[1].accountID != "validation-upstream-a" || receipts[1].status != http.StatusTooManyRequests || receipts[2].accountID != "validation-upstream-b" || receipts[2].status != http.StatusTooManyRequests || receipts[3].accountID != "validation-upstream-default" || receipts[3].status != http.StatusOK || receipts[1].payload != receipts[2].payload || receipts[2].payload != receipts[3].payload {
		t.Fatalf("receipts = %#v, want A/200, A/429, identical B/429, identical C/200", receipts)
	}
	harness.backend.assertNoFailure(t)
}
