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

func TestProxyRescueControlUsesAuthenticatedLoopback(t *testing.T) {
	var gotMethod, gotPath, gotAuthorization string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod, gotPath, gotAuthorization = request.Method, request.URL.Path, request.Header.Get("Authorization")
		_, _ = writer.Write([]byte("{\"mode\":\"rescue\"}\n"))
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
	err = runProxyRescueWithDependencies(context.Background(), []string{"enter", "--port", strconv.Itoa(port)}, &output, func() (*proxy.Config, error) {
		return &proxy.Config{Port: port, LocalToken: "local-token"}, nil
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != proxy.RuntimeRescueEnterPath || gotAuthorization != "Bearer local-token" {
		t.Fatalf("request = %s %s auth=%q", gotMethod, gotPath, gotAuthorization)
	}
	if output.String() != "{\"mode\":\"rescue\"}\n" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestProxyRescueControlRejectsInvalidArgumentsBeforeRequest(t *testing.T) {
	called := false
	err := runProxyRescueWithDependencies(context.Background(), []string{"enter", "--port", "19280", "--extra"}, &bytes.Buffer{}, func() (*proxy.Config, error) {
		called = true
		return &proxy.Config{}, nil
	}, http.DefaultClient)
	if err == nil || called {
		t.Fatalf("error=%v load-called=%v", err, called)
	}
}
