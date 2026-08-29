package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

type fakeCodexRegistryAuthority struct {
	mu          sync.Mutex
	inventories []codexprov.Inventory
	listErr     error
	listCalls   int
	resolve     func(int, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error)
	resolvePlan []codexprov.PlannedCandidate
	refresh     func(codexprov.CandidateRef, codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error)
	refreshCall int
	refreshArgs []registryRefreshArgs
}

type readableFailingManagedInventoryFS struct {
	*fsutil.MemFS
	mu      sync.Mutex
	failing bool
}

func (f *readableFailingManagedInventoryFS) ReadDir(name string) ([]os.DirEntry, error) {
	f.mu.Lock()
	failing := f.failing
	f.mu.Unlock()
	if failing && filepath.Clean(name) == "/home/test/.codex/accounts" {
		return nil, errors.New("private managed directory and token-secret")
	}
	return f.MemFS.ReadDir(name)
}

func (f *readableFailingManagedInventoryFS) setFailing(failing bool) {
	f.mu.Lock()
	f.failing = failing
	f.mu.Unlock()
}

func newReadableManagedInventoryCoordinator(t testing.TB) (*codexprov.CredentialCoordinator, *readableFailingManagedInventoryFS, string, []byte) {
	t.Helper()
	fsys := &readableFailingManagedInventoryFS{MemFS: fsutil.NewMemFS()}
	path := "/home/test/.codex/accounts/readable.auth.json"
	if err := fsys.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": "readable-filesystem-token", "refresh_token": "readable-filesystem-refresh",
			"id_token": registryCredentialJWT(identity), "account_id": identity.AccountID,
		},
		"cq_expires_at": time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := codexprov.NewManagedStore(fsys)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := codexprov.NewCredentialCoordinator(store, testCQRoots().State)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, fsys, path, append([]byte(nil), data...)
}

type registryRefreshArgs struct {
	ref      codexprov.CandidateRef
	revision codexprov.Revision
}

func (a *fakeCodexRegistryAuthority) List(context.Context) (codexprov.Inventory, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listCalls++
	if a.listErr != nil {
		return codexprov.Inventory{}, a.listErr
	}
	if len(a.inventories) == 0 {
		return codexprov.Inventory{}, errors.New("unexpected inventory read")
	}
	index := a.listCalls - 1
	if index >= len(a.inventories) {
		index = len(a.inventories) - 1
	}
	return a.inventories[index], nil
}

func (a *fakeCodexRegistryAuthority) ResolveExact(_ context.Context, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resolvePlan = append(a.resolvePlan, planned)
	if a.resolve == nil {
		return codexprov.CredentialMaterial{}, errors.New("unexpected exact resolve")
	}
	return a.resolve(len(a.resolvePlan), planned)
}

func (a *fakeCodexRegistryAuthority) RefreshManagedReference(_ context.Context, ref codexprov.CandidateRef, revision codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshCall++
	a.refreshArgs = append(a.refreshArgs, registryRefreshArgs{ref: ref, revision: revision})
	if a.refresh == nil {
		return codexprov.CandidateRef{}, "", errors.New("unexpected refresh")
	}
	return a.refresh(ref, revision)
}

func (a *fakeCodexRegistryAuthority) refreshSnapshot() []registryRefreshArgs {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]registryRefreshArgs(nil), a.refreshArgs...)
}

func (a *fakeCodexRegistryAuthority) snapshot() (int, []codexprov.PlannedCandidate, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listCalls, append([]codexprov.PlannedCandidate(nil), a.resolvePlan...), a.refreshCall
}

func registryCredentialJWT(identity codexprov.AccountIdentity) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{
		"email": identity.Email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": identity.AccountID,
			"chatgpt_user_id":    identity.UserID,
			"chatgpt_plan_type":  identity.PlanType,
		},
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func registryCredentialMaterial(identity codexprov.AccountIdentity, token string) codexprov.CredentialMaterial {
	return codexprov.CredentialMaterial{
		AccessToken: token,
		IDToken:     registryCredentialJWT(identity),
		AccountID:   identity.AccountID,
	}
}

func registryInventory(identity codexprov.AccountIdentity, candidates ...codexprov.CredentialCandidate) codexprov.Inventory {
	return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
		Key:        candidates[0].Ref.AccountKey,
		Identity:   identity,
		Candidates: candidates,
		Routable:   true,
	}}}
}

func TestCodexRegistryTokenUsesExternalOnlyAuthority(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{
		AccountID: "account-external", UserID: "user-external", Email: "external@example.test", PlanType: "plus",
	}
	ref := codexprov.CandidateRef{AccountKey: "logical-external", CandidateID: "candidate-external"}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, codexprov.CredentialCandidate{
			Ref: ref, Revision: "revision-external", Source: codexprov.SourceExternal,
			AccessExpiresAt: now.Add(time.Hour), Routable: true,
		})},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return registryCredentialMaterial(planned.Identity, "external-access-token"), nil
		},
	}

	token, err := codexRegistryAccessToken(context.Background(), authority, now)
	if err != nil {
		t.Fatalf("codexRegistryAccessToken() error = %v", err)
	}
	if token != "external-access-token" {
		t.Fatalf("token = %q, want external authority token", token)
	}
	listCalls, plans, refreshCalls := authority.snapshot()
	if listCalls != 1 || len(plans) != 1 || refreshCalls != 0 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want 1/1/0", listCalls, len(plans), refreshCalls)
	}
	if plans[0].Source != codexprov.SourceExternal || plans[0].Ref != ref || plans[0].Revision != "revision-external" {
		t.Fatalf("resolved plan = %+v, want exact external generation", plans[0])
	}
}

func TestCodexRegistryTokenReplansOneExternalRotation(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	ref := codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "external-one"}
	first := registryInventory(identity, codexprov.CredentialCandidate{
		Ref: ref, Revision: "revision-one", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	})
	rotated := registryInventory(identity, codexprov.CredentialCandidate{
		Ref: ref, Revision: "revision-two", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(2 * time.Hour), Routable: true,
	})
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{first, rotated},
		resolve: func(call int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			if call == 1 {
				return codexprov.CredentialMaterial{}, codexprov.ErrStaleRevision
			}
			return registryCredentialMaterial(planned.Identity, "rotated-access-token"), nil
		},
	}

	token, err := codexRegistryAccessToken(context.Background(), authority, now)
	if err != nil {
		t.Fatalf("codexRegistryAccessToken() error = %v", err)
	}
	if token != "rotated-access-token" {
		t.Fatalf("token = %q, want rotated token", token)
	}
	listCalls, plans, refreshCalls := authority.snapshot()
	if listCalls != 2 || len(plans) != 2 || refreshCalls != 0 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want 2/2/0", listCalls, len(plans), refreshCalls)
	}
	if plans[0].Revision != "revision-one" || plans[1].Revision != "revision-two" {
		t.Fatalf("resolved revisions = %q, %q, want bounded replan", plans[0].Revision, plans[1].Revision)
	}
}

