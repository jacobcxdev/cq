package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestRunProxyRegistryUsesFederatedCredentialAuthority(t *testing.T) {
	now := time.Now()
	const token = "external-only-proxy-token"
	authority := externalRegistryAuthority(now, token)
	fsys := fsutil.NewMemFS()
	cfg := &proxy.Config{
		ClaudeUpstream: "https://claude.example",
		CodexUpstream:  "https://codex.example/backend-api/codex",
		CodexWindowPriming: proxy.CodexWindowPrimingConfig{
			Enabled: true,
		},
	}

	pipeline, err := newProxyRegistryPipeline(cfg, proxyRegistryDependencies{
		FS:                  fsys,
		HomeDir:             "/home/test",
		Roots:               testCQRoots(),
		HTTPClient:          registryModelsDoer{wantToken: token},
		CodexClientVersion:  "test-client",
		ClaudeToken:         func() (string, error) { return "", errors.New("Claude unavailable") },
		CredentialAuthority: authority,
		Env:                 func(string) string { return "" },
		Stderr:              io.Discard,
	})
	if err != nil {
		t.Fatalf("newProxyRegistryPipeline() error = %v", err)
	}

	assertCodexRegistryModel(t, pipeline, token)
	_, primerErr := buildCodexPrimer(cfg, true, &proxy.CodexRequestRouter{}, &staticProxyCodexRoutingInventory{}, pipeline.Catalog, nil)
	if primerErr == nil || !strings.Contains(primerErr.Error(), "journal") {
		t.Fatalf("buildCodexPrimer() error = %v, want post-catalogue journal dependency", primerErr)
	}
	if strings.Contains(primerErr.Error(), "registry") || strings.Contains(primerErr.Error(), token) {
		t.Fatalf("buildCodexPrimer() failed at catalogue or leaked token: %v", primerErr)
	}
	listCalls, plans, refreshCalls := authority.snapshot()
	if listCalls != 1 || len(plans) != 1 || refreshCalls != 0 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want 1/1/0", listCalls, len(plans), refreshCalls)
	}
}
