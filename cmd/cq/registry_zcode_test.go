package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func TestRegistryConstructionDidNotRefreshOAuth(t *testing.T) {
	fsys := fsutil.NewMemFS()
	roots := testCQRoots()
	_, err := zaiprov.StoreZCodeAccount(fsys, roots.Config, zcode.LoginSession{
		UserID: "own-user", ZCodeJWT: "own-session", RawOAuth: "own-oauth", BusinessJWT: "own-business",
	}, "own account", true)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS: fsys, HomeDir: "/home/test", Roots: roots,
		ClaudeToken: func() (string, error) { return "", nil },
		CodexToken:  func() (string, error) { return "", nil },
		HTTPClient: zaiCommandDoer(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("network must not run during construction")
		}), Stderr: io.Discard,
	})
	if err != nil || calls != 0 || pipeline.Refresher.ZAI == nil {
		t.Fatalf("construction refreshed credentials or lost configured source: calls=%d err=%v", calls, err)
	}
}

func TestStartRegistryUsedPreferredEntitlementsWithoutPaidToken(t *testing.T) {
	fsys, roots := fsutil.NewMemFS(), testCQRoots()
	account, err := zaiprov.StoreZCodeAccount(fsys, roots.Config, zcode.LoginSession{
		UserID: "own-user", ZCodeJWT: "own-session", RawOAuth: "own-oauth", BusinessJWT: "own-business", DeviceMid: "own-device",
	}, "Start account", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := zaiprov.SetTransport(fsys, roots.Config, account.ID, "zcode-start"); err != nil {
		t.Fatal(err)
	}
	calls, failBalance := 0, false
	now := time.Now().Unix()
	client := zaiCommandDoer(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "zcode.z.ai" || req.Method != "GET" {
			t.Fatal("Start catalogue attempted paid project/API authentication")
		}
		var body string
		switch req.URL.Path {
		case "/api/v1/zcode-plan/billing/balance":
			if req.Header.Get("Authorization") != "Bearer own-session" || req.Header.Get("X-Device-Mid") != "own-device" {
				t.Fatal("wrong Start account context")
			}
			if failBalance {
				return nil, errors.New("temporary balance read failure")
			}
			body = fmt.Sprintf(`{"code":0,"data":{"server_time":%d,"plans":[{"user_plan_id":"p","status":"active"}],"balances":[{"user_plan_id":"p","unit_type":"token","capabilities":["model:glm-future"],"total_units":100,"remaining_units":100,"period_start":%d,"period_end":%d}]}}`, now, now-60, now+3600)
		case "/api/v1/client/configs":
			if req.Header.Get("Authorization") != "" {
				t.Fatal("public metadata received account credentials")
			}
			body = `{"code":0,"data":{"builtinModels":[{"modelId":"GLM-Future","name":"GLM-Future","contextWindow":1234567,"maxCompletionTokens":2222,"reasoning":{"defaultLevel":"max","levels":{"low":{},"max":{}}}}]}}`
		default:
			t.Fatal("unexpected Start discovery endpoint")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	pipeline, err := newRegistryPipeline(registryPipelineOptions{FS: fsys, HomeDir: "/home/test", Roots: roots, HTTPClient: client,
		ClaudeToken: func() (string, error) { return "", nil }, CodexToken: func() (string, error) { return "", nil }, Stderr: io.Discard})
	if err != nil || calls != 0 {
		t.Fatal("registry construction accessed network")
	}
	result, err := pipeline.Refresher.ZAI.Fetch(context.Background())
	if err != nil || len(result.Entries) != 1 || result.Entries[0].ID != "glm-future" || result.Entries[0].ContextWindow != 1234567 || calls != 2 {
		t.Fatalf("Start discovery failed: calls=%d err=%v", calls, err)
	}
	var record struct {
		Max     int    `json:"max_output_tokens"`
		Default string `json:"default_reasoning_level"`
	}
	if json.Unmarshal(result.Entries[0].Raw, &record) != nil || record.Max != 2222 || record.Default != "max" {
		t.Fatal("public source metadata lost")
	}
	if _, err := zaiprov.StoreAccount(fsys, roots.Config, "saved-paid-key", "other paid account", false); err != nil {
		t.Fatal(err)
	}
	if result, err := pipeline.Refresher.ZAI.Fetch(context.Background()); err != nil || len(result.Entries) != 1 || result.Entries[0].ID != "glm-future" || calls != 3 {
		t.Fatal("saved paid account overrode preferred Start entitlement")
	}
	other, err := zaiprov.StoreZCodeAccount(fsys, roots.Config, zcode.LoginSession{UserID: "expired-other", ZCodeJWT: "expired-other-session", RawOAuth: "other-oauth", BusinessJWT: "other-business", DeviceMid: "other-device"}, "expired other Start", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := zaiprov.SetTransport(fsys, roots.Config, other.ID, "zcode-start"); err != nil {
		t.Fatal(err)
	}
	if result, err := pipeline.Refresher.ZAI.Fetch(context.Background()); err != nil || len(result.Entries) != 1 || calls != 4 {
		t.Fatal("unused Start account suppressed pinned healthy entitlement")
	}
	failBalance = true
	if _, err := pipeline.Refresher.ZAI.Fetch(context.Background()); err == nil {
		t.Fatal("transient quota failure became successful empty catalogue")
	}
}