func TestCodexRegistryTokenRejectsIdentityChangingReplan(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	changedIdentity := codexprov.AccountIdentity{AccountID: "account-two", UserID: "user-two", Email: "two@example.test", PlanType: "plus"}
	ref := codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "external-one"}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{
			registryInventory(identity, codexprov.CredentialCandidate{Ref: ref, Revision: "revision-one", Source: codexprov.SourceExternal, AccessExpiresAt: now.Add(time.Hour), Routable: true}),
			registryInventory(changedIdentity, codexprov.CredentialCandidate{Ref: ref, Revision: "revision-two", Source: codexprov.SourceExternal, AccessExpiresAt: now.Add(time.Hour), Routable: true}),
		},
		resolve: func(_ int, _ codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return codexprov.CredentialMaterial{}, codexprov.ErrStaleRevision
		},
	}

	token, err := codexRegistryAccessToken(context.Background(), authority, now)
	if !errors.Is(err, errCodexRegistryCredentialUnavailable) {
		t.Fatalf("error = %v, want generic unavailable", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty", token)
	}
	listCalls, plans, refreshCalls := authority.snapshot()
	if listCalls != 2 || len(plans) != 1 || refreshCalls != 0 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want bounded 2/1/0", listCalls, len(plans), refreshCalls)
	}
}

func TestCodexRegistryTokenNeverRefreshesExternalOrSystem(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	accountKey := codexprov.AccountKey("logical-one")
	externalRef := codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "external-one"}
	systemRef := codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "system-one"}
	managedRef := codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "managed-one"}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity,
			codexprov.CredentialCandidate{Ref: externalRef, Revision: "external-revision", Source: codexprov.SourceExternal, AccessExpiresAt: now.Add(-time.Hour), Routable: true},
			codexprov.CredentialCandidate{Ref: systemRef, Revision: "system-revision", Source: codexprov.SourceSystem, AccessExpiresAt: now.Add(-time.Hour), Routable: true},
			codexprov.CredentialCandidate{Ref: managedRef, Revision: "managed-revision", Source: codexprov.SourceManaged, AccessExpiresAt: now.Add(-time.Hour), Routable: true},
		)},
		refresh: func(ref codexprov.CandidateRef, revision codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
			if ref != managedRef || revision != "managed-revision" {
				return codexprov.CandidateRef{}, "", fmt.Errorf("refreshed non-managed candidate: %+v %q", ref, revision)
			}
			return managedRef, "managed-refreshed", nil
		},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			if planned.Source != codexprov.SourceManaged || planned.Ref != managedRef || planned.Revision != "managed-refreshed" {
				return codexprov.CredentialMaterial{}, errors.New("resolved candidate before managed reference refresh")
			}
			return registryCredentialMaterial(planned.Identity, "managed-access-token"), nil
		},
	}

	token, err := codexRegistryAccessToken(context.Background(), authority, now)
	if err != nil {
		t.Fatalf("codexRegistryAccessToken() error = %v", err)
	}
	if token != "managed-access-token" {
		t.Fatalf("token = %q, want refreshed managed token", token)
	}
	listCalls, plans, refreshCalls := authority.snapshot()
	if listCalls != 1 || len(plans) != 1 || refreshCalls != 1 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want 1/1/1", listCalls, len(plans), refreshCalls)
	}
}

func TestCodexRegistryTokenContainsNoMaterialInPlansOrErrors(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-sensitive", UserID: "user-sensitive", Email: "sensitive@example.test", PlanType: "plus"}
	ref := codexprov.CandidateRef{AccountKey: "logical-sensitive", CandidateID: "external-sensitive"}
	const token = "synthetic-secret-access-token"
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, codexprov.CredentialCandidate{
			Ref: ref, Revision: "revision-sensitive", Source: codexprov.SourceExternal,
			AccessExpiresAt: now.Add(time.Hour), Routable: true,
		})},
		resolve: func(_ int, _ codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return codexprov.CredentialMaterial{}, errors.New("resolver rejected " + token)
		},
	}

	_, err := codexRegistryAccessToken(context.Background(), authority, now)
	if !errors.Is(err, errCodexRegistryCredentialUnavailable) {
		t.Fatalf("error = %v, want generic unavailable", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked token: %q", err)
	}
	_, plans, _ := authority.snapshot()
	if len(plans) != 1 {
		t.Fatalf("resolve plans = %d, want 1", len(plans))
	}
	if strings.Contains(fmt.Sprintf("%+v", plans[0]), token) {
		t.Fatalf("plan leaked token: %+v", plans[0])
	}
}

func TestCodexRegistryTokenKeepsCoordinatorFailureTypedAndSanitised(t *testing.T) {
	const sensitiveFailure = "dial failed for /private/token-path with bearer-secret"
	authority := &fakeCodexRegistryAuthority{listErr: errors.New(sensitiveFailure)}

	_, err := codexRegistryAccessToken(context.Background(), authority, time.Now())
	if !errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
		t.Fatalf("error = %v, want typed authority unavailable", err)
	}
	if strings.Contains(err.Error(), sensitiveFailure) || strings.Contains(err.Error(), "token-path") || strings.Contains(err.Error(), "bearer-secret") {
		t.Fatalf("error leaked coordinator detail: %q", err)
	}
	listCalls, plans, refreshCalls := authority.snapshot()
	if listCalls != 1 || len(plans) != 0 || refreshCalls != 0 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want 1/0/0", listCalls, len(plans), refreshCalls)
	}
}

func TestCodexRegistryModelsRealCoordinatorFailureDoesNotUseReadableCQFiles(t *testing.T) {
	coordinator, fsys, path, before := newReadableManagedInventoryCoordinator(t)
	fsys.setFailing(true)
	authority := &registryCoordinatorAuthority{coordinator: coordinator}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"readable-filesystem-token": http.StatusOK}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, time.Now(), req)
	if response != nil {
		response.Body.Close()
		t.Fatalf("response = %+v, want authority failure before dispatch", response)
	}
	if !errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
		t.Fatalf("error = %v, want typed registry authority unavailable", err)
	}
	if strings.Contains(err.Error(), "private managed directory") || strings.Contains(err.Error(), "token-secret") {
		t.Fatalf("registry authority error exposed private detail: %v", err)
	}
	attempts, _ := doer.snapshot()
	if len(attempts) != 0 {
		t.Fatalf("HTTP attempts = %q, want none", attempts)
	}
	after, readErr := fsys.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("readable CQ credential changed after registry failure: %v", readErr)
	}
}

