package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type failingCodexHealthInventory struct{ err error }

func (i failingCodexHealthInventory) List(context.Context) (codexprov.Inventory, error) {
	return codexprov.Inventory{}, i.err
}

type deadlineCodexHealthInventory struct{ sawDeadline bool }

func (i *deadlineCodexHealthInventory) List(ctx context.Context) (codexprov.Inventory, error) {
	_, i.sawDeadline = ctx.Deadline()
	return codexprov.Inventory{}, errors.New("coordinator unavailable")
}

func TestCodexInventoryStatusDiagnosticsArePrivacySafe(t *testing.T) {
	sensitiveIdentity := codexprov.AccountIdentity{
		AccountID: "provider-account-secret",
		UserID:    "workspace-account-secret",
		Email:     "private@example.test",
		PlanType:  "/Library/Application Support/CodexBar/managed-codex-homes/private token-fingerprint-secret raw error with token-secret",
		RecordKey: "file-identity-secret",
	}
	sensitiveCandidate := codexprov.CredentialCandidate{
		Ref:      codexprov.CandidateRef{AccountKey: "account-key-secret", CandidateID: "manifest-record-secret"},
		Revision: "credential-revision-secret",
		Credential: codexprov.CodexAccount{
			AccessToken:  "access-token-secret",
			RefreshToken: "refresh-token-secret",
			IDToken:      "id-token-secret",
		},
	}
	tests := []struct {
		name       string
		inventory  codexprov.Inventory
		wantJSON   string
		wantStderr string
	}{
		{
			name: "present",
			inventory: codexprov.Inventory{
				Accounts:        []codexprov.LogicalAccount{{Identity: sensitiveIdentity, Candidates: []codexprov.CredentialCandidate{sensitiveCandidate}}},
				ExternalSources: []codexprov.ExternalSourceStatus{{Name: "codexbar", CandidateCount: 1}},
			},
			wantJSON:   `{"account_count":1,"external_sources":[{"name":"codexbar","candidate_count":1,"health_code":"ok"}]}`,
			wantStderr: "cq: codex accounts: 1\ncq: codex source: name=codexbar candidates=1 health=ok\n",
		},
		{
			name: "absent",
			inventory: codexprov.Inventory{
				Accounts:        []codexprov.LogicalAccount{{Identity: sensitiveIdentity, Candidates: []codexprov.CredentialCandidate{sensitiveCandidate}}},
				ExternalSources: []codexprov.ExternalSourceStatus{{Name: "codexbar", ErrorCode: "unavailable"}},
			},
			wantJSON:   `{"account_count":1,"external_sources":[{"name":"codexbar","candidate_count":0,"health_code":"unavailable"}]}`,
			wantStderr: "cq: codex accounts: 1\ncq: codex source: name=codexbar candidates=0 health=unavailable\n",
		},
		{
			name: "optional absent",
			inventory: codexprov.Inventory{
				Accounts: []codexprov.LogicalAccount{{Identity: sensitiveIdentity, Candidates: []codexprov.CredentialCandidate{sensitiveCandidate}}},
				ExternalSources: []codexprov.ExternalSourceStatus{{
					Name: "codexbar", ErrorCode: "unavailable", OptionalAbsent: true,
				}},
			},
			wantJSON:   `{"account_count":1,"external_sources":[{"name":"codexbar","candidate_count":0,"health_code":"ok"}]}`,
			wantStderr: "cq: codex accounts: 1\ncq: codex source: name=codexbar candidates=0 health=ok\n",
		},
		{
			name: "invalid",
			inventory: codexprov.Inventory{
				Accounts:        []codexprov.LogicalAccount{{Identity: sensitiveIdentity, Candidates: []codexprov.CredentialCandidate{sensitiveCandidate}}},
				ExternalSources: []codexprov.ExternalSourceStatus{{Name: "codexbar", ErrorCode: "invalid"}},
			},
			wantJSON:   `{"account_count":1,"external_sources":[{"name":"codexbar","candidate_count":0,"health_code":"invalid"}]}`,
			wantStderr: "cq: codex accounts: 1\ncq: codex source: name=codexbar candidates=0 health=invalid\n",
		},
		{
			name:       "zero sources",
			inventory:  codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Identity: sensitiveIdentity, Candidates: []codexprov.CredentialCandidate{sensitiveCandidate}}}},
			wantJSON:   `{"account_count":1,"external_sources":[]}`,
			wantStderr: "cq: codex accounts: 1\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := codexHealthFromInventory(tt.inventory)
			encoded, err := json.Marshal(health)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tt.wantJSON {
				t.Fatalf("JSON = %s, want %s", encoded, tt.wantJSON)
			}

			var stderr bytes.Buffer
			writeCodexHealthDiagnostics(&stderr, health)
			if stderr.String() != tt.wantStderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}

			combined := string(encoded) + stderr.String()
			for _, forbidden := range []string{
				"/Library/Application Support/CodexBar/managed-codex-homes/private",
				"manifest-record-secret", "private@example.test", "provider-account-secret",
				"workspace-account-secret", "file-identity-secret", "token-fingerprint-secret",
				"credential-revision-secret", "access-token-secret", "refresh-token-secret",
				"id-token-secret", "raw error with token-secret",
			} {
				if strings.Contains(combined, forbidden) {
					t.Fatalf("diagnostics exposed forbidden fixture %q: %s", forbidden, combined)
				}
			}
		})
	}
}

