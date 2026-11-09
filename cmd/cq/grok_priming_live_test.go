package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/grok"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type grokPrimerReadOnlyAuthority struct {
	grok.Authority
	posts atomic.Int32
}

func (a *grokPrimerReadOnlyAuthority) Do(ctx context.Context, id grok.AccountID, request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet {
		a.posts.Add(1)
		return nil, errors.New("live primer qualification forbids inference")
	}
	return a.Authority.Do(ctx, id, request)
}
func TestGrokLiveWeeklyPrimerSkipsInference(t *testing.T) {
	if os.Getenv("CQ_GROK_PRIME_LIVE") != "1" {
		t.Skip("requires explicitly selected saved subscription")
	}
	config := os.Getenv("CQ_GROK_LIVE_CONFIG")
	if config == "" {
		t.Fatal("CQ_GROK_LIVE_CONFIG required")
	}
	authority := &grokPrimerReadOnlyAuthority{Authority: newGrokAuthority(fsutil.OSFileSystem{}, config, grokHTTPClient(15*time.Second))}
	primer, err := proxy.NewGrokPrimer(fsutil.NewMemFS(), "/state", authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := primer.PrimeNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if authority.posts.Load() != 0 || len(primer.Health(true).Accounts) != 0 {
		t.Fatal("weekly subscription attempted priming")
	}
	t.Log("live subscription observed; no inference attempted and no dispatch recorded")
}
