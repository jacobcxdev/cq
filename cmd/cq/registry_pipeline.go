package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/provider/grok"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type registryPipeline struct {
	Catalog         *modelregistry.Catalog
	Refresher       *modelregistry.Refresher
	Publish         func()
	StartReconciler func(context.Context) <-chan struct{}
	claudeCodePath  string
	publishMu       sync.Mutex
	reconcilerStart sync.Once
}

// tokenIsFresh reports whether a token with the given expiresAt (Unix ms) is
// usable. ExpiresAt==0 is treated as "unknown expiry" and considered usable.
// A non-zero ExpiresAt must be strictly after now to be considered fresh.
func tokenIsFresh(expiresAt int64, now time.Time) bool {
	if expiresAt == 0 {
		return true
	}
	return expiresAt > now.UnixMilli()
}

// betterTokenCandidate returns whichever of (currentToken, nextToken) is the
// better choice given their expiry timestamps and the current time.
//
// Selection rules (in priority order):
//  1. Skip nextToken if it is empty or stale.
//  2. If currentToken is empty, accept nextToken unconditionally.
//  3. If currentExpires==0 (unknown) but nextExpires!=0 (known-fresh), prefer next.
//  4. Otherwise pick the greater ExpiresAt (later expiry wins).
func betterTokenCandidate(currentToken string, currentExpires int64, nextToken string, nextExpires int64, now time.Time) (string, int64) {
	if nextToken == "" || !tokenIsFresh(nextExpires, now) {
		return currentToken, currentExpires
	}
	if currentToken == "" {
		return nextToken, nextExpires
	}
	if currentExpires == 0 && nextExpires != 0 {
		return nextToken, nextExpires
	}
	if nextExpires > currentExpires {
		return nextToken, nextExpires
	}
	return currentToken, currentExpires
}

func firstClaudeAccessToken() (string, error) {
	return firstClaudeAccessTokenFromAccounts(discoverClaudeAccountsFn())()
}

func firstCodexAccessToken(accounts []codexprov.CodexAccount) (string, error) {
	if len(accounts) == 0 {
		return "", fmt.Errorf("no codex accounts")
	}
	now := time.Now()
	best, bestExpires := "", int64(0)
	for _, account := range accounts {
		best, bestExpires = betterTokenCandidate(best, bestExpires, account.AccessToken, account.ExpiresAt, now)
	}
	if best == "" {
		return "", fmt.Errorf("no codex account with token")
	}
	return best, nil
}

func firstCodexAccessTokenFromInventory(ctx context.Context, inventory codexprov.Inventory, broker codexprov.CredentialRefreshBroker) (string, error) {
	if len(inventory.Accounts) == 0 {
		return "", fmt.Errorf("no codex accounts")
	}
	now := time.Now()
	best, bestExpires := "", int64(0)
	for _, logical := range inventory.Accounts {
		for _, candidate := range codexprov.ResolveCandidate(logical, "", now) {
			best, bestExpires = betterTokenCandidate(best, bestExpires, candidate.Credential.AccessToken, candidate.Credential.ExpiresAt, now)
		}
	}
	if best != "" {
		return best, nil
	}
	if broker == nil {
		return "", fmt.Errorf("no codex account with token")
	}
	for _, logical := range inventory.Accounts {
		for _, candidate := range codexprov.ResolveCandidate(logical, "", now) {
			if candidate.Source != codexprov.SourceManaged {
				continue
			}
			result, err := broker.Refresh(ctx, candidate.Ref, candidate.Revision)
			if err == nil && result.Material.AccessToken != "" {
				return result.Material.AccessToken, nil
			}
		}
	}
	return "", fmt.Errorf("no codex account with token")
}

// nilSafeStderr returns w if non-nil, otherwise io.Discard.
func nilSafeStderr(w io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return io.Discard
}

func firstClaudeAccessTokenFromAccounts(accounts []keyring.ClaudeOAuth) func() (string, error) {
	return func() (string, error) {
		if len(accounts) == 0 {
			return "", fmt.Errorf("no claude accounts")
		}
		now := time.Now()
		best, bestExpires := "", int64(0)
		for _, account := range accounts {
			best, bestExpires = betterTokenCandidate(best, bestExpires, account.AccessToken, account.ExpiresAt, now)
		}
		if best == "" {
			return "", fmt.Errorf("no claude account with token")
		}
		return best, nil
	}
}

type registryPipelineOptions struct {
	FS                        fsutil.FileSystem
	HomeDir                   string
	Roots                     userdirs.Roots
	ClaudeUpstream            string
	CodexUpstream             string
	HTTPClient                httputil.Doer
	CodexHTTPClient           httputil.Doer
	CodexClientVersion        string
	ResolveCodexClientVersion func() string
	RefreshInterval           time.Duration
	ClaudeToken               func() (string, error)
	CodexToken                func() (string, error)
	CodexTokenContext         func(context.Context) (string, error)
	CodexAuthenticatedDo      func(context.Context, *http.Request) (*http.Response, error)
	GrokAuthority             grok.Authority
	Env                       func(string) string
	Stderr                    io.Writer
}