func TestRegistryPipelineAllSourceFailurePreservesCodexAuthorityType(t *testing.T) {
	coordinator, fsys, path, before := newReadableManagedInventoryCoordinator(t)
	fsys.setFailing(true)
	pipeline, err := newRegistryPipelineWithCodexAuthority(registryPipelineOptions{
		FS: fsys, HomeDir: "/home/test", Roots: testCQRoots(), ClaudeUpstream: "https://claude.example", CodexUpstream: "https://codex.example",
		HTTPClient: &scriptedRegistryModelsDoer{}, ClaudeToken: func() (string, error) {
			return "", errors.New("Anthropic unavailable")
		}, Env: func(string) string { return "" }, Stderr: io.Discard,
	}, &registryCoordinatorAuthority{coordinator: coordinator})
	if err != nil {
		t.Fatalf("newRegistryPipelineWithCodexAuthority() error = %v", err)
	}

	diagnostics, err := pipeline.Refresher.Refresh(context.Background())
	if !errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
		t.Fatalf("Refresh() error = %v, want typed Codex authority unavailable", err)
	}
	if !errors.Is(diagnostics.SourceErrors[modelregistry.ProviderCodex], errCodexRegistryCredentialAuthorityUnavailable) {
		t.Fatalf("Codex source error = %v, want typed authority unavailable", diagnostics.SourceErrors[modelregistry.ProviderCodex])
	}
	after, readErr := fsys.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("registry refresh changed managed credential: %v", readErr)
	}
}

func TestCodexRegistryModelsKeepsDegradedExternalInventoryTerminalTyped(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	candidate := codexprov.CredentialCandidate{
		Ref:      codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "managed-one"},
		Revision: "managed-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	tests := []struct {
		name       string
		candidate  bool
		status     int
		wantUsable bool
		wantCalls  int
	}{
		{name: "no visible candidates"},
		{name: "visible candidate rejected", candidate: true, status: http.StatusUnauthorized, wantCalls: 1},
		{name: "visible candidate succeeds", candidate: true, status: http.StatusOK, wantUsable: true, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inventory := codexprov.Inventory{
				ExternalSources: []codexprov.ExternalSourceStatus{{Name: "codexbar", ErrorCode: "invalid"}},
			}
			if test.candidate {
				inventory = registryInventory(identity, candidate)
				inventory.ExternalSources = []codexprov.ExternalSourceStatus{{Name: "codexbar", ErrorCode: "invalid"}}
			}
			authority := &fakeCodexRegistryAuthority{
				inventories: []codexprov.Inventory{inventory},
				resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
					return registryCredentialMaterial(planned.Identity, "visible-managed-token"), nil
				},
			}
			doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"visible-managed-token": test.status}}
			req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
			if test.wantUsable {
				if err != nil || response == nil || response.StatusCode != http.StatusOK {
					t.Fatalf("response/error = %+v/%v, want visible candidate success", response, err)
				}
				response.Body.Close()
			} else {
				if response != nil {
					response.Body.Close()
					t.Fatalf("response = %+v, want typed degraded error", response)
				}
				if !errors.Is(err, errCodexRegistryCredentialInventoryDegraded) {
					t.Fatalf("error = %v, want typed degraded inventory", err)
				}
			}
			attempts, _ := doer.snapshot()
			if len(attempts) != test.wantCalls {
				t.Fatalf("HTTP attempts = %q, want %d", attempts, test.wantCalls)
			}
		})
	}
}

func TestCodexRegistryModelsIgnoresAbsentOptionalExternalSource(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	candidate := codexprov.CredentialCandidate{
		Ref:      codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "managed-one"},
		Revision: "managed-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	tests := []struct {
		name      string
		candidate bool
		wantCalls int
	}{
		{name: "no visible candidates"},
		{name: "visible candidate rejected", candidate: true, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inventory := codexprov.Inventory{ExternalSources: []codexprov.ExternalSourceStatus{{
				Name: "codexbar", ErrorCode: "unavailable", OptionalAbsent: true,
			}}}
			if test.candidate {
				inventory = registryInventory(identity, candidate)
				inventory.ExternalSources = []codexprov.ExternalSourceStatus{{
					Name: "codexbar", ErrorCode: "unavailable", OptionalAbsent: true,
				}}
			}
			authority := &fakeCodexRegistryAuthority{
				inventories: []codexprov.Inventory{inventory},
				resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
					return registryCredentialMaterial(planned.Identity, "visible-managed-token"), nil
				},
			}
			doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"visible-managed-token": http.StatusUnauthorized}}
			req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
			if test.candidate {
				if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("response/error = %+v/%v, want visible 401", response, err)
				}
				response.Body.Close()
			} else if response != nil || !errors.Is(err, errCodexRegistryCredentialUnavailable) {
				closeCodexRegistryResponse(response)
				t.Fatalf("response/error = %+v/%v, want no usable credential", response, err)
			}
			attempts, _ := doer.snapshot()
			if len(attempts) != test.wantCalls {
				t.Fatalf("HTTP attempts = %q, want %d", attempts, test.wantCalls)
			}
		})
	}
}

func TestCodexRegistryModelsExternalSourceDisappearsDuringExactResolution(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	idToken := registryCredentialJWT(identity)
	root, _, manifestPath := writeRegistryCodexBarFixture(t, identity, "external-token", idToken)
	fsys := fsutil.NewMemFS()
	store, err := codexprov.NewManagedStore(fsys)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := codexprov.NewCredentialCoordinator(store, testCQRoots().State)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ExternalSources = []codexprov.ExternalCredentialSource{codexprov.NewCodexBarSource(root)}
	var removeOnce sync.Once
	authority := &registryCoordinatorAuthority{
		coordinator: coordinator,
		beforeResolve: func() {
			removeOnce.Do(func() {
				if err := os.Remove(manifestPath); err != nil {
					t.Fatalf("Remove(CodexBar manifest) error = %v", err)
				}
			})
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"external-token": http.StatusOK}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if response != nil {
		response.Body.Close()
		t.Fatalf("response = %+v, want degraded inventory error", response)
	}
	if !errors.Is(err, errCodexRegistryCredentialInventoryDegraded) {
		t.Fatalf("error = %v, want typed degraded inventory", err)
	}
	attempts, _ := doer.snapshot()
	if len(attempts) != 0 {
		t.Fatalf("HTTP attempts = %q, want none after source loss", attempts)
	}
}

func writeRegistryCodexBarFixture(t *testing.T, identity codexprov.AccountIdentity, accessToken, idToken string) (root, authPath, manifestPath string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "CodexBar")
	managedHome := filepath.Join(root, "managed-codex-homes", "external-one")
	if err := os.MkdirAll(managedHome, 0o700); err != nil {
		t.Fatalf("MkdirAll(CodexBar managed home) error = %v", err)
	}
	for _, path := range []string{root, filepath.Dir(managedHome), managedHome} {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatalf("Chmod(%s) error = %v", filepath.Base(path), err)
		}
	}
	authBytes, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": accessToken, "refresh_token": "synthetic-codexbar-refresh-token",
			"id_token": idToken, "account_id": identity.AccountID,
		},
	})
	if err != nil {
		t.Fatalf("Marshal(CodexBar auth) error = %v", err)
	}
	authPath = filepath.Join(managedHome, "auth.json")
	if err := os.WriteFile(authPath, authBytes, 0o600); err != nil {
		t.Fatalf("WriteFile(CodexBar auth) error = %v", err)
	}
	fingerprint := sha256.Sum256(authBytes)
	manifestBytes, err := json.Marshal(map[string]any{
		"version": 3,
		"accounts": []any{map[string]any{
			"id": "external-one", "managedHomePath": managedHome,
			"providerAccountID": identity.AccountID, "workspaceAccountID": identity.AccountID,
			"authFingerprint": hex.EncodeToString(fingerprint[:]),
		}},
	})
	if err != nil {
		t.Fatalf("Marshal(CodexBar manifest) error = %v", err)
	}
	manifestPath = filepath.Join(root, "managed-codex-accounts.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatalf("WriteFile(CodexBar manifest) error = %v", err)
	}
	return root, authPath, manifestPath
}

