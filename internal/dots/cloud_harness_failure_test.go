package dots

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessFailureRetainedSafeStageAndRollback(t *testing.T) {
	for _, test := range []struct{ stage, method, rollback string }{
		{"release_subscriptions", "thread/unsubscribe", "restored"},
		{"resume", "thread/resume", "restored"},
		{"settings", "thread/settings/update", "restored"},
		{"resume", "thread/resume", "failed"},
	} {
		t.Run(test.stage+"/"+test.rollback, func(t *testing.T) {
			h, store, id, methods := cloudHarnessFailureFixture(t)
			attempts := 0
			base := h.b.local.input
			h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
				request := cloudHarnessObject(raw)
				if request.method() == test.method {
					attempts++
					if attempts == 1 || test.rollback == "failed" {
						h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32042, "message": "private-token-must-not-escape", "data": map[string]string{"secret": "private-token-must-not-escape"}})}
						return len(raw), nil
					}
				}
				return base.Write(raw)
			}}
			if err := h.reconcile(id, "", ""); err == nil {
				t.Fatal("native rejection was reported as successful")
			}
			receipt, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			encoded := string(cloudHarnessRaw(receipt))
			var record struct {
				Failure struct {
					Stage, Reason, Rollback string
					NativeCode              *int `json:"native_code"`
				}
			}
			if err := json.Unmarshal([]byte(encoded), &record); err != nil {
				t.Fatal(err)
			}
			if record.Failure.Stage != test.stage || record.Failure.Reason != "rpc_rejected" || record.Failure.Rollback != test.rollback || record.Failure.NativeCode == nil || *record.Failure.NativeCode != -32042 {
				t.Fatalf("failure lost stage or rollback: %s", encoded)
			}
			if strings.Contains(encoded, "private-token") {
				t.Fatal("native message or data escaped into receipt")
			}
			if test.rollback == "failed" && (receipt.Failure.RollbackStage != "resume" || receipt.Failure.RollbackReason != "rpc_rejected" || receipt.Failure.RollbackNativeCode == nil || *receipt.Failure.RollbackNativeCode != -32042) {
				t.Fatalf("rollback failure was conflated with original error: %#v", receipt.Failure)
			}
			if receipt.State != "failed" || len(*methods) == 0 {
				t.Fatal("failed lifecycle was lost")
			}
		})
	}
}

func TestCloudHarnessSubscriptionRestoreFailureWasRecorded(t *testing.T) {
	h, store, id, _ := cloudHarnessFailureFixture(t)
	h.threads[id].subscriptions["gateway/other"] = true
	resumes := 0
	h.gateway = cloudHarnessSourceGateway{write: func(_ string, raw []byte) error {
		request := cloudHarnessObject(raw)
		result := map[string]any{"status": "unsubscribed"}
		response := cloudMessage{"result": cloudHarnessRaw(result)}
		if request.method() == "thread/resume" {
			resumes++
			if resumes == 1 {
				response = cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32044, "message": "private-secret"})}
			} else {
				response = cloudMessage{"result": cloudHarnessRaw(map[string]any{"thread": map[string]string{"id": id}})}
			}
		}
		h.observeFrom("gateway/other", cloudHarnessRaw(map[string]any{"id": request["id"], "result": json.RawMessage(response["result"]), "error": json.RawMessage(response["error"])}))
		return nil
	}}
	if err := h.reconcile(id, "", ""); err == nil {
		t.Fatal("failed subscription restore was reported as successful")
	}
	receipt, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Failure == nil || receipt.Failure.Stage != "restore_subscriptions" || receipt.Failure.Reason != "rpc_rejected" || receipt.Failure.Rollback != "restored" || receipt.Failure.NativeCode == nil || *receipt.Failure.NativeCode != -32044 {
		t.Fatalf("restore failure not preserved: %#v", receipt.Failure)
	}
}

func TestCloudHarnessFailureEventWasSafeAndRecoveryClearedDetail(t *testing.T) {
	h, store, id, _ := cloudHarnessFailureFixture(t)
	var events []map[string]any
	h.b.deps.event = func(event map[string]any) { events = append(events, event) }
	h.recordFailure(id, harness.Selection{Profile: harness.Minimal, Source: "default"}, "settings", &cloudHarnessRPCError{reason: "rpc_rejected"}, "not_needed", "", nil)
	if len(events) != 1 || events[0]["failure"] == nil {
		t.Fatal("failure event omitted classification")
	}
	if err := h.record(id, harness.Selection{Profile: harness.Minimal, Source: "default"}, "applied"); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.Failure != nil || h.threads[id].failure != nil {
		t.Fatalf("recovery retained stale failure: %#v, %v", receipt, err)
	}
}

