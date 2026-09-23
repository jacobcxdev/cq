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
	result, err := requestProxyLeaseInvalidation(ctx, port, deps)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

type proxyLeaseError struct {
	kind   string
	status int
	code   string
}

func (e *proxyLeaseError) Error() string { return "proxy lease invalidation failed" }

func requestProxyLeaseInvalidation(ctx context.Context, port int, deps proxyLeaseDependencies) (proxy.CodexLeaseInvalidationResult, error) {
	var zero proxy.CodexLeaseInvalidationResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	cfg, err := deps.LoadConfig()
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if errors.Is(err, proxy.ErrLocalTokenRequired) {
		return zero, &proxyLeaseError{kind: "auth"}
	}
	if err != nil || cfg == nil {
		return zero, &proxyLeaseError{kind: "io"}
	}
	if cfg.LocalToken == "" {
		return zero, &proxyLeaseError{kind: "auth"}
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, proxy.RuntimeCodexLeaseInvalidationPath), http.NoBody)
	if err != nil {
		return zero, &proxyLeaseError{kind: "io"}
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	response, err := deps.Doer.Do(request)
	if err != nil {
		return zero, &proxyLeaseError{kind: "control"}
	}
	if response == nil {
		return zero, &proxyLeaseError{kind: "control"}
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	// Classify the status and allowlisted receipt before touching an unused body.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return zero, &proxyLeaseError{status: response.StatusCode, code: response.Header.Get(proxy.CodexLeaseErrorHeader)}
	}
	if response.Body == nil {
		return zero, &proxyLeaseError{kind: "io"}
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return zero, &proxyLeaseError{kind: "io"}
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
