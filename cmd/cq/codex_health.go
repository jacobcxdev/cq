package main

import (
	"context"
	"sync"
	"time"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const codexHealthInventoryTimeout = 250 * time.Millisecond

type codexHealthInventory interface {
	List(context.Context) (codexprov.Inventory, error)
}

type codexHealthTracker struct {
	inventory               codexHealthInventory
	routingDefaultInventory codexHealthInventory
	defaultKey              codexprov.AccountKey

	mu      sync.Mutex
	last    proxy.CodexHealth
	hasLast bool
}

func newCodexHealthTracker(inventory codexHealthInventory, defaultKey codexprov.AccountKey, last proxy.CodexHealth) *codexHealthTracker {
	return newCodexHealthTrackerWithRoutingDefaultInventory(inventory, nil, defaultKey, last)
}

func newCodexHealthTrackerWithRoutingDefaultInventory(inventory, routingDefaultInventory codexHealthInventory, defaultKey codexprov.AccountKey, last proxy.CodexHealth) *codexHealthTracker {
	return &codexHealthTracker{
		inventory:               inventory,
		routingDefaultInventory: routingDefaultInventory,
		defaultKey:              codexprov.AccountKey(string(defaultKey)),
		last:                    cloneCodexHealth(last),
		hasLast:                 last.AccountCountKnown,
	}
}

func (t *codexHealthTracker) Health(ctx context.Context) proxy.CodexHealth {
	ctx, cancel := context.WithTimeout(ctx, codexHealthInventoryTimeout)
	defer cancel()

	inventory, err := t.inventory.List(ctx)
	if err == nil {
		health := codexHealthFromInventory(inventory)
		routingDefaultInventory := inventory
		if t.defaultKey != "" && t.routingDefaultInventory != nil {
			routingDefaultInventory, err = t.routingDefaultInventory.List(ctx)
		}
		if err != nil {
			health.RoutingDefault = proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown}
		} else {
			health.RoutingDefault = codexRoutingDefaultHealth(routingDefaultInventory, t.defaultKey)
		}
		t.mu.Lock()
		t.last = cloneCodexHealth(health)
		t.hasLast = true
		t.mu.Unlock()
		return health
	}

	t.mu.Lock()
	hasLast := t.hasLast
	health := cloneCodexHealth(t.last)
	t.mu.Unlock()
	if hasLast {
		health.HealthCode = "stale"
	} else {
		health = proxy.CodexHealth{
			HealthCode:      "unavailable",
			ExternalSources: make([]proxy.CodexSourceHealth, 0),
		}
	}
	if t.defaultKey == "" {
		health.RoutingDefault = proxy.CodexRoutingDefaultHealth{Status: proxy.CodexRoutingDefaultStatusUnconfigured}
	} else {
		health.RoutingDefault = proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown}
	}
	return health
}

func codexRoutingDefaultHealth(inventory codexprov.Inventory, key codexprov.AccountKey) proxy.CodexRoutingDefaultHealth {
	if key == "" {
		return proxy.CodexRoutingDefaultHealth{Status: proxy.CodexRoutingDefaultStatusUnconfigured}
	}
	for _, account := range inventory.Accounts {
		if account.Key == key && account.Unstable {
			return proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnknown}
		}
	}
	resolved, routable, _ := codexprov.AccountKeyState(inventory, key)
	if !resolved {
		return proxy.CodexRoutingDefaultHealth{Configured: true, Status: proxy.CodexRoutingDefaultStatusUnresolved}
	}
	if !routable {
		return proxy.CodexRoutingDefaultHealth{
			Configured: true,
			Resolved:   true,
			Status:     proxy.CodexRoutingDefaultStatusUnroutable,
		}
	}
	return proxy.CodexRoutingDefaultHealth{
		Configured: true,
		Resolved:   true,
		Routable:   true,
		Status:     proxy.CodexRoutingDefaultStatusResolved,
	}
}

func cloneCodexHealth(health proxy.CodexHealth) proxy.CodexHealth {
	if health.ExternalSources != nil {
		sources := make([]proxy.CodexSourceHealth, len(health.ExternalSources))
		copy(sources, health.ExternalSources)
		health.ExternalSources = sources
	}
	return health
}
