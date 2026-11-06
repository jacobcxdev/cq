package dots

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type cloudHarnessSourceGateway struct {
	write   func(string, []byte) error
	sources func(string) []string
}

func (g cloudHarnessSourceGateway) Write(source string, raw []byte) error {
	return g.write(source, raw)
}
func (g cloudHarnessSourceGateway) ResumeParams(string, string) cloudMessage {
	return cloudMessage{"experimentalRawEvents": cloudHarnessRaw(true), "history": cloudHarnessRaw([]string{"must-not-replace-history"})}
}

func (g cloudHarnessSourceGateway) ResumeSources(id string) []string {
	if g.sources != nil {
		return g.sources(id)
	}
	return nil
}

func TestCloudHarnessSuspendedOnlyTargetSubscriptions(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{
		"target":  {owned: true, subscriptions: map[string]bool{"gateway/target": true, "stdio": true}},
		"sibling": {owned: true, subscriptions: map[string]bool{"gateway/sibling": true}},
	}, calls: make(map[string]chan cloudMessage), prefix: "cq-harness/test/"}
	var observed []string
	write := func(source string, raw []byte) error {
		message := cloudHarnessObject(raw)
		params := cloudHarnessObject(message["params"])
		if cloudHarnessString(params, "threadId") != "target" {
			t.Fatal("subscription operation escaped target chat")
		}
		observed = append(observed, source+":"+message.method())
		result := cloudMessage{"status": cloudHarnessRaw("unsubscribed")}
		if message.method() == "thread/resume" {
			if params["history"] != nil || params["path"] != nil || string(params["excludeTurns"]) != "true" || string(params["experimentalRawEvents"]) != "true" {
				t.Fatal("restore lost subscription options or transferred history")
			}
			result = cloudMessage{"thread": cloudHarnessRaw(map[string]string{"id": "target"})}
		}
		h.observeFrom(source, cloudHarnessRaw(cloudMessage{"id": message["id"], "result": cloudHarnessRaw(result)}))
		return nil
	}
	h.gateway = cloudHarnessSourceGateway{write: write}
	h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) { return len(raw), write("stdio", raw) }}}
	primary, restore, err := h.suspendSubscriptions("target")
	if err != nil || primary != "stdio" {
		t.Fatalf("primary=%s error=%v", primary, err)
	}
	if err := restore(primary); err != nil {
		t.Fatal(err)
	}
	if err := restore(primary); err != nil || !reflect.DeepEqual(observed, []string{"gateway/target:thread/unsubscribe", "stdio:thread/unsubscribe", "gateway/target:thread/resume"}) || !h.threads["sibling"].owned {
		t.Fatalf("target restoration changed sibling or repeated successful work: %v %v", observed, err)
	}
}

func TestCloudHarnessResumeCandidatesRestoredOnlyActualOwners(t *testing.T) {
	for _, absent := range []string{"notSubscribed", "notLoaded"} {
		t.Run(absent, func(t *testing.T) {
			h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"target": {id: "target", owned: true, subscriptions: map[string]bool{"stdio": true}}}, calls: make(map[string]chan cloudMessage), prefix: "cq-harness/test/"}
			var operations []string
			write := func(source string, raw []byte) error {
				message := cloudHarnessObject(raw)
				if cloudThreadID(message["params"]) != "target" {
					t.Fatal("candidate probing escaped the selected chat")
				}
				operations = append(operations, source+":"+message.method())
				var result any = map[string]string{"status": "unsubscribed"}
				if source == "gateway/absent" {
					result = map[string]string{"status": absent}
				}
				if message.method() == "thread/resume" {
					result = map[string]any{"thread": map[string]string{"id": "target"}}
				}
				h.observeFrom(source, cloudHarnessRaw(map[string]any{"id": message["id"], "result": result}))
				return nil
			}
			h.gateway = cloudHarnessSourceGateway{write: write, sources: func(id string) []string {
				if id != "target" {
					t.Fatal("candidate lookup escaped target chat")
				}
				return []string{"gateway/present", "gateway/absent", "gateway/present"}
			}}
			h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) { return len(raw), write("stdio", raw) }}}
			primary, restore, err := h.suspendSubscriptions("target")
			if err != nil || primary != "stdio" {
				t.Fatalf("candidate suspension failed: %s %v", primary, err)
			}
			if err := restore(primary); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(operations, []string{"gateway/absent:thread/unsubscribe", "gateway/present:thread/unsubscribe", "stdio:thread/unsubscribe", "gateway/present:thread/resume"}) {
				t.Fatalf("candidate ownership was guessed or known owner was lost: %v", operations)
			}
		})
	}
}

func TestCloudHarnessReleaseFailureRestoredOriginalOwners(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{
		"target": {owned: true, subscriptions: map[string]bool{"gateway/target": true, "stdio": true}},
	}, calls: make(map[string]chan cloudMessage), prefix: "cq-harness/test/"}
	var restored []string
	write := func(source string, raw []byte) error {
		message := cloudHarnessObject(raw)
		if message.method() == "thread/unsubscribe" {
			return errors.New("synthetic transport failure")
		}
		restored = append(restored, source)
		h.observeFrom(source, cloudHarnessRaw(map[string]any{"id": message["id"], "result": map[string]any{"thread": map[string]string{"id": "target"}}}))
		return nil
	}
	h.gateway = cloudHarnessSourceGateway{write: write}
	h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) { return len(raw), write("stdio", raw) }}}
	if _, _, err := h.suspendSubscriptions("target"); err == nil || !reflect.DeepEqual(restored, []string{"gateway/target", "stdio"}) {
		t.Fatalf("release failure did not restore original subscriptions: %v %v", err, restored)
	}
}

