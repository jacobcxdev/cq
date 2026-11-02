package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
)

func TestZAICommandClassification(t *testing.T) {
	for _, test := range []struct {
		args []string
		row  string
	}{
		{[]string{"check", "zai"}, "check"},
		{[]string{"--json", "check", "codex", "zai"}, "check"},
		{[]string{"models", "list", "--provider", "zai"}, "models_list"},
		{[]string{"models", "overlay", "add", "--provider", "zai", "--id", "glm-future"}, "models_overlay_add"},
		{[]string{"zai", "login"}, "zai_login"},
		{[]string{"zai", "login", "--api-key-stdin", "--label", "second plan", "--activate"}, "zai_login"},
		{[]string{"zai", "accounts", "--json"}, "zai_accounts"},
		{[]string{"zai", "switch", "second"}, "zai_switch"},
		{[]string{"zai", "remove", "second"}, "zai_remove"},
	} {
		authority, err := ClassifyProxyCommand(test.args)
		if err != nil || authority.Row != test.row {
			t.Errorf("ClassifyProxyCommand(%v) row = %q, err = %v; want %q", test.args, authority.Row, err, test.row)
		}
	}
}

type zaiCommandDoer func(*http.Request) (*http.Response, error)

func (f zaiCommandDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func zaiTestDependencies(fsys fsutil.FileSystem, out io.Writer) zaiCommandDependencies {
	return zaiCommandDependencies{
		fs: fsys, configDir: func() (string, error) { return "/config", nil },
		getenv: func(string) string { return "" }, output: out,
		client: zaiCommandDoer(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"level":"pro","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":10}]}}`))}, nil
		}),
		invalidate: func() {},
	}
}

func TestZAILoginPreservesExistingAccountAndListsWithoutKeys(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.WriteFile("/config/zai.json", []byte(`{"api_key":"original-private-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	deps := zaiTestDependencies(fsys, &out)
	deps.input = strings.NewReader("new-private-key\n")
	invalidated := false
	deps.invalidate = func() { invalidated = true }
	options := ZAICmd{Login: ZAILoginCmd{APIKeyStdin: true, Label: "second"}}
	if err := runZAICommand(context.Background(), "login", options, true, deps); err != nil {
		t.Fatal(err)
	}
	if !invalidated || strings.Contains(out.String(), "private-key") {
		t.Fatalf("login invalidated=%t, output=%q", invalidated, out.String())
	}
	accounts, err := zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].APIKey != "original-private-key" || accounts[1].APIKey != "new-private-key" {
		t.Fatal("login did not preserve both subscriptions")
	}
	out.Reset()
	if err := runZAICommand(context.Background(), "accounts", ZAICmd{}, true, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private-key") || !strings.Contains(out.String(), "second") || !strings.Contains(out.String(), `"preferred":true`) {
		t.Fatalf("unsafe or incomplete listing: %q", out.String())
	}
	if err := runZAICommand(context.Background(), "switch <reference>", ZAICmd{Switch: ZAIReferenceCmd{Reference: "second"}}, false, deps); err != nil {
		t.Fatal(err)
	}
	key, _ := zaiprov.ReadAPIKey(fsys, deps.getenv, "/config")
	if key != "new-private-key" {
		t.Fatal("switch did not change preferred key")
	}
	if err := runZAICommand(context.Background(), "remove <reference>", ZAICmd{Remove: ZAIReferenceCmd{Reference: "second"}}, false, deps); err != nil {
		t.Fatal(err)
	}
	key, _ = zaiprov.ReadAPIKey(fsys, deps.getenv, "/config")
	if key != "original-private-key" {
		t.Fatal("remove did not preserve original key")
	}
}

func TestZAILoginRejectedKeyDoesNotChangeCredentials(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		fsys := fsutil.NewMemFS()
		original := []byte(`{"api_key":"original-private-key"}`)
		_ = fsys.WriteFile("/config/zai.json", original, 0o600)
		var out bytes.Buffer
		deps := zaiTestDependencies(fsys, &out)
		deps.input = strings.NewReader("rejected-private-key\n")
		deps.client = zaiCommandDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("rejected-private-key"))}, nil
		})
		err := runZAICommand(context.Background(), "login", ZAICmd{Login: ZAILoginCmd{APIKeyStdin: true}}, false, deps)
		after, _ := fsys.ReadFile("/config/zai.json")
		if err == nil || !bytes.Equal(after, original) || strings.Contains(err.Error(), "private-key") {
			t.Fatalf("status=%d error=%v credentials preserved=%t", status, err, bytes.Equal(after, original))
		}
	}
}

func TestZAILoginInvalidInputNeverReachesUpstream(t *testing.T) {
	for _, key := range []string{"", "\n", "private-key\nsecond-key", "private-key\x00", strings.Repeat("x", 4097)} {
		fsys := fsutil.NewMemFS()
		deps := zaiTestDependencies(fsys, io.Discard)
		deps.input = strings.NewReader(key)
		deps.client = zaiCommandDoer(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid input reached upstream")
			return nil, errors.New("unexpected request")
		})
		err := runZAICommand(context.Background(), "login", ZAICmd{Login: ZAILoginCmd{APIKeyStdin: true}}, false, deps)
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("invalid input accepted or leaked")
		}
	}
}

func TestZAILoginUsesHiddenPrompt(t *testing.T) {
	fsys := fsutil.NewMemFS()
	var out bytes.Buffer
	deps := zaiTestDependencies(fsys, &out)
	prompted := false
	deps.promptKey = func() ([]byte, error) {
		prompted = true
		return []byte("prompt-private-key"), nil
	}
	if err := runZAICommand(context.Background(), "login", ZAICmd{}, false, deps); err != nil {
		t.Fatal(err)
	}
	if !prompted || strings.Contains(out.String(), "private-key") {
		t.Fatal("hidden prompt not used or key exposed")
	}
}

func TestZAICommandRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"zai"}, {"zai", "login", "private-key"}, {"zai", "login", "--api-key", "private-key"},
		{"zai", "accounts", "extra"}, {"zai", "switch"}, {"zai", "remove"},
	} {
		authority, err := ClassifyProxyCommand(args)
		if err != nil || !authority.Terminating || authority.Row != "ordinary_usage_error" {
			t.Fatalf("invalid args classified as runnable: %v", args)
		}
	}
}
