package dots

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCloudHarnessFreshReadRespectedNewerActivity(t *testing.T) {
	for _, event := range []string{"", `{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"new"}}}`, `{"method":"thread/status/changed","params":{"threadId":"chat","status":{"type":"active"}}}`} {
		name := "current-idle"
		if event != "" {
			name = cloudHarnessObject([]byte(event)).method()
		}
		t.Run(name, func(t *testing.T) {
			changed := event != ""
			thread := &cloudHarnessThread{id: "chat", owned: true, active: true, activeTurnID: "old", refresh: true}
			h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": thread}, calls: make(map[string]chan cloudMessage), prefix: "cq-harness/activity/"}
			h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
				request := cloudHarnessObject(raw)
				if changed {
					h.observe([]byte(event))
				}
				h.observe(cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "chat", "model": "glm-5.3", "status": map[string]string{"type": "idle"}}}}))
				return len(raw), nil
			}}}
			if err := h.refresh("chat"); err != nil {
				t.Fatal(err)
			}
			if thread.active != changed || thread.refresh != changed {
				t.Fatalf("fresh read applied stale activity: active=%v refresh=%v", thread.active, thread.refresh)
			}
			if strings.Contains(event, "turn/started") && thread.activeTurnID != "new" || !changed && thread.activeTurnID != "" {
				t.Fatal("fresh read lost the current turn identity")
			}
		})
	}
}

func TestCloudHarnessFreshReadRespectedDifferentSecondaryTurn(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(map[bool]string{false: "notification", true: "reply"}[reply], func(t *testing.T) {
			thread := &cloudHarnessThread{id: "chat", owned: true, active: true, activeTurnID: "old", refresh: true, subscriptions: map[string]bool{"stdio": true, "gateway/background": true}}
			h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": thread}, calls: make(map[string]chan cloudMessage), requests: make(map[string]cloudHarnessRequest), prefix: "cq-harness/activity/"}
			h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
				request := cloudHarnessObject(raw)
				if reply {
					h.mu.Lock()
					h.requests["gateway/background\x007"] = cloudHarnessRequest{method: "turn/start", params: cloudHarnessObject([]byte(`{"threadId":"chat"}`))}
					h.mu.Unlock()
					h.observeFrom("gateway/background", []byte(`{"id":7,"result":{"turn":{"id":"new","status":"inProgress"}}}`))
				} else {
					h.observeFrom("gateway/background", []byte(`{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"new"}}}`))
				}
				h.observe(cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "chat", "model": "glm-5.3", "status": map[string]string{"type": "idle"}}}}))
				return len(raw), nil
			}}}
			if err := h.refresh("chat"); err != nil {
				t.Fatal(err)
			}
			if !thread.active || !thread.refresh || thread.activeTurnID != "old" {
				t.Fatal("older idle read cleared a different secondary turn's active guard")
			}
		})
	}
}

func TestCloudHarnessServerRequestKeptPendingClientRequest(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": {id: "chat", owned: true, pendingTurns: 1}}, requests: map[string]cloudHarnessRequest{`7`: {method: "turn/start", params: cloudHarnessObject([]byte(`{"threadId":"chat"}`))}}}
	h.observe([]byte(`{"id":7,"method":"item/commandExecution/requestApproval","params":{"threadId":"chat"}}`))
	if len(h.requests) != 1 || h.threads["chat"].pendingTurns != 1 {
		t.Fatal("server request consumed a client request with the same RPC ID")
	}
	h.observe(cloudHarnessRaw(map[string]any{"id": json.RawMessage(`7`), "result": map[string]any{"turn": map[string]string{"id": "current", "status": "inProgress"}}}))
	if len(h.requests) != 0 || h.threads["chat"].pendingTurns != 0 || !h.threads["chat"].active {
		t.Fatal("client response did not finish its pending request")
	}
}

