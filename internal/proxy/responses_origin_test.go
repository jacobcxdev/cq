package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponsesRejectsNonLocalBrowserOrigin(t *testing.T) {
	server := &Server{Config: &Config{ClaudeUpstream: "http://127.0.0.1"}}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, endpoint string
	}{
		{http.MethodPost, "/responses"},
		{http.MethodPost, "/v1/responses"},
		{http.MethodPost, "/responses/compact"},
		{http.MethodPost, "/v1/responses/compact"},
		{http.MethodGet, "/responses"},
	} {
		t.Run(tc.method+" "+tc.endpoint, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://127.0.0.1:19280"+tc.endpoint, nil)
			req.Header.Set("Origin", "https://browser.example")
			if tc.method == http.MethodGet {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status %d, want forbidden", response.Code)
			}
		})
	}
}

func TestAllowsResponsesOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, host, origin, fetchSite string
		want                          bool
	}{
		{"native", "127.0.0.1:19280", "", "", true},
		{"local IPv4", "127.0.0.1:19280", "http://127.0.0.1:19280", "same-origin", true},
		{"local IPv6", "[::1]:19280", "http://[::1]:19280", "", true},
		{"loopback other port", "127.0.0.1:19280", "http://127.0.0.1:3000", "", false},
		{"remote host name", "browser.example", "http://browser.example", "same-origin", false},
		{"opaque origin", "127.0.0.1:19280", "null", "", false},
		{"cross-site fetch", "127.0.0.1:19280", "", "cross-site", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/responses", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			if got := allowsResponsesOrigin(req); got != tc.want {
				t.Fatalf("allows origin = %v, want %v", got, tc.want)
			}
		})
	}
}
