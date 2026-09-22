package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	daybreakBlue = "daybreak_blue"
	daybreakRed  = "daybreak_red"
)

type cyberCatalogue struct {
	models    map[string]map[string]proxy.CyberAccessState
	poolState proxy.CyberAccessState
}

// cyberModelAccess keeps model-specific uncertainty separate from explicit
// access-program lists. Only an explicit programme advertises eligibility.
func cyberModelAccess(body []byte) (cyberCatalogue, error) {
	var catalogue struct {
		Models []struct {
			Slug     string          `json:"slug"`
			Programs json.RawMessage `json:"available_access_programs"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalogue); err != nil {
		return cyberCatalogue{}, err
	}
	if catalogue.Models == nil {
		return cyberCatalogue{}, fmt.Errorf("Codex model catalogue missing models")
	}
	result := cyberCatalogue{models: make(map[string]map[string]proxy.CyberAccessState, len(catalogue.Models))}
	allExplicit := len(catalogue.Models) > 0
	for _, model := range catalogue.Models {
		if model.Slug == "" {
			return cyberCatalogue{}, fmt.Errorf("Codex model catalogue missing slug")
		}
		if _, duplicate := result.models[model.Slug]; duplicate {
			return cyberCatalogue{}, fmt.Errorf("Codex model catalogue duplicate slug")
		}
		result.models[model.Slug] = nil
		if len(model.Programs) == 0 || string(model.Programs) == "null" {
			allExplicit = false
			continue
		}
		var programs struct {
			Cyber json.RawMessage `json:"cyber"`
		}
		if err := json.Unmarshal(model.Programs, &programs); err != nil {
			return cyberCatalogue{}, fmt.Errorf("Codex model access programmes: %w", err)
		}
		if len(programs.Cyber) == 0 || string(programs.Cyber) == "null" {
			allExplicit = false
			continue
		}
		var advertised []string
		if err := json.Unmarshal(programs.Cyber, &advertised); err != nil || advertised == nil {
			return cyberCatalogue{}, fmt.Errorf("Codex model cyber programmes invalid")
		}
		states := map[string]proxy.CyberAccessState{
			daybreakBlue: proxy.CyberAccessIneligible,
			daybreakRed:  proxy.CyberAccessIneligible,
		}
		for _, programme := range advertised {
			if programme == daybreakBlue || programme == daybreakRed {
				states[programme] = proxy.CyberAccessEligible
				result.poolState = proxy.CyberAccessEligible
			}
		}
		result.models[model.Slug] = states
	}
	if result.poolState != proxy.CyberAccessEligible && allExplicit {
		result.poolState = proxy.CyberAccessIneligible
	}
	return result, nil
}

type accountScopedCodexRegistryAuthority struct {
	codexRegistryCredentialAuthority
	account codexprov.LogicalAccount
}

func (scoped accountScopedCodexRegistryAuthority) List(context.Context) (codexprov.Inventory, error) {
	return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{scoped.account}}, nil
}

// discoverCyberAccounts reads each logical account's own catalogue. A failed
// request stays absent from the result, preserving prior evidence in callers.
func discoverCyberAccounts(ctx context.Context, authority codexRegistryCredentialAuthority, client httpClientDoer, baseURL, clientVersion string) (map[codexprov.AccountKey]cyberCatalogue, []codexprov.AccountKey, error) {
	if authority == nil || client == nil {
		return nil, nil, errCodexRegistryCredentialAuthorityUnavailable
	}
	inventory, err := authority.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	if codexRegistryInventoryDegraded(inventory) {
		return nil, nil, errCodexRegistryCredentialInventoryDegraded
	}
	observed := make(map[codexprov.AccountKey]cyberCatalogue)
	all := make([]codexprov.AccountKey, 0, len(inventory.Accounts))
	for _, account := range inventory.Accounts {
		if !account.Routable || account.Key == "" {
			continue
		}
		all = append(all, account.Key)
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		url := strings.TrimRight(baseURL, "/") + "/models"
		req, requestErr := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
		if requestErr == nil && clientVersion != "" {
			query := req.URL.Query()
			query.Set("client_version", clientVersion)
			req.URL.RawQuery = query.Encode()
		}
		if requestErr == nil {
			var response *http.Response
			response, requestErr = codexRegistryModelsRequest(probeCtx, accountScopedCodexRegistryAuthority{authority, account}, client, time.Now(), req)
			if requestErr == nil && response != nil {
				if response.StatusCode == http.StatusOK {
					var body []byte
					body, requestErr = httputil.ReadBody(response.Body)
					if requestErr == nil {
						var parsed cyberCatalogue
						parsed, requestErr = cyberModelAccess(body)
						if requestErr == nil {
							observed[account.Key] = parsed
						}
					}
				}
				closeCodexRegistryResponse(response)
			}
		}
		cancel()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return observed, all, nil
}

func refreshCyberPool(ctx context.Context, authority codexRegistryCredentialAuthority, client httpClientDoer, baseURL, clientVersion string, routing *proxy.RoutingPolicyStore, eligibility *proxy.CyberEligibilityStore, resolver *proxy.SessionPolicyResolver) error {
	observed, all, err := discoverCyberAccounts(ctx, authority, client, baseURL, clientVersion)
	if err != nil {
		return err
	}
	known := make(map[codexprov.AccountKey]bool, len(observed))
	for account, catalogue := range observed {
		eligibility.ReplaceAccount(account, catalogue.models)
		switch catalogue.poolState {
		case proxy.CyberAccessEligible:
			known[account] = true
		case proxy.CyberAccessIneligible:
			known[account] = false
		}
	}
	changed, err := routing.ReconcileCyberPool(known, all)
	if err != nil {
		return err
	}
	if changed {
		resolver.Replace(routing.Current())
	}
	return nil
}

func runCyberPoolDiscovery(ctx context.Context, authority codexRegistryCredentialAuthority, client httpClientDoer, baseURL, clientVersion string, routing *proxy.RoutingPolicyStore, eligibility *proxy.CyberEligibilityStore, resolver *proxy.SessionPolicyResolver) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = refreshCyberPool(ctx, authority, client, baseURL, clientVersion, routing, eligibility, resolver)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
