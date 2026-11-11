package app

import (
	"context"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

type reportEnrichmentProvider struct {
	mockProvider
	calls int
}

func (p *reportEnrichmentProvider) EnrichResults(_ context.Context, rows []quota.Result) []quota.Result {
	p.calls++
	out := append([]quota.Result(nil), rows...)
	for i := range out {
		out[i].Tier = "current local policy"
	}
	return out
}

func TestRunnerEnrichesCurrentLocalStateAfterCache(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "cached"}[cached], func(t *testing.T) {
			p := &reportEnrichmentProvider{mockProvider: mockProvider{results: []quota.Result{{Status: quota.StatusOK}}}}
			runner := &Runner{Clock: fixedClock(time.Unix(1, 0)), Services: map[provider.ID]provider.Services{provider.Codex: {Usage: p}}}
			if cached {
				runner.Cache = &mockCache{data: map[string][]quota.Result{"codex": {{Status: quota.StatusOK}}}}
			}
			report, err := runner.BuildReport(context.Background(), RunRequest{Providers: []provider.ID{provider.Codex}})
			if err != nil {
				t.Fatal(err)
			}
			if p.calls != 1 || report.Providers[0].Results[0].Tier != "current local policy" {
				t.Fatalf("enrichment calls = %d, report = %+v", p.calls, report)
			}
		})
	}
}
