package claude

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/quota"
)

// TestNewProvider verifies that New creates a Provider with a non-nil client
// using the supplied HTTP doer.
func TestNewProvider(t *testing.T) {
	p := New(http.DefaultClient)
	if p == nil {
		t.Fatal("New returned nil")
	}
	if p.client == nil {
		t.Fatal("provider.client is nil")
	}
	if p.client.http == nil {
		t.Fatal("provider.client.http is nil")
	}
}

// panicDoer is an httputil.Doer that panics on every request, used to test
// that panic recovery in inner goroutines prevents process crashes.
type panicDoer struct{}

func (panicDoer) Do(*http.Request) (*http.Response, error) {
	panic("test panic in HTTP call")
}

func TestFetchAccountPanicInInnerGoroutines(t *testing.T) {
	p := &Provider{client: &Client{http: panicDoer{}}}
	acct := keyring.ClaudeOAuth{AccessToken: "test-token"}

	// Must not panic — the inner goroutine recovery should catch it.
	result := p.fetchAccount(context.Background(), acct, time.Now())

	if result.Status != quota.StatusError {
		t.Fatalf("expected error status, got %v", result.Status)
	}
	if result.Error == nil || result.Error.Code != "fetch_error" {
		t.Fatalf("expected fetch_error code, got %+v", result.Error)
	}
}
