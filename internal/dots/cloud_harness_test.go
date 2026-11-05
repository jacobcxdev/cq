package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

type cloudHarnessFixture struct {
	*cloudTestHarness
	mu          sync.Mutex
	policy      harness.Policy
	models      map[string]string
	plugins     map[string][]string
	globalModel string
	next        int
	onRequest   func(cloudMessage) (any, error, bool)
}

func newCloudHarnessFixture(t *testing.T) *cloudHarnessFixture {
	f := &cloudHarnessFixture{cloudTestHarness: newCloudTestHarness(t), policy: harness.Policy{Version: 1, Enabled: true}, models: make(map[string]string), plugins: make(map[string][]string), globalModel: "glm-5.3"}
	f.deps.readHarnessPolicy = func() (harness.Policy, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.policy, nil }
	f.deps.harnessTimeout = 100 * time.Millisecond
	f.deps.launch = func(_ context.Context, _ string, _ []string, _ []string, _ io.Writer) (*cloudLocal, error) {
		reader, writer := io.Pipe()
		f.localOutput = writer
		var once sync.Once
		return &cloudLocal{output: reader, input: cloudTestWriter{write: func(raw []byte) (int, error) {
			f.localMu.Lock()
			f.localRaw = append(f.localRaw, bytes.Clone(raw))
			f.localMu.Unlock()
			var request cloudMessage
			if err := json.Unmarshal(raw, &request); err != nil {
				return 0, err
			}
			if len(request["id"]) == 0 || request.method() == "" {
				return len(raw), nil
			}
			f.mu.Lock()
			var result any = map[string]any{}
			var responseErr error
			handled := false
			if f.onRequest != nil {
				result, responseErr, handled = f.onRequest(request)
			}
			if !handled {
				var params map[string]any
				_ = json.Unmarshal(request["params"], &params)
				switch request.method() {
				case "config/read":
					result = map[string]any{"config": map[string]any{"model": f.globalModel, "features": map[string]any{"plugins": true}, "mcp_servers": map[string]any{"first": map[string]any{"enabled": true}}}, "layers": []any{map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"second": map[string]any{"command": "fixture"}}}}}}
				case "thread/start":
					f.next++
					id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.next)
					model, _ := params["model"].(string)
					if model == "" {
						var config struct{ Model string }
						_ = json.Unmarshal(cloudHarnessRaw(params["config"]), &config)
						model = config.Model
						if model == "" {
							model = f.globalModel
						}
					}
					f.models[id] = model
					var plugins []string
					_ = json.Unmarshal(cloudHarnessRaw(params["disabledPluginIds"]), &plugins)
					f.plugins[id] = plugins
					result = map[string]any{"thread": map[string]any{"id": id, "cwd": params["cwd"]}, "model": model, "disabledPluginIds": f.plugins[id]}
				case "thread/resume":
					id, _ := params["threadId"].(string)
					model, _ := params["model"].(string)
					if model == "" {
						model = f.models[id]
					}
					f.models[id] = model
					result = map[string]any{"thread": map[string]any{"id": id, "cwd": params["cwd"]}, "model": model, "disabledPluginIds": f.plugins[id]}
				case "thread/settings/update":
					id, _ := params["threadId"].(string)
					var plugins []string
					_ = json.Unmarshal(cloudHarnessRaw(params["disabledPluginIds"]), &plugins)
					f.plugins[id] = plugins
				case "thread/read":
					id, _ := params["threadId"].(string)
					result = map[string]any{"thread": map[string]any{"id": id, "model": f.models[id], "cwd": "/project"}}
				case "thread/unsubscribe":
					result = map[string]any{"status": "unsubscribed"}
				case "turn/start":
					id, _ := params["threadId"].(string)
					if model, ok := params["model"].(string); ok {
						f.models[id] = model
					}
					var mode struct{ Settings struct{ Model string } }
					_ = json.Unmarshal(cloudHarnessRaw(params["collaborationMode"]), &mode)
					if mode.Settings.Model != "" {
						f.models[id] = mode.Settings.Model
					}
					result = map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}, "model": f.models[id]}
				}
			}
			f.mu.Unlock()
			if handled && result == nil && responseErr == nil {
				return len(raw), nil
			}
			response := map[string]any{"id": request["id"], "result": result}
			if responseErr != nil {
				response = map[string]any{"id": request["id"], "error": map[string]any{"code": -32000, "message": "synthetic rejection"}}
			}
			encoded, _ := json.Marshal(response)
			if _, err := writer.Write(append(encoded, '\n')); err != nil {
				return 0, err
			}
			return len(raw), nil
		}}, wait: func() error { return <-f.localExit }, stop: func() { once.Do(func() { _ = reader.Close(); _ = writer.Close(); f.localExit <- nil }) }}, nil
	}
	return f
}

