//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type legacyMaintenanceHealthDoerFunc func(*http.Request) (*http.Response, error)

func (do legacyMaintenanceHealthDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return do(request)
}

func TestLegacyMaintenanceFinaliseVerifierRequiresFreshCompleteRuntimeHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*legacyMaintenanceRuntimeHealth)
	}{
		{name: "healthy"},
		{name: "status", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.Status = "degraded" }},
		{name: "headroom disabled", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.Headroom = false }},
		{name: "headroom token", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.HeadroomMode = "token" }},
		{name: "inventory unknown", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.Accounts.Codex = nil }},
		{name: "inventory empty", mutate: func(health *legacyMaintenanceRuntimeHealth) { zero := 0; health.Accounts.Codex = &zero }},
		{name: "inventory unhealthy", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.InventoryHealth = "fetch_error" }},
		{name: "source unhealthy", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.ExternalSources[0].HealthCode = "unavailable" }},
		{name: "HTTP inhibited", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.HTTP.InhibitionReason = "stale" }},
		{name: "WS off", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.WebSocket.Effective = proxy.CodexRoutingOff }},
		{name: "default absent", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.RoutingDefault = legacyMaintenanceDefaultHealth{} }},
		{name: "default unconfigured", mutate: func(health *legacyMaintenanceRuntimeHealth) {
			health.RoutingDefault.Configured = false
			health.RoutingDefault.Status = "unconfigured"
		}},
		{name: "default unresolved", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.RoutingDefault.Resolved = false }},
		{name: "default unroutable", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.RoutingDefault.Routable = false }},
		{name: "default status unresolved", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.RoutingDefault.Status = "unresolved" }},
		{name: "default status unroutable", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.RoutingDefault.Status = "unroutable" }},
		{name: "default status unknown", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.RoutingDefault.Status = "unknown" }},
		{name: "default status invalid", mutate: func(health *legacyMaintenanceRuntimeHealth) { health.RoutingDefault.Status = "ready" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			verifier, runtime, health, proof := newTestLegacyMaintenanceVerifier(t)
			if test.mutate != nil {
				test.mutate(&health)
			}
			verifier.http = encodedLegacyMaintenanceHealthDoer(t, health)
			err := verifier.VerifyLegacyMaintenanceFinalise(context.Background(), proof)
			if test.mutate == nil {
				if err != nil {
					t.Fatalf("Verify error = %v", err)
				}
				return
			}
			if !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
				t.Fatalf("Verify error = %v, want not-ready", err)
			}
			if !legacyMaintenanceRoutingRuntimeEqual(*runtime, verifier.frozen) && test.name != "HTTP inhibited" && test.name != "WS off" {
				t.Fatal("fixture unexpectedly changed frozen runtime")
			}
		})
	}
}

func TestLegacyMaintenanceFinaliseVerifierUsesFreshHealthAndFrozenRuntime(t *testing.T) {
	t.Parallel()
	verifier, runtime, health, proof := newTestLegacyMaintenanceVerifier(t)
	unhealthy := health
	unhealthy.RoutingDefault.Resolved = false
	responses := [][]byte{marshalLegacyMaintenanceHealth(t, health), marshalLegacyMaintenanceHealth(t, unhealthy)}
	var mu sync.Mutex
	requests := 0
	verifier.http = legacyMaintenanceHealthDoerFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		body := responses[min(requests, len(responses)-1)]
		requests++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	if err := verifier.VerifyLegacyMaintenanceFinalise(context.Background(), proof); err != nil {
		t.Fatalf("first Verify error = %v", err)
	}
	if err := verifier.VerifyLegacyMaintenanceFinalise(context.Background(), proof); !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("second Verify error = %v, want fresh not-ready result", err)
	}
	mu.Lock()
	requestCount := requests
	mu.Unlock()
	if requestCount != 2 {
		t.Fatalf("health requests = %d, want 2", requestCount)
	}
	runtime.HTTP.InhibitionReason = "toggled"
	verifier.http = encodedLegacyMaintenanceHealthDoer(t, health)
	if err := verifier.VerifyLegacyMaintenanceFinalise(context.Background(), proof); !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("toggled runtime Verify error = %v, want not-ready", err)
	}
}

func TestLegacyMaintenanceFinaliseVerifierRejectsExecutableReplacementAndHidesAuthority(t *testing.T) {
	t.Parallel()
	verifier, _, health, proof := newTestLegacyMaintenanceVerifier(t)
	verifier.http = encodedLegacyMaintenanceHealthDoer(t, health)
	replacement := verifier.executable
	replacement.inode++
	verifier.capture = func() (legacyMaintenanceExecutableProof, error) { return replacement, nil }
	err := verifier.VerifyLegacyMaintenanceFinalise(context.Background(), proof)
	if !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("Verify error = %v, want not-ready", err)
	}
	message := err.Error()
	if strings.Contains(message, proof.TicketHash) || strings.Contains(message, proof.OwnerGeneration) || strings.Contains(message, verifier.executable.path) {
		t.Fatalf("verification error leaked authority or executable path: %q", message)
	}
}

