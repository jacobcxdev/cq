package dots

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCloudStdioPreservedPublicWorkspaceOrigin(t *testing.T) {
	for _, test := range []struct {
		name, method, origin, want string
	}{
		{"owned-loopback", "account/read", "https://localhost:8000", "https://chatgpt.com"},
		{"explicit-residency", "account/read", "https://us.chatgpt.com", "https://us.chatgpt.com"},
		{"canonical", "account/read", "https://chatgpt.com", "https://chatgpt.com"},
		{"other-port", "account/read", "https://localhost:8001", "https://localhost:8001"},
		{"other-host", "account/read", "https://127.0.0.1:8000", "https://127.0.0.1:8000"},
		{"other-method", "getAuthStatus", "https://localhost:8000", "https://localhost:8000"},
		{"native-disabled", "account/read", "https://localhost:8000", "https://localhost:8000"},
		{"env-mismatch", "account/read", "https://localhost:8000", "https://localhost:8000"},
		{"missing-port", "account/read", "https://localhost:8000", "https://localhost:8000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newCloudTestHarness(t)
			h.cfg.IntegrationMode = "native"
			if err := os.WriteFile(filepath.Join(h.cfg.RuntimeRoot, "port"), []byte("8000"), 0o600); err != nil {
				t.Fatal(err)
			}
			h.deps.environment = append(h.deps.environment, "CODEX_API_BASE_URL=https://chatgpt.com/backend-api", "CODEX_APP_SERVER_CHATGPT_BASE_URL=https://localhost:8000/backend-api")
			switch test.name {
			case "native-disabled":
				h.cfg.IntegrationMode = "isolated"
			case "env-mismatch":
				h.deps.environment = setEnvironment(h.deps.environment, "CODEX_APP_SERVER_CHATGPT_BASE_URL", "https://localhost:8001/backend-api")
			case "missing-port":
				if err := os.Remove(filepath.Join(h.cfg.RuntimeRoot, "port")); err != nil {
					t.Fatal(err)
				}
			}
			launch := h.deps.launch
			h.deps.launch = func(ctx context.Context, path string, args, env []string, stderr io.Writer) (*cloudLocal, error) {
				local, err := launch(ctx, path, args, env, stderr)
				if err != nil {
					return nil, err
				}
				input := local.input
				local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
					var request cloudMessage
					if json.Unmarshal(raw, &request) != nil || request.method() != test.method {
						return input.Write(raw)
					}
					reply, err := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{
						"account": map[string]any{"type": "chatgpt", "planType": "pro"}, "requiresOpenaiAuth": true,
						"workspaceRouting": map[string]any{"chatgptAccountId": "primary-account", "backendOrigin": test.origin, "accountRoutingOverride": "NO_CONSTRAINT", "futureField": "preserved"},
					}})
					if err != nil {
						return 0, err
					}
					if _, err := h.localOutput.Write(append(reply, '\n')); err != nil {
						return 0, err
					}
					return len(raw), nil
				}, close: input.Close}
				return local, nil
			}
			h.start()
			h.initialize()
			h.send("account-snapshot", test.method, "")
			reply := h.await("account-snapshot")
			var got map[string]any
			if err := json.Unmarshal(reply["result"], &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "pro"}, "requiresOpenaiAuth": true,
				"workspaceRouting": map[string]any{"chatgptAccountId": "primary-account", "backendOrigin": test.want, "accountRoutingOverride": "NO_CONSTRAINT", "futureField": "preserved"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("workspace routing authority changed: got %#v, want %#v", got, want)
			}
		})
	}
}

func TestCloudStdioWorkspaceOriginRejectedUnownedAuthority(t *testing.T) {
	for _, name := range []string{"wrong-account", "missing-account", "explicit-residency", "null-routing", "malformed-routing", "unsolicited", "consumed"} {
		t.Run(name, func(t *testing.T) {
			b := &cloudBridge{cfg: &Settings{SystemAccountID: "primary-account"}, backendOrigin: "https://localhost:8000", accountReads: make(map[string]cloudLocalAccountRequest)}
			routing := map[string]any{"backendOrigin": "https://localhost:8000", "chatgptAccountId": "primary-account", "accountRoutingOverride": "NO_CONSTRAINT"}
			result := map[string]any{"workspaceRouting": any(routing)}
			switch name {
			case "wrong-account":
				routing["chatgptAccountId"] = "other-account"
			case "missing-account":
				delete(routing, "chatgptAccountId")
				b.cfg.SystemAccountID = ""
			case "explicit-residency":
				routing["accountRoutingOverride"] = "us"
			case "null-routing":
				result["workspaceRouting"] = nil
			case "malformed-routing":
				result["workspaceRouting"] = "untrusted"
			}
			request := cloudMessage{"id": cloudHarnessRaw("account-snapshot"), "method": cloudHarnessRaw("account/read")}
			if name != "unsolicited" {
				b.trackLocalAccountRequest(request)
			}
			raw, _ := json.Marshal(map[string]any{"id": "account-snapshot", "result": result})
			if name == "consumed" {
				_ = b.localAccountResponse(raw)
			}
			if got := b.localAccountResponse(raw); string(got) != string(raw) {
				t.Fatalf("unowned authority was rewritten: %s", got)
			}
		})
	}
}
