package dots

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessExplicitRetryWaitedForIdleAndRanOnce(t *testing.T) {
	for _, recovered := range []bool{true, false} {
		t.Run(fmt.Sprint(recovered), func(t *testing.T) {
			h, store, id, methods := cloudHarnessFailureFixture(t)
			policy := harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}}
			h.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return policy, nil }
			h.b.deps.readHarnessInventory = func(string) (harness.Inventory, error) { return harness.Inventory{}, nil }
			base := h.b.local.input
			reject := true
			h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
				request := cloudHarnessObject(raw)
				if reject && request.method() == "thread/resume" {
					reject = false
					h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32042, "message": "private-secret"})}
					return len(raw), nil
				}
				return base.Write(raw)
			}}
			if err := h.reconcile(id, "", ""); err == nil {
				t.Fatal("fixture did not fail")
			}
			receipt, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.State != "failed" {
				t.Fatalf("fixture receipt = %#v", receipt)
			}
			baseline := cloudHarnessClone(h.threads[id].baseline)
			lease := h.threads[id].lease
			beforeOther := len(*methods)
			otherID := "00000000-0000-0000-0000-000000000002"
			otherReceipt := receipt
			otherReceipt.ChatID = otherID
			if err := store.Save(otherReceipt); err != nil {
				t.Fatal(err)
			}
			otherReceipt, err = store.Load(otherID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.RequestRetry(otherID, harness.RetryRequest{Fingerprint: otherReceipt.Fingerprint, Model: otherReceipt.Model, Cwd: otherReceipt.Cwd, FailedAt: otherReceipt.UpdatedAt}); err != nil {
				t.Fatal(err)
			}
			if err := h.reconcile(id, "", ""); err == nil || len(*methods) != beforeOther {
				t.Fatal("another chat's intent retried this failed application")
			}
			// The real command writes a request beside the existing profile, without
			// changing its effective identity or replacing the full bridge's lease.
			if _, err := store.RequestRetry(id, harness.RetryRequest{Fingerprint: receipt.Fingerprint, Model: receipt.Model, Cwd: receipt.Cwd, FailedAt: receipt.UpdatedAt}); err != nil {
				t.Fatal(err)
			}
			reject = false
			before := len(*methods)
			h.threads[id].active = true
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatalf("explicit retry did not defer: %v", err)
			}
			if len(*methods) != before {
				t.Fatal("active retry touched native subscriptions")
			}
			h.threads[id].active = false
			reject = !recovered
			err = h.reconcile(id, "", "")
			if recovered && err != nil || !recovered && err == nil {
				t.Fatalf("retry recovery=%t, err=%v", recovered, err)
			}
			after := len(*methods)
			if after <= before {
				t.Fatal("same-binding request did not retry")
			}
			receipt, err = store.Load(id)
			if err != nil || recovered && (receipt.State == "failed" || receipt.Failure != nil) || !recovered && (receipt.State != "failed" || receipt.Failure == nil) {
				t.Fatalf("retry outcome lost: %#v, %v", receipt, err)
			}
			err = h.reconcile(id, "", "")
			if recovered && err != nil || !recovered && err == nil {
				t.Fatalf("cached retry recovery=%t, err=%v", recovered, err)
			}
			if len(*methods) != after {
				t.Fatal("one request repeatedly retried")
			}
			if !reflect.DeepEqual(baseline, h.threads[id].baseline) || h.threads[id].lease != lease {
				t.Fatal("retry replaced baseline or harness ownership")
			}
			for _, method := range *methods {
				if method == "turn/interrupt" || method == "thread/archive" {
					t.Fatal("retry interrupted or archived the chat")
				}
			}
		})
	}
}