func snapshotHasProvider(snap modelregistry.Snapshot, provider modelregistry.Provider) bool {
	for _, entry := range snap.Entries {
		if entry.Provider == provider {
			return true
		}
	}
	return false
}

func cachedRegistryEntries(opts registryPipelineOptions) []modelregistry.Entry {
	deps := modelsDeps{
		FS:      opts.FS,
		HomeDir: opts.HomeDir,
		Roots:   opts.Roots,
		Env:     opts.Env,
		Stderr:  opts.Stderr,
	}
	entries, err := loadCachedNativeEntries(deps)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "cq: registry: load cached models: %v\n", err)
		return nil
	}
	overlays, err := loadModelsOverlayFile(deps)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "cq: registry: load model overlays: %v\n", err)
		return entries
	}
	return modelregistry.Merge(entries, overlays.Models).Active
}

func newRegistryPipeline(opts registryPipelineOptions) (*registryPipeline, error) {
	if opts.FS == nil {
		return nil, fmt.Errorf("registry pipeline: missing filesystem")
	}
	if opts.HomeDir == "" {
		return nil, fmt.Errorf("registry pipeline: missing home dir")
	}
	if opts.HTTPClient == nil {
		return nil, fmt.Errorf("registry pipeline: missing HTTP client")
	}
	if opts.ClaudeToken == nil {
		return nil, fmt.Errorf("registry pipeline: missing Claude token provider")
	}
	if opts.CodexToken == nil && opts.CodexTokenContext == nil && opts.CodexAuthenticatedDo == nil {
		return nil, fmt.Errorf("registry pipeline: missing Codex token provider")
	}
	if opts.Env == nil {
		opts.Env = func(string) string { return "" }
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = 5 * time.Minute
	}
	seedEntries := cachedRegistryEntries(opts)
	if opts.GrokAuthority == nil {
		opts.GrokAuthority = grokReadAuthority(opts.FS, opts.Roots.Config, opts.HTTPClient, opts.Env, opts.HomeDir)
	}
	// Grok's display union is repopulated by the cancellable source fetch.
	// Construction must not invoke an external source's identity HTTP reads,
	// and a previous account's cache cannot seed another account's catalogue.
	filteredGrok := seedEntries[:0]
	for _, entry := range seedEntries {
		if entry.Provider != modelregistry.ProviderGrok {
			filteredGrok = append(filteredGrok, entry)
		}
	}
	seedEntries = filteredGrok
	zcodeTokens := &zcode.ProjectTokenClient{HTTP: opts.HTTPClient, Version: zcode.ClientVersion}
	zaiTokens := func(ctx context.Context) ([]string, error) {
		accounts, err := zaiprov.ReadAccounts(opts.FS, opts.Env, opts.Roots.Config)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(accounts))
		for _, account := range accounts {
			if account.Transport == "zcode-start" {
				continue
			}
			key, err := account.Credential(ctx, zcodeTokens)
			if err != nil {
				continue
			}
			keys = append(keys, key)
		}
		return keys, nil
	}
	// Construction only discovers local accounts. Project-token refresh belongs
	// to the cancellable source fetch, not application startup.
	zaiAccounts, zaiKeyErr := zaiprov.ReadAccounts(opts.FS, opts.Env, opts.Roots.Config)
	if zaiKeyErr != nil {
		fmt.Fprintln(opts.Stderr, "cq: registry: Z.ai credentials unavailable")
	}
	if zaiKeyErr != nil || len(zaiAccounts) == 0 {
		filtered := seedEntries[:0]
		for _, entry := range seedEntries {
			if entry.Provider != modelregistry.ProviderZAI {
				filtered = append(filtered, entry)
			}
		}
		seedEntries = filtered
	}
	seedVersion := modelregistry.DiscoverCodexClientVersion(opts.FS, codexModelCachePath(modelsDeps{HomeDir: opts.HomeDir, Env: opts.Env}))
	seedSnap := modelregistry.Snapshot{Entries: seedEntries, CodexClientVersion: seedVersion}
	catalog := modelregistry.NewCatalog(seedSnap)
	refresher := &modelregistry.Refresher{
		Catalog: catalog,
		Grok:    &modelregistry.GrokSource{Authority: opts.GrokAuthority},
		Anthropic: &modelregistry.AnthropicSource{
			Client:  opts.HTTPClient,
			BaseURL: opts.ClaudeUpstream,
			Token: func(ctx context.Context) (string, error) {
				return opts.ClaudeToken()
			},
		},
		Codex: &modelregistry.CodexSource{
			Client:  opts.HTTPClient,
			BaseURL: opts.CodexUpstream,
			Token: func(ctx context.Context) (string, error) {
				if opts.CodexTokenContext != nil {
					return opts.CodexTokenContext(ctx)
				}
				return opts.CodexToken()
			},
			AuthenticatedDo:      opts.CodexAuthenticatedDo,
			ClientVersion:        opts.CodexClientVersion,
			ResolveClientVersion: opts.ResolveCodexClientVersion,
		},
		Overlays: modelregistry.FileOverlayStore{
			FS:   opts.FS,
			Path: modelregistry.OverlayPath(opts.Roots),
		},
	}

	if zaiKeyErr == nil && len(zaiAccounts) > 0 {
		native := &modelregistry.ZAISource{Client: opts.HTTPClient, Tokens: zaiTokens}
		readStartAccounts := func() ([]zaiprov.Account, error) {
			accounts, err := zaiprov.ReadAccounts(opts.FS, opts.Env, opts.Roots.Config)
			if err != nil {
				return nil, err
			}
			// Inference pins an explicitly selected Start route to its preferred
			// account. Discovery must expose that same entitlement scope.
			if len(accounts) > 0 && accounts[0].Transport == "zcode-start" {
				return accounts[:1], nil
			}
			return nil, nil
		}
		startQuota := zaiprov.NewAccounts(opts.HTTPClient, readStartAccounts)
		start := &modelregistry.ZCodeStartSource{
			Catalogue: &zcode.ModelCatalogue{HTTP: opts.HTTPClient, Version: zcode.ClientVersion},
			Quota: func(ctx context.Context) ([]quota.Result, error) {
				rows, err := startQuota.Fetch(ctx, time.Now())
				if err != nil {
					return nil, err
				}
				for _, row := range rows {
					if row.Error != nil {
						return nil, errors.New("ZCode Start model quota unavailable")
					}
				}
				return rows, nil
			},
		}
		refresher.ZAI = modelregistry.SourceFunc(func(ctx context.Context) (modelregistry.SourceResult, error) {
			accounts, err := zaiprov.ReadAccounts(opts.FS, opts.Env, opts.Roots.Config)
			if err != nil {
				return modelregistry.SourceResult{}, err
			}
			if len(accounts) > 0 && accounts[0].Transport == "zcode-start" {
				return start.Fetch(ctx)
			}
			return native.Fetch(ctx)
		})
	}

	p := &registryPipeline{
		Catalog:        catalog,
		Refresher:      refresher,
		claudeCodePath: filepath.Join(opts.HomeDir, ".claude.json"),
	}
	p.Publish = func() {
		p.publishMu.Lock()
		defer p.publishMu.Unlock()

		snap := catalog.Snapshot()
		now := time.Now()
		if err := modelregistry.PublishClaudeCodeOptions(opts.FS, p.claudeCodePath, snap); err != nil {
			fmt.Fprintf(opts.Stderr, "cq: registry: publish Claude Code options: %v\n", err)
		}
		if snapshotHasProvider(snap, modelregistry.ProviderAnthropic) {
			if err := modelregistry.PublishClaudeCapabilities(opts.FS, filepath.Join(opts.HomeDir, ".claude", "cache", "model-capabilities.json"), snap, now); err != nil {
				fmt.Fprintf(opts.Stderr, "cq: registry: publish Claude capabilities: %v\n", err)
			}
		}
		if err := modelregistry.PublishCodexRegistryCache(opts.FS, opts.Roots, snap); err != nil {
			fmt.Fprintf(opts.Stderr, "cq: registry: publish CQ model cache: %v\n", err)
		}
	}
	done := make(chan struct{})
	p.StartReconciler = func(ctx context.Context) <-chan struct{} {
		p.reconcilerStart.Do(func() {
			go func() {
				defer close(done)
				defer func() {
					if recover() != nil {
						fmt.Fprintln(opts.Stderr, "cq: registry: background refresh stopped after panic")
					}
				}()
				refreshTicker := time.NewTicker(opts.RefreshInterval)
				defer refreshTicker.Stop()
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-refreshTicker.C:
						refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
						diag, err := p.Refresher.Refresh(refreshCtx)
						cancel()
						if ctx.Err() != nil {
							return
						}
						writeRegistrySourceDiagnostics(opts.Stderr, diag)
						if err != nil {
							fmt.Fprintf(opts.Stderr, "cq: registry: background refresh: %v\n", err)
						} else {
							p.Publish()
						}
					case <-ticker.C:
						snap := catalog.Snapshot()
						need, err := modelregistry.ClaudeCodeOptionsNeedPublish(opts.FS, p.claudeCodePath, snap)
						if err != nil {
							fmt.Fprintf(opts.Stderr, "cq: registry: check Claude Code options: %v\n", err)
							continue
						}
						if need {
							p.Publish()
						}
					}
				}
			}()
		})
		return done
	}

	return p, nil
}

func newRegistryPipelineWithCodexAuthority(opts registryPipelineOptions, authority codexRegistryCredentialAuthority) (*registryPipeline, error) {
	if authority == nil {
		return nil, fmt.Errorf("registry pipeline: %w", errCodexRegistryCredentialAuthorityUnavailable)
	}
	opts.CodexToken = nil
	opts.CodexTokenContext = nil
	codexClient := opts.CodexHTTPClient
	if codexClient == nil {
		codexClient = opts.HTTPClient
	}
	opts.CodexAuthenticatedDo = func(ctx context.Context, req *http.Request) (*http.Response, error) {
		return codexRegistryModelsRequest(ctx, authority, codexClient, time.Now(), req)
	}
	return newRegistryPipeline(opts)
}
