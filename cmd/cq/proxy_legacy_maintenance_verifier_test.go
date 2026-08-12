//go:build unix

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestLegacyMaintenanceRuntimeHealthRequiresCompleteReadyState(t *testing.T) {
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
			health, runtime := healthyLegacyMaintenanceRuntimeFixture()
			if test.mutate != nil {
				test.mutate(&health)
			}
			if got := legacyMaintenanceRuntimeHealthReady(health, *runtime); got != (test.mutate == nil) {
				t.Fatalf("health ready = %t, want %t", got, test.mutate == nil)
			}
		})
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

func marshalLegacyMaintenanceHealth(t *testing.T, health legacyMaintenanceRuntimeHealth) []byte {
	t.Helper()
	data, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