type registryCoordinatorAuthority struct {
	coordinator   *codexprov.CredentialCoordinator
	beforeResolve func()
	mu            sync.Mutex
	plans         []codexprov.PlannedCandidate
	refreshes     int
}

func (a *registryCoordinatorAuthority) List(ctx context.Context) (codexprov.Inventory, error) {
	return a.coordinator.List(ctx)
}

func (a *registryCoordinatorAuthority) ResolveExact(ctx context.Context, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
	a.mu.Lock()
	a.plans = append(a.plans, planned)
	a.mu.Unlock()
	if a.beforeResolve != nil {
		a.beforeResolve()
	}
	return a.coordinator.ResolveExact(ctx, planned)
}

func (a *registryCoordinatorAuthority) RefreshManagedReference(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
	a.mu.Lock()
	a.refreshes++
	a.mu.Unlock()
	return codexprov.CandidateRef{}, "", codexprov.ErrRefreshIneligible
}

func (a *registryCoordinatorAuthority) snapshot() ([]codexprov.PlannedCandidate, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]codexprov.PlannedCandidate(nil), a.plans...), a.refreshes
}

type registryCloseTrackingBody struct {
	io.Reader
	once    sync.Once
	onClose func()
}

func (b *registryCloseTrackingBody) Close() error {
	b.once.Do(b.onClose)
	return nil
}

type inverseRegistryModelsDoer struct {
	staleToken string
	freshToken string

	mu          sync.Mutex
	attempts    []string
	rejectClose int
}

func (d *inverseRegistryModelsDoer) Do(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	d.mu.Lock()
	d.attempts = append(d.attempts, token)
	d.mu.Unlock()

	status := http.StatusUnauthorized
	body := io.ReadCloser(&registryCloseTrackingBody{
		Reader: strings.NewReader(`{"error":"unauthorised"}`),
		onClose: func() {
			d.mu.Lock()
			d.rejectClose++
			d.mu.Unlock()
		},
	})
	if token == d.freshToken {
		status = http.StatusOK
		body = io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5"}]}`))
	} else if token != d.staleToken {
		status = http.StatusForbidden
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: req}, nil
}

func (d *inverseRegistryModelsDoer) snapshot() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.attempts...), d.rejectClose
}

func registryAccessJWT(expiresAt time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{"exp": expiresAt.Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestCodexRegistryModelsRetriesFreshExternalAfterStaleManaged401WithoutWrites(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus", RecordKey: "user-one::account-one"}
	idToken := registryCredentialJWT(identity)
	staleManagedToken := registryAccessJWT(now.Add(2 * time.Hour))
	const freshExternalToken = "synthetic-fresh-external-token"

	fsys := fsutil.NewMemFS()
	store, err := codexprov.NewManagedStore(fsys)
	if err != nil {
		t.Fatalf("NewManagedStore() error = %v", err)
	}
	coordinator, err := codexprov.NewCredentialCoordinator(store, testCQRoots().State)
	if err != nil {
		t.Fatalf("NewCredentialCoordinator() error = %v", err)
	}
	managedRef, _, err := coordinator.SaveLogin(context.Background(), codexprov.LoginCredential{
		Tokens: auth.CodexTokenResponse{
			AccessToken: staleManagedToken, RefreshToken: "synthetic-managed-refresh-token", IDToken: idToken,
		},
		Claims: auth.DecodeCodexClaims(idToken), CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("SaveLogin() error = %v", err)
	}

	codexBarRoot, externalPath, manifestPath := writeRegistryCodexBarFixture(t, identity, freshExternalToken, idToken)
	coordinator.ExternalSources = []codexprov.ExternalCredentialSource{codexprov.NewCodexBarSource(codexBarRoot)}

	managedPath := filepath.Join("/home/test/.codex/accounts", string(managedRef.CandidateID)+".auth.json")
	managedBefore, err := fsys.ReadFile(managedPath)
	if err != nil {
		t.Fatalf("ReadFile(managed before) error = %v", err)
	}
	externalBefore, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatalf("ReadFile(external before) error = %v", err)
	}
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(manifest before) error = %v", err)
	}

	authority := &registryCoordinatorAuthority{coordinator: coordinator}
	doer := &inverseRegistryModelsDoer{staleToken: staleManagedToken, freshToken: freshExternalToken}
	pipeline, err := newRegistryPipelineWithCodexAuthority(registryPipelineOptions{
		FS: fsys, HomeDir: "/home/test", Roots: testCQRoots(), ClaudeUpstream: "https://claude.example", CodexUpstream: "https://codex.example",
		HTTPClient: doer, CodexClientVersion: "test-client", ClaudeToken: func() (string, error) {
			return "", errors.New("Claude unavailable")
		}, Env: func(string) string { return "" }, Stderr: io.Discard,
	}, authority)
	if err != nil {
		t.Fatalf("newRegistryPipelineWithCodexAuthority() error = %v", err)
	}
	diagnostics, err := pipeline.Refresher.Refresh(context.Background())
	if err != nil {
		attempts, _ := doer.snapshot()
		plans, refreshes := authority.snapshot()
		inventory, _ := authority.List(context.Background())
		t.Fatalf("Refresh() error = %v; HTTP attempts = %q; plans = %+v; refreshes = %d; inventory = %+v", err, attempts, plans, refreshes, inventory)
	}
	if got := diagnostics.Counts["codex"]; got != 1 {
		t.Fatalf("Codex model count = %d, want 1", got)
	}

	attempts, rejectClose := doer.snapshot()
	if len(attempts) != 2 || attempts[0] != staleManagedToken || attempts[1] != freshExternalToken {
		t.Fatalf("HTTP credential attempts = %q, want stale managed then fresh external", attempts)
	}
	if rejectClose != 1 {
		t.Fatalf("rejected response closes = %d, want 1", rejectClose)
	}
	plans, refreshes := authority.snapshot()
	if len(plans) != 2 || plans[0].Source != codexprov.SourceManaged || plans[1].Source != codexprov.SourceExternal {
		t.Fatalf("exact plans = %+v, want managed then same-identity external", plans)
	}
	if plans[0].Ref.AccountKey != plans[1].Ref.AccountKey || plans[0].Identity != plans[1].Identity {
		t.Fatalf("exact plans crossed identity: %+v", plans)
	}
	if refreshes != 0 {
		t.Fatalf("managed refresh calls = %d, want 0 before same-identity external succeeds", refreshes)
	}

	managedAfter, err := fsys.ReadFile(managedPath)
	if err != nil {
		t.Fatalf("ReadFile(managed after) error = %v", err)
	}
	externalAfter, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatalf("ReadFile(external after) error = %v", err)
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(manifest after) error = %v", err)
	}
	if !bytes.Equal(managedAfter, managedBefore) || !bytes.Equal(externalAfter, externalBefore) || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatal("registry discovery changed managed credential or Codex Bar bytes")
	}
}

