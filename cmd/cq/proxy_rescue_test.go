package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

type refusedRescueDoer struct{}

func (refusedRescueDoer) Do(*http.Request) (*http.Response, error) {
	return nil, syscall.ECONNREFUSED
}

type rescueDoerFunc func(*http.Request) (*http.Response, error)

func (do rescueDoerFunc) Do(request *http.Request) (*http.Response, error) { return do(request) }

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
	}, server.Client(), nil)
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
	}, http.DefaultClient, nil)
	if err == nil || called {
		t.Fatalf("error=%v load-called=%v", err, called)
	}
}

func TestProxyRescueControlExplainsMissingListener(t *testing.T) {
	err := runProxyRescueWithDependencies(context.Background(), []string{"enter"}, &bytes.Buffer{}, func() (*proxy.Config, error) {
		return &proxy.Config{Port: 19280, LocalToken: "local-token"}, nil
	}, refusedRescueDoer{}, nil)
	if !errors.Is(err, syscall.ECONNREFUSED) || !strings.Contains(err.Error(), "cq proxy restart") {
		t.Fatalf("error = %v, want restart guidance with original cause", err)
	}
}

func TestProxyRescueEnterRestoresMissingListener(t *testing.T) {
	restarts := 0
	requests := 0
	var output bytes.Buffer
	err := runProxyRescueWithDependencies(context.Background(), []string{"enter"}, &output, func() (*proxy.Config, error) {
		return &proxy.Config{Port: 19280, LocalToken: "local-token"}, nil
	}, rescueDoerFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if restarts == 0 {
			return nil, syscall.ECONNREFUSED
		}
		if request.Header.Get("Authorization") != "Bearer local-token" {
			t.Fatal("recovery request lost local authentication")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("rescue entered\n"))}, nil
	}), func() error {
		restarts++
		return nil
	})
	if err != nil || restarts != 1 || requests != 2 || output.String() != "rescue entered\n" {
		t.Fatalf("error=%v restarts=%d requests=%d output=%q", err, restarts, requests, output.String())
	}
}

func TestProxyRescueExplicitPortDoesNotRestartService(t *testing.T) {
	restarts := 0
	err := runProxyRescueWithDependencies(context.Background(), []string{"enter", "--port", "19281"}, &bytes.Buffer{}, func() (*proxy.Config, error) {
		return &proxy.Config{Port: 19280, LocalToken: "local-token"}, nil
	}, refusedRescueDoer{}, func() error {
		restarts++
		return nil
	})
	if !errors.Is(err, syscall.ECONNREFUSED) || restarts != 0 {
		t.Fatalf("error=%v restarts=%d", err, restarts)
	}
}