func TestCloudHarnessFailedContextSwitchRetriedAttemptedContext(t *testing.T) {
	for _, target := range []struct{ name, model, cwd string }{
		{"model", "glm-5.3-flash", "/fixture"},
		{"project", "glm-5.3", "/other"},
		{"model_and_project", "glm-5.3-flash", "/other"},
	} {
		for _, recovered := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/recovered=%t", target.name, recovered), func(t *testing.T) {
				h, store, id, methods := cloudHarnessFailureFixture(t)
				policy := harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Changes: harness.Changes{Default: harness.Enabled}}, Models: map[string]harness.Binding{"glm-5.3-flash": {Changes: harness.Changes{Default: harness.Disabled}}}}, Workspaces: map[string]harness.Scope{"/other": {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}}
				h.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return policy, nil }
				h.b.deps.readHarnessInventory = func(string) (harness.Inventory, error) { return harness.Inventory{}, nil }
				thread := h.threads[id]
				originalModel, originalCwd := thread.model, thread.cwd
				baseline := cloudHarnessClone(thread.baseline)
				base, reject := h.b.local.input, true
				h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
					request := cloudHarnessObject(raw)
					if reject && request.method() == "thread/resume" {
						reject = false
						h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32042, "message": "private-fixture"})}
						return len(raw), nil
					}
					return base.Write(raw)
				}}
				if err := h.reconcile(id, target.model, target.cwd); err == nil {
					t.Fatal("fixture did not reject context switch")
				}
				failed, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				attempt := cloudHarnessObject(cloudHarnessObject(cloudHarnessRaw(failed))["attempted_context"])
				if failed.Model != originalModel || failed.Cwd != originalCwd || thread.model != originalModel || thread.cwd != originalCwd || cloudHarnessString(attempt, "model") != target.model || cloudHarnessString(attempt, "cwd") != target.cwd || failed.Fingerprint != policy.Resolve(target.model, target.cwd, id).Fingerprint {
					t.Fatalf("failed receipt lost attempted or observed context: %#v", failed)
				}
				lease, before := thread.lease, len(*methods)
				for range 3 {
					if err := h.reconcile(id, "", ""); err == nil || len(*methods) != before {
						t.Fatal("failed context was retried or replaced without intent")
					}
				}
				if _, err := store.RequestRetry(id, harness.RetryRequest{Fingerprint: failed.Fingerprint, Model: target.model, Cwd: target.cwd, FailedAt: failed.UpdatedAt}); err != nil {
					t.Fatal(err)
				}
				thread.active = true
				if err := h.reconcile(id, "", ""); err != nil || len(*methods) != before || thread.model != originalModel || thread.cwd != originalCwd {
					t.Fatalf("active retry changed native context: %v", err)
				}
				pending, err := store.Load(id)
				if err != nil || pending.State != "pending" || pending.Model != originalModel || pending.Cwd != originalCwd || pending.RetryContext() != (harness.ApplicationContext{Model: target.model, Cwd: target.cwd}) {
					t.Fatalf("deferred retry lost attempted/current context: %#v %v", pending, err)
				}
				thread.active = false
				reject = !recovered
				if err := h.reconcile(id, "", ""); recovered && err != nil || !recovered && err == nil {
					t.Fatalf("context retry recovered=%t err=%v", recovered, err)
				}
				after := len(*methods)
				if after <= before || recovered && (thread.model != target.model || thread.cwd != target.cwd) || !recovered && (thread.model != originalModel || thread.cwd != originalCwd) {
					t.Fatal("retry did not apply the attempted context")
				}
				for range 3 {
					if err := h.reconcile(id, "", ""); recovered && err != nil || !recovered && err == nil || len(*methods) != after {
						t.Fatalf("successful context retry repeated: %v", err)
					}
				}
				outcome, err := store.Load(id)
				if err != nil || recovered && (outcome.State == "failed" || outcome.Failure != nil || outcome.Model != target.model || outcome.Cwd != target.cwd || outcome.AttemptedContext != nil) || !recovered && (outcome.State != "failed" || outcome.Failure == nil || outcome.Failure.Rollback != "restored" || outcome.Model != originalModel || outcome.Cwd != originalCwd || outcome.RetryContext() != (harness.ApplicationContext{Model: target.model, Cwd: target.cwd})) {
					t.Fatalf("context recovery was not recorded: %#v %v", outcome, err)
				}
				if !reflect.DeepEqual(thread.baseline, baseline) || thread.lease != lease {
					t.Fatal("context retry replaced baseline or ownership")
				}
				for _, method := range *methods {
					if method == "turn/interrupt" || method == "thread/archive" {
						t.Fatal("context retry interrupted or archived the chat")
					}
				}
			})
		}
	}
}
