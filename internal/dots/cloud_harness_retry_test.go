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
