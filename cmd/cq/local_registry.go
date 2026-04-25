package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// localRegistry bundles a catalog, the refresher that updates it, and a
// publish closure that writes the snapshot to the Claude Code and Codex
// caches. It is built from real OS resources and is the one-shot equivalent
// of the in-proxy registry pipeline.
type localRegistry struct {
	Catalog   *modelregistry.Catalog
	Refresher *modelregistry.Refresher
	Publish   func()
}

// buildLocalRegistry constructs a fresh catalog, refresher, and publisher
// closure using OS-backed filesystem, httputil client, and Codex account
// discovery. Intended for cq models refresh when the proxy is not running.
func buildLocalRegistry(cfg *proxy.Config, versionStr string) (*localRegistry, error) {
	fsys := fsutil.OSFileSystem{}
	home, err := fsys.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}

	httpClient := newHTTPClientFn(30*time.Second, versionStr)
	codexClientVersion := defaultCodexClientVersion()

	codexDiscover := func() []codexprov.CodexAccount {
		return codexprov.DiscoverAccounts(fsys)
	}
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS:                 fsys,
		HomeDir:            home,
		ClaudeUpstream:     cfg.ClaudeUpstream,
		CodexUpstream:      cfg.CodexUpstream,
		HTTPClient:         httpClient,
		CodexClientVersion: codexClientVersion,
		ClaudeToken:        firstClaudeAccessToken,
		CodexToken: func() (string, error) {
			return firstCodexAccessTokenWithRefresh(
				context.Background(),
				codexDiscover(),
				func(ctx context.Context, refreshToken string) (*auth.CodexTokenResponse, error) {
					return auth.RefreshCodexToken(ctx, httpClient, refreshToken)
				},
				fsys,
				home,
				codexprov.PersistCodexAccount,
			)
		},
		Env:    os.Getenv,
		Stderr: os.Stderr,
	})
	if err != nil {
		return nil, err
	}

	return &localRegistry{
		Catalog:   pipeline.Catalog,
		Refresher: pipeline.Refresher,
		Publish:   pipeline.Publish,
	}, nil
}
