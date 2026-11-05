package zcode

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// One explicitly requested live flagship inference, capped at 2,048 output
// tokens including thinking. No tools, claims, resets or model retries.
func TestZCodeLiveFlagshipMax(t *testing.T) {
	if os.Getenv("CQ_ZCODE_FLAGSHIP_LIVE") != "1" {
		t.Skip("explicit flagship Max qualification")
	}
	login, device := zcodeLiveLogin(t)
	client := httputil.NewClient(60*time.Second, "zcode-flagship-qualification")
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	tokens := &ProjectTokenClient{HTTP: client, Version: ClientVersion}
	defer tokens.Clear()
	credential, err := tokens.Resolve(ctx, login)
	if err != nil {
		t.Fatal("resolve own project token")
	}
	gateway := &Gateway{HTTP: client, Signer: &Signer{HTTP: client, Version: ClientVersion}, Version: ClientVersion, DeviceMid: device, Catalogue: &ModelCatalogue{HTTP: client, Version: ClientVersion}}
	response, err := gateway.Do(ctx, []byte(`{"model":"glm-5.3","input":"Reply exactly ZCODE_FLAGSHIP_OK. Do not use tools.","reasoning":{"effort":"max"},"max_output_tokens":2048,"store":false}`), GatewayAuth{Credential: credential, ZCodeJWT: login.ZCodeJWT, DeviceMid: device}, "cq-flagship-qualification")
	if err != nil {
		t.Fatal("flagship request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("flagship status=%d", response.StatusCode)
	}
	body, err := httputil.ReadBody(response.Body)
	var result zcodeLiveResponse
	if err != nil || json.Unmarshal(body, &result) != nil || result.Status != "completed" || result.Usage == nil || result.Usage.Output > 2048 || zcodeLiveText(result) != "ZCODE_FLAGSHIP_OK" {
		t.Fatal("flagship Max did not complete bounded marker task")
	}
	t.Logf("model=glm-5.3 effort=max status=200 input_tokens=%d output_tokens=%d", result.Usage.Input, result.Usage.Output)
}