type scriptedRegistryModelsDoer struct {
	statusByToken map[string]int
	mu            sync.Mutex
	attempts      []string
	rejectClose   int
}

type transportRegistryModelsDoer struct {
	err      error
	mu       sync.Mutex
	attempts []string
}

func (d *transportRegistryModelsDoer) Do(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	d.mu.Lock()
	d.attempts = append(d.attempts, token)
	attempt := len(d.attempts)
	d.mu.Unlock()
	if attempt == 1 {
		return nil, d.err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-5.5"}]}`)), Request: req,
	}, nil
}

func (d *transportRegistryModelsDoer) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.attempts...)
}

type cancellingRegistryModelsDoer struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	closed int
}

func (d *cancellingRegistryModelsDoer) Do(req *http.Request) (*http.Response, error) {
	d.cancel()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: &registryCloseTrackingBody{Reader: strings.NewReader(`{"models":[]}`), onClose: func() {
			d.mu.Lock()
			d.closed++
			d.mu.Unlock()
		}},
		Request: req,
	}, nil
}

func (d *cancellingRegistryModelsDoer) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func (d *scriptedRegistryModelsDoer) Do(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	d.mu.Lock()
	d.attempts = append(d.attempts, token)
	d.mu.Unlock()
	status := d.statusByToken[token]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	body := io.ReadCloser(io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-5.5"}]}`)))
	if status != http.StatusOK {
		body = &registryCloseTrackingBody{
			Reader: strings.NewReader(`{"error":"rejected"}`),
			onClose: func() {
				d.mu.Lock()
				d.rejectClose++
				d.mu.Unlock()
			},
		}
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: req}, nil
}

func (d *scriptedRegistryModelsDoer) snapshot() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.attempts...), d.rejectClose
}

func TestCodexRegistryModelsKeeps403RetryWithinSameIdentity(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identityA := codexprov.AccountIdentity{AccountID: "account-a", UserID: "user-a", Email: "a@example.test", PlanType: "plus"}
	identityB := codexprov.AccountIdentity{AccountID: "account-b", UserID: "user-b", Email: "b@example.test", PlanType: "plus"}
	accountA := codexprov.AccountKey("logical-a")
	accountB := codexprov.AccountKey("logical-b")
	managedA := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "managed-a"}, Revision: "managed-a-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(3 * time.Hour), CQAuthored: true, Routable: true,
	}
	externalA := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "external-a"}, Revision: "external-a-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(4 * time.Hour), Routable: true,
	}
	managedB := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountB, CandidateID: "managed-b"}, Revision: "managed-b-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(2 * time.Hour), CQAuthored: true, Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{{Accounts: []codexprov.LogicalAccount{
			{Key: accountA, Identity: identityA, Candidates: []codexprov.CredentialCandidate{managedA, externalA}, Routable: true},
			{Key: accountB, Identity: identityB, Candidates: []codexprov.CredentialCandidate{managedB}, Routable: true},
		}}},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			token := map[codexprov.CandidateID]string{
				"managed-a": "stale-managed-a", "external-a": "fresh-external-a", "managed-b": "usable-managed-b",
			}[planned.Ref.CandidateID]
			return registryCredentialMaterial(planned.Identity, token), nil
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{
		"stale-managed-a": http.StatusForbidden, "fresh-external-a": http.StatusOK, "usable-managed-b": http.StatusOK,
	}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if err != nil {
		t.Fatalf("codexRegistryModelsRequest() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("base request Authorization = %q, want unchanged request", got)
	}
	attempts, rejectClose := doer.snapshot()
	if len(attempts) != 2 || attempts[0] != "stale-managed-a" || attempts[1] != "fresh-external-a" {
		t.Fatalf("HTTP credential attempts = %q, want account A managed then account A external", attempts)
	}
	if rejectClose != 1 {
		t.Fatalf("rejected response closes = %d, want 1", rejectClose)
	}
	_, plans, refreshes := authority.snapshot()
	wantStrongIdentity := codexprov.AccountIdentity{AccountID: identityA.AccountID, UserID: identityA.UserID}
	if len(plans) != 2 || plans[0].Identity != wantStrongIdentity || plans[1].Identity != wantStrongIdentity {
		t.Fatalf("exact plans crossed identity before same-account exhaustion: %+v", plans)
	}
	if refreshes != 0 {
		t.Fatalf("refresh calls = %d, want 0", refreshes)
	}
}

func TestCodexRegistryModelsDoesNotRetryTransportFailure(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identityA := codexprov.AccountIdentity{AccountID: "account-a", UserID: "user-a", Email: "a@example.test", PlanType: "plus"}
	identityB := codexprov.AccountIdentity{AccountID: "account-b", UserID: "user-b", Email: "b@example.test", PlanType: "plus"}
	candidateA := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: "logical-a", CandidateID: "external-a"}, Revision: "external-a-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(2 * time.Hour), Routable: true,
	}
	candidateB := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: "logical-b", CandidateID: "external-b"}, Revision: "external-b-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{{Accounts: []codexprov.LogicalAccount{
			{Key: candidateA.Ref.AccountKey, Identity: identityA, Candidates: []codexprov.CredentialCandidate{candidateA}, Routable: true},
			{Key: candidateB.Ref.AccountKey, Identity: identityB, Candidates: []codexprov.CredentialCandidate{candidateB}, Routable: true},
		}}},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return registryCredentialMaterial(planned.Identity, string(planned.Ref.CandidateID)+"-token"), nil
		},
	}
	transportErr := errors.New("synthetic transport failure")
	doer := &transportRegistryModelsDoer{err: transportErr}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if response != nil {
		response.Body.Close()
		t.Fatalf("response = %+v, want transport error", response)
	}
	if !errors.Is(err, transportErr) {
		t.Fatalf("error = %v, want original transport failure", err)
	}
	if attempts := doer.snapshot(); len(attempts) != 1 || attempts[0] != "external-a-token" {
		t.Fatalf("HTTP attempts = %q, want no credential/account retry after transport failure", attempts)
	}
	_, plans, refreshes := authority.snapshot()
	if len(plans) != 1 || refreshes != 0 {
		t.Fatalf("resolve/refresh calls = %d/%d, want 1/0", len(plans), refreshes)
	}
}

