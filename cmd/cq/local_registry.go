package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
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

type localRegistryDependencies struct {
	FS                  fsutil.FileSystem
	HomeDir             string
	HTTPClient          httputil.Doer
	CodexClientVersion  string
	ClaudeToken         func() (string, error)
	CredentialAuthority codexRegistryCredentialAuthority
	Env                 func(string) string
	Stderr              io.Writer
	Close               func() error
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
	registry, err := buildLocalRegistryFromAuthority(cfg, localRegistryDependencies{
		FS:                  fsys,
		HomeDir:             home,
		HTTPClient:          httpClient,
		CodexClientVersion:  codexClientVersion,
		ClaudeToken:         firstClaudeAccessToken,
		CredentialAuthority: newCodexRegistryControlAdapter(credentialControl),
		Env:                 os.Getenv,
		Stderr:              os.Stderr,
		Close:               credentialControl.Close,
	})
	if err != nil {
		_ = credentialControl.Close()
		return nil, err
	}
	return registry, nil
}

func buildLocalRegistryFromAuthority(cfg *proxy.Config, deps localRegistryDependencies) (*localRegistry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("registry pipeline: missing proxy config")
	}
	pipeline, err := newRegistryPipelineWithCodexAuthority(registryPipelineOptions{
		FS:                 deps.FS,
		HomeDir:            deps.HomeDir,
		ClaudeUpstream:     cfg.ClaudeUpstream,
		CodexUpstream:      cfg.CodexUpstream,
		HTTPClient:         deps.HTTPClient,
		CodexClientVersion: deps.CodexClientVersion,
		ClaudeToken:        deps.ClaudeToken,
		Env:                deps.Env,
		Stderr:             deps.Stderr,
	}, deps.CredentialAuthority)
	if err != nil {
		return nil, err
	}
	closeRegistry := deps.Close
	if closeRegistry == nil {
		closeRegistry = func() error { return nil }
	}
	return &localRegistry{
		Catalog:   pipeline.Catalog,
		Refresher: pipeline.Refresher,
		Publish:   pipeline.Publish,
		Close:     closeRegistry,
	}, nil
}
