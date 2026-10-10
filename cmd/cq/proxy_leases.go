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
	"github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const proxyLeaseInvalidationTimeout = 10 * time.Second
const proxyLeasesUsage = "usage: cq proxy leases {invalidate|redistribute} [--port PORT]"

type proxyLeaseDependencies struct {
	LoadConfig func() (*proxy.Config, error)
	Doer       httputil.Doer
}

func defaultProxyLeaseDependencies() proxyLeaseDependencies {
	return proxyLeaseDependencies{
		LoadConfig: proxy.LoadConfig,
		Doer: &http.Client{
			Timeout: proxyLeaseInvalidationTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("proxy lease control redirect refused")
			},
		},
	}
}

func runProxyLeases(args []string, output io.Writer) error {
	return runProxyLeasesWithDependencies(context.Background(), args, output, defaultProxyLeaseDependencies())
}

func runProxyLeasesWithDependencies(ctx context.Context, args []string, output io.Writer, deps proxyLeaseDependencies) error {
	if ctx == nil || output == nil || deps.LoadConfig == nil || deps.Doer == nil || len(args) == 0 {
		return errors.New(proxyLeasesUsage)
	}
	var path string
	var result any
	switch args[0] {
	case "invalidate":
		path, result = proxy.RuntimeCodexLeaseInvalidationPath, &proxy.CodexLeaseInvalidationResult{}
	case "redistribute":
		path, result = proxy.RuntimeCodexLeaseRedistributionPath, &proxy.CodexLeaseRedistributionResult{}
	default:
		return fmt.Errorf("unknown proxy leases command: %s", args[0])
	}
	port := 0
	if len(args) != 1 {
		if len(args) != 3 || args[1] != "--port" {
			return errors.New(proxyLeasesUsage)
		}
		var err error
		port, err = strconv.Atoi(args[2])
		if err != nil || port < 1 || port > 65535 {
			return errors.New("proxy leases: invalid port")
		}
	}
	if err := proxyLeaseRequest(ctx, port, path, http.NoBody, result, deps); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

// notifyProxyCodexReset asks the worker to refresh capacity before scheduling
// redistribution. Reset consumption has already succeeded when this runs.
func notifyProxyCodexReset(ctx context.Context, accountKey codex.AccountKey, eventID string) error {
	return notifyProxyCodexResetWithDependencies(ctx, accountKey, eventID, defaultProxyLeaseDependencies())
}

func notifyProxyCodexResetWithDependencies(ctx context.Context, accountKey codex.AccountKey, eventID string, deps proxyLeaseDependencies) error {
	body, err := json.Marshal(proxy.CodexLeaseResetRequest{AccountKey: accountKey, EventID: eventID})
	if err != nil {
		return err
	}
	var result proxy.CodexLeaseRedistributionResult
	return proxyLeaseRequest(ctx, 0, proxy.RuntimeCodexLeaseRedistributionPath, bytes.NewReader(body), &result, deps)
}

func proxyLeaseRequest(ctx context.Context, port int, path string, payload io.Reader, result any, deps proxyLeaseDependencies) error {
	if ctx == nil || deps.LoadConfig == nil || deps.Doer == nil {
		return errors.New("proxy lease control unavailable")
	}
	cfg, err := deps.LoadConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("proxy lease control configuration unavailable")
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), payload)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	if payload != http.NoBody {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := deps.Doer.Do(request)
	if err != nil {
		return err
	}
	if response == nil || response.Body == nil {
		return errors.New("proxy lease control response unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = httputil.ReadBody(response.Body)
		return fmt.Errorf("proxy lease control failed: HTTP %d", response.StatusCode)
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return errors.New("proxy lease control response invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("proxy lease control response invalid")
	}
	return nil
}
