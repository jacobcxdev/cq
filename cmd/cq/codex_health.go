package main

import (
	"context"
	"sync"
	"time"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const codexHealthInventoryTimeout = 2 * time.Second

type codexHealthInventory interface {
	List(context.Context) (codexprov.Inventory, error)
}

type codexHealthTracker struct {
	inventory codexHealthInventory

	mu   sync.Mutex
	last proxy.CodexHealth
}

func newCodexHealthTracker(inventory codexHealthInventory, last proxy.CodexHealth) *codexHealthTracker {
	return &codexHealthTracker{inventory: inventory, last: cloneCodexHealth(last)}
}

func (t *codexHealthTracker) Health(ctx context.Context) proxy.CodexHealth {
	ctx, cancel := context.WithTimeout(ctx, codexHealthInventoryTimeout)
	defer cancel()

	inventory, err := t.inventory.List(ctx)
	if err == nil {
		health := codexHealthFromInventory(inventory)
		t.mu.Lock()
		t.last = cloneCodexHealth(health)
		t.mu.Unlock()
		return health
	}

	t.mu.Lock()
	health := cloneCodexHealth(t.last)
	t.mu.Unlock()
	health.HealthCode = "fetch_error"
	return health
}

func cloneCodexHealth(health proxy.CodexHealth) proxy.CodexHealth {
	if health.ExternalSources != nil {
		sources := make([]proxy.CodexSourceHealth, len(health.ExternalSources))
		copy(sources, health.ExternalSources)
		health.ExternalSources = sources
	}
	return health
}