func TestCodexRegistryModelsClosesResponseWhenContextCancelsAfterDo(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	candidate := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "external-one"}, Revision: "external-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, candidate)},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return registryCredentialMaterial(planned.Identity, "external-token"), nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doer := &cancellingRegistryModelsDoer{cancel: cancel}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(ctx, authority, doer, now, req)
	if response != nil {
		response.Body.Close()
		t.Fatalf("response = %+v, want cancelled request", response)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if closed := doer.closeCount(); closed != 1 {
		t.Fatalf("discarded response closes = %d, want 1", closed)
	}
}

func TestCodexRegistryModelsRefreshesManagedOnceAfterSameIdentityCandidates(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identityA := codexprov.AccountIdentity{AccountID: "account-a", UserID: "user-a", Email: "a@example.test", PlanType: "plus"}
	identityB := codexprov.AccountIdentity{AccountID: "account-b", UserID: "user-b", Email: "b@example.test", PlanType: "plus"}
	accountA := codexprov.AccountKey("logical-a")
	accountB := codexprov.AccountKey("logical-b")
	managedA := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "managed-a"}, Revision: "managed-a-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(3 * time.Hour), CQAuthored: true, Routable: true,
	}
	externalA := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "external-a"}, Revision: "external-a-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(4 * time.Hour), Routable: true,
	}
	managedB := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountB, CandidateID: "managed-b"}, Revision: "managed-b-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(2 * time.Hour), CQAuthored: true, Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{{Accounts: []codexprov.LogicalAccount{
			{Key: accountA, Identity: identityA, Candidates: []codexprov.CredentialCandidate{managedA, externalA}, Routable: true},
			{Key: accountB, Identity: identityB, Candidates: []codexprov.CredentialCandidate{managedB}, Routable: true},
		}}},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			token := map[codexprov.CandidateID]string{
				"managed-a": "stale-managed-a", "external-a": "rejected-external-a", "managed-b": "usable-managed-b",
			}[planned.Ref.CandidateID]
			if planned.Ref == managedA.Ref && planned.Revision == "managed-a-refreshed" {
				token = "fresh-managed-a"
			}
			return registryCredentialMaterial(planned.Identity, token), nil
		},
		refresh: func(ref codexprov.CandidateRef, revision codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
			if ref != managedA.Ref || revision != managedA.Revision {
				return codexprov.CandidateRef{}, "", fmt.Errorf("unexpected refresh target: %+v %q", ref, revision)
			}
			return ref, "managed-a-refreshed", nil
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{
		"stale-managed-a": http.StatusUnauthorized, "rejected-external-a": http.StatusForbidden,
		"fresh-managed-a": http.StatusOK, "usable-managed-b": http.StatusOK,
	}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if err != nil {
		t.Fatalf("codexRegistryModelsRequest() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	attempts, rejectClose := doer.snapshot()
	wantAttempts := []string{"stale-managed-a", "rejected-external-a", "fresh-managed-a"}
	if fmt.Sprint(attempts) != fmt.Sprint(wantAttempts) {
		t.Fatalf("HTTP credential attempts = %q, want %q", attempts, wantAttempts)
	}
	if rejectClose != 2 {
		t.Fatalf("rejected response closes = %d, want 2", rejectClose)
	}
	_, plans, refreshes := authority.snapshot()
	wantStrongIdentity := codexprov.AccountIdentity{AccountID: identityA.AccountID, UserID: identityA.UserID}
	if len(plans) != 3 || plans[0].Identity != wantStrongIdentity || plans[1].Identity != wantStrongIdentity || plans[2].Identity != wantStrongIdentity {
		t.Fatalf("exact plans crossed identity before managed refresh: %+v", plans)
	}
	if plans[2].Ref != managedA.Ref || plans[2].Revision != "managed-a-refreshed" {
		t.Fatalf("refreshed exact plan = %+v, want returned ref/revision", plans[2])
	}
	refreshArgs := authority.refreshSnapshot()
	if refreshes != 1 || len(refreshArgs) != 1 || refreshArgs[0].ref != managedA.Ref || refreshArgs[0].revision != managedA.Revision {
		t.Fatalf("managed refresh calls = %+v (count %d), want original exact managed generation once", refreshArgs, refreshes)
	}
}

func TestCodexRegistryModelsRefreshesAtMostOneManagedLineagePerAccount(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identityA := codexprov.AccountIdentity{AccountID: "account-a", UserID: "user-a", Email: "a@example.test", PlanType: "plus"}
	identityB := codexprov.AccountIdentity{AccountID: "account-b", UserID: "user-b", Email: "b@example.test", PlanType: "plus"}
	accountA := codexprov.AccountKey("logical-a")
	accountB := codexprov.AccountKey("logical-b")
	managedA1 := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "managed-a1"}, Revision: "managed-a1-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(3 * time.Hour), CQAuthored: true, Routable: true,
	}
	managedA2 := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "managed-a2"}, Revision: "managed-a2-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(2 * time.Hour), CQAuthored: true, Routable: true,
	}
	externalB := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountB, CandidateID: "external-b"}, Revision: "external-b-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{{Accounts: []codexprov.LogicalAccount{
			{Key: accountA, Identity: identityA, Candidates: []codexprov.CredentialCandidate{managedA1, managedA2}, Routable: true},
			{Key: accountB, Identity: identityB, Candidates: []codexprov.CredentialCandidate{externalB}, Routable: true},
		}}},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			token := map[codexprov.CandidateID]string{
				"managed-a1": "rejected-managed-a1", "managed-a2": "rejected-managed-a2", "external-b": "usable-external-b",
			}[planned.Ref.CandidateID]
			if planned.Revision == "managed-a1-refreshed" {
				token = "rejected-refreshed-a1"
			}
			if planned.Revision == "managed-a2-refreshed" {
				token = "unexpected-usable-refreshed-a2"
			}
			return registryCredentialMaterial(planned.Identity, token), nil
		},
		refresh: func(ref codexprov.CandidateRef, _ codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
			return ref, codexprov.Revision(string(ref.CandidateID) + "-refreshed"), nil
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{
		"rejected-managed-a1": http.StatusUnauthorized, "rejected-managed-a2": http.StatusUnauthorized,
		"rejected-refreshed-a1": http.StatusUnauthorized, "unexpected-usable-refreshed-a2": http.StatusOK,
		"usable-external-b": http.StatusOK,
	}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if err != nil {
		t.Fatalf("codexRegistryModelsRequest() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	attempts, _ := doer.snapshot()
	wantAttempts := []string{"rejected-managed-a1", "rejected-managed-a2", "rejected-refreshed-a1", "usable-external-b"}
	if fmt.Sprint(attempts) != fmt.Sprint(wantAttempts) {
		t.Fatalf("HTTP attempts = %q, want one managed refresh before next account %q", attempts, wantAttempts)
	}
	_, _, refreshes := authority.snapshot()
	if refreshes != 1 {
		t.Fatalf("managed refresh calls = %d, want at most one for logical account", refreshes)
	}
}

func TestCodexRegistryModelsRefreshesResolvedManagedRevision(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	ref := codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "managed-one"}
	candidate := func(revision codexprov.Revision) codexprov.CredentialCandidate {
		return codexprov.CredentialCandidate{
			Ref: ref, Revision: revision, Source: codexprov.SourceManaged,
			AccessExpiresAt: now.Add(time.Hour), CQAuthored: true, Routable: true,
		}
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{
			registryInventory(identity, candidate("planned-revision")),
			registryInventory(identity, candidate("resolved-revision")),
		},
		resolve: func(call int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			switch call {
			case 1:
				return codexprov.CredentialMaterial{}, codexprov.ErrStaleRevision
			case 2:
				if planned.Revision != "resolved-revision" {
					return codexprov.CredentialMaterial{}, fmt.Errorf("replanned revision = %q", planned.Revision)
				}
				return registryCredentialMaterial(planned.Identity, "replanned-stale-token"), nil
			case 3:
				if planned.Revision != "refreshed-revision" {
					return codexprov.CredentialMaterial{}, fmt.Errorf("refreshed revision = %q", planned.Revision)
				}
				return registryCredentialMaterial(planned.Identity, "refreshed-token"), nil
			default:
				return codexprov.CredentialMaterial{}, errors.New("unexpected exact resolve")
			}
		},
		refresh: func(gotRef codexprov.CandidateRef, revision codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
			if gotRef != ref || revision != "resolved-revision" {
				return codexprov.CandidateRef{}, "", fmt.Errorf("refresh target = %+v %q, want resolved revision", gotRef, revision)
			}
			return ref, "refreshed-revision", nil
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{
		"replanned-stale-token": http.StatusUnauthorized,
		"refreshed-token":       http.StatusOK,
	}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if err != nil {
		t.Fatalf("codexRegistryModelsRequest() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	listCalls, plans, refreshes := authority.snapshot()
	if listCalls != 2 || len(plans) != 3 || refreshes != 1 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want bounded 2/3/1", listCalls, len(plans), refreshes)
	}
	refreshArgs := authority.refreshSnapshot()
	if len(refreshArgs) != 1 || refreshArgs[0].revision != "resolved-revision" {
		t.Fatalf("refresh args = %+v, want actual resolved revision", refreshArgs)
	}
}

func TestCodexRegistryModelsBoundsManagedRefreshAfterRepeated401(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	managed := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "managed-one"}, Revision: "managed-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(time.Hour), CQAuthored: true, Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, managed)},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			token := "rejected-managed"
			if planned.Revision == "refreshed-revision" {
				token = "rejected-refreshed-managed"
			}
			return registryCredentialMaterial(planned.Identity, token), nil
		},
		refresh: func(ref codexprov.CandidateRef, revision codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
			return ref, "refreshed-revision", nil
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{
		"rejected-managed":           http.StatusUnauthorized,
		"rejected-refreshed-managed": http.StatusUnauthorized,
	}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if err != nil {
		t.Fatalf("codexRegistryModelsRequest() error = %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("status = %d, want final 401", response.StatusCode)
	}
	attempts, rejectClose := doer.snapshot()
	_, plans, refreshes := authority.snapshot()
	if len(attempts) != 2 || len(plans) != 2 || refreshes != 1 {
		response.Body.Close()
		t.Fatalf("attempts/plans/refreshes = %d/%d/%d, want bounded 2/2/1", len(attempts), len(plans), refreshes)
	}
	if rejectClose != 1 {
		response.Body.Close()
		t.Fatalf("superseded response closes = %d, want 1", rejectClose)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("Close(final response) error = %v", err)
	}
}

func TestCodexRegistryModelsNeverRefreshesExternalOrSystem(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identityA := codexprov.AccountIdentity{AccountID: "account-a", UserID: "user-a", Email: "a@example.test", PlanType: "plus"}
	identityB := codexprov.AccountIdentity{AccountID: "account-b", UserID: "user-b", Email: "b@example.test", PlanType: "plus"}
	accountA := codexprov.AccountKey("logical-a")
	accountB := codexprov.AccountKey("logical-b")
	systemA := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "system-a"}, Revision: "system-a-revision", Source: codexprov.SourceSystem,
		AccessExpiresAt: now.Add(3 * time.Hour), Routable: true,
	}
	externalA := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountA, CandidateID: "external-a"}, Revision: "external-a-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(4 * time.Hour), Routable: true,
	}
	managedB := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountB, CandidateID: "managed-b"}, Revision: "managed-b-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(2 * time.Hour), CQAuthored: true, Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{{Accounts: []codexprov.LogicalAccount{
			{Key: accountA, Identity: identityA, Candidates: []codexprov.CredentialCandidate{systemA, externalA}, Routable: true},
			{Key: accountB, Identity: identityB, Candidates: []codexprov.CredentialCandidate{managedB}, Routable: true},
		}}},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			token := map[codexprov.CandidateID]string{
				"system-a": "rejected-system-a", "external-a": "rejected-external-a", "managed-b": "usable-managed-b",
			}[planned.Ref.CandidateID]
			return registryCredentialMaterial(planned.Identity, token), nil
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{
		"rejected-system-a": http.StatusUnauthorized, "rejected-external-a": http.StatusForbidden, "usable-managed-b": http.StatusOK,
	}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if err != nil {
		t.Fatalf("codexRegistryModelsRequest() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	_, _, refreshes := authority.snapshot()
	if refreshes != 0 {
		t.Fatalf("refresh calls = %d, want zero for rejected external/system candidates", refreshes)
	}
}

func TestCodexRegistryModelsProbesLocallyExpiredCandidateBeforeRefresh(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	for _, source := range []codexprov.CredentialSource{codexprov.SourceExternal, codexprov.SourceSystem, codexprov.SourceManaged} {
		t.Run(fmt.Sprintf("source-%d", source), func(t *testing.T) {
			candidate := codexprov.CredentialCandidate{
				Ref:      codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: codexprov.CandidateID(fmt.Sprintf("candidate-%d", source))},
				Revision: "expired-revision", Source: source, AccessExpiresAt: now.Add(-time.Hour),
				CQAuthored: source == codexprov.SourceManaged, Routable: true,
			}
			authority := &fakeCodexRegistryAuthority{
				inventories: []codexprov.Inventory{registryInventory(identity, candidate)},
				resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
					return registryCredentialMaterial(planned.Identity, "backend-valid-expired-metadata"), nil
				},
			}
			doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"backend-valid-expired-metadata": http.StatusOK}}
			req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
			if err != nil {
				t.Fatalf("codexRegistryModelsRequest() error = %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want backend-authoritative 200", response.StatusCode)
			}
			attempts, _ := doer.snapshot()
			_, plans, refreshes := authority.snapshot()
			if len(attempts) != 1 || len(plans) != 1 || refreshes != 0 {
				t.Fatalf("HTTP/resolve/refresh calls = %d/%d/%d, want 1/1/0", len(attempts), len(plans), refreshes)
			}
		})
	}
}