func (f *cloudHarnessFixture) request(id any, method string, params any) cloudMessage {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if _, err := f.input.Write(append(raw, '\n')); err != nil {
		f.t.Fatal(err)
	}
	return f.await(id)
}

func (f *cloudHarnessFixture) messages(method string) []cloudMessage {
	var found []cloudMessage
	for _, raw := range f.localMessages() {
		var message cloudMessage
		_ = json.Unmarshal(raw, &message)
		if message.method() == method {
			found = append(found, message)
		}
	}
	return found
}

func TestCloudHarnessNewGLMUsesCompleteMinimalConfig(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.start()
	f.initialize()
	response := f.request(2, "thread/start", map[string]any{"model": "glm-5.3", "cwd": "/work", "unknown": map[string]any{"retained": true}, "config": map[string]any{"other.setting": 17, "mcp_servers.third.command": "fixture"}})
	if len(response["error"]) != 0 {
		t.Fatal("GLM start failed")
	}
	requests := f.messages("thread/start")
	if len(requests) != 1 {
		t.Fatal("missing thread start")
	}
	var params struct {
		Config  map[string]any
		Unknown map[string]any
	}
	_ = json.Unmarshal(requests[0]["params"], &params)
	for _, key := range []string{"skills.include_instructions", "features.plugins", "mcp_servers.first.enabled", "mcp_servers.second.enabled", "mcp_servers.third.enabled"} {
		if value, ok := params.Config[key]; !ok || value != false {
			t.Errorf("minimal config %s = %v, present %t", key, value, ok)
		}
	}
	if params.Config["other.setting"] != float64(17) || params.Unknown["retained"] != true {
		t.Fatal("unknown settings lost")
	}
	reads := f.messages("config/read")
	if len(reads) != 1 || !strings.Contains(string(reads[0]["params"]), `"includeLayers":true`) {
		t.Fatal("minimal must enumerate native config layers")
	}
}

func TestCloudHarnessDisabledAndOtherModelsPreserveBytes(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		enabled     bool
	}{{"disabled", "glm-5.3", false}, {"other", "gpt-5.5", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCloudHarnessFixture(t)
			f.policy.Enabled = tc.enabled
			f.start()
			f.initialize()
			raw := []byte(`{ "id": 2, "method": "thread/start", "params": { "model": "` + tc.model + `", "cwd": "/work", "config": {"other":7} } }` + "\n")
			if _, err := f.input.Write(raw); err != nil {
				t.Fatal(err)
			}
			f.await(2)
			messages := f.localMessages()
			if !bytes.Equal(messages[len(messages)-1], raw) {
				t.Fatal("unmanaged bytes changed")
			}
			if len(f.messages("config/read")) != 0 {
				t.Fatal("unmanaged request caused internal RPC")
			}
		})
	}
}

func (f *cloudHarnessFixture) setPolicy(policy harness.Policy) {
	f.mu.Lock()
	f.policy = policy
	f.mu.Unlock()
}

func (f *cloudHarnessFixture) startThread(model, cwd string) string {
	f.t.Helper()
	response := f.request(time.Now().UnixNano(), "thread/start", map[string]any{"model": model, "cwd": cwd, "config": map[string]any{"user.option": "retained"}, "disabledPluginIds": []string{"user-excluded@market"}})
	id := cloudThreadID(response["result"])
	if id == "" {
		f.t.Fatal("start did not return thread")
	}
	return id
}