func TestCloudHarnessGatewayTurnReplyGuardedBeforeDesktopNotification(t *testing.T) {
	source := "gateway/background"
	thread := &cloudHarnessThread{id: "chat", owned: true, subscriptions: map[string]bool{"stdio": true, source: true}, pendingTurns: 1}
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": thread}, requests: map[string]cloudHarnessRequest{source + "\x007": {method: "turn/start", params: cloudHarnessObject([]byte(`{"threadId":"chat"}`))}}}
	h.observeFrom(source, []byte(`{"id":7,"result":{"turn":{"id":"current","status":"inProgress"},"model":"glm-5.3"}}`))
	if thread.pendingTurns != 0 || !thread.active || thread.activeTurnID != "current" || !thread.refresh {
		t.Fatal("gateway acknowledgement left a reload window before the desktop notification")
	}
}

func TestCloudHarnessGatewayActivityGuardedOpaqueTurns(t *testing.T) {
	for _, event := range []string{`{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"current"}}}`, `{"method":"thread/status/changed","params":{"threadId":"chat","status":{"type":"active"}}}`} {
		t.Run(cloudHarnessObject([]byte(event)).method(), func(t *testing.T) {
			thread := &cloudHarnessThread{id: "chat", owned: true, subscriptions: map[string]bool{"stdio": true, "gateway/background": true}}
			h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": thread}}
			h.observeFrom("gateway/background", []byte(event))
			if !thread.active || !thread.refresh {
				t.Fatal("secondary activity left a reload window for an opaque turn request")
			}
			h.observeFrom("gateway/background", []byte(`{"method":"turn/completed","params":{"threadId":"chat","turn":{"id":"old"}}}`))
			if !thread.active {
				t.Fatal("secondary completion cleared the conservative active guard")
			}
		})
	}
}

func TestCloudHarnessSecondaryIdleRefreshedActiveSnapshot(t *testing.T) {
	for _, event := range []string{
		`{"method":"turn/completed","params":{"threadId":"chat","turn":{"id":"current"}}}`,
		`{"method":"thread/status/changed","params":{"threadId":"chat","status":{"type":"idle"}}}`,
	} {
		for _, status := range []string{"idle", "active"} {
			t.Run(cloudHarnessObject([]byte(event)).method()+"/"+status, func(t *testing.T) {
				source := "gateway/background"
				thread := &cloudHarnessThread{id: "chat", owned: true, subscriptions: map[string]bool{"stdio": true, source: true}}
				h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": thread}, calls: make(map[string]chan cloudMessage), prefix: "cq-harness/activity/"}
				nativeStatus, reads := "active", 0
				h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
					reads++
					request := cloudHarnessObject(raw)
					h.observe(cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "chat", "model": "gpt-6.1-sol", "status": map[string]string{"type": nativeStatus}}}}))
					return len(raw), nil
				}}}
				h.observeFrom(source, []byte(`{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"current"}}}`))
				if err := h.refresh("chat"); err != nil {
					t.Fatal(err)
				}
				if !thread.active || thread.refresh {
					t.Fatal("fixture did not establish a refreshed active snapshot")
				}
				nativeStatus = status
				h.observeFrom(source, []byte(event))
				if !thread.active {
					t.Fatal("secondary idle event cleared the guard before authoritative readback")
				}
				if err := h.refresh("chat"); err != nil {
					t.Fatal(err)
				}
				if reads != 2 || thread.active != (status == "active") || thread.refresh {
					t.Fatalf("secondary idle event stranded or cleared the guard: reads=%d active=%v refresh=%v", reads, thread.active, thread.refresh)
				}
			})
		}
	}
}

