package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// Kept portable so the native adapter's HTTP boundary can be tested without a user manager.
func probeSelectedLinuxProxyHealth(ctx context.Context, address, token string) bool {
	if ctx == nil || ctx.Err() != nil || token == "" {
		return false
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("service health redirect refused") }}
	for _, path := range []string{proxy.RuntimeRescueStatusPath, "/health"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, http.NoBody)
		if err != nil {
			return false
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Content-Encoding") != "" {
			response.Body.Close()
			return false
		}
		body, err := httputil.ReadBody(response.Body)
		response.Body.Close()
		if err != nil || len(body) > 64<<10 {
			return false
		}
		if path == proxy.RuntimeRescueStatusPath {
			var result struct {
				Mode string `json:"mode"`
			}
			if json.Unmarshal(body, &result) != nil || result.Mode != "normal" {
				return false
			}
		} else {
			var result struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(body, &result) != nil || result.Status != "ok" {
				return false
			}
		}
	}
	return ctx.Err() == nil
}
