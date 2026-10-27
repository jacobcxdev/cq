package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestResetExecutorStatusRequiresAuthenticatedFreshHeartbeat(t *testing.T) {
	now := time.Now().UTC()
	for _, age := range []time.Duration{0, 2 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != proxy.RuntimeCodexResetScheduleStatusPath || r.Header.Get("Authorization") != "Bearer local-test-token" {
					t.Error("wrong executor probe")
				}
				_ = json.NewEncoder(w).Encode(proxy.CodexResetSchedulerStatus{Running: true, State: "running", CheckedAt: now.Add(-age)})
			}))
			defer server.Close()
			_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			status := codexResetExecutorStatusWithDependencies(context.Background(), proxyLeaseDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local-test-token"}, nil }, Doer: server.Client()}, now)
			if status.Running != (age == 0) {
				t.Fatalf("bad freshness %+v", status)
			}
		})
	}
}
