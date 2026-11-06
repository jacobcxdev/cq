package dots

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/harness"
)

func cloudHarnessFeedbackFixture(t *testing.T) (*cloudHarness, *int) {
	t.Helper()
	policy := harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}
	selection := policy.Resolve("gpt-5.5", "/fixture", "chat")
	h := &cloudHarness{
		b: &cloudBridge{ctx: context.Background()}, threads: make(map[string]*cloudHarnessThread),
		requests: make(map[string]cloudHarnessRequest), calls: make(map[string]chan cloudMessage),
		prefix: "cq-harness/feedback/", wake: make(chan struct{}, 1),
	}
	h.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return policy, nil }
	h.threads["chat"] = &cloudHarnessThread{
		id: "chat", model: "gpt-5.5", cwd: "/fixture", owned: true,
		subscriptions: map[string]bool{"stdio": true}, baseline: cloudMessage{},
		profileManaged: true, profilePlan: &cloudHarnessProfilePlan{key: "same-plan", plugins: []string{}},
		profileFingerprint: selection.Fingerprint, selection: selection, receiptState: "partial",
		runtimeIssues: []string{"native_mcp_runtime_retained:first"},
	}
	writes := 0
	h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		writes++
		return 0, errors.New("synthetic native write failure")
	}}}
	return h, &writes
}

func TestCloudHarnessSourceFeedbackRetriedRetainedRuntime(t *testing.T) {
	h, writes := cloudHarnessFeedbackFixture(t)
	g := newCloudHarnessGateway(h)
	s := &cloudHarnessGatewaySession{}
	request := []byte(`{"type":"client_message","client_id":"background","stream_id":"stream","message":{"id":"resume-late","method":"thread/resume","params":{"threadId":"chat"}}}`)
	if _, err := g.message(s, true, request); err != nil {
		t.Fatal(err)
	}
	reply := []byte(`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":1,"message":{"id":"resume-late","result":{"thread":{"id":"chat"},"disabledPluginIds":[]}}}`)
	if _, err := g.message(s, false, reply); err != nil {
		t.Fatal(err)
	}
	if len(h.threads["chat"].subscriptions) != 2 {
		t.Fatal("fixture did not learn the additional owner")
	}
	_ = h.reconcile("chat", "", "")
	if *writes == 0 {
		t.Fatal("late owner was learned, but identical-profile fast path prevented a retained-runtime retry")
	}
}