func TestCloudHarnessFailedHotSettingsWaitedForExplicitRetry(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching-plan", true: "before-first-turn"}[early], func(t *testing.T) {
			h, store, id, _ := cloudHarnessFailureFixture(t)
			policy := harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}}
			h.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return policy, nil }
			h.b.deps.readHarnessInventory = func(string) (harness.Inventory, error) { return harness.Inventory{}, nil }
			selection := policy.Resolve("glm-5.3", "/fixture", id)
			thread := h.threads[id]
			thread.profileManaged, thread.profileFingerprint = true, selection.Fingerprint
			thread.profileNeedsSettings = true
			thread.awaitingRollout = early
			thread.pluginsKnown = true
			thread.plugins = []string{"existing@fixture"}
			if !early {
				thread.profilePlan = &cloudHarnessProfilePlan{key: "same", config: cloudMessage{}, plugins: []string{}}
			}
			settings := 0
			base := h.b.local.input
			h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
				request := cloudHarnessObject(raw)
				if request.method() == "thread/settings/update" {
					settings++
					h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32042, "message": "private-secret"})}
					return len(raw), nil
				}
				return base.Write(raw)
			}}
			if err := h.reconcile(id, "", ""); err == nil {
				t.Fatal("fixture did not reject hot settings")
			}
			first, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := h.reconcile(id, "", ""); err == nil {
					t.Fatal("failed hot settings were not cached")
				}
			}
			cached, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if settings != 1 || !cached.UpdatedAt.Equal(first.UpdatedAt) {
				t.Fatalf("poll repeated failed settings: calls=%d first=%v last=%v", settings, first.UpdatedAt, cached.UpdatedAt)
			}
			if _, err := store.RequestRetry(id, harness.RetryRequest{Fingerprint: cached.Fingerprint, Model: cached.Model, Cwd: cached.Cwd, FailedAt: cached.UpdatedAt}); err != nil {
				t.Fatal(err)
			}
			if err := h.reconcile(id, "", ""); err == nil {
				t.Fatal("retry fixture unexpectedly succeeded")
			}
			if settings != 2 {
				t.Fatalf("one explicit retry made %d settings calls", settings)
			}
			for range 2 {
				if err := h.reconcile(id, "", ""); err == nil {
					t.Fatal("failed retry was not cached")
				}
			}
			if settings != 2 {
				t.Fatalf("failed explicit retry repeated: %d settings calls", settings)
			}
		})
	}
}

func cloudHarnessFailureFixture(t *testing.T) (*cloudHarness, harness.Receipts, string, *[]string) {
	t.Helper()
	const id = "00000000-0000-0000-0000-000000000001"
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	b := &cloudBridge{ctx: context.Background()}
	b.deps.readHarnessPolicy = func() (harness.Policy, error) { return harness.Policy{Version: 1, Enabled: true}, nil }
	b.deps.harnessReceipts = store
	h := newCloudHarness(b)
	h.threads[id] = &cloudHarnessThread{id: id, model: "glm-5.3", cwd: "/fixture", owned: true, subscriptions: map[string]bool{"stdio": true}, baseline: cloudMessage{"config": cloudHarnessRaw(map[string]any{"fixture": true})}}
	methods := []string{}
	b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		request := cloudHarnessObject(raw)
		methods = append(methods, request.method())
		var result any = map[string]any{}
		switch request.method() {
		case "config/read":
			result = map[string]any{"config": map[string]any{"model": "glm-5.3", "features": map[string]bool{"plugins": true}}, "layers": []any{}}
		case "thread/unsubscribe":
			result = map[string]string{"status": "unsubscribed"}
		case "thread/resume":
			result = map[string]any{"thread": map[string]string{"id": id}}
		case "mcpServerStatus/list":
			result = map[string]any{"data": []any{}, "nextCursor": nil}
		case "experimentalFeature/list":
			result = map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": false}}, "nextCursor": nil}
		}
		h.calls[string(request["id"])] <- cloudMessage{"result": cloudHarnessRaw(result)}
		return len(raw), nil
	}}}
	t.Cleanup(h.close)
	return h, store, id, &methods
}
