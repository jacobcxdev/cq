package output

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
)

type zaiCapacityDoer func(*http.Request) (*http.Response, error)

func (f zaiCapacityDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestBuildTTYModelTwoZAIMaxAccountsUsePublishedCapacity(t *testing.T) {
	now := time.Unix(1800000000, 0)
	p := zaiprov.NewAccounts(zaiCapacityDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"level":"max","limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":28000,"percentage":25,"nextResetTime":1800018000000},{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":140000,"percentage":50,"nextResetTime":1800604800000}]}}`))}, nil
	}), func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{{ID: "first", APIKey: "fixture-one"}, {ID: "second", APIKey: "fixture-two"}}, nil
	})
	results, err := p.Fetch(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	windows, summary := aggregate.Compute(results, now.Unix(), string(provider.ZAI), nil)
	if summary == nil || summary.Count != 2 || summary.TotalMulti != 28 {
		t.Fatalf("two Max accounts were not 28 Lite allowances: %+v", summary)
	}
	model := BuildTTYModel(app.Report{Providers: []app.ProviderReport{{ID: provider.ZAI, Results: results, Aggregate: &app.AggregateReport{ProviderID: provider.ZAI, Summary: *summary, Windows: windows}}}}, now)
	if len(model.Sections) != 2 || !strings.Contains(model.Sections[0].Header, "Max 14x") || !strings.Contains(model.Sections[1].Header, "Max 14x") || !strings.Contains(model.Sections[1].AggHeader, "2 × Max 14x = 28x") {
		t.Fatal("TTY output lost individual or total Max capacity")
	}
}