func TestCloudHarnessUnchangedPartialRuntimeDidNotRetry(t *testing.T) {
	h, writes := cloudHarnessFeedbackFixture(t)
	for range 2 {
		if err := h.reconcile("chat", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if *writes != 0 {
		t.Fatal("unchanged partial runtime repeatedly retried native assembly")
	}
}

func TestCloudHarnessRecoveredSourceRetriedFailureOnce(t *testing.T) {
	h, writes := cloudHarnessFeedbackFixture(t)
	thread := h.threads["chat"]
	cloudHarnessSubscription(thread, "gateway/known", true)
	_ = h.reconcile("chat", "", "")
	failedWrites := *writes
	if failedWrites == 0 || thread.failureKey == "" {
		t.Fatal("fixture did not retain a failed attempt")
	}
	_ = h.reconcile("chat", "", "")
	if *writes != failedWrites {
		t.Fatal("unchanged failed source repeatedly retried native assembly")
	}
	h.gatewayRecovered("gateway/unknown")
	_ = h.reconcile("chat", "", "")
	if *writes != failedWrites || thread.subscriptions["gateway/unknown"] {
		t.Fatal("unknown recovered writer invented ownership or retried the target")
	}
	h.gatewayRecovered("gateway/known")
	_ = h.reconcile("chat", "", "")
	if *writes <= failedWrites {
		t.Fatal("recovered known writer remained blocked by the previous failure key")
	}
	recoveredWrites := *writes
	_ = h.reconcile("chat", "", "")
	if *writes != recoveredWrites {
		t.Fatal("one recovered writer caused repeated failed attempts")
	}
}

func TestCloudHarnessSourceFeedbackDidNotRetryConfirmedRuntime(t *testing.T) {
	h, writes := cloudHarnessFeedbackFixture(t)
	thread := h.threads["chat"]
	thread.runtimeIssues = nil
	thread.receiptState = "applied"
	cloudHarnessSubscription(thread, "gateway/known", true)
	h.gatewayRecovered("gateway/known")
	if err := h.reconcile("chat", "", ""); err != nil {
		t.Fatal(err)
	}
	if *writes != 0 {
		t.Fatal("new source unnecessarily reloaded confirmed runtime")
	}
}

func TestCloudHarnessSourceFeedbackRetriedLegacyRuntime(t *testing.T) {
	h, writes := cloudHarnessFeedbackFixture(t)
	thread := h.threads["chat"]
	thread.profileManaged, thread.profilePlan = false, nil
	thread.minimal = true
	thread.model = "glm-5.3"
	h.b.deps.readHarnessPolicy = func() (harness.Policy, error) {
		return harness.Policy{Version: 1, Enabled: true}, nil
	}
	cloudHarnessSubscription(thread, "gateway/known", true)
	_ = h.reconcile("chat", "", "")
	if *writes == 0 {
		t.Fatal("legacy minimal fast path prevented a retained-runtime retry")
	}
}

func TestCloudHarnessSourceFeedbackTrackedOnlyMembershipChanges(t *testing.T) {
	h, writes := cloudHarnessFeedbackFixture(t)
	thread := h.threads["chat"]
	cloudHarnessSubscription(thread, "gateway/known", true)
	thread.lastAttemptEpoch = thread.sourceEpoch
	cloudHarnessSubscription(thread, "gateway/known", true)
	cloudHarnessSubscription(thread, "gateway/unknown", false)
	if err := h.reconcile("chat", "", ""); err != nil {
		t.Fatal(err)
	}
	if *writes != 0 {
		t.Fatal("unchanged membership retried native assembly")
	}
	cloudHarnessSubscription(thread, "gateway/known", false)
	_ = h.reconcile("chat", "", "")
	if *writes == 0 || !thread.subscriptions["stdio"] {
		t.Fatal("departed owner did not retry retained runtime through its remaining owner")
	}
}

func TestCloudHarnessSourceArrivingDuringAttemptWasNotConsumed(t *testing.T) {
	h, _ := cloudHarnessFeedbackFixture(t)
	thread := h.threads["chat"]
	thread.profilePlan.config = cloudMessage{"mcp_servers.first.enabled": cloudHarnessRaw(false)}
	cloudHarnessSubscription(thread, "gateway/first", true)
	late := false
	unsubscribes := 0
	write := func(source string, raw []byte) error {
		request := cloudHarnessObject(raw)
		var result any = map[string]any{}
		switch request.method() {
		case "mcpServerStatus/list":
			result = map[string]any{"data": []any{map[string]any{"name": "first", "tools": map[string]any{"probe": map[string]any{}}, "resources": []any{}, "resourceTemplates": []any{}, "runtimeStatus": "connected"}}, "nextCursor": nil}
		case "experimentalFeature/list":
			result = map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": true}}, "nextCursor": nil}
		case "thread/unsubscribe":
			unsubscribes++
			if !late {
				late = true
				h.mu.Lock()
				cloudHarnessSubscription(thread, "gateway/late", true)
				h.mu.Unlock()
			}
			result = map[string]any{"status": "unsubscribed"}
		case "thread/resume":
			result = map[string]any{"thread": map[string]string{"id": "chat"}}
		}
		h.observeFrom(source, cloudHarnessRaw(map[string]any{"id": request["id"], "result": result}))
		return nil
	}
	h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) { return len(raw), write("stdio", raw) }}}
	h.gateway = cloudHarnessSourceGateway{write: write}
	if err := h.reconcile("chat", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(thread.runtimeIssues) == 0 || !strings.HasPrefix(thread.runtimeIssues[0], "native_mcp_runtime_retained") || thread.sourceEpoch == thread.lastAttemptEpoch {
		t.Fatal("partial verification consumed a source that arrived during the attempt")
	}
	firstUnsubscribes := unsubscribes
	if err := h.reconcile("chat", "", ""); err != nil {
		t.Fatal(err)
	}
	if unsubscribes <= firstUnsubscribes {
		t.Fatal("source arriving during the attempt did not trigger a later retry")
	}
	secondUnsubscribes := unsubscribes
	if err := h.reconcile("chat", "", ""); err != nil {
		t.Fatal(err)
	}
	if unsubscribes != secondUnsubscribes {
		t.Fatal("unchanged partial verification retried after consuming the source change")
	}
}
