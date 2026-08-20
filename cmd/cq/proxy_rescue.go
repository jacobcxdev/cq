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

const proxyRescueResponseMaxBytes = 64 << 10

func runProxyRescue(args []string, output io.Writer) error {
	return runProxyRescueContext(context.Background(), args, output)
}

func runProxyRescueContext(ctx context.Context, args []string, output io.Writer) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
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
	method := http.MethodPost
	path := ""
	switch args[0] {
	case "enter":
		path = proxy.RuntimeRescueEnterPath
	case "exit":
		path = proxy.RuntimeRescueExitPath
	case "status":
		method = http.MethodGet
		path = proxy.RuntimeRescueStatusPath
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
	cfg, err := load()
	if err != nil {
		return err
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), http.NoBody)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	response, err := doer.Do(request)
	if err != nil {
		return err
	}
	if response == nil || response.Body == nil {
		return errors.New("proxy rescue response unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, proxyRescueResponseMaxBytes))
		return fmt.Errorf("proxy rescue control failed: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, proxyRescueResponseMaxBytes+1))
	if err != nil {
		return err
	}
	if len(body) > proxyRescueResponseMaxBytes {
		return errors.New("proxy rescue response exceeds 64 KiB")
	}
	_, err = output.Write(body)
	return err
}
