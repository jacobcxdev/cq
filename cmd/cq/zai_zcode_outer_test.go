package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func TestZCodeOuterLoginUsedInjectedOAuthAndKeptJSONClean(t *testing.T) {
	fsys := fsutil.NewMemFS()
	var output, diagnostics bytes.Buffer
	deps := zaiTestDependencies(fsys, &output)
	deps.errOutput = &diagnostics
	deps.deviceMid = func() (string, error) { return "persisted-device", nil }
	opened := false
	deps.openURL = func(_ context.Context, target string) error {
		opened = true
		if target != "https://chat.z.ai/fixture-login" {
			t.Fatal("login changed injected browser target")
		}
		return nil
	}
	deps.promptKey = func() ([]byte, error) { t.Fatal("OAuth login prompted API key"); return nil, nil }
	deps.client = zaiCommandDoer(func(*http.Request) (*http.Response, error) { t.Fatal("fake login sent HTTP"); return nil, nil })
	called, invalidated := false, false
	deps.invalidate = func() { invalidated = true }
	deps.oauthLogin = func(_ context.Context, client zcode.OAuthClient) (zcode.LoginSession, error) {
		called = true
		if client.Version != zcode.ClientVersion || client.DeviceMid != "persisted-device" {
			t.Fatal("login lost ZCode identity or device context")
		}
		if err := client.OpenURL(context.Background(), "https://chat.z.ai/fixture-login"); err != nil {
			return zcode.LoginSession{}, err
		}
		return zcode.LoginSession{UserID: "outer-user", Email: "outer@example.com", ZCodeJWT: "private-zcode", RawOAuth: "private-oauth", BusinessJWT: "private-business", RefreshToken: "private-refresh"}, nil
	}
	activate := true
	if err := runZAICommand(context.Background(), "login", ZAICmd{Login: ZAILoginCmd{ZCode: true, Label: "browser", Activate: &activate}}, true, deps); err != nil {
		t.Fatal(err)
	}
	if !called || !invalidated || !opened || !strings.Contains(diagnostics.String(), "Complete ZCode sign-in") {
		t.Fatal("OAuth login/invalidation not invoked")
	}
	decoder := json.NewDecoder(&output)
	var public map[string]any
	if decoder.Decode(&public) != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("OAuth --json output contained prose")
	}
	for _, value := range public {
		if text, ok := value.(string); ok && strings.Contains(text, "private-") {
			t.Fatal("OAuth JSON leaked credentials")
		}
	}
	accounts, err := zaiprov.ReadAccounts(fsys, func(string) string { return "" }, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].OAuth == nil || accounts[0].OAuth.DeviceMid != "persisted-device" || accounts[0].Label != "browser" {
		t.Fatal("OAuth login did not persist selected device/account")
	}
}

func TestZCodeOuterLoginRejectedConflictingAuthenticationBeforeReads(t *testing.T) {
	deps := zaiTestDependencies(fsutil.NewMemFS(), io.Discard)
	deps.deviceMid = func() (string, error) { t.Fatal("conflicting login read device state"); return "", nil }
	deps.oauthLogin = func(context.Context, zcode.OAuthClient) (zcode.LoginSession, error) {
		t.Fatal("conflicting login started OAuth")
		return zcode.LoginSession{}, nil
	}
	if err := runZAICommand(context.Background(), "login", ZAICmd{Login: ZAILoginCmd{ZCode: true, APIKeyStdin: true}}, true, deps); err == nil {
		t.Fatal("conflicting authentication accepted")
	}
}

func TestZCodeOuterStatusUsedPersistedAccountDeviceAndInjectedHTTP(t *testing.T) {
	var output bytes.Buffer
	requests := 0
	account := zcodeResetTestAccount("stored-oauth", "browser")
	deps := zcodeCommandDependencies{Accounts: func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{{ID: "native", APIKey: "private-key"}, account}, nil
	}, Out: &output, Client: zcode.Client{Version: zcode.ClientVersion, HTTP: zaiCommandDoer(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Header.Get("X-Device-Mid") != "device-stored-oauth" || r.Header.Get("Authorization") != "Bearer zcode-stored-oauth" {
			t.Fatal("status used another account or a device lookup")
		}
		if strings.Contains(r.URL.Path, "billing/balance") {
			return zcodeResetTestResponse(`{"code":0,"data":{"server_time":1800000000,"plans":[],"balances":[]}}`), nil
		}
		return zcodeResetTestResponse(zcodeResetTestStatus(false, time.Unix(1_800_000_000, 0))), nil
	})}}
	if err := runZAIZCodeCommand(context.Background(), "status", ZAIZCodeCmd{}, true, deps); err != nil {
		t.Fatal(err)
	}
	var result struct {
		AccountID string `json:"account_id"`
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || result.AccountID != "stored-oauth" || requests != 2 {
		t.Fatal("status dispatch or JSON output incorrect")
	}
}