func TestCloudHarnessSecondaryIdleInvalidatedInFlightActiveRead(t *testing.T) {
	for _, event := range []string{
		`{"method":"turn/completed","params":{"threadId":"chat","turn":{"id":"current"}}}`,
		`{"method":"thread/status/changed","params":{"threadId":"chat","status":{"type":"idle"}}}`,
	} {
		t.Run(cloudHarnessObject([]byte(event)).method(), func(t *testing.T) {
			thread := &cloudHarnessThread{id: "chat", owned: true, active: true, activeTurnID: "current", refresh: true, subscriptions: map[string]bool{"stdio": true, "gateway/background": true}}
			h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": thread}, calls: make(map[string]chan cloudMessage), prefix: "cq-harness/activity/"}
			reads := 0
			h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
				reads++
				status := "idle"
				if reads == 1 {
					status = "active"
					h.observeFrom("gateway/background", []byte(event))
				}
				request := cloudHarnessObject(raw)
				h.observe(cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "chat", "model": "gpt-6.1-sol", "status": map[string]string{"type": status}}}}))
				return len(raw), nil
			}}}
			if err := h.refresh("chat"); err != nil {
				t.Fatal(err)
			}
			if !thread.active || !thread.refresh {
				t.Fatal("in-flight active read consumed a newer secondary idle event")
			}
			if err := h.refresh("chat"); err != nil {
				t.Fatal(err)
			}
			if reads != 2 || thread.active || thread.refresh {
				t.Fatal("fresh native idle read did not release the active guard")
			}
		})
	}
}

func TestCloudHarnessDelayedGatewayCompletionKeptCurrentDesktopTurn(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": {id: "chat", owned: true, baseline: cloudMessage{}}}, requests: map[string]cloudHarnessRequest{}, calls: map[string]chan cloudMessage{}, prefix: "cq-harness/review/", wake: make(chan struct{}, 1)}
	g := newCloudHarnessGateway(h)
	s := &cloudHarnessGatewaySession{}
	completed := []byte(`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":1,"message":{"method":"turn/completed","params":{"threadId":"chat","turn":{"id":"old-turn"}}}}`)
	if _, err := g.message(s, false, completed); err != nil {
		t.Fatal(err)
	}
	h.observe([]byte(`{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"current-turn"}}}`))
	if !h.threads["chat"].active {
		t.Fatal("fixture did not start current turn")
	}
	completed = []byte(strings.ReplaceAll(string(completed), `"seq_id":1`, `"seq_id":2`))
	if _, err := g.message(s, false, completed); err != nil {
		t.Fatal(err)
	}
	if !h.threads["chat"].active {
		t.Fatal("delayed old completion cleared the current turn's active guard")
	}
}

func TestCloudHarnessDelayedGatewayCompletionMatchedActiveTurn(t *testing.T) {
	h := &cloudHarness{b: &cloudBridge{ctx: context.Background()}, threads: map[string]*cloudHarnessThread{"chat": {id: "chat", owned: true, subscriptions: map[string]bool{cloudHarnessGatewaySource("background", "stream"): true}, baseline: cloudMessage{}}}, requests: map[string]cloudHarnessRequest{}, calls: map[string]chan cloudMessage{}, prefix: "cq-harness/review/", wake: make(chan struct{}, 1)}
	g := newCloudHarnessGateway(h)
	s := &cloudHarnessGatewaySession{}
	completed := []byte(`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":1,"message":{"method":"turn/completed","params":{"threadId":"chat","turn":{"id":"old-turn"}}}}`)
	if _, err := g.message(s, false, completed); err != nil {
		t.Fatal(err)
	}
	h.observeFrom(cloudHarnessGatewaySource("background", "stream"), []byte(`{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"current-turn"}}}`))
	if !h.threads["chat"].active {
		t.Fatal("fixture did not start current turn")
	}
	completed = []byte(strings.ReplaceAll(string(completed), `"seq_id":1`, `"seq_id":2`))
	if _, err := g.message(s, false, completed); err != nil {
		t.Fatal(err)
	}
	if !h.threads["chat"].active {
		t.Fatal("delayed old completion cleared the current turn's active guard")
	}
}
