package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type registryPipeline struct {
	Catalog           *modelregistry.Catalog
	Refresher         *modelregistry.Refresher
	Publish           func()
	PublishReport     func(modelregistry.Snapshot) []modelregistry.PublicationTarget
	StartReconciler   func(context.Context)
	claudeCodePath    string
	publishMu         sync.Mutex
	reconcilerStartMu sync.Mutex
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
	FS                   fsutil.FileSystem
	HomeDir              string
	CWD                  string
	Roots                userdirs.Roots
	ClaudeUpstream       string
	CodexUpstream        string
	HTTPClient           httputil.Doer
	CodexClientVersion   string
	ClaudeToken          func() (string, error)
	CodexToken           func() (string, error)
	CodexTokenContext    func(context.Context) (string, error)
	CodexAuthenticatedDo func(context.Context, *http.Request) (*http.Response, error)
	Env                  func(string) string
	Stderr               io.Writer
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
		CWD:     opts.CWD,
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

	if opts.CWD == "" {
		var err error
		opts.CWD, err = userdirs.WorkingDirectory()
		if err != nil {
			return nil, err
		}
	}
	claudePaths, err := userdirs.ResolveClientPaths("claude", opts.CWD, opts.HomeDir, opts.Env)
	if err != nil {
		return nil, err
	}
	codexPaths, err := userdirs.ResolveClientPaths("codex", opts.CWD, opts.HomeDir, opts.Env)
	if err != nil {
		return nil, err
	}
	// Seed and publisher share the captured absolute locations.
	opts.Env = func(name string) string {
		switch name {
		case "CODEX_HOME":
			return filepath.Dir(codexPaths.CodexModels)
		case "CLAUDE_CONFIG_DIR":
			return filepath.Dir(filepath.Dir(claudePaths.ClaudeCapabilities))
		}
		return ""
	}
	seedEntries := cachedRegistryEntries(opts)
	seedSnap := modelregistry.Snapshot{Entries: seedEntries, CodexRawByID: map[string]json.RawMessage{}}
	for _, entry := range seedEntries {
		if entry.Provider == modelregistry.ProviderCodex && entry.Source == modelregistry.SourceNative && entry.Raw != nil {
			seedSnap.CodexRawByID[entry.ID] = entry.Raw
		}
	}
	catalog := modelregistry.NewCatalog(seedSnap)
	refresher := &modelregistry.Refresher{
		Catalog: catalog,
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
			AuthenticatedDo: opts.CodexAuthenticatedDo,
			ClientVersion:   opts.CodexClientVersion,
		},
		Overlays: modelregistry.FileOverlayStore{
			FS:   opts.FS,
			Path: modelregistry.OverlayPath(opts.Roots),
		},
	}

	p := &registryPipeline{
		Catalog:        catalog,
		Refresher:      refresher,
		claudeCodePath: filepath.Join(opts.HomeDir, ".claude.json"),
	}
	publishSnapshot := func(snap modelregistry.Snapshot, optionalClients bool) []modelregistry.PublicationTarget {
		p.publishMu.Lock()
		defer p.publishMu.Unlock()
		now := time.Now()
		targets := []modelregistry.PublicationTarget{}
		publish := func(name, path string, optional bool, write func() error) {
			result := modelregistry.PublicationTarget{Target: name, Path: path, Status: "written", Reason: "published"}
			var err error
			if optional && optionalClients {
				_, err = opts.FS.Stat(path)
				if errors.Is(err, os.ErrNotExist) {
					result.Status = "skipped"
					result.Reason = "optional_client_absent"
					targets = append(targets, result)
					return
				}
			}
			if err == nil {
				err = write()
			}
			if err != nil {
				result.Status = "failed"
				result.Reason = "write_failed"
				code, message := "models_publication_failed", "Model cache publication failed."
				result.ErrorCode = &code
				result.Message = &message
				fmt.Fprintf(opts.Stderr, "cq: registry: publish %s: %v\n", name, err)
			}
			targets = append(targets, result)
		}
		publish("codex_cache", codexPaths.CodexModels, false, func() error {
			return modelregistry.PublishCodexCache(opts.FS, codexPaths.CodexModels, snap, now, opts.CodexClientVersion)
		})
		if optionalClients || snapshotHasProvider(snap, modelregistry.ProviderAnthropic) {
			publish("claude_capabilities", claudePaths.ClaudeCapabilities, true, func() error {
				return modelregistry.PublishClaudeCapabilities(opts.FS, claudePaths.ClaudeCapabilities, snap, now)
			})
		}
		publish("claude_picker", p.claudeCodePath, true, func() error { return modelregistry.PublishClaudeCodeOptions(opts.FS, p.claudeCodePath, snap) })
		return targets
	}
	p.PublishReport = func(snap modelregistry.Snapshot) []modelregistry.PublicationTarget {
		return publishSnapshot(snap, true)
	}
	p.Publish = func() { publishSnapshot(catalog.Snapshot(), false) }
	p.StartReconciler = func(ctx context.Context) {
		p.reconcilerStartMu.Lock()
		defer p.reconcilerStartMu.Unlock()
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
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
	}

	return p, nil
}

func newRegistryPipelineWithCodexAuthority(opts registryPipelineOptions, authority codexRegistryCredentialAuthority) (*registryPipeline, error) {
	if authority == nil {
		return nil, fmt.Errorf("registry pipeline: %w", errCodexRegistryCredentialAuthorityUnavailable)
	}
	opts.CodexToken = nil
	opts.CodexTokenContext = nil
	opts.CodexAuthenticatedDo = func(ctx context.Context, req *http.Request) (*http.Response, error) {
		return codexRegistryModelsRequest(ctx, authority, opts.HTTPClient, time.Now(), req)
	}
	return newRegistryPipeline(opts)
}