func TestCloudHarnessBackgroundOwnerDidNotAcquireDesktopSubscription(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{
		"target": {owned: true, subscriptions: map[string]bool{"gateway/target": true}},
	}, calls: make(map[string]chan cloudMessage), prefix: "cq-harness/test/"}
	count := 0
	h.gateway = cloudHarnessSourceGateway{write: func(source string, raw []byte) error {
		count++
		message := cloudHarnessObject(raw)
		h.observeFrom(source, cloudHarnessRaw(map[string]any{"id": message["id"], "result": map[string]string{"status": "unsubscribed"}}))
		return nil
	}}
	primary, restore, err := h.suspendSubscriptions("target")
	if err != nil || primary != "gateway/target" {
		t.Fatalf("background owner changed: %s %v", primary, err)
	}
	if err := restore(primary); err != nil || count != 1 {
		t.Fatalf("background-only reload acquired another native owner: %d %v", count, err)
	}
}

func TestCloudHarnessTrackedDistinctNativeSubscriptionSources(t *testing.T) {
	h := &cloudHarness{
		b:       &cloudBridge{ctx: context.Background()},
		threads: make(map[string]*cloudHarnessThread), requests: make(map[string]cloudHarnessRequest),
		calls: make(map[string]chan cloudMessage), wake: make(chan struct{}, 1),
	}
	request := func(source, id, method, thread string) {
		t.Helper()
		message := cloudMessage{"id": cloudHarnessRaw(id), "method": cloudHarnessRaw(method), "params": cloudHarnessRaw(map[string]string{"threadId": thread})}
		if _, err := h.prepareFrom(source, message, cloudHarnessRaw(message)); err != nil {
			t.Fatal(err)
		}
	}
	reply := func(source, id, thread string) {
		t.Helper()
		result := map[string]any{"status": "unsubscribed"}
		if thread != "" {
			result = map[string]any{"thread": map[string]any{"id": thread, "status": map[string]string{"type": "idle"}}}
		}
		h.observeFrom(source, cloudHarnessRaw(map[string]any{"id": id, "result": result}))
	}
	request("stdio", "same-id", "thread/resume", "first")
	request("gateway/stream", "same-id", "thread/resume", "sibling")
	reply("stdio", "same-id", "first")
	reply("gateway/stream", "same-id", "sibling")
	if h.threads["first"] == nil || h.threads["sibling"] == nil {
		t.Fatal("request IDs from independent native connections collided")
	}
	request("gateway/stream", "second", "thread/resume", "first")
	reply("gateway/stream", "second", "first")
	request("stdio", "release", "thread/unsubscribe", "first")
	reply("stdio", "release", "")
	if !h.threads["first"].owned || !h.threads["first"].subscriptions["gateway/stream"] {
		t.Fatal("releasing Desktop subscription lost background-owned chat")
	}
	request("gateway/stream", "release", "thread/unsubscribe", "first")
	reply("gateway/stream", "release", "")
	if h.threads["first"].owned || !h.threads["sibling"].owned {
		t.Fatal("release changed another chat or retained a released subscription")
	}
}

func TestCloudHarnessUnverifiedCandidateWasNotRestoredOnReleaseFailure(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"target": {id: "target", owned: true, subscriptions: map[string]bool{"stdio": true}}}, calls: map[string]chan cloudMessage{}, prefix: "cq-harness/review/"}
	candidateResumed := false
	write := func(source string, raw []byte) error {
		m := cloudHarnessObject(raw)
		if source == "gateway/candidate" && m.method() == "thread/unsubscribe" {
			return errors.New("synthetic release failure")
		}
		if source == "gateway/candidate" && m.method() == "thread/resume" {
			candidateResumed = true
		}
		h.observeFrom(source, cloudHarnessRaw(map[string]any{"id": m["id"], "result": map[string]any{"thread": map[string]string{"id": "target"}}}))
		return nil
	}
	h.gateway = cloudHarnessSourceGateway{write: write, sources: func(string) []string { return []string{"gateway/candidate"} }}
	h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) { return len(raw), write("stdio", raw) }}}
	if _, _, err := h.suspendSubscriptions("target"); err == nil {
		t.Fatal("fixture did not fail release")
	}
	if candidateResumed {
		t.Fatal("failed candidate probe caused restore to acquire an unverified subscription")
	}
}
func TestCloudHarnessAbsentCandidateWasNotChosenAsPrimary(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"target": {id: "target", owned: true, subscriptions: map[string]bool{"gateway/z-known": true}}}, calls: map[string]chan cloudMessage{}, prefix: "cq-harness/review/"}
	write := func(source string, raw []byte) error {
		m := cloudHarnessObject(raw)
		status := "unsubscribed"
		if source == "gateway/a-candidate" {
			status = "notSubscribed"
		}
		h.observeFrom(source, cloudHarnessRaw(map[string]any{"id": m["id"], "result": map[string]string{"status": status}}))
		return nil
	}
	h.gateway = cloudHarnessSourceGateway{write: write, sources: func(string) []string { return []string{"gateway/a-candidate"} }}
	primary, _, err := h.suspendSubscriptions("target")
	if err != nil {
		t.Fatal(err)
	}
	if primary != "gateway/z-known" {
		t.Fatalf("absent candidate became primary resume source: %s", primary)
	}
}
