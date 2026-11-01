package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestCheckCodexHTTPClientRetainsDefault(t *testing.T) {
	for _, test := range []struct {
		name      string
		providers []provider.ID
		config    *proxy.Config
		loadError error
		wantLoads int
	}{
		{name: "Codex not selected", providers: []provider.ID{provider.Claude, provider.Gemini}},
		{name: "no proxy config", providers: []provider.ID{provider.Claude, provider.Codex, provider.Gemini}, loadError: os.ErrNotExist, wantLoads: 1},
		{name: "egress unset", providers: []provider.ID{provider.Claude, provider.Codex, provider.Gemini}, config: &proxy.Config{Port: 12345}, wantLoads: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &http.Client{}
			loads := 0
			client, closeClient := checkCodexHTTPClient(test.providers, base, checkClientDependencies{
				loadConfig: func() (*proxy.Config, error) {
					loads++
					return test.config, test.loadError
				},
				openEgress: func(int) (http.RoundTripper, func() error, error) {
					t.Fatal("unexpected runtime broker open")
					return nil, nil, nil
				},
			})
			if client != base || closeClient != nil || loads != test.wantLoads {
				t.Fatalf("default client retained = %t, cleanup present = %t, config loads = %d", client == base, closeClient != nil, loads)
			}
		})
	}
}

func TestCheckCodexHTTPClientUsesRuntimeBroker(t *testing.T) {
	for _, config := range []*proxy.Config{
		{Port: 23456, CodexWireGuardProfile: "/private/synthetic/wireguard.conf"},
		{Port: 34567, CodexEgressProxy: "socks5://proxy.invalid:1080"},
		{Port: 45678, CodexEgressProxy: "direct"},
	} {
		t.Run(config.CodexWireGuardProfile+config.CodexEgressProxy, func(t *testing.T) {
			baseCalls, brokerCalls, closes := 0, 0, 0
			base := &http.Client{Transport: checkClientTransport(func(*http.Request) (*http.Response, error) {
				baseCalls++
				return nil, errors.New("unexpected default transport")
			})}
			closeError := errors.New("synthetic cleanup error")
			client, closeClient := checkCodexHTTPClient([]provider.ID{provider.Claude, provider.Codex, provider.Gemini}, base, checkClientDependencies{
				loadConfig: func() (*proxy.Config, error) { return config, nil },
				openEgress: func(port int) (http.RoundTripper, func() error, error) {
					if port != config.Port {
						t.Fatalf("broker port = %d, want %d", port, config.Port)
					}
					return checkClientTransport(func(req *http.Request) (*http.Response, error) {
						brokerCalls++
						if req.URL.Host != "chatgpt.com" || req.Header.Get("User-Agent") != "cq/"+version {
							t.Error("request did not retain usage endpoint and CQ user agent")
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
					}), func() error { closes++; return closeError }, nil
				},
			})
			httpClient, ok := client.(*http.Client)
			if !ok || httpClient.Timeout != 10*time.Second || client == base || closeClient == nil {
				t.Fatal("configured Codex client lacks isolated transport, timeout or cleanup")
			}
			req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if err := closeClient(); !errors.Is(err, closeError) || closes != 1 {
				t.Fatalf("cleanup error = %v, calls = %d", err, closes)
			}
			if brokerCalls != 1 || baseCalls != 0 {
				t.Fatalf("broker calls = %d, default calls = %d", brokerCalls, baseCalls)
			}
		})
	}
}

func TestCheckCodexHTTPClientFailsClosedWithoutBlockingOtherProviders(t *testing.T) {
	for _, failure := range []string{"config", "broker", "request"} {
		t.Run(failure, func(t *testing.T) {
			failureError := errors.New("synthetic " + failure + " failure")
			baseCalls := 0
			base := &http.Client{Transport: checkClientTransport(func(*http.Request) (*http.Response, error) {
				baseCalls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			client, closeClient := checkCodexHTTPClient([]provider.ID{provider.Claude, provider.Codex, provider.Gemini}, base, checkClientDependencies{
				loadConfig: func() (*proxy.Config, error) {
					if failure == "config" {
						return nil, failureError
					}
					return &proxy.Config{Port: 23456, CodexWireGuardProfile: "/private/synthetic/wireguard.conf"}, nil
				},
				openEgress: func(int) (http.RoundTripper, func() error, error) {
					if failure == "broker" {
						return nil, nil, failureError
					}
					return checkClientTransport(func(*http.Request) (*http.Response, error) { return nil, failureError }), func() error { return nil }, nil
				},
			})
			if closeClient != nil {
				defer func() { _ = closeClient() }()
			}
			req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Do(req); !errors.Is(err, failureError) {
				t.Fatalf("Codex request error = %v, want configured egress failure", err)
			}
			if baseCalls != 0 {
				t.Fatal("Codex fell back to the default transport")
			}
			// Claude and Gemini retain this same client even when Codex setup fails.
			resp, err := base.Do(req)
			if err != nil {
				t.Fatalf("other provider transport failed: %v", err)
			}
			_ = resp.Body.Close()
			if baseCalls != 1 {
				t.Fatalf("other provider calls = %d", baseCalls)
			}
		})
	}
}

type checkClientTransport func(*http.Request) (*http.Response, error)

func (f checkClientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