func (f *cloudHarnessFixture) waitEvent(threadID, state, profile string) map[string]any {
	f.t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		f.eventsMu.Lock()
		for _, event := range f.events {
			if event["thread_id"] == threadID && event["scope"] == "full" && event["state"] == state && event["harness_profile"] == profile {
				f.eventsMu.Unlock()
				return event
			}
		}
		f.eventsMu.Unlock()
		select {
		case <-deadline.C:
			f.t.Fatalf("missing %s %s receipt for %s", state, profile, threadID)
			return nil
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCloudHarnessLoadedPolicyReloadAndModelReturnRestore(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.start()
	f.initialize()
	first := f.startThread("glm-5.3", "/project")
	sibling := f.startThread("glm-5.3", "/project")
	f.setPolicy(harness.Policy{Version: 1, Enabled: true, Chats: map[string]harness.Profile{sibling: harness.Full}})
	f.request(20, "turn/start", map[string]any{"threadId": first})
	f.waitEvent(first, "applied", "minimal")
	resumes := f.messages("thread/resume")
	if len(resumes) != 1 {
		t.Fatalf("got %d reloads", len(resumes))
	}
	if cloudThreadID(resumes[0]["params"]) != first {
		t.Fatal("reload changed chat identity")
	}
	if len(f.messages("thread/unsubscribe")) != 1 {
		t.Fatal("loaded config was changed without releasing its subscription")
	}
	f.request(21, "turn/start", map[string]any{"threadId": sibling})
	if len(f.messages("thread/resume")) != 1 {
		t.Fatal("full sibling was reloaded")
	}
	// The completed notification is an idle boundary; no turn is interrupted.
	_, _ = f.localOutput.Write([]byte(`{"method":"turn/completed","params":{"threadId":"` + first + `","turn":{"id":"turn","status":"completed"}}}` + "\n"))
	f.request(22, "turn/start", map[string]any{"threadId": first, "model": "gpt-5.5", "unknown": "retained"})
	f.waitEvent(first, "applied", "full")
	resumes = f.messages("thread/resume")
	if len(resumes) != 2 {
		t.Fatalf("got %d reloads after model return", len(resumes))
	}
	var restored struct {
		Config            map[string]any
		DisabledPluginIDs []string
		Model             string
	}
	_ = json.Unmarshal(resumes[1]["params"], &restored)
	if restored.Model != "gpt-5.5" || restored.Config["user.option"] != "retained" || restored.Config["skills.include_instructions"] != true || restored.Config["features.plugins"] != true || len(restored.DisabledPluginIDs) != 1 || restored.DisabledPluginIDs[0] != "user-excluded@market" {
		t.Fatalf("original thread configuration not restored: %s", resumes[1]["params"])
	}
	if !strings.Contains(string(f.messages("turn/start")[2]["params"]), `"unknown":"retained"`) {
		t.Fatal("model return lost application params")
	}
}

func TestCloudHarnessActiveTurnDefersReloadUntilCompletion(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.request(20, "turn/start", map[string]any{"threadId": id})
	f.setPolicy(harness.Policy{Version: 1, Enabled: true})
	_, _ = f.localOutput.Write([]byte(`{"method":"thread/status/changed","params":{"threadId":"` + id + `","status":{"type":"active"}}}` + "\n"))
	f.waitEvent(id, "pending", "minimal")
	if len(f.messages("thread/unsubscribe")) != 0 {
		t.Fatal("active turn subscription was released")
	}
	_, _ = f.localOutput.Write([]byte(`{"method":"turn/completed","params":{"threadId":"` + id + `"}}` + "\n"))
	f.waitEvent(id, "applied", "minimal")
	if len(f.messages("thread/unsubscribe")) != 1 || len(f.messages("turn/interrupt")) != 0 {
		t.Fatal("idle boundary did not apply without interruption")
	}
}

func TestCloudHarnessFailedReloadResumesOriginalConfig(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.mu.Lock()
	f.policy.Enabled = true
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "thread/resume" && strings.Contains(string(request["params"]), `"skills.include_instructions":false`) {
			return nil, errors.New("reject minimal"), true
		}
		return nil, nil, false
	}
	f.mu.Unlock()
	response := f.request(20, "turn/start", map[string]any{"threadId": id})
	if len(response["error"]) == 0 {
		t.Fatal("failed minimal reload was reported as successful")
	}
	f.waitEvent(id, "failed", "minimal")
	resumes := f.messages("thread/resume")
	if len(resumes) != 2 {
		t.Fatalf("reload failure made %d resume attempts", len(resumes))
	}
	if strings.Contains(string(resumes[1]["params"]), `"skills.include_instructions":false`) || !strings.Contains(string(resumes[1]["params"]), `"user.option":"retained"`) {
		t.Fatal("rollback did not use original baseline")
	}
	if len(f.messages("turn/start")) != 0 {
		t.Fatal("rejected turn reached native model")
	}
}

func TestCloudHarnessPrivateRepliesDoNotConsumeApplicationIDs(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.start()
	f.initialize()
	f.request("cq-harness/not-ours/1", "thread/start", map[string]any{"model": "glm-5.3", "cwd": "/work"})
	text := f.output.text()
	if !strings.Contains(text, `"id":"cq-harness/not-ours/1"`) {
		t.Fatal("application ID was consumed")
	}
	for _, request := range f.messages("config/read") {
		if bytes.Contains([]byte(text), request["id"]) {
			t.Fatal("private RPC response escaped to application")
		}
	}
	// App notifications continue flowing even after the internal exchange.
	_, _ = f.localOutput.Write([]byte(`{"method":"fixture/notification","params":{"unchanged":true}}` + "\n"))
	select {
	case value := <-f.output.values:
		if value.method() != "fixture/notification" {
			t.Fatal("notification changed")
		}
	case <-time.After(time.Second):
		t.Fatal("native reader stalled")
	}
}

func TestCloudHarnessTimeoutConsumesLateReplyAndKeepsOtherModelsWorking(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "config/read" {
			return nil, nil, true
		}
		return nil, nil, false
	}
	f.start()
	f.initialize()
	started := time.Now()
	response := f.request(2, "thread/start", map[string]any{"model": "glm-5.3"})
	if len(response["error"]) == 0 || time.Since(started) > time.Second {
		t.Fatal("private RPC did not fail within its bound")
	}
	requests := f.messages("config/read")
	if len(requests) != 1 {
		t.Fatal("missing internal request")
	}
	raw, _ := json.Marshal(map[string]any{"id": requests[0]["id"], "result": map[string]any{}})
	_, _ = f.localOutput.Write(append(raw, '\n'))
	f.request(3, "thread/start", map[string]any{"model": "gpt-5.5"})
	if bytes.Contains([]byte(f.output.text()), requests[0]["id"]) {
		t.Fatal("late internal reply reached application")
	}
}

