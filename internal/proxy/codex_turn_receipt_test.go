package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexTurnReceiptStoreLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x42}, 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	receipt := testCodexTurnReceipt()
	handle := store.register([]byte("session-secret"), []byte("turn-secret"), receipt)
	if handle == nil {
		t.Fatal("register returned nil")
	}

	got, found := store.lookup([]byte("session-secret"), []byte("turn-secret"))
	if !found || got != receipt {
		t.Fatalf("lookup = (%+v, %v), want (%+v, true)", got, found, receipt)
	}
	if encoded, marshalErr := json.Marshal(got); marshalErr != nil {
		t.Fatal(marshalErr)
	} else if bytes.Contains(encoded, []byte("session-secret")) || bytes.Contains(encoded, []byte("turn-secret")) {
		t.Fatalf("receipt leaked raw identity: %s", encoded)
	}

	handle.attempt(codex.AccountKey("account-b"))
	got, found = store.lookup([]byte("session-secret"), []byte("turn-secret"))
	if !found || got.State != CodexTurnReceiptAttempted || got.ActualAccountHint != redactedAccountHint("codex", "account-b") {
		t.Fatalf("attempt receipt = (%+v, %v)", got, found)
	}
	handle.terminal(CodexTurnReceiptCompleted)
	got, found = store.lookup([]byte("session-secret"), []byte("turn-secret"))
	if !found || got.State != CodexTurnReceiptCompleted {
		t.Fatalf("terminal receipt = (%+v, %v)", got, found)
	}

	// Terminal state is monotonic.
	handle.attempt(codex.AccountKey("account-c"))
	handle.terminal(CodexTurnReceiptFailed)
	got, _ = store.lookup([]byte("session-secret"), []byte("turn-secret"))
	if got.State != CodexTurnReceiptCompleted || got.ActualAccountHint != redactedAccountHint("codex", "account-b") {
		t.Fatalf("terminal receipt regressed: %+v", got)
	}

	now = now.Add(codexTurnReceiptTerminalTTL)
	if _, found := store.lookup([]byte("session-secret"), []byte("turn-secret")); found {
		t.Fatal("terminal receipt survived expiry")
	}
}

func TestCodexTurnReceiptStoreBoundedAndConcurrent(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x24}, 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= codexTurnReceiptMaxEntries; index++ {
		session := []byte("session-" + strings.Repeat("x", index%7))
		turn := []byte(strings.Repeat("0", 8-len(string(rune(index%10)))-1) + string(rune('0'+index%10)) + "-" + strings.Repeat("y", index/10))
		if store.register(session, turn, testCodexTurnReceipt()) == nil {
			t.Fatalf("register %d returned nil", index)
		}
		now = now.Add(time.Nanosecond)
	}
	store.mu.RLock()
	entryCount := len(store.entries)
	store.mu.RUnlock()
	if entryCount != codexTurnReceiptMaxEntries {
		t.Fatalf("entry count = %d, want %d", entryCount, codexTurnReceiptMaxEntries)
	}

	handle := store.register([]byte("concurrent-session"), []byte("concurrent-turn"), testCodexTurnReceipt())
	if handle == nil {
		t.Fatal("concurrent handle unavailable")
	}
	var group sync.WaitGroup
	for index := 0; index < 32; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			handle.attempt(codex.AccountKey("account-b"))
			_, _ = store.lookup([]byte("concurrent-session"), []byte("concurrent-turn"))
		}()
	}
	group.Wait()
	got, found := store.lookup([]byte("concurrent-session"), []byte("concurrent-turn"))
	if !found || got.State != CodexTurnReceiptAttempted {
		t.Fatalf("concurrent receipt = (%+v, %v)", got, found)
	}
}

func TestCodexTurnReceiptStoreRejectsInvalidIdentityAndRandomness(t *testing.T) {
	if _, err := NewCodexTurnReceiptStore(bytes.NewReader([]byte("short")), time.Now); err == nil {
		t.Fatal("short random key accepted")
	}
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x11}, 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][]byte{nil, []byte("bad\nvalue"), bytes.Repeat([]byte("x"), canonicalSessionIDMaxBytes+1)} {
		if handle := store.register(identity, []byte("turn"), testCodexTurnReceipt()); handle != nil {
			t.Fatalf("invalid session accepted: %q", identity)
		}
		if handle := store.register([]byte("session"), identity, testCodexTurnReceipt()); handle != nil {
			t.Fatalf("invalid turn accepted: %q", identity)
		}
	}
}

func TestCodexTurnReceiptStoreRejectsInvalidShadowComparison(t *testing.T) {
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x31}, 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		comparison CodexTurnReceiptShadowComparison
		hint       string
	}{
		{name: "missing"},
		{name: "unknown", comparison: "unknown"},
		{name: "alternative without hint", comparison: CodexTurnReceiptShadowAlternativeAccount},
		{name: "same with hint", comparison: CodexTurnReceiptShadowSameAccount, hint: redactedAccountHint("codex", "account-b")},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := testCodexTurnReceipt()
			receipt.ShadowComparison = test.comparison
			receipt.ShadowAlternativeAccountHint = test.hint
			if handle := store.register([]byte("session"), []byte("turn-"+test.name), receipt); handle != nil {
				t.Fatalf("invalid shadow comparison accepted: %+v", receipt)
			}
		})
	}

	receipt := testCodexTurnReceipt()
	receipt.ShadowComparison = CodexTurnReceiptShadowAlternativeAccount
	receipt.ShadowAlternativeAccountHint = redactedAccountHint("codex", "account-b")
	if handle := store.register([]byte("session"), []byte("valid-alternative"), receipt); handle == nil {
		t.Fatal("valid alternative shadow comparison rejected")
	}
}