func TestZCodeOuterResetUseDispatchedAndInvalidatedQuotaCache(t *testing.T) {
	posts := 0
	resetDeps, output, _ := zcodeResetTestDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return zcodeResetTestResponse(zcodeResetTestStatus(true, time.Unix(1_800_000_000, 0))), nil
		}
		posts++
		return zcodeResetTestResponse(`{"code":0,"data":{"used":true}}`), nil
	})
	invalidated := 0
	deps := zcodeCommandDependencies{Out: output, Resets: resetDeps, Invalidate: func() { invalidated++ }}
	opts := ZAIZCodeCmd{Resets: ZAIZCodeResetsCmd{Use: ZAIZCodeResetUseCmd{Reference: "primary", Window: "5h", Yes: true}}}
	if err := runZAIZCodeCommand(context.Background(), "resets use <reference>", opts, true, deps); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || invalidated != 1 || !strings.Contains(output.String(), `"accepted":true`) {
		t.Fatal("reset dispatch failed to invalidate quota cache")
	}
}

func TestZCodeOuterClaimsDispatchedAndInvalidatedOnlyAfterClaim(t *testing.T) {
	for _, operation := range []string{"list", "claim"} {
		t.Run(operation, func(t *testing.T) {
			posts := 0
			claimDeps, output, _, _ := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					return zcodeClaimResponse(zcodeClaimAcceptedFixture), nil
				}
				if r.URL.Path == "/api/v1/client/configs" {
					return zcodeClaimResponse(zcodeClaimConfigFixture), nil
				}
				return zcodeClaimResponse(zcodeClaimOfferFixture), nil
			})
			invalidated := 0
			deps := zcodeCommandDependencies{Out: output, Claims: claimDeps, Invalidate: func() { invalidated++ }}
			opts := ZAIZCodeCmd{Claims: ZAIZCodeClaimsCmd{List: ZAIZCodeClaimListCmd{Reference: "primary"}, Claim: ZAIZCodeClaimCmd{Reference: "primary", PlanID: "gift-plan", Yes: true}}}
			command := "claims list <reference>"
			if operation == "claim" {
				command = "claims claim <reference> <plan-id>"
			}
			if err := runZAIZCodeCommand(context.Background(), command, opts, true, deps); err != nil {
				t.Fatal(err)
			}
			want := 0
			if operation == "claim" {
				want = 1
			}
			if posts != want || invalidated != want {
				t.Fatal("claim dispatch/invalidation incorrect")
			}
		})
	}
}

func TestZCodeOuterCommandTreeAcceptedExplicitOAuthClaimsAndResets(t *testing.T) {
	for _, args := range [][]string{{"login", "--zcode"}, {"login", "--api-key-stdin"}, {"zcode", "status"}, {"zcode", "resets", "issue", "account"}, {"zcode", "resets", "use", "account", "--window", "7d", "--new", "--yes"}, {"zcode", "claims", "list", "account"}, {"zcode", "claims", "claim", "account", "plan", "--yes"}} {
		var tree ZAICmd
		parser, err := kong.New(&tree, kong.Writers(io.Discard, io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parser.Parse(args); err != nil {
			t.Fatalf("command %v rejected: %v", args, err)
		}
	}
}

func TestZCodeOuterDeviceRegistrationWorkedWithoutInstalledApp(t *testing.T) {
	fsys := fsutil.NewMemFS()
	first, err := ensureZCodeCommandDevice(fsys, "/config", "/not-installed/telemetry-state.json")
	if err != nil || !zcodeResetUUID(first) {
		t.Fatal("fresh device registration failed")
	}
	second, err := ensureZCodeCommandDevice(fsys, "/config", "/not-installed/telemetry-state.json")
	if err != nil || second != first {
		t.Fatal("device registration changed after restart")
	}
	if _, err := fsys.ReadFile("/not-installed/telemetry-state.json"); err == nil {
		t.Fatal("device registration changed ZCode-owned state")
	}
	file, err := fsys.Stat("/config/zcode-device.json")
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatal("device registration file was not private")
	}
	dir, err := fsys.Stat("/config")
	if err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatal("device registration directory was not private")
	}
	_ = fsys.WriteFile("/config/zcode-device.json", []byte(`{"deviceMid":"invalid\nvalue"}`), 0o600)
	if _, err := ensureZCodeCommandDevice(fsys, "/config", "/not-installed/telemetry-state.json"); err == nil {
		t.Fatal("corrupt device identity was replaced")
	}
}

func TestZCodeOuterDeviceReusedInstalledMetadataWithoutCredentials(t *testing.T) {
	fsys := fsutil.NewMemFS()
	installed := filepath.Join("/installed", "telemetry-state.json")
	data := []byte(`{"deviceMid":"existing-device","unrelated":"left-intact"}`)
	if err := fsys.WriteFile(installed, data, 0o600); err != nil {
		t.Fatal(err)
	}
	device, err := ensureZCodeCommandDevice(fsys, "/config", installed)
	if err != nil || device != "existing-device" {
		t.Fatal("installed device metadata was not retained")
	}
	after, err := fsys.ReadFile(installed)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("installed metadata changed")
	}
	if err := fsys.WriteFile(installed, []byte(`{"deviceMid":"different-device"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	device, err = ensureZCodeCommandDevice(fsys, "/config", installed)
	if err != nil || device != "existing-device" {
		t.Fatal("CQ-owned identity changed after installed metadata changed")
	}
}
