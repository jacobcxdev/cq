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
		path = proxy.RuntimeCodexLeaseInvalidationPath
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
	if path == proxy.RuntimeCodexLeaseInvalidationPath {
		value, err := requestProxyLeaseInvalidation(ctx, port, deps)
		if err != nil {
			return err
		}
		result = value
	} else {
		value, err := requestProxyLeaseRedistribution(ctx, port, http.NoBody, deps)
		if err != nil {
			return err
		}
		result = value
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
	_, err = requestProxyLeaseRedistribution(ctx, 0, bytes.NewReader(body), deps)
	return err
}

type proxyLeaseError struct {
	kind   string
	status int
	code   string
}

func (e *proxyLeaseError) Error() string { return "proxy lease control failed" }

func proxyLeaseRequest(ctx context.Context, port int, path string, payload io.Reader, deps proxyLeaseDependencies) ([]byte, error) {
	if ctx == nil || deps.LoadConfig == nil || deps.Doer == nil {
		return nil, &proxyLeaseError{kind: "io"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := deps.LoadConfig()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, proxy.ErrLocalTokenRequired) {
		return nil, &proxyLeaseError{kind: "auth"}
	}
	if err != nil || cfg == nil {
		return nil, &proxyLeaseError{kind: "io"}
	}
	if cfg.LocalToken == "" {
		return nil, &proxyLeaseError{kind: "auth"}
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), payload)
	if err != nil {
		return nil, &proxyLeaseError{kind: "io"}
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	if payload != http.NoBody {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := deps.Doer.Do(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, &proxyLeaseError{kind: "control"}
	}
	if response == nil {
		return nil, &proxyLeaseError{kind: "control"}
	}
	// Classify the status and allowlisted receipt before touching an unused body.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &proxyLeaseError{status: response.StatusCode, code: response.Header.Get(proxy.CodexLeaseErrorHeader)}
	}
	if response.Body == nil {
		return nil, &proxyLeaseError{kind: "io"}
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, &proxyLeaseError{kind: "io"}
	}
	return body, nil
}
func requestProxyLeaseInvalidation(ctx context.Context, port int, deps proxyLeaseDependencies) (proxy.CodexLeaseInvalidationResult, error) {
	var zero proxy.CodexLeaseInvalidationResult
	body, err := proxyLeaseRequest(ctx, port, proxy.RuntimeCodexLeaseInvalidationPath, http.NoBody, deps)
	if err != nil {
		return zero, err
	}
	var result struct {
		InvalidatedLeases *int    `json:"invalidated_leases"`
		JournalGeneration *uint64 `json:"journal_generation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || result.InvalidatedLeases == nil || result.JournalGeneration == nil || *result.InvalidatedLeases < 0 {
		return zero, &proxyLeaseError{kind: "io"}
	}
	return proxy.CodexLeaseInvalidationResult{InvalidatedLeases: *result.InvalidatedLeases, JournalGeneration: *result.JournalGeneration}, nil
}

func requestProxyLeaseRedistribution(ctx context.Context, port int, payload io.Reader, deps proxyLeaseDependencies) (proxy.CodexLeaseRedistributionResult, error) {
	var zero proxy.CodexLeaseRedistributionResult
	body, err := proxyLeaseRequest(ctx, port, proxy.RuntimeCodexLeaseRedistributionPath, payload, deps)
	if err != nil {
		return zero, err
	}
	var result struct {
		ScheduledLeases   *int    `json:"scheduled_leases"`
		DeferredLeases    *int    `json:"deferred_leases"`
		JournalGeneration *uint64 `json:"journal_generation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || result.ScheduledLeases == nil || result.DeferredLeases == nil || result.JournalGeneration == nil || *result.ScheduledLeases < 0 || *result.DeferredLeases < 0 {
		return zero, &proxyLeaseError{kind: "io"}
	}
	return proxy.CodexLeaseRedistributionResult{ScheduledLeases: *result.ScheduledLeases, DeferredLeases: *result.DeferredLeases, JournalGeneration: *result.JournalGeneration}, nil
}
