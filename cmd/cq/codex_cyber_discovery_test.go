package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestCyberModelAccessDistinguishesExplicitAndUnknownPerModel(t *testing.T) {
	parsed, err := cyberModelAccess([]byte(`{"models":[{"slug":"sol","available_access_programs":{"cyber":["standard","daybreak_blue"]}},{"slug":"red","available_access_programs":{"cyber":["daybreak_red"]}},{"slug":"plain","available_access_programs":{"cyber":[]}},{"slug":"legacy"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.poolState != proxy.CyberAccessEligible || parsed.models["sol"][daybreakBlue] != proxy.CyberAccessEligible || parsed.models["sol"][daybreakRed] != proxy.CyberAccessIneligible || parsed.models["red"][daybreakRed] != proxy.CyberAccessEligible || parsed.models["plain"][daybreakBlue] != proxy.CyberAccessIneligible {
		t.Fatalf("model-specific access = %#v, pool state = %v", parsed.models, parsed.poolState)
	}
	if parsed.models["legacy"] != nil {
		t.Fatalf("missing metadata became explicit denial: %#v", parsed.models["legacy"])
	}

	negative, err := cyberModelAccess([]byte(`{"models":[{"slug":"sol","available_access_programs":{"cyber":["standard"]}}]}`))
	if err != nil || negative.poolState != proxy.CyberAccessIneligible {
		t.Fatalf("explicit non-Daybreak catalogue = %#v, %v", negative, err)
	}
	unknown, err := cyberModelAccess([]byte(`{"models":[{"slug":"sol","available_access_programs":{"cyber":["standard"]}},{"slug":"other"}]}`))
	if err != nil || unknown.poolState != proxy.CyberAccessUnknown {
		t.Fatalf("partial catalogue = %#v, %v; want unknown", unknown, err)
	}
}

func TestCyberModelAccessRejectsMalformedCatalogue(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"models":[{"slug":""}]}`,
		`{"models":[{"slug":"sol"},{"slug":"sol"}]}`,
		`{"models":[{"slug":"sol","available_access_programs":{"cyber":"daybreak_blue"}}]}`,
	} {
		if _, err := cyberModelAccess([]byte(body)); err == nil {
			t.Fatalf("accepted malformed catalogue: %s", body)
		}
	}
}

type cyberModelsDoer func(*http.Request) (*http.Response, error)

func (do cyberModelsDoer) Do(req *http.Request) (*http.Response, error) { return do(req) }

func TestDiscoverCyberAccountsUsesExactAccountAndPreservesFailedProbe(t *testing.T) {
	account := func(key codexprov.AccountKey, accountID string) codexprov.LogicalAccount {
		return codexprov.LogicalAccount{
			Key: key, Routable: true,
			Identity: codexprov.AccountIdentity{AccountID: accountID, UserID: string(key), Email: string(key) + "@example.test", PlanType: "pro"},
			Candidates: []codexprov.CredentialCandidate{{
				Ref:      codexprov.CandidateRef{AccountKey: key, CandidateID: codexprov.CandidateID("candidate-" + string(key))},
				Revision: "revision", Source: codexprov.SourceExternal, AccessExpiresAt: time.Now().Add(time.Hour), Routable: true,
			}},
		}
	}
	first, second := account("account-a", "workspace-a"), account("account-b", "workspace-b")
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{{Accounts: []codexprov.LogicalAccount{first, second}}},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return registryCredentialMaterial(planned.Identity, "token-"+string(planned.Ref.AccountKey)), nil
		},
	}
	var requests int
	client := cyberModelsDoer(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Path != "/models" || req.URL.Query().Get("client_version") != "0.155.0" {
			t.Errorf("catalogue route = %q", req.URL.String())
		}
		workspace := req.Header.Get("ChatGPT-Account-ID")
		if (workspace == "workspace-a" && req.Header.Get("Authorization") != "Bearer token-account-a") || (workspace == "workspace-b" && req.Header.Get("Authorization") != "Bearer token-account-b") {
			t.Errorf("credential crossed account boundary for %q", workspace)
		}
		status, body := http.StatusOK, `{"models":[{"slug":"gpt-6-sol","available_access_programs":{"cyber":["daybreak_blue"]}}]}`
		if workspace == "workspace-b" {
			status, body = http.StatusForbidden, `{"error":"workspace unavailable"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	observed, all, err := discoverCyberAccounts(context.Background(), authority, client, "https://codex.example", "0.155.0")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(all) != 2 || len(observed) != 1 || observed["account-a"].poolState != proxy.CyberAccessEligible || observed["account-b"].models != nil {
		t.Fatalf("observed = %#v; all = %v; requests = %d", observed, all, requests)
	}
}

func TestDiscoverCyberAccountsRejectsDegradedInventory(t *testing.T) {
	authority := &fakeCodexRegistryAuthority{inventories: []codexprov.Inventory{{
		ExternalSources: []codexprov.ExternalSourceStatus{{Name: "codexbar", ErrorCode: "unavailable"}},
	}}}
	client := cyberModelsDoer(func(*http.Request) (*http.Response, error) {
		t.Fatal("degraded inventory must not be used for account discovery")
		return nil, nil
	})
	if _, _, err := discoverCyberAccounts(context.Background(), authority, client, "https://codex.example", "0.155.0"); err != errCodexRegistryCredentialInventoryDegraded {
		t.Fatalf("degraded inventory error = %v", err)
	}
}