func TestCodexInventoryStatusDiagnosticsRejectUntypedHealth(t *testing.T) {
	inventory := codexprov.Inventory{ExternalSources: []codexprov.ExternalSourceStatus{{
		Name:      "codexbar",
		ErrorCode: "open /private/managed-home for private@example.test: token-secret",
	}}}

	health := codexHealthFromInventory(inventory)
	if len(health.ExternalSources) != 1 || health.ExternalSources[0].HealthCode != "unknown" {
		t.Fatalf("health = %+v, want typed unknown code", health)
	}
	var stderr bytes.Buffer
	writeCodexHealthDiagnostics(&stderr, health)
	for _, forbidden := range []string{"/private/managed-home", "private@example.test", "token-secret"} {
		if strings.Contains(stderr.String(), forbidden) {
			t.Fatalf("stderr exposed raw error fixture %q: %s", forbidden, stderr.String())
		}
	}
}

func TestCodexHealthTrackerDegradesSafelyOnCoordinatorListFailure(t *testing.T) {
	last := codexHealthFromInventory(codexprov.Inventory{
		Accounts: []codexprov.LogicalAccount{{}, {}},
		ExternalSources: []codexprov.ExternalSourceStatus{{
			Name: "codexbar", CandidateCount: 1,
		}},
	})
	tracker := newCodexHealthTracker(failingCodexHealthInventory{err: errors.New(
		"open /private/managed-home for private@example.test: token-secret",
	)}, "", last)

	health := tracker.Health(context.Background())
	if health.AccountCount != 2 || !health.AccountCountKnown {
		t.Fatalf("account count = %d known=%t, want last-known 2", health.AccountCount, health.AccountCountKnown)
	}
	if health.HealthCode != "stale" {
		t.Fatalf("health code = %q, want stale", health.HealthCode)
	}
	if len(health.ExternalSources) != 1 || health.ExternalSources[0].Name != "codexbar" {
		t.Fatalf("external source snapshot = %+v, want last-known source", health.ExternalSources)
	}

	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	writeCodexHealthDiagnostics(&stderr, health)
	combined := string(encoded) + stderr.String()
	for _, forbidden := range []string{"/private/managed-home", "private@example.test", "token-secret"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("degraded diagnostics exposed raw error fixture %q: %s", forbidden, combined)
		}
	}
}

func TestCodexHealthTrackerDistinguishesStaleSnapshotFromColdUnavailable(t *testing.T) {
	coordinator, fsys, path, before := newReadableManagedInventoryCoordinator(t)
	tracker := newCodexHealthTracker(coordinator, "", proxy.CodexHealth{})

	fresh := tracker.Health(context.Background())
	if fresh.AccountCount != 1 || !fresh.AccountCountKnown || fresh.HealthCode != "ok" {
		t.Fatalf("fresh health = %+v, want one known healthy account", fresh)
	}

	fsys.setFailing(true)
	stale := tracker.Health(context.Background())
	if stale.AccountCount != 1 || !stale.AccountCountKnown || stale.HealthCode != "stale" {
		t.Fatalf("stale health = %+v, want retained logical inventory marked stale", stale)
	}
	cold := newCodexHealthTracker(coordinator, "", proxy.CodexHealth{}).Health(context.Background())
	if cold.AccountCountKnown || cold.HealthCode != "unavailable" || cold.ExternalSources == nil {
		t.Fatalf("cold health = %+v, want unknown account count and typed unavailable state", cold)
	}
	after, err := fsys.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("health discovery changed readable CQ credential: %v", err)
	}
}

func TestCodexHealthTrackerBoundsCoordinatorList(t *testing.T) {
	inventory := &deadlineCodexHealthInventory{}
	tracker := newCodexHealthTracker(inventory, "", codexHealthFromInventory(codexprov.Inventory{}))

	_ = tracker.Health(context.Background())

	if !inventory.sawDeadline {
		t.Fatal("coordinator List context has no deadline")
	}
}

func TestCodexHealthTrackerPreservesEmptySourceSnapshotOnFailure(t *testing.T) {
	last := codexHealthFromInventory(codexprov.Inventory{})
	if last.ExternalSources == nil {
		t.Fatal("test fixture external sources are nil")
	}
	tracker := newCodexHealthTracker(failingCodexHealthInventory{err: errors.New("unavailable")}, "", last)

	health := tracker.Health(context.Background())
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"external_sources":[]`) {
		t.Fatalf("health JSON = %s, want empty external_sources array", encoded)
	}
}
