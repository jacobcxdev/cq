package dots

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// prepareStartupDiscovery fills only missing owner bindings before launching
// workers with a fixed catalogue. Existing known Dots never wait for HTTP
// revalidation; the account watcher owns that background work.
func (c Controller) prepareStartupDiscovery(ctx context.Context, s *Settings) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	client := c.client(s)
	inventory, err := client.Inventory()
	if err != nil {
		return false, err
	}
	known := make(map[string]string, len(s.Dots))
	for _, dot := range s.Dots {
		known[dot.OwnerAlias] = dot.OwnerAccountID
	}
	missing := make([]Account, 0)
	for _, account := range inventory.Accounts {
		owner, exists := known[account.Alias]
		if exists && owner != "" && owner != account.AccountID {
			return false, &IdentityCollision{"startup account alias collision"}
		}
		if !exists {
			missing = append(missing, account)
		}
	}
	pending := false
	for _, failure := range inventory.Failures {
		if _, exists := known[failure.OwnerAlias]; !exists {
			pending = true
		}
	}
	if len(missing) == 0 {
		return pending, nil
	}
	// Share the existing per-request budget across missing owners. A slow or
	// unavailable new account cannot turn this into N serial request timeouts.
	discoveryCtx, cancel := context.WithTimeout(ctx, catalogueRequestTimeout)
	defer cancel()
	type outcome struct {
		discovery Discovery
		err       error
	}
	jobs := make(chan Account, len(missing))
	results := make(chan outcome, len(missing))
	for _, account := range missing {
		jobs <- account
	}
	close(jobs)
	for i := 0; i < min(4, len(missing)); i++ {
		go func() {
			defer func() { _ = recover() }()
			for account := range jobs {
				result := func() (result outcome) {
					defer func() {
						if recover() != nil {
							result.err = fmt.Errorf("startup owner discovery failed")
						}
					}()
					result.discovery, result.err = client.discoverInventory(discoveryCtx, Inventory{System: inventory.System, Accounts: []Account{account}})
					return result
				}()
				results <- result
			}
		}()
	}
	next := *s
	next.Dots = make(map[string]Dot, len(s.Dots)+len(missing))
	for handle, dot := range s.Dots {
		next.Dots[handle] = dot
	}
	for remaining := len(missing); remaining > 0; remaining-- {
		select {
		case result := <-results:
			var collision *IdentityCollision
			if errors.As(result.err, &collision) {
				return false, result.err
			}
			if result.err != nil {
				pending = true
				continue
			}
			for _, account := range result.discovery.Accounts {
				pending = pending || account.Status == "unavailable"
			}
			for _, dot := range result.discovery.Dots {
				next.Dots[dot.Handle] = dot.Dot
			}
		case <-discoveryCtx.Done():
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			pending = true
			remaining = 0
		}
	}
	if err := next.Validate(); err != nil {
		return false, err
	}
	if next.CatalogueRevision() != s.CatalogueRevision() {
		if err := SaveJSON(filepath.Join(c.Root, "settings.json"), &next); err != nil {
			return false, err
		}
		*s = next
	}
	return pending, nil
}
