package gemini

import (
	"context"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

const (
	antigravityCLIName = "agy"
	usageTimeout       = 20 * time.Second
)

var usageArgs = []string{"-p", "/usage", "--output-format", "json", "--print-timeout", "15s"}

// Provider implements provider.Provider by delegating Gemini quota reads to
// the authenticated Antigravity CLI.
type Provider struct {
	runner commandRunner
}

// New creates a Provider backed by the installed Antigravity CLI.
func New() *Provider {
	return newProvider(osCommandRunner{})
}

func newProvider(runner commandRunner) *Provider {
	return &Provider{runner: runner}
}

// DiscoverAccounts reports the externally managed Antigravity CLI identity
// when its executable is available. It does not read credentials or run agy.
func (p *Provider) DiscoverAccounts(_ context.Context) ([]provider.Account, error) {
	if _, err := p.runner.LookPath(antigravityCLIName); err != nil {
		return nil, nil
	}
	return []provider.Account{{
		AccountID: antigravityAccountID,
		Label:     "Antigravity CLI",
		Active:    true,
	}}, nil
}

// Fetch invokes the structured zero-token usage command and maps Gemini quota.
func (p *Provider) Fetch(ctx context.Context, _ time.Time) ([]quota.Result, error) {
	path, err := p.runner.LookPath(antigravityCLIName)
	if err != nil {
		return []quota.Result{quota.ErrorResult(
			"not_configured",
			"install and authenticate antigravity-cli",
			0,
		)}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	data, err := p.runner.Run(runCtx, path, usageArgs...)
	if err != nil {
		return []quota.Result{quota.ErrorResult(
			"fetch_error",
			"antigravity-cli usage failed",
			0,
		)}, nil
	}

	result, err := parseUsage(data)
	if err != nil {
		return []quota.Result{quota.ErrorResult(
			"parse_error",
			"invalid antigravity-cli usage output",
			0,
		)}, nil
	}
	return []quota.Result{result}, nil
}
