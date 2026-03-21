package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchUsageAccountIDHeader(t *testing.T) {
	var gotAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &urlRewriter{client: srv.Client(), baseURL: srv.URL}
	_, _, err := fetchUsage(context.Background(), client, "test-token", "acct-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAccountID != "acct-123" {
		t.Errorf("ChatGPT-Account-Id = %q, want acct-123", gotAccountID)
	}
}

func TestFetchUsageNoAccountIDHeader(t *testing.T) {
	var gotAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &urlRewriter{client: srv.Client(), baseURL: srv.URL}
	_, _, err := fetchUsage(context.Background(), client, "test-token", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAccountID != "" {
		t.Errorf("ChatGPT-Account-Id = %q, want empty (not set when accountID is empty)", gotAccountID)
	}
}
