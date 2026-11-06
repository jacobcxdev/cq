package grok

import (
	"bytes"
	"context"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"io"
	"net/http"
	"strings"
	"testing"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthorityRebuildsSubscriptionHeaders(t *testing.T) {
	a := testAuthority(t, nil)
	account, err := a.SaveLogin(context.Background(), loginFixture("one"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.Client = doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer access-private" || r.Header.Get("X-Userid") != "one" || r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" {
			t.Fatal(r.Header)
		}
		for _, name := range []string{"Cookie", "Chatgpt-Account-Id", "X-Local-Secret"} {
			if r.Header.Get(name) != "" {
				t.Fatalf("leaked header %s", name)
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	request, _ := http.NewRequest(http.MethodGet, BaseURL+"/billing?format=credits", nil)
	request.Header.Set("Authorization", "Bearer local-private")
	request.Header.Set("Cookie", "other-private")
	request.Header.Set("Chatgpt-Account-Id", "other-account")
	request.Header.Set("X-Local-Secret", "local")
	response, err := a.Do(context.Background(), account.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 || request.Header.Get("Authorization") != "Bearer local-private" {
		t.Fatal("caller request changed")
	}
	for _, target := range []string{"https://api.x.ai/v1/responses", "https://cli-chat-proxy.grok.com.attacker.test/v1/responses", "http://cli-chat-proxy.grok.com/v1/responses", "https://cli-chat-proxy.grok.com/other"} {
		r, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := a.Do(context.Background(), account.ID, r); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	if calls != 1 {
		t.Fatal("untrusted endpoint dispatched")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAuthorityRejectsRedirectAndScopesResetRead(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://attacker.test/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	a := NewAuthority(Store{FS: fsutil.OSFileSystem{}, ConfigDir: privateTestDir(t)}, client, nil)
	account, err := a.SaveLogin(context.Background(), loginFixture("one"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, BaseURL+"/models", nil)
	response, err := a.Do(context.Background(), account.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 || response.StatusCode != 302 {
		t.Fatal("followed credential redirect")
	}
	a.Client = doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://grok.com/prod_mc_billing.ConsumerUiSvc/GetRemainingResets" || r.Method != http.MethodPost {
			t.Fatal("wrong reset scope")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte{0, 0, 0, 0, 0}) {
			t.Fatal("wrong reset body")
		}
		if r.Header.Get("X-Grpc-Web") != "1" || r.Header.Get("Authorization") != "Bearer access-private" {
			t.Fatal("wrong reset headers")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	reset, _ := http.NewRequest(http.MethodPost, "https://grok.com/prod_mc_billing.ConsumerUiSvc/GetRemainingResets", bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	response, err = a.DoResetRead(context.Background(), account.ID, reset)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for _, target := range []string{"https://grok.com/prod_mc_billing.ConsumerUiSvc/ResetUsage", "https://grok.com/prod_mc_billing.ConsumerUiSvc/GetRemainingResets?x=1", "https://other.test/prod_mc_billing.ConsumerUiSvc/GetRemainingResets"} {
		r, _ := http.NewRequest(http.MethodPost, target, nil)
		if _, err := a.DoResetRead(context.Background(), account.ID, r); err == nil {
			t.Fatal("accepted other reset scope")
		}
	}
	if calls != 2 {
		t.Fatal("dispatched invalid scoped request")
	}
}
