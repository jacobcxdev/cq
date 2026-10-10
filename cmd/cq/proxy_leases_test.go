package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyLeasesInvalidateUsesAuthenticatedLoopback(t *testing.T) {
	var gotMethod, gotPath, gotAuthorization string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod, gotPath, gotAuthorization = request.Method, request.URL.Path, request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"invalidated_leases":3,"journal_generation":42}`))
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	var output bytes.Buffer
	err = runProxyLeasesWithDependencies(context.Background(), []string{"invalidate", "--port", strconv.Itoa(port)}, &output, proxyLeaseDependencies{
		LoadConfig: func() (*proxy.Config, error) {
			return &proxy.Config{Port: port, LocalToken: "local-token"}, nil
		},
		Doer: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != proxy.RuntimeCodexLeaseInvalidationPath || gotAuthorization != "Bearer local-token" {
		t.Fatalf("request = %s %s auth=%q", gotMethod, gotPath, gotAuthorization)
	}
	if output.String() != "{\"invalidated_leases\":3,\"journal_generation\":42}\n" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestProxyLeasesInvalidateRejectsInvalidArgumentsBeforeRequest(t *testing.T) {
	called := false
	err := runProxyLeasesWithDependencies(context.Background(), []string{"invalidate", "--port", "0"}, &bytes.Buffer{}, proxyLeaseDependencies{
		LoadConfig: func() (*proxy.Config, error) {
			called = true
			return &proxy.Config{}, nil
		},
		Doer: http.DefaultClient,
	})
	if err == nil || called {
		t.Fatalf("error=%v load-called=%v", err, called)
	}
}

func TestProxyLeasesRedistributeUsesAuthenticatedLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/_cq/control/codex/leases/redistribute" || request.Header.Get("Authorization") != "Bearer local-token" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"scheduled_leases":3,"deferred_leases":1,"journal_generation":42}`))
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	var output bytes.Buffer
	err := runProxyLeasesWithDependencies(context.Background(), []string{"redistribute", "--port", strconv.Itoa(port)}, &output, proxyLeaseDependencies{
		LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "local-token"}, nil },
		Doer:       server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "{\"scheduled_leases\":3,\"deferred_leases\":1,\"journal_generation\":42}\n" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestProxyLeasesRedistributeRejectsInvalidResponse(t *testing.T) {
	for _, body := range []string{
		`{"scheduled_leases":3,"deferred_leases":1,"journal_generation":42} {}`,
		`{"scheduled_leases":3,"unknown":1}`,
		`not-json`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()
			port := server.Listener.Addr().(*net.TCPAddr).Port
			var output bytes.Buffer
			err := runProxyLeasesWithDependencies(context.Background(), []string{"redistribute"}, &output, proxyLeaseDependencies{
				LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port}, nil },
				Doer:       server.Client(),
			})
			if err == nil || output.Len() != 0 {
				t.Fatalf("error = %v, output = %q", err, output.String())
			}
		})
	}
}

func TestNotifyProxyCodexResetUsesFreshResetControl(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != proxy.RuntimeCodexLeaseRedistributionPath || request.Header.Get("Authorization") != "Bearer local-token" {
			t.Fatal("incorrect reset request")
		}
		var payload proxy.CodexLeaseResetRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload.AccountKey != "account:test" || payload.EventID != "event" {
			t.Errorf("payload=%#v, error=%v", payload, err)
		}
		_, _ = writer.Write([]byte(`{"scheduled_leases":3,"deferred_leases":1,"journal_generation":42}`))
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	err := notifyProxyCodexResetWithDependencies(context.Background(), codex.AccountKey("account:test"), "event", proxyLeaseDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local-token"}, nil }, Doer: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
}
