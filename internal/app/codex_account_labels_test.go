package app

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/provider"
)

func TestDiscoveredCodexPlanMetadataSurvivesUnverifiedUsage(t *testing.T) {
	for _, id := range []provider.ID{provider.Codex, provider.Claude} {
		t.Run(string(id), func(t *testing.T) {
			p := &mockProvider{discovered: []provider.Account{{
				Email: "member@example.com", Label: "self_serve_business_prolite", RateLimitTier: "business_8x",
			}}}
			runner := &Runner{Services: map[provider.ID]provider.Services{id: {Usage: p}}}
			rows := runner.enrichCachedWithDiscovered(context.Background(), id, nil)
			if len(rows) != 1 || rows[0].Error == nil || rows[0].Error.Code != "usage_unverified" {
				t.Fatalf("rows = %+v, want unverified discovery", rows)
			}
			if id == provider.Codex {
				if rows[0].Plan != "self_serve_business_prolite" || rows[0].RateLimitTier != "business_8x" {
					t.Fatalf("discovered raw metadata was lost: %+v", rows[0])
				}
			} else if rows[0].Plan != "" || rows[0].RateLimitTier != "" {
				t.Fatalf("Claude discovery behaviour changed: %+v", rows[0])
			}
		})
	}
}

func TestPrintCodexAccountsIncludesPlanMultiplier(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	oldStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()
	PrintCodexAccounts([]provider.Account{
		{Email: "pro@example.com", Label: "pro", RateLimitTier: "codex_prolite_10x"},
		{Email: "member@example.com", Label: "self_serve_business_prolite"},
	})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "pro 10x") || !strings.Contains(string(out), "business premium 5x") {
		t.Fatalf("missing account plan label or multiplier: %s", out)
	}
}