func TestProductionLegacyMaintenanceFinaliseVerifierRejectsForeignHealthWithoutProcessProof(t *testing.T) {
	t.Parallel()
	_, runtime, health, proof := newTestLegacyMaintenanceVerifier(t)
	verifier := newProxyLegacyMaintenanceFinaliseVerifier("candidate-build", "client-build", 12345)
	verifier.initialErr = nil
	verifier.capture = func() (legacyMaintenanceExecutableProof, error) { return verifier.executable, nil }
	var requests atomic.Int32
	verifier.http = legacyMaintenanceHealthDoerFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(marshalLegacyMaintenanceHealth(t, health))),
		}, nil
	})
	if err := verifier.bind(runtime, true, proxy.HeadroomModeCache); err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyLegacyMaintenanceFinalise(context.Background(), proof); !errors.Is(err, errLegacyMaintenanceProcessProofUnavailable) {
		t.Fatalf("Verify error = %v, want process-proof blocker", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("foreign health requests = %d, want 0 before process proof", got)
	}
}

func TestDecodeLegacyMaintenanceRuntimeHealthRejectsDuplicateTrailingAndOversizedInput(t *testing.T) {
	t.Parallel()
	for _, input := range [][]byte{
		[]byte(`{"status":"ok","status":"ok"}`),
		[]byte(`{"status":"ok"} {}`),
		[]byte(`{"codex_routing_default":{"configured":true,"configured":true}}`),
	} {
		if _, err := decodeLegacyMaintenanceRuntimeHealth(input); err == nil {
			t.Fatalf("unsafe health JSON decoded: %s", input)
		}
	}
	if _, err := readLegacyMaintenanceHealth(strings.NewReader(strings.Repeat("x", legacyMaintenanceHealthMaxBytes+1))); err == nil {
		t.Fatal("oversized health response was accepted")
	}
}

func newTestLegacyMaintenanceVerifier(t *testing.T) (*proxyLegacyMaintenanceFinaliseVerifier, *proxy.CodexRoutingRuntime, legacyMaintenanceRuntimeHealth, codexprov.LegacyMaintenanceFinaliseVerification) {
	t.Helper()
	runtime := &proxy.CodexRoutingRuntime{
		HTTP: proxy.CodexModeStatus{
			Configured: proxy.CodexRoutingEnforce, Effective: proxy.CodexRoutingEnforce,
			ModeEpoch: 1, AuthoritativeEpoch: 1,
		},
		WebSocket: proxy.CodexModeStatus{
			Configured: proxy.CodexRoutingObserve, Effective: proxy.CodexRoutingObserve,
			ModeEpoch: 2, ShadowEpoch: 2,
		},
	}
	executable := legacyMaintenanceExecutableProof{
		path: "/private/test/candidate", device: 1, inode: 2, links: 1, size: 3, mode: 0o755,
	}
	verifier := &proxyLegacyMaintenanceFinaliseVerifier{
		build: "candidate-build", clientBuild: "client-build", healthURL: "http://127.0.0.1/health",
		executable: executable, capture: func() (legacyMaintenanceExecutableProof, error) { return executable, nil },
		processProof: func(context.Context) error { return nil },
	}
	if err := verifier.bind(runtime, true, proxy.HeadroomModeCache); err != nil {
		t.Fatal(err)
	}
	accounts := 3
	health := legacyMaintenanceRuntimeHealth{
		Status: "ok", Headroom: true, HeadroomMode: "cache",
		Accounts: legacyMaintenanceAccountHealth{Codex: &accounts}, InventoryHealth: "ok",
		ExternalSources: []proxy.CodexSourceHealth{{Name: "external", CandidateCount: 1, HealthCode: "ok"}},
		HTTP:            runtime.HTTP, WebSocket: runtime.WebSocket,
		RoutingDefault: legacyMaintenanceDefaultHealth{Configured: true, Resolved: true, Routable: true, Status: "resolved"},
	}
	proof := codexprov.LegacyMaintenanceFinaliseVerification{
		TicketHash: strings.Repeat("a", 64), OwnerGeneration: strings.Repeat("b", 32),
	}
	return verifier, runtime, health, proof
}

func encodedLegacyMaintenanceHealthDoer(t *testing.T, health legacyMaintenanceRuntimeHealth) legacyMaintenanceHealthDoer {
	t.Helper()
	data := marshalLegacyMaintenanceHealth(t, health)
	return legacyMaintenanceHealthDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/health" {
			t.Fatalf("health request = %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
}

func marshalLegacyMaintenanceHealth(t *testing.T, health legacyMaintenanceRuntimeHealth) []byte {
	t.Helper()
	data, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
