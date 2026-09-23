package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestSystemdServiceAuthenticatedProxyHealth(t *testing.T) {
	for _, scenario := range []string{"valid", "missing token", "wrong token", "unauthenticated only", "rescue mode", "bad health", "redirect", "bad json", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/health" {
					if scenario == "bad health" {
						w.Write([]byte(`{"status":"failed"}`))
					} else {
						w.Write([]byte(`{"status":"ok"}`))
					}
					return
				}
				if r.URL.Path != proxy.RuntimeRescueStatusPath {
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if scenario == "unauthenticated only" {
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("Authorization") != "Bearer installed-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch scenario {
				case "redirect":
					http.Redirect(w, r, "/health", http.StatusFound)
				case "rescue mode":
					w.Write([]byte(`{"mode":"rescue"}`))
				case "bad json":
					w.Write([]byte(`invalid`))
				case "oversized":
					w.Write([]byte(strings.Repeat("x", 1<<20+1)))
				default:
					w.Write([]byte(`{"mode":"normal"}`))
				}
			}))
			defer server.Close()
			token := "installed-token"
			if scenario == "missing token" {
				token = ""
			}
			if scenario == "wrong token" {
				token = "wrong"
			}
			got := probeSelectedLinuxProxyHealth(context.Background(), strings.TrimPrefix(server.URL, "http://"), token)
			if got != (scenario == "valid") {
				t.Fatalf("health=%v for %s", got, scenario)
			}
		})
	}
}

func TestSystemdServiceAuthenticatedHealthBoundaries(t *testing.T) {
	t.Run("status before unused body", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.(http.Flusher).Flush()
			<-release
		}))
		defer server.Close()
		defer close(release)
		result := make(chan bool, 1)
		go func() {
			result <- probeSelectedLinuxProxyHealth(context.Background(), strings.TrimPrefix(server.URL, "http://"), "token")
		}()
		select {
		case healthy := <-result:
			if healthy {
				t.Fatal("unauthorised health accepted")
			}
		case <-time.After(time.Second):
			t.Fatal("read unused unauthorised body")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if probeSelectedLinuxProxyHealth(ctx, strings.TrimPrefix(server.URL, "http://"), "token") || ctx.Err() != context.DeadlineExceeded {
			t.Fatal("health ignored shared deadline")
		}
	})
	t.Run("nonloopback rejected", func(t *testing.T) {
		if probeSelectedLinuxProxyHealth(context.Background(), "example.invalid:1234", "token") {
			t.Fatal("remote target accepted")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if probeSelectedLinuxProxyHealth(ctx, "127.0.0.1:1", "token") {
			t.Fatal("cancelled health accepted")
		}
	})
	t.Run("environment proxy ignored", func(t *testing.T) {
		t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == proxy.RuntimeRescueStatusPath {
				w.Write([]byte(`{"mode":"normal"}`))
			} else {
				w.Write([]byte(`{"status":"ok"}`))
			}
		}))
		defer server.Close()
		if !probeSelectedLinuxProxyHealth(context.Background(), strings.TrimPrefix(server.URL, "http://"), "token") {
			t.Fatal("environment proxy altered local health")
		}
	})
}