func TestCloudHarnessTracksReturnedModelAndActiveResume(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "thread/resume" {
			id := cloudThreadID(request["params"])
			return map[string]any{"thread": map[string]any{"id": id, "cwd": "/project", "model": "glm-5.3", "status": map[string]any{"type": "active"}}, "model": "glm-5.3"}, nil, true
		}
		return nil, nil, false
	}
	f.start()
	f.initialize()
	f.request(2, "thread/resume", map[string]any{"threadId": "existing", "model": "gpt-5.5", "cwd": "/wrong"})
	event := f.waitEvent("existing", "pending", "minimal")
	if event["source"] != "default" || len(f.messages("thread/unsubscribe")) != 0 {
		t.Fatal("actual active GLM state was not honoured")
	}
}

func TestCloudHarnessOwnsSharedLeaseAndPersistsFullReceipt(t *testing.T) {
	f := newCloudHarnessFixture(t)
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	f.deps.harnessReceipts = store
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.waitEvent(id, "applied", "minimal")
	receipt, err := store.Load(id)
	if err != nil || receipt.Mode != "full" || receipt.Profile != harness.Minimal || receipt.State != "applied" {
		t.Fatalf("missing durable full receipt: %#v %v", receipt, err)
	}
	lease, err := store.Acquire(id)
	if err == nil {
		_ = lease.Close()
		t.Fatal("standard controller could acquire full-owned chat")
	}
	f.request(30, "thread/unsubscribe", map[string]any{"threadId": id})
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		lease, err = store.Acquire(id)
		if err == nil {
			_ = lease.Close()
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("app unsubscribe retained harness lease")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCloudHarnessHandoffRestoresOnlyMatchingPluginBaseline(t *testing.T) {
	for _, diverged := range []bool{false, true} {
		t.Run(fmt.Sprintf("diverged=%t", diverged), func(t *testing.T) {
			f := newCloudHarnessFixture(t)
			store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
			f.deps.harnessReceipts = store
			f.policy.Enabled = false
			f.start()
			f.initialize()
			id := f.startThread("glm-5.3", "/project")
			applied := []string{"user-excluded@market"}
			if diverged {
				applied = []string{"different@market"}
			}
			lease, err := store.Acquire(id)
			if err != nil {
				t.Fatal(err)
			}
			err = store.Save(harness.Receipt{Version: 1, ChatID: id, Mode: "plugins", Profile: harness.Minimal, Source: "default", State: "applied", BaselinePlugins: []string{"original@market"}, AppliedPlugins: applied})
			_ = lease.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.setPolicy(harness.Policy{Version: 1, Enabled: true, Chats: map[string]harness.Profile{id: harness.Full}})
			response := f.request(40, "turn/start", map[string]any{"threadId": id})
			if diverged {
				if len(response["error"]) == 0 || len(f.messages("thread/settings/update")) != 0 {
					t.Fatal("diverged plugin choices changed")
				}
				return
			}
			if len(response["error"]) != 0 {
				t.Fatalf("matching handoff failed: %s", response["error"])
			}
			updates := f.messages("thread/settings/update")
			if len(updates) != 1 || !strings.Contains(string(updates[0]["params"]), "original@market") {
				t.Fatal("standard controller baseline was not restored")
			}
			receipt, err := store.Load(id)
			if err != nil || len(receipt.BaselinePlugins) != 1 || receipt.BaselinePlugins[0] != "original@market" {
				t.Fatal("handoff replaced original baseline")
			}
		})
	}
}

func TestCloudHarnessMissingPolicyDoesNotCreateReceiptState(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	fsys := fsutil.NewMemFS()
	f.deps.harnessReceipts = harness.Receipts{FS: fsys, Dir: "/state/harness"}
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.request(40, "turn/start", map[string]any{"threadId": id})
	if _, err := fsys.Stat("/state/harness"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled policy created receipt state: %v", err)
	}
}

func TestCloudHarnessDoesNotReloadBeforeTurnStartAcknowledgement(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "turn/start" {
			return nil, nil, true
		}
		return nil, nil, false
	}
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	raw, _ := json.Marshal(map[string]any{"id": 40, "method": "turn/start", "params": map[string]any{"threadId": id}})
	if _, err := f.input.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(f.messages("turn/start")) == 0 {
		select {
		case <-deadline.C:
			t.Fatal("turn was not forwarded")
		case <-time.After(time.Millisecond):
		}
	}
	f.setPolicy(harness.Policy{Version: 1, Enabled: true})
	_, _ = f.localOutput.Write([]byte(`{"method":"thread/status/changed","params":{"threadId":"` + id + `","status":{"type":"idle"}}}` + "\n"))
	f.waitEvent(id, "pending", "minimal")
	if len(f.messages("thread/unsubscribe")) != 0 {
		t.Fatal("forwarded, unacknowledged turn was reloaded")
	}
	_, _ = f.localOutput.Write([]byte(`{"id":40,"result":{"turn":{"id":"done","status":"inProgress"}}}` + "\n"))
	f.await(40)
	_, _ = f.localOutput.Write([]byte(`{"method":"turn/completed","params":{"threadId":"` + id + `","turn":{"id":"done"}}}` + "\n"))
	f.waitEvent(id, "applied", "minimal")
}

func TestCloudHarnessRequestConfigModelOverridesGlobalDefault(t *testing.T) {
	for _, tc := range []struct {
		global, override string
		minimal          bool
	}{{"glm-5.3", "gpt-5.5", false}, {"gpt-5.5", "glm-5.3", true}} {
		t.Run(tc.override, func(t *testing.T) {
			f := newCloudHarnessFixture(t)
			f.globalModel = tc.global
			f.start()
			f.initialize()
			raw, _ := json.Marshal(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": "/project", "config": map[string]any{"model": tc.override}}})
			raw = append(raw, '\n')
			if _, err := f.input.Write(raw); err != nil {
				t.Fatal(err)
			}
			f.await(2)
			messages := f.messages("thread/start")
			if len(messages) != 1 {
				t.Fatal("missing native start")
			}
			var params struct{ Config map[string]any }
			_ = json.Unmarshal(messages[0]["params"], &params)
			_, minimal := params.Config["skills.include_instructions"]
			if minimal != tc.minimal {
				t.Fatalf("config model override classified incorrectly: %s", messages[0]["params"])
			}
			if !tc.minimal {
				for _, sent := range f.localMessages() {
					var message cloudMessage
					_ = json.Unmarshal(sent, &message)
					if message.method() == "thread/start" && !bytes.Equal(sent, raw) {
						t.Fatal("request-config OpenAI bytes changed")
					}
				}
			}
		})
	}
}

func TestCloudHarnessReloadPreservesLatestAcceptedNativeSettings(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.start()
	f.initialize()
	started := f.request(2, "thread/start", map[string]any{"model": "glm-5.3", "cwd": "/project", "approvalPolicy": "never", "permissions": ":read-only", "serviceTier": "flex", "runtimeWorkspaceRoots": []string{"/A"}})
	id := cloudThreadID(started["result"])
	f.request(3, "thread/settings/update", map[string]any{"threadId": id, "approvalPolicy": "on-request", "permissions": ":workspace", "serviceTier": nil, "effort": "xhigh", "summary": "detailed"})
	f.request(4, "turn/start", map[string]any{"threadId": id, "runtimeWorkspaceRoots": []string{"/B"}})
	_, _ = f.localOutput.Write([]byte(`{"method":"turn/completed","params":{"threadId":"` + id + `","turn":{"id":"turn"}}}` + "\n"))
	f.setPolicy(harness.Policy{Version: 1, Enabled: true})
	_, _ = f.localOutput.Write([]byte(`{"method":"thread/status/changed","params":{"threadId":"` + id + `","status":{"type":"idle"}}}` + "\n"))
	f.waitEvent(id, "applied", "minimal")
	f.setPolicy(harness.Policy{Version: 1, Enabled: true, Chats: map[string]harness.Profile{id: harness.Full}})
	_, _ = f.localOutput.Write([]byte(`{"method":"thread/status/changed","params":{"threadId":"` + id + `","status":{"type":"idle"}}}` + "\n"))
	f.waitEvent(id, "applied", "full")
	resumes := f.messages("thread/resume")
	if len(resumes) != 2 {
		t.Fatalf("got %d profile reloads", len(resumes))
	}
	for _, resume := range resumes {
		var params map[string]any
		_ = json.Unmarshal(resume["params"], &params)
		if params["approvalPolicy"] != "on-request" || params["permissions"] != ":workspace" || params["serviceTier"] != nil || !strings.Contains(string(resume["params"]), `"runtimeWorkspaceRoots":["/B"]`) {
			t.Fatalf("reload reverted current settings: %s", resume["params"])
		}
	}
	updates := f.messages("thread/settings/update")
	if len(updates) != 3 {
		t.Fatalf("got %d settings updates", len(updates))
	}
	for _, update := range updates[1:] {
		if !strings.Contains(string(update["params"]), `"effort":"xhigh"`) || !strings.Contains(string(update["params"]), `"summary":"detailed"`) {
			t.Fatalf("reload lost settings unsupported by resume: %s", update["params"])
		}
	}
}

func TestCloudHarnessCollaborationModeModelWinsForTurnPreflight(t *testing.T) {
	for _, tc := range []struct {
		top, mode string
		minimal   bool
	}{{"glm-5.3", "gpt-5.5", false}, {"gpt-5.5", "glm-5.3", true}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newCloudHarnessFixture(t)
			f.start()
			f.initialize()
			id := f.startThread("gpt-5.5", "/project")
			response := f.request(20, "turn/start", map[string]any{"threadId": id, "model": tc.top, "collaborationMode": map[string]any{"mode": "default", "settings": map[string]any{"model": tc.mode}}})
			if len(response["error"]) != 0 {
				t.Fatal("conflicting model turn failed")
			}
			resumes := f.messages("thread/resume")
			if (len(resumes) > 0) != tc.minimal {
				t.Fatalf("wrong profile chosen for collaboration model %s: %d reloads", tc.mode, len(resumes))
			}
			if tc.minimal && !strings.Contains(string(resumes[0]["params"]), `"skills.include_instructions":false`) {
				t.Fatal("GLM collaboration model was not minimal")
			}
		})
	}
}

