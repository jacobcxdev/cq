package dots

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessRetainedOrUnknownAppRuntimeWasPartial(t *testing.T) {
	for _, mode := range []string{"retained", "missing", "duplicate", "error"} {
		t.Run(mode, func(t *testing.T) {
			f := newCloudHarnessSelectiveFixture(t)
			f.policy.Global = harness.Scope{}
			store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
			f.deps.harnessReceipts = store
			f.start()
			f.initialize()
			id := f.startThread("gpt-5.5", "/project")
			f.completeStoredTurn(id)
			f.mu.Lock()
			f.onRequest = func(request cloudMessage) (any, error, bool) {
				if request.method() != "experimentalFeature/list" {
					return nil, nil, false
				}
				if mode == "error" {
					return nil, errors.New("private-native-error"), true
				}
				rows := []any{map[string]any{"name": "plugins", "enabled": cloudHarnessBool(f.configs[id], "features.plugins", true)}}
				if mode != "missing" {
					rows = append(rows, map[string]any{"name": "apps", "enabled": true})
				}
				if mode == "duplicate" {
					rows = append(rows, map[string]any{"name": "apps", "enabled": true})
				}
				return map[string]any{"data": rows, "nextCursor": nil}, nil, true
			}
			f.mu.Unlock()
			f.setPolicy(harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}})
			if reply := f.request(3, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) != 0 {
				t.Fatalf("app runtime uncertainty blocked the chat: %s", reply["error"])
			}
			want := "native_app_runtime_unverified"
			if mode == "retained" {
				want = "native_app_runtime_retained"
			}
			receipt, err := store.Load(id)
			if err != nil || receipt.State != "partial" || !slices.Contains(receipt.Issues, want) {
				t.Fatalf("Apps runtime uncertainty claimed applied: state=%s issues=%v err=%v", receipt.State, receipt.Issues, err)
			}
		})
	}
}

func TestCloudHarnessAppsRestoredOriginalTableInSameChat(t *testing.T) {
	for _, original := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"_default":{"enabled":false,"default_tools_approval_mode":"approve"},"existing":{"enabled":true,"tools":{"read":{"enabled":false}}}}`)} {
		t.Run(string(original), func(t *testing.T) {
			f := newCloudHarnessSelectiveFixture(t)
			f.policy.Global = harness.Scope{}
			f.start()
			f.initialize()
			started := f.request(2, "thread/start", map[string]any{"model": "gpt-5.5", "cwd": "/project", "config": map[string]any{"apps": original, "features.apps": true}})
			id := cloudThreadID(started["result"])
			if id == "" {
				t.Fatal("missing original chat")
			}
			f.completeStoredTurn(id)
			f.setPolicy(harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}})
			f.request(3, "turn/start", map[string]any{"threadId": id})
			f.completeStoredTurn(id)
			f.setPolicy(harness.Policy{Version: 2, Enabled: true})
			f.request(4, "turn/start", map[string]any{"threadId": id})
			resumes := f.messages("thread/resume")
			if len(resumes) != 2 {
				t.Fatalf("same-chat reduction/restore made %d reloads", len(resumes))
			}
			params := cloudHarnessObject(resumes[1]["params"])
			config := cloudHarnessObject(params["config"])
			if cloudThreadID(resumes[1]["params"]) != id || cloudHarnessString(params, "model") != "gpt-5.5" || !bytes.Equal(config["features.apps"], []byte("true")) {
				t.Fatal("Apps restoration changed chat/model or retained gate")
			}
			var got, want any
			_ = json.Unmarshal(config["apps"], &got)
			_ = json.Unmarshal(original, &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("original app policy not restored: %s", config["apps"])
			}
		})
	}
}
