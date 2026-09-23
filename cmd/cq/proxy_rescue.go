package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	proxyRescueControlTimeout   = 30 * time.Second
	proxyRescueResponseMaxBytes = 64 << 10
)

func runProxyRescue(args []string, output io.Writer) error {
	return runProxyRescueContext(context.Background(), args, output)
}

func runProxyRescueContext(ctx context.Context, args []string, output io.Writer) error {
	client := &http.Client{
		Timeout: proxyRescueControlTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("proxy rescue redirect refused")
		},
	}
	load := func() (*proxy.Config, error) {
		bootstrap, err := proxy.LoadProxyRescueBootstrapConfig()
		if err != nil {
			return nil, err
		}
		return &proxy.Config{Port: bootstrap.Port, LocalToken: bootstrap.LocalToken}, nil
	}
	return runProxyRescueWithDependencies(ctx, args, output, load, client)
}

func runProxyRescueWithDependencies(ctx context.Context, args []string, output io.Writer, load func() (*proxy.Config, error), doer httputil.Doer) error {
	if ctx == nil || output == nil || load == nil || doer == nil || len(args) == 0 {
		return errors.New("usage: cq proxy rescue <enter|exit|status> [--port PORT]")
	}
	switch args[0] {
	case "enter", "exit", "status":
	default:
		return fmt.Errorf("unknown proxy rescue command: %s", args[0])
	}
	port := 0
	var err error
	if len(args) != 1 {
		if len(args) != 3 || args[1] != "--port" {
			return errors.New("usage: cq proxy rescue <enter|exit|status> [--port PORT]")
		}
		port, err = strconv.Atoi(args[2])
		if err != nil || port < 1 || port > 65535 {
			return errors.New("proxy rescue: invalid port")
		}
	}
	body, err := requestProxyRescue(ctx, args[0], port, load, doer)
	if err != nil {
		return err
	}
	_, err = output.Write(body)
	return err
}

// proxyRescueError retains the legacy diagnostic while giving canonical callers
// a typed control outcome. Untrusted response bodies never become diagnostics.
type proxyRescueError struct {
	kind   string
	status int
	cause  error
}

func (e *proxyRescueError) Error() string { return e.cause.Error() }
func (e *proxyRescueError) Unwrap() error { return e.cause }

func requestProxyRescue(ctx context.Context, action string, port int, load func() (*proxy.Config, error), doer httputil.Doer) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := load()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("proxy rescue configuration unavailable")
	}
	if cfg.LocalToken == "" {
		return nil, proxy.ErrLocalTokenRequired
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	method, path := http.MethodPost, proxy.RuntimeRescueEnterPath
	switch action {
	case "exit":
		path = proxy.RuntimeRescueExitPath
	case "status":
		method, path = http.MethodGet, proxy.RuntimeRescueStatusPath
	}
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), http.NoBody)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	response, err := doer.Do(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("proxy rescue response unavailable")
	}
	// Classify known HTTP failures before reading any unused body.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &proxyRescueError{status: response.StatusCode, cause: fmt.Errorf("proxy rescue control failed: HTTP %d", response.StatusCode)}
	}
	if response.Body == nil {
		return nil, &proxyRescueError{kind: "response", cause: errors.New("proxy rescue response unavailable")}
	}
	body, err := httputil.ReadBodyLimit(response.Body, proxyRescueResponseMaxBytes)
	if errors.Is(err, httputil.ErrBodyTooLarge) {
		err = errors.New("proxy rescue response exceeds 64 KiB")
	}
	if err != nil {
		return nil, &proxyRescueError{kind: "response", cause: err}
	}
	return body, nil
}
