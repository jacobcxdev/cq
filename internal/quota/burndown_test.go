package quota

import (
	"math"
	"testing"
)

func TestBurndownUsesShorterOfRecentAndWindowETAs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		recent *float64
		want   int64
	}{
		{"cold start", nil, 9000},
		{"fast recent burn", ratePointer(0.01), 5000},
		{"slow recent burn", ratePointer(0.001), 9000},
		{"idle recent usage", ratePointer(0), 9000},
		{"invalid negative rate", ratePointer(-1), 9000},
		{"invalid nan rate", ratePointer(math.NaN()), 9000},
		{"invalid infinite rate", ratePointer(math.Inf(1)), 9000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := Window{RemainingPct: 50, ResetAtUnix: 10_000, RecentBurnRate: tt.recent}
			got, ok := w.Burndown(18000, 1000)
			if !ok || got != tt.want {
				t.Fatalf("burndown = %d/%v, want %d/true", got, ok, tt.want)
			}
		})
	}
}

func TestBurndownUsesExactRemainingCapacity(t *testing.T) {
	w := Window{RemainingPct: 0, RemainingPctExact: ratePointer(0.25), ResetAtUnix: 10000, RecentBurnRate: ratePointer(0.05)}
	if got, ok := w.Burndown(18000, 1000); !ok || got != 5 {
		t.Fatalf("burndown = %d/%v, want 5/true", got, ok)
	}
}

func ratePointer(rate float64) *float64 { return &rate }

func TestBurndownWithoutHistory(t *testing.T) {
	tests := []struct {
		name       string
		periodS    int64
		resetEpoch int64
		nowEpoch   int64
		pct        int
		wantS      int64
		wantOK     bool
	}{
		{"zero_pct", 18000, 18100, 9100, 0, 0, true},
		{"no_elapsed", 18000, 18100, 100, 50, 0, false},
		{"half_used_half_elapsed", 18000, 18100, 9100, 50, 9000, true},
		{"nothing_used", 18000, 18100, 9100, 100, 0, false},
		{"pct_above_100", 18000, 18100, 9100, 150, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := (Window{RemainingPct: tt.pct, ResetAtUnix: tt.resetEpoch}).Burndown(tt.periodS, tt.nowEpoch)
			if ok != tt.wantOK {
				t.Errorf("Burndown ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantS {
				t.Errorf("Burndown = %d, want %d", got, tt.wantS)
			}
		})
	}
}
