package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyReserveControl(t *testing.T) {
	for _, command := range []string{"set", "status", "windows", "disable", "enable", "clear"} {
		t.Run(command, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/_cq/control/reserve" || r.Header.Get("Authorization") != "Bearer local" {
					t.Errorf("wrong request: %s", r.URL.Path)
				}
				if command == "status" || command == "windows" {
					if r.Method != http.MethodGet {
						t.Errorf("method = %s", r.Method)
					}
				} else {
					var body struct {
						Action  string  `json:"action"`
						Window  string  `json:"window"`
						Percent float64 `json:"percent"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if r.Method != http.MethodPost || body.Action != command {
						t.Errorf("mutation = %#v", body)
					}
					if command == "set" && (body.Window != "7d" || body.Percent != 2) {
						t.Errorf("set = %#v", body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"configured":false,"windows":{}}`))
			}))
			defer server.Close()
			port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
			args := []string{command, "--json"}
			if command == "set" {
				args = append(args, "--window", "7d", "--percent", "2")
			}
			var output bytes.Buffer
			err := runProxyReserveWithDependencies(context.Background(), args, &output, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local"}, nil }, Doer: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(output.Bytes()) {
				t.Fatalf("invalid JSON: %s", output.String())
			}
		})
	}
}

func TestProxyReserveRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{nil, {"set"}, {"set", "--window", "7d", "--percent", "NaN"}, {"set", "--window", "7d", "--percent", "101"}, {"set", "--window", "7d", "--percent", "0"}, {"disable", "--window", "7d"}, {"wat"}, {"status", "--json", "--json"}} {
		if err := runProxyReserveWithDependencies(context.Background(), args, &bytes.Buffer{}, proxyPolicyDependencies{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestProxyReserveReportsUnavailableService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	err := runProxyReserveWithDependencies(context.Background(), []string{"status"}, &bytes.Buffer{}, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local"}, nil }, Doer: server.Client()})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("error = %v", err)
	}
}