func TestCloudHarnessReplayUpdatesCollaborationModelWithoutLosingInstructions(t *testing.T) {
	snapshot := cloudHarnessThread{id: "thread", settings: cloudMessage{"effort": cloudHarnessRaw("xhigh"), "collaborationMode": cloudHarnessRaw(map[string]any{"mode": "plan", "settings": map[string]any{"model": "glm-5.3", "reasoning_effort": "high", "developer_instructions": "retain me"}})}}
	replay := cloudHarnessReplaySettings(snapshot, "gpt-5.5", "/project")
	var mode struct {
		Mode     string
		Settings struct {
			Model        string
			Effort       string `json:"reasoning_effort"`
			Instructions string `json:"developer_instructions"`
		}
	}
	_ = json.Unmarshal(replay["collaborationMode"], &mode)
	if mode.Mode != "plan" || mode.Settings.Model != "gpt-5.5" || mode.Settings.Effort != "xhigh" || mode.Settings.Instructions != "retain me" {
		t.Fatalf("collaboration settings changed incorrectly: %s", replay["collaborationMode"])
	}
}

func TestCloudHarnessCapabilityBaselineUsesEffectiveNativeValues(t *testing.T) {
	for _, tc := range []struct {
		name              string
		native, original  string
		want, found, fail bool
	}{
		{"merged disabled", `{"config":{"features":{"plugins":false}},"layers":[{"config":{"features":{"plugins":true}}}]}`, `{}`, false, true, false},
		{"request disabled", `{"config":{"features":{"plugins":true}},"layers":[]}`, `{"features.plugins":false}`, false, true, false},
		{"origin selects layer", `{"config":{},"origins":{"features.plugins":{"version":"selected"}},"layers":[{"version":"selected","config":{"features":{"plugins":false}}},{"version":"other","config":{"features":{"plugins":true}}}]}`, `{}`, false, true, false},
		{"unknown precedence", `{"config":{},"layers":[{"config":{"features":{"plugins":false}}},{"config":{"features":{"plugins":true}}}]}`, `{}`, false, false, true},
		{"disabled layer omitted", `{"config":{},"layers":[{"disabledReason":"untrusted","config":{"features":{"plugins":true}}}]}`, `{}`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, found, err := cloudHarnessBaselineBool(cloudHarnessObject([]byte(tc.native)), cloudHarnessObject([]byte(tc.original)), []string{"features", "plugins"})
			if value != tc.want || found != tc.found || (err != nil) != tc.fail {
				t.Fatalf("effective baseline = %t,%t,%v", value, found, err)
			}
		})
	}
}

