package provider

import (
	"context"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// ID identifies a provider.
type ID string

const (
	Claude ID = "claude"
	Codex  ID = "codex"
	Gemini ID = "gemini"
)

// Provider fetches quota information for an AI service.
type Provider interface {
	Fetch(ctx context.Context, now time.Time) ([]quota.Result, error)
}

type Account struct {
	AccountID     string `json:"id"`
	Email         string `json:"email,omitempty"`
	Label         string `json:"label,omitempty"`
	RateLimitTier string `json:"rate_limit_tier,omitempty"`
	Active        bool   `json:"active"`
	SwitchID      string `json:"switch_id,omitempty"`
}

type AccountManager interface {
	ProviderID() ID
	Discover(ctx context.Context) ([]Account, error)
	Switch(ctx context.Context, identifier string) (Account, error)
	Remove(ctx context.Context, identifier string) error
}

// Services groups the service implementations for a provider.
// Currently only Usage is populated; additional fields can be added
// without changing the Runner interface.
type Services struct {
	Usage Provider
}
