package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

const zaiResetCLIInventory = `{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":"first","grantType":"CAMPAIGN","available":true,"expireTime":"2026-11-04T12:00:00Z"}],"weekResets":[]}}`

func TestZAIResetCommandClassification(t *testing.T) {
	for _, args := range [][]string{{"zai", "resets", "list"}, {"zai", "resets", "list", "primary"}, {"zai", "resets", "use", "primary", "--window", "7d", "--credit", "7d:card", "--yes"}} {
		authority, err := ClassifyProxyCommand(args)
		if err != nil || authority.Terminating || authority.Row != "zai_resets" {
			t.Fatalf("reset command not runnable: %v authority=%+v err=%v", args, authority, err)
		}
	}
}

func zaiResetTestDeps(out io.Writer) zaiResetsDependencies {
	return zaiResetsDependencies{
		Accounts: func() ([]zaiprov.Account, error) {
			return []zaiprov.Account{{ID: "zai_test", Label: "primary", APIKey: "private-key"}}, nil
		},
		FS: fsutil.NewMemFS(), StateDir: func() (string, error) { return "/state/zai-resets", nil },
		In: strings.NewReader(""), Out: out, ErrOut: out,
		Now:       func() time.Time { return time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC) },
		RequestID: func() string { return "fixed-request-id" }, Invalidate: func() {},
	}
}

func TestZAIResetUseDefaultsToNo(t *testing.T) {
	var out bytes.Buffer
	deps := zaiResetTestDeps(&out)
	deps.Client.HTTP = zaiCommandDoer(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("unconfirmed card consumed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(zaiResetCLIInventory))}, nil
	})
	err := runZAIResetsCommand(context.Background(), "use <reference>", ZAIResetsCmd{Use: ZAIResetsUseCmd{Reference: "primary", Window: "5h"}}, false, deps)
	if err == nil || !strings.Contains(err.Error(), "cancelled") || !strings.Contains(out.String(), "[y/N]") || strings.Contains(out.String(), "private-key") {
		t.Fatalf("confirmation=%q error=%v", out.String(), err)
	}
}

func TestZAIResetUseExplicitConfirmationAndJSON(t *testing.T) {
	for _, yes := range []bool{false, true} {
		var out bytes.Buffer
		deps := zaiResetTestDeps(&out)
		deps.In = strings.NewReader("yes\n")
		posts := 0
		invalidated := false
		deps.Invalidate = func() { invalidated = true }
		deps.Client.HTTP = zaiCommandDoer(func(r *http.Request) (*http.Response, error) {
			body := zaiResetCLIInventory
			if r.Method == http.MethodPost {
				posts++
				body = `{"code":200,"success":true}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		err := runZAIResetsCommand(context.Background(), "use <reference>", ZAIResetsCmd{Use: ZAIResetsUseCmd{Reference: "primary", Window: "5h", Yes: yes}}, true, deps)
		if err != nil || posts != 1 || !invalidated || !strings.Contains(out.String(), `"accepted":true`) || strings.Contains(out.String(), "private-key") {
			t.Fatalf("posts=%d invalidated=%t out=%q err=%v", posts, invalidated, out.String(), err)
		}
	}
}

func TestZAIResetListKeepsHealthyAccountAndOmitsKeys(t *testing.T) {
	var out bytes.Buffer
	deps := zaiResetTestDeps(&out)
	deps.Accounts = func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{{ID: "first", APIKey: "bad-private-key"}, {ID: "second", APIKey: "good-private-key"}}, nil
	}
	deps.Client.HTTP = zaiCommandDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "bad-private-key" {
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("bad-private-key"))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(zaiResetCLIInventory))}, nil
	})
	err := runZAIResetsCommand(context.Background(), "list", ZAIResetsCmd{}, true, deps)
	if err == nil || !strings.Contains(out.String(), `"account_id":"second"`) || !strings.Contains(out.String(), `"id":"5h:first"`) || strings.Contains(out.String(), "private-key") {
		t.Fatalf("partial inventory lost or leaked: %s err=%v", out.String(), err)
	}
}

func TestZAIResetCLIListedAndUsedOAuthExactCards(t *testing.T) {
	var output bytes.Buffer
	deps := zaiResetTestDeps(&output)
	deps.Accounts = func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{{ID: "oauth-stable", Label: "browser", OAuth: &zcode.LoginSession{UserID: "user", BusinessJWT: "private-business", ZCodeJWT: "private-zcode", RawOAuth: "private-oauth"}}}, nil
	}
	posts := 0
	deps.Client.HTTP = zaiCommandDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "private-business" || r.Header.Get("User-Agent") != "ZCode/"+zcode.ClientVersion {
			t.Fatal("CLI did not use selected OAuth business identity")
		}
		body := zaiResetCLIInventory
		if r.Method == http.MethodPost {
			posts++
			body = `{"code":200,"success":true}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err := runZAIResetsCommand(t.Context(), "list <reference>", ZAIResetsCmd{List: ZAIResetsListCmd{Reference: "browser"}}, true, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id":"5h:first"`) || strings.Contains(output.String(), "private-") || posts != 0 {
		t.Fatal("OAuth card listing failed, leaked or consumed a card")
	}
	output.Reset()
	if err := runZAIResetsCommand(t.Context(), "use <reference>", ZAIResetsCmd{Use: ZAIResetsUseCmd{Reference: "browser", Credit: "5h:first", Window: "5h", Yes: true}}, true, deps); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || !strings.Contains(output.String(), `"credit_id":"5h:first"`) || strings.Contains(output.String(), "private-") {
		t.Fatal("OAuth exact-card CLI use failed or leaked")
	}
}