func TestCodexRegistryModelsAuthorityLossStopsWithoutLeakingDetails(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	accountKey := codexprov.AccountKey("logical-one")
	first := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "external-a"}, Revision: "external-a-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(2 * time.Hour), Routable: true,
	}
	second := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "external-b"}, Revision: "external-b-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	const sensitive = "private socket path and credential fingerprint"
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, first, second)},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			if planned.Ref == first.Ref {
				return registryCredentialMaterial(planned.Identity, "rejected-external"), nil
			}
			return codexprov.CredentialMaterial{}, fmt.Errorf("%s: %w", sensitive, codexprov.ErrCredentialAuthorityUnavailable)
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"rejected-external": http.StatusUnauthorized}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if response != nil {
		response.Body.Close()
		t.Fatalf("response = %+v, want authority error", response)
	}
	if !errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
		t.Fatalf("error = %v, want typed registry authority unavailable", err)
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("authority error leaked private detail: %v", err)
	}
	attempts, rejectClose := doer.snapshot()
	if len(attempts) != 1 || rejectClose != 1 {
		t.Fatalf("HTTP attempts/rejected closes = %q/%d, want one/one before authority loss", attempts, rejectClose)
	}
}

func TestCodexRegistryModelsPreservesReturnedContextError(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	accountKey := codexprov.AccountKey("logical-one")
	first := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "external-a"}, Revision: "external-a-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(2 * time.Hour), Routable: true,
	}
	second := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "external-b"}, Revision: "external-b-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	const sensitive = "private RPC transport context"
	t.Run("list", func(t *testing.T) {
		authority := &fakeCodexRegistryAuthority{listErr: fmt.Errorf("%s: %w", sensitive, context.DeadlineExceeded)}
		doer := &scriptedRegistryModelsDoer{}
		req, _ := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
		response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error = %v, want canonical deadline without private detail", err)
		}
	})
	t.Run("resolve", func(t *testing.T) {
		authority := &fakeCodexRegistryAuthority{
			inventories: []codexprov.Inventory{registryInventory(identity, first, second)},
			resolve: func(call int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
				if call == 1 {
					return codexprov.CredentialMaterial{}, fmt.Errorf("%s: %w", sensitive, context.Canceled)
				}
				return registryCredentialMaterial(planned.Identity, "must-not-dispatch"), nil
			},
		}
		doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"must-not-dispatch": http.StatusOK}}
		req, _ := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
		response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error = %v, want canonical cancellation without private detail", err)
		}
		if attempts, _ := doer.snapshot(); len(attempts) != 0 {
			t.Fatalf("HTTP attempts = %q, want none after resolver context error", attempts)
		}
	})
	t.Run("refresh", func(t *testing.T) {
		managed := first
		managed.Ref.CandidateID = "managed-a"
		managed.Source = codexprov.SourceManaged
		managed.CQAuthored = true
		authority := &fakeCodexRegistryAuthority{
			inventories: []codexprov.Inventory{registryInventory(identity, managed)},
			resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
				return registryCredentialMaterial(planned.Identity, "rejected-managed"), nil
			},
			refresh: func(codexprov.CandidateRef, codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
				return codexprov.CandidateRef{}, "", fmt.Errorf("%s: %w", sensitive, context.DeadlineExceeded)
			},
		}
		doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"rejected-managed": http.StatusUnauthorized}}
		req, _ := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
		response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error = %v, want canonical deadline without private detail", err)
		}
		_, rejectClose := doer.snapshot()
		if rejectClose != 1 {
			t.Fatalf("rejected response closes = %d, want 1", rejectClose)
		}
	})
}