func TestCloudHarnessFullRestoresDisabledUserDefaults(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		switch request.method() {
		case "config/read":
			return map[string]any{"config": map[string]any{"model": "glm-5.3", "mcp_servers": map[string]any{"probe": map[string]any{"enabled": false}}}, "layers": []any{}}, nil, true
		case "experimentalFeature/list":
			return map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": false}}}, nil, true
		}
		return nil, nil, false
	}
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.setPolicy(harness.Policy{Version: 1, Enabled: true, Chats: map[string]harness.Profile{id: harness.Full}})
	f.request(30, "turn/start", map[string]any{"threadId": id})
	resumes := f.messages("thread/resume")
	if len(resumes) != 1 {
		t.Fatal("missing full restore")
	}
	var params struct{ Config map[string]any }
	_ = json.Unmarshal(resumes[0]["params"], &params)
	if params.Config["features.plugins"] != false || params.Config["mcp_servers.probe.enabled"] != false || params.Config["skills.include_instructions"] != true {
		t.Fatalf("full enabled a user-disabled capability: %s", resumes[0]["params"])
	}
}

func TestCloudHarnessReloadUsesSettingsArrivingDuringConfigRead(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.start()
	f.initialize()
	started := f.request(2, "thread/start", map[string]any{"model": "glm-5.3", "cwd": "/project", "approvalPolicy": "never", "permissions": ":read-only"})
	id := cloudThreadID(started["result"])
	f.mu.Lock()
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "config/read" {
			notification, _ := json.Marshal(map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": id, "threadSettings": map[string]any{"model": "glm-5.3", "cwd": "/project", "approvalPolicy": "on-request", "activePermissionProfile": map[string]any{"id": ":workspace"}, "effort": "xhigh"}}})
			_, _ = f.localOutput.Write(append(notification, '\n'))
		}
		return nil, nil, false
	}
	f.policy.Enabled = true
	f.mu.Unlock()
	f.request(20, "turn/start", map[string]any{"threadId": id})
	resumes := f.messages("thread/resume")
	if len(resumes) != 1 || !strings.Contains(string(resumes[0]["params"]), `"approvalPolicy":"on-request"`) || !strings.Contains(string(resumes[0]["params"]), `"permissions":":workspace"`) {
		t.Fatal("reload ignored authoritative settings arriving during config lookup")
	}
	updates := f.messages("thread/settings/update")
	if len(updates) != 1 || !strings.Contains(string(updates[0]["params"]), `"effort":"xhigh"`) {
		t.Fatal("late native effort was not preserved")
	}
}

