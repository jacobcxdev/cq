package zcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/httputil"
)

func TestGatewayStartGroupsCallerInstructionsAfterSourceIdentity(t *testing.T) {
	const input = `{"model":"glm-5.3-flash","instructions":"foundation\nline","input":[{"type":"message","role":"developer","content":"permission rules"},{"type":"message","role":"system","content":"runtime rules"},{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"function","name":"probe","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}}],"max_output_tokens":128}`
	want := []string{"foundation\nline", "permission rules", "runtime rules"}
	for _, start := range []bool{false, true} {
		name := "paid"
		if start {
			name = "start"
		}
		t.Run(name, func(t *testing.T) {
			g := gatewayFixture(func(req *http.Request) (*http.Response, error) {
				data, err := httputil.ReadBody(req.Body)
				var body struct {
					System []struct{ Type, Text string }
					Tools  []json.RawMessage
				}
				if err != nil || json.Unmarshal(data, &body) != nil || len(body.System) != 3 {
					t.Fatal("incorrect system block projection")
				}
				if start {
					identityHash := sha256.Sum256([]byte(body.System[1].Text))
					if body.System[0].Text != "You are ZCode, an interactive agent" || hex.EncodeToString(identityHash[:]) != "aac4ba4cc7b555b3a32704850028e242a6e3045df91d7970f04d18c378e57f40" {
						t.Fatal("Start request lacks the pinned public source identity")
					}
					if body.System[2].Text != strings.Join(want, "\n\n") {
						t.Fatal("Start grouping lost or reordered caller instructions")
					}
				} else {
					for i, text := range want {
						if body.System[i].Text != text {
							t.Fatal("paid caller instructions changed")
						}
					}
				}
				if len(body.Tools) != 1 || !strings.Contains(string(body.Tools[0]), "9007199254740993") {
					t.Fatal("prompt assembly changed tool definitions or numeric precision")
				}
				response := jsonResponse(gatewayReply)
				response.Header.Set("Content-Type", "application/json")
				return response, nil
			})
			response, err := g.Do(context.Background(), []byte(input), gatewayAuth(start), "source-identity-test")
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatal("source identity request failed")
			}
			_ = response.Body.Close()
		})
	}
}

func TestGatewayStartRejectsNonTextSystemBeforeNetwork(t *testing.T) {
	const input = `{"model":"glm-5.3-flash","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep this instruction"},{"type":"input_image","image_url":"https://example.com/instruction.png"}]},{"type":"message","role":"user","content":"hello"}]}`
	calls := 0
	g := gatewayFixture(func(*http.Request) (*http.Response, error) {
		calls++
		response := jsonResponse(gatewayReply)
		response.Header.Set("Content-Type", "application/json")
		return response, nil
	})
	response, err := g.Do(context.Background(), []byte(input), gatewayAuth(true), "system-image-test")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || calls != 0 {
		t.Fatal("unsupported system content was silently discarded before dispatch")
	}
}
