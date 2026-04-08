package output

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestTTYRendererOutput(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1000, 0)
	r := &TTYRenderer{W: &buf, Now: now}
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Codex,
				Name: "codex",
				Results: []quota.Result{{
					Status: quota.StatusOK,
					Plan:   "plus",
					Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: 75, ResetAtUnix: 1000 + 9000},
					},
				}},
			},
		},
	}
	err := r.Render(context.Background(), report)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	output := buf.String()
	if output == "" {
		t.Fatal("expected non-empty output")
	}
	// Should contain provider name
	if !strings.Contains(output, "Codex") {
		t.Error("output should contain provider name 'Codex'")
	}
	// Should contain window label
	if !strings.Contains(output, "5h") {
		t.Error("output should contain window label '5h'")
	}
	// Should contain percentage
	if !strings.Contains(output, "75%") {
		t.Error("output should contain '75%'")
	}
}

func TestTTYRendererEmpty(t *testing.T) {
	var buf bytes.Buffer
	r := &TTYRenderer{W: &buf, Now: time.Unix(1000, 0)}
	report := app.Report{Providers: []app.ProviderReport{}}
	err := r.Render(context.Background(), report)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty output for empty report, got %d bytes", buf.Len())
	}
}

func TestTTYRendererMultipleProviders(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1000, 0)
	r := &TTYRenderer{W: &buf, Now: now}
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Claude,
				Name: "claude",
				Results: []quota.Result{{
					Status: quota.StatusOK,
					Plan:   "max",
					Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 10000},
					},
				}},
			},
			{
				ID:   provider.Codex,
				Name: "codex",
				Results: []quota.Result{{
					Status: quota.StatusOK,
					Plan:   "plus",
					Windows: map[quota.WindowName]quota.Window{
						quota.WindowPro: {RemainingPct: 50, ResetAtUnix: 80000},
					},
				}},
			},
		},
	}
	err := r.Render(context.Background(), report)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "Claude") {
		t.Error("output should contain 'Claude'")
	}
	if !strings.Contains(output, "Codex") {
		t.Error("output should contain 'Codex'")
	}
}

func TestTTYRendererWithAggregate(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1000, 0)
	r := &TTYRenderer{W: &buf, Now: now}
	report := app.Report{
		GeneratedAt: now,
		Providers: []app.ProviderReport{
			{
				ID:   provider.Claude,
				Name: "claude",
				Results: []quota.Result{{
					Status: quota.StatusOK,
					Plan:   "max",
					Windows: map[quota.WindowName]quota.Window{
						quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 10000},
					},
				}},
				Aggregate: &app.AggregateReport{
					Kind: "weighted_pace",
					Summary: aggregate.AccountSummary{
						Count: 2, TotalMulti: 2, Label: "2 x max",
					},
					Windows: map[quota.WindowName]quota.AggregateResult{
						quota.Window5Hour: {
							RemainingPct: 75,
							ExpectedPct:  50,
							PaceDiff:     25,
							Burndown:     3600,
						},
					},
				},
			},
		},
	}
	err := r.Render(context.Background(), report)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	output := buf.String()
	if output == "" {
		t.Fatal("expected non-empty output")
	}
	// Aggregate header should contain the function notation
	if !strings.Contains(output, "Claude") {
		t.Error("output should contain 'Claude' in aggregate header")
	}
	// Should contain aggregate window data
	if !strings.Contains(output, "75%") {
		t.Error("output should contain aggregate percentage '75%'")
	}
}