func TestCloudHarnessRejectedSettingsDoNotReplaceCurrentSettings(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.start()
	f.initialize()
	started := f.request(2, "thread/start", map[string]any{"model": "glm-5.3", "cwd": "/project", "approvalPolicy": "on-request", "permissions": ":workspace"})
	id := cloudThreadID(started["result"])
	f.mu.Lock()
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "thread/settings/update" && strings.Contains(string(request["params"]), `"permissions":":read-only"`) {
			return nil, errors.New("reject settings"), true
		}
		return nil, nil, false
	}
	f.mu.Unlock()
	rejected := f.request(3, "thread/settings/update", map[string]any{"threadId": id, "approvalPolicy": "never", "permissions": ":read-only"})
	if len(rejected["error"]) == 0 {
		t.Fatal("fixture did not reject settings")
	}
	f.setPolicy(harness.Policy{Version: 1, Enabled: true})
	f.request(20, "turn/start", map[string]any{"threadId": id})
	resumes := f.messages("thread/resume")
	if len(resumes) != 1 || !strings.Contains(string(resumes[0]["params"]), `"approvalPolicy":"on-request"`) || !strings.Contains(string(resumes[0]["params"]), `"permissions":":workspace"`) {
		t.Fatal("rejected settings replaced accepted native state")
	}
}

