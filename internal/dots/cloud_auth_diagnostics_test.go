package dots

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCloudLocalAuthenticationDiagnosticsPreservedRPCAndPrivacy(t *testing.T) {
	for _, test := range []struct {
		name, body, method      string
		refresh, token, failure bool
	}{
		{"cached", `{"result":{"authMethod":"chatgpt","authToken":"synthetic-private-token"}}`, "chatgpt", false, true, false},
		{"missing", `{"result":{"authMethod":"chatgpt","authToken":null}}`, "chatgpt", true, false, false},
		{"external-token", `{"result":{"authMethod":"chatgptAuthTokens","authToken":"synthetic-private-token"}}`, "chatgptAuthTokens", false, true, false},
		{"api-key", `{"result":{"authMethod":"apiKey"}}`, "apiKey", false, false, false},
		{"unknown-method", `{"result":{"authMethod":"synthetic-private-auth-method","authToken":"synthetic-private-token"}}`, "unknown", false, true, false},
		{"malformed-method", `{"result":{"authMethod":{"secret":"synthetic-private-auth-method"},"authToken":"synthetic-private-token"}}`, "unknown", false, true, false},
		{"error", `{"error":{"code":-32000,"message":"private-error-text"}}`, "", true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []map[string]any
			b := &cloudBridge{cfg: &Settings{}, accountReads: make(map[string]cloudLocalAccountRequest), deps: cloudStdioDependencies{event: func(event map[string]any) { events = append(events, event) }}}
			request := cloudMessage{"id": cloudHarnessRaw("private-rpc-id"), "method": cloudHarnessRaw("getAuthStatus"), "params": cloudHarnessRaw(map[string]any{"includeToken": true, "refreshToken": test.refresh})}
			b.trackLocalAccountRequest(request)
			var response cloudMessage
			if err := json.Unmarshal([]byte(test.body), &response); err != nil {
				t.Fatal(err)
			}
			response["id"] = request["id"]
			raw, _ := json.Marshal(response)
			if got := b.localAccountResponse(raw); !bytes.Equal(got, raw) {
				t.Fatal("diagnostics changed authentication RPC")
			}
			if len(events) != 2 {
				t.Fatalf("request/result diagnostics missing: got %d", len(events))
			}
			for i, stage := range []string{"request", "result"} {
				event := events[i]
				if event["operation"] != "local_account_rpc" || event["stage"] != stage || event["source"] != "local_stock" || event["method"] != "getAuthStatus" || event["include_token"] != true || event["refresh_token"] != test.refresh {
					t.Fatal("authentication request context missing")
				}
				encoded, _ := json.Marshal(event)
				for _, secret := range []string{"synthetic-private-token", "private-rpc-id", "private-error-text", "synthetic-private-auth-method"} {
					if bytes.Contains(encoded, []byte(secret)) {
						t.Fatal("private RPC content leaked")
					}
				}
			}
			if events[0]["correlation"] == nil || events[0]["correlation"] != events[1]["correlation"] {
				t.Fatal("request and result not correlated")
			}
			result := events[1]
			if result["auth_method"] != test.method || result["has_token"] != test.token || result["has_result_error"] != test.failure {
				t.Fatal("result classification incorrect")
			}
		})
	}
}
