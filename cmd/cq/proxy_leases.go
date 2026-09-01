package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const proxyLeaseInvalidationTimeout = 10 * time.Second

type proxyLeaseDependencies struct {
	LoadConfig func() (*proxy.Config, error)
	Doer       httputil.Doer
}

func runProxyLeases(args []string, output io.Writer) error {
	client := &http.Client{
		Timeout: proxyLeaseInvalidationTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("proxy lease invalidation redirect refused")
		},
	}
	return runProxyLeasesWithDependencies(context.Background(), args, output, proxyLeaseDependencies{
		LoadConfig: proxy.LoadConfig,
		Doer:       client,
	})
}

func runProxyLeasesWithDependencies(ctx context.Context, args []string, output io.Writer, deps proxyLeaseDependencies) error {
	if ctx == nil || output == nil || deps.LoadConfig == nil || deps.Doer == nil || len(args) == 0 {
		return errors.New("usage: cq proxy leases invalidate [--port PORT]")
	}
	if args[0] != "invalidate" {
		return fmt.Errorf("unknown proxy leases command: %s", args[0])
	}
	port := 0
	var err error
	if len(args) != 1 {
		if len(args) != 3 || args[1] != "--port" {
			return errors.New("usage: cq proxy leases invalidate [--port PORT]")
		}
		port, err = strconv.Atoi(args[2])
		if err != nil || port < 1 || port > 65535 {
			return errors.New("proxy leases: invalid port")
		}
	}
	cfg, err := deps.LoadConfig()
	if err != nil {
		return err
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, proxy.RuntimeCodexLeaseInvalidationPath), http.NoBody)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	response, err := deps.Doer.Do(request)
	if err != nil {
		return err
	}
	if response == nil || response.Body == nil {
		return errors.New("proxy lease invalidation response unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = httputil.ReadBody(response.Body)
		return fmt.Errorf("proxy lease invalidation failed: HTTP %d", response.StatusCode)
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return err
	}
	var result proxy.CodexLeaseInvalidationResult
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return errors.New("proxy lease invalidation response invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("proxy lease invalidation response invalid")
	}
	return json.NewEncoder(output).Encode(result)
}
