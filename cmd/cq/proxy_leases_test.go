package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

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