func TestCodexTurnReceiptV1JSONContractExcludesShadowAdvice(t *testing.T) {
	receipt := testCodexTurnReceipt().CodexTurnReceiptV1
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("shadow_")) {
		t.Fatalf("V1 receipt contains V2 shadow fields: %s", encoded)
	}
}

func TestCodexTurnReceiptHTTPLifecycleTracksActualAndTerminalEvidence(t *testing.T) {
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x12}, 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	handle := store.register([]byte("session"), []byte("turn"), testCodexTurnReceipt())
	lifecycle := wrapCodexTurnReceiptLifecycle(&codexTurnReceiptLifecycleStub{account: "account-b"}, handle)
	lifecycle, err = lifecycle.MarkDispatchedContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := store.lookup([]byte("session"), []byte("turn"))
	if receipt.State != CodexTurnReceiptAttempted || receipt.ActualAccountHint != redactedAccountHint("codex", "account-b") {
		t.Fatalf("attempt receipt = %+v", receipt)
	}
	lifecycle, err = lifecycle.ProviderCompleted(CodexHTTPCompletionEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ = store.lookup([]byte("session"), []byte("turn"))
	if receipt.State != CodexTurnReceiptCompleted {
		t.Fatalf("completed receipt = %+v", receipt)
	}

	for _, test := range []struct {
		name  string
		apply func(CodexHTTPRequestLifecycle) (CodexHTTPRequestLifecycle, error)
		want  CodexTurnReceiptState
	}{
		{name: "abandoned", apply: func(value CodexHTTPRequestLifecycle) (CodexHTTPRequestLifecycle, error) {
			return value.AbandonBeforeDispatchContext(context.Background())
		}, want: CodexTurnReceiptRejected},
		{name: "rejected", apply: func(value CodexHTTPRequestLifecycle) (CodexHTTPRequestLifecycle, error) {
			return value.FinishRejected()
		}, want: CodexTurnReceiptRejected},
		{name: "failed", apply: func(value CodexHTTPRequestLifecycle) (CodexHTTPRequestLifecycle, error) {
			return value.ProviderFailed(CodexHTTPResponseEvidence{})
		}, want: CodexTurnReceiptFailed},
		{name: "indeterminate", apply: func(value CodexHTTPRequestLifecycle) (CodexHTTPRequestLifecycle, error) {
			return value.IndeterminateContext(context.Background(), CodexHTTPResponseEvidence{})
		}, want: CodexTurnReceiptIndeterminate},
	} {
		t.Run(test.name, func(t *testing.T) {
			turn := []byte("turn-" + test.name)
			testHandle := store.register([]byte("session"), turn, testCodexTurnReceipt())
			wrapped := wrapCodexTurnReceiptLifecycle(&codexTurnReceiptLifecycleStub{account: "account-a"}, testHandle)
			if _, err := test.apply(wrapped); err != nil {
				t.Fatal(err)
			}
			got, found := store.lookup([]byte("session"), turn)
			if !found || got.State != test.want {
				t.Fatalf("receipt = (%+v, %v), want %q", got, found, test.want)
			}
		})
	}
}

type codexTurnReceiptLifecycleStub struct {
	account codex.AccountKey
}

func (lifecycle *codexTurnReceiptLifecycleStub) EverAdmitted() bool { return false }
func (lifecycle *codexTurnReceiptLifecycleStub) AccountKey() codex.AccountKey {
	return lifecycle.account
}
func (lifecycle *codexTurnReceiptLifecycleStub) MarkDispatchedContext(context.Context) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) RejectAndPrepareContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) RecordAccountUnavailableContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) RecordQuotaExhaustedContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) CompleteAccountUnavailableCycleContext(context.Context) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) AbandonBeforeDispatchContext(context.Context) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) FinishRejected() (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) IndeterminateContext(context.Context, CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) Drain() (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) AdmitHTTP2xxContext(context.Context, CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) ProviderCompleted(CodexHTTPCompletionEvidence) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}
func (lifecycle *codexTurnReceiptLifecycleStub) ProviderFailed(CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	return lifecycle, nil
}

func testCodexTurnReceipt() CodexTurnReceiptV2 {
	return CodexTurnReceiptV2{
		CodexTurnReceiptV1: CodexTurnReceiptV1{
			State:                    CodexTurnReceiptPlanned,
			Transport:                CodexTurnReceiptTransportHTTP,
			RequestKind:              "turn",
			RequestLineage:           codexRequestLineagePreviousResponseIDAbsent,
			RequestedModelClass:      codexRequestedModelClassSol,
			RequestedReasoningEffort: "high",
			CompactionPhase:          "not_applicable",
			Pool:                     "protected",
			RouteReason:              CodexTurnReceiptRouteAffinityReuse,
			PlannedAccountHint:       redactedAccountHint("codex", "account-a"),
		},
		ShadowComparison: CodexTurnReceiptShadowNotApplicable,
	}
}
