package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jacobcxdev/cq/internal/httputil"
)

func TestGatewayMetadataUsedActualIdentityWithoutRounding(t *testing.T) {
	body, err := gatewayMetadata([]byte(`{"max_tokens":128,"large":9007199254740993}`), "actual-device", "actual-session")
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Large    json.RawMessage `json:"large"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &request) != nil || string(request.Large) != "9007199254740993" {
		t.Fatal("metadata changed request precision")
	}
	var identity map[string]string
	if json.Unmarshal([]byte(request.Metadata.UserID), &identity) != nil || identity["device_id"] != "actual-device" || identity["session_id"] != "actual-session" || identity["account_uuid"] != "" {
		t.Fatal("incorrect request attribution")
	}
}

func TestGatewayUsedAdvertisedModelAndSelectedAccountDevice(t *testing.T) {
	posts := 0
	httpClient := doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == "GET" {
			if req.Header.Get("Authorization") != "" {
				t.Fatal("public metadata received credentials")
			}
			return jsonResponse(`{"code":0,"data":{"builtinModels":[{"modelId":"GLM-5.3"}]}}`), nil
		}
		posts++
		var body struct {
			Model    string `json:"model"`
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		raw, _ := httputil.ReadBody(req.Body)
		if json.Unmarshal(raw, &body) != nil || body.Model != "GLM-5.3" || req.Header.Get("X-Device-Mid") != "selected-device" {
			t.Fatal("gateway used stale global context")
		}
		var identity map[string]string
		if json.Unmarshal([]byte(body.Metadata.UserID), &identity) != nil || identity["device_id"] != "selected-device" || identity["session_id"] != "actual-session" {
			t.Fatal("body attribution crossed account/device")
		}
		for _, key := range []string{"X-Request-Id", "X-ZCode-Trace-Id"} {
			id := req.Header.Get(key)
			if len(id) != 36 || id[14] != '4' {
				t.Fatal("request context lacks normal UUID")
			}
		}
		response := jsonResponse(gatewayReply)
		response.Header.Set("Content-Type", "application/json")
		return response, nil
	})
	gateway := &Gateway{HTTP: httpClient, Version: ClientVersion, DeviceMid: "wrong-global-device", Catalogue: &ModelCatalogue{HTTP: httpClient, Version: ClientVersion}}
	response, err := gateway.Do(context.Background(), []byte(gatewayInput), GatewayAuth{StartPlan: true, ZCodeJWT: "own-session", DeviceMid: "selected-device"}, "sess_actual-session")
	if err != nil || posts != 1 {
		t.Fatalf("selected account projection failed: %v", err)
	}
	_ = response.Body.Close()
}
