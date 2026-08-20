package main

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestNewProxyCodexRoutingCapacityRefresher(t *testing.T) {
	ledger := proxy.NewCodexCapacityLedger(time.Now, 5*time.Minute)
	router := &proxy.CodexRequestRouter{}
	refresher, err := newProxyCodexRoutingCapacityRefresher(
		"https://chatgpt.com/backend-api/codex",
		router,
		ledger,
	)
	if err != nil {
		t.Fatalf("newProxyCodexRoutingCapacityRefresher() error = %v", err)
	}
	reader, ok := refresher.Usage.(*proxy.CodexPrimerUsageReader)
	if !ok {
		t.Fatalf("usage reader = %T, want *proxy.CodexPrimerUsageReader", refresher.Usage)
	}
	if reader.Router != router || reader.UsageURL != proxy.DefaultCodexUsageURL || reader.Timeout != 5*time.Second {
		t.Fatalf("usage reader = %+v, want supplied router/default URL/5s timeout", reader)
	}
	if refresher.Capacity != ledger {
		t.Fatal("capacity ledger was not preserved")
	}
}

func TestNewProxyCodexRoutingCapacityRefresherRejectsInvalidUpstream(t *testing.T) {
	_, err := newProxyCodexRoutingCapacityRefresher(
		"https://chatgpt.com/backend-api/not-codex",
		&proxy.CodexRequestRouter{},
		proxy.NewCodexCapacityLedger(time.Now, 5*time.Minute),
	)
	if err == nil {
		t.Fatal("invalid upstream error = nil")
	}
}
