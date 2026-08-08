package main

import (
	"context"
	"fmt"
	"os"
	"time"

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
	Close     func() error
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
	credentialControl, err := codexprov.OpenDefaultCredentialRefreshControl(context.Background(), fsys, httpClient)
	if err != nil {
		return nil, fmt.Errorf("Codex credential coordinator: %w", err)
	}
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS:                 fsys,
		HomeDir:            home,
		ClaudeUpstream:     cfg.ClaudeUpstream,
		CodexUpstream:      cfg.CodexUpstream,
		HTTPClient:         httpClient,
		CodexClientVersion: codexClientVersion,
		ClaudeToken:        firstClaudeAccessToken,
		CodexTokenContext: func(ctx context.Context) (string, error) {
			return firstCodexAccessTokenFromInventory(ctx, codexprov.DiscoverInventory(fsys), credentialControl)
		},
		Env:    os.Getenv,
		Stderr: os.Stderr,
	})
	if err != nil {
		_ = credentialControl.Close()
		return nil, err
	}

	return &localRegistry{
		Catalog:   pipeline.Catalog,
		Refresher: pipeline.Refresher,
		Publish:   pipeline.Publish,
		Close:     credentialControl.Close,
	}, nil
}