func TestCodexRegistryModelsRefreshAuthorityLossStopsWithoutLeakingDetails(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	managed := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: "logical-one", CandidateID: "managed-one"}, Revision: "managed-revision", Source: codexprov.SourceManaged,
		AccessExpiresAt: now.Add(time.Hour), CQAuthored: true, Routable: true,
	}
	const sensitive = "private refresh lineage and socket path"
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, managed)},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return registryCredentialMaterial(planned.Identity, "rejected-managed"), nil
		},
		refresh: func(codexprov.CandidateRef, codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
			return codexprov.CandidateRef{}, "", fmt.Errorf("%s: %w", sensitive, codexprov.ErrCredentialAuthorityUnavailable)
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{"rejected-managed": http.StatusUnauthorized}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if response != nil {
		response.Body.Close()
		t.Fatalf("response = %+v, want authority error", response)
	}
	if !errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
		t.Fatalf("error = %v, want typed registry authority unavailable", err)
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("authority error leaked private detail: %v", err)
	}
	_, rejectClose := doer.snapshot()
	_, _, refreshes := authority.snapshot()
	if rejectClose != 1 || refreshes != 1 {
		t.Fatalf("rejected closes/refreshes = %d/%d, want 1/1", rejectClose, refreshes)
	}
}

func TestCodexRegistryModelsReturnsFinalAuthRejectionUnchanged(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	identity := codexprov.AccountIdentity{AccountID: "account-one", UserID: "user-one", Email: "one@example.test", PlanType: "plus"}
	accountKey := codexprov.AccountKey("logical-one")
	first := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "external-a"}, Revision: "external-a-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(2 * time.Hour), Routable: true,
	}
	second := codexprov.CredentialCandidate{
		Ref: codexprov.CandidateRef{AccountKey: accountKey, CandidateID: "external-b"}, Revision: "external-b-revision", Source: codexprov.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour), Routable: true,
	}
	authority := &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, first, second)},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			token := map[codexprov.CandidateID]string{"external-a": "rejected-a", "external-b": "rejected-b"}[planned.Ref.CandidateID]
			return registryCredentialMaterial(planned.Identity, token), nil
		},
	}
	doer := &scriptedRegistryModelsDoer{statusByToken: map[string]int{
		"rejected-a": http.StatusUnauthorized,
		"rejected-b": http.StatusForbidden,
	}}
	req, err := http.NewRequest(http.MethodGet, "https://codex.example/models", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := codexRegistryModelsRequest(context.Background(), authority, doer, now, req)
	if err != nil {
		t.Fatalf("codexRegistryModelsRequest() error = %v", err)
	}
	if response.StatusCode != http.StatusForbidden {
		response.Body.Close()
		t.Fatalf("final status = %d, want 403", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		response.Body.Close()
		t.Fatalf("ReadAll(final response) error = %v", err)
	}
	if string(body) != `{"error":"rejected"}` {
		response.Body.Close()
		t.Fatalf("final response body = %q, want final upstream rejection", body)
	}
	_, rejectClose := doer.snapshot()
	if rejectClose != 1 {
		response.Body.Close()
		t.Fatalf("rejected closes before caller ownership = %d, want only superseded response closed", rejectClose)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("Close(final response) error = %v", err)
	}
	_, rejectClose = doer.snapshot()
	if rejectClose != 2 {
		t.Fatalf("rejected closes after caller close = %d, want 2", rejectClose)
	}
}