func TestCloudHarnessLateActualModelChangePreventsGLMReload(t *testing.T) {
	f := newCloudHarnessFixture(t)
	f.policy.Enabled = false
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.mu.Lock()
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "config/read" {
			f.models[id] = "gpt-5.5"
			notification, _ := json.Marshal(map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": id, "threadSettings": map[string]any{"model": "gpt-5.5", "cwd": "/project"}}})
			_, _ = f.localOutput.Write(append(notification, '\n'))
		}
		return nil, nil, false
	}
	f.policy.Enabled = true
	f.mu.Unlock()
	response := f.request(20, "turn/start", map[string]any{"threadId": id})
	if len(response["error"]) != 0 || len(f.messages("thread/unsubscribe")) != 0 {
		t.Fatal("late actual OpenAI model was overwritten by stale GLM profile")
	}
}

func TestCloudHarnessQuotedServerOverrideKeepsExactNameAndBaseline(t *testing.T) {
	for _, key := range []string{`mcp_servers."project.tools".enabled`, `mcp_servers.'project.tools'.enabled`} {
		t.Run(key, func(t *testing.T) {
			original := cloudMessage{key: json.RawMessage("true")}
			native := cloudHarnessObject(cloudHarnessRaw(map[string]any{"config": map[string]any{"features": map[string]any{"plugins": true}, "mcp_servers": map[string]any{"project.tools": map[string]any{"enabled": false}}}, "layers": []any{}}))
			minimal := cloudHarnessMinimalConfig(native, cloudHarnessRaw(original))
			canonical := `mcp_servers."project.tools".enabled`
			if !bytes.Equal(minimal[canonical], []byte("false")) {
				t.Fatalf("exact quoted server was not disabled: %v", minimal)
			}
			for setting := range minimal {
				if strings.HasPrefix(setting, "mcp_servers.") && setting != canonical {
					t.Fatalf("minimal added or retained wrong server setting %q", setting)
				}
			}
			h := &cloudHarness{}
			baseline, err := h.baselineConfig(native, cloudHarnessRaw(original), "")
			if err != nil || !bytes.Equal(baseline[canonical], []byte("true")) {
				t.Fatalf("original quoted override did not beat native disabled default: %v %v", baseline, err)
			}
		})
	}
}
