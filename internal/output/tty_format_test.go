package output

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		name string
		s    int64
		want string
	}{
		{"negative", -10, "now"},
		{"zero", 0, "now"},
		{"under_one_minute", 30, "<1m"},
		{"one_minute", 60, "1m"},
		{"minutes", 300, "5m"},
		{"one_hour", 3600, "1h"},
		{"hour_and_minutes", 5400, "1h 30m"},
		{"hours", 7200, "2h"},
		{"one_day", 86400, "1d"},
		{"day_and_hours", 90000, "1d 1h"},
		{"days", 172800, "2d"},
		{"complex", 100000, "1d 3h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fmtDuration(tt.s)
			if got != tt.want {
				t.Errorf("fmtDuration(%d) = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}

func TestCalcPace(t *testing.T) {
	tests := []struct {
		name      string
		periodS   int64
		resetEpoch int64
		nowEpoch  int64
		want      int
	}{
		{"start_of_period", 18000, 18100, 100, 100},
		{"half_elapsed", 18000, 18100, 9100, 50},
		{"end_of_period", 18000, 18100, 18100, 0},
		{"past_period", 18000, 100, 200, 0},
		{"zero_elapsed", 18000, 18100, 0, 100},
		{"zero_period", 0, 100, 50, 100},
		{"negative_period", -1, 100, 50, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calcPace(tt.periodS, tt.resetEpoch, tt.nowEpoch)
			if got != tt.want {
				t.Errorf("calcPace(%d, %d, %d) = %d, want %d",
					tt.periodS, tt.resetEpoch, tt.nowEpoch, got, tt.want)
			}
		})
	}
}

func TestPeriodSeconds(t *testing.T) {
	tests := []struct {
		name string
		win  string
		want int64
	}{
		{"5h", "5h", 18000},
		{"7d", "7d", 604800},
		{"pro", "pro", 86400},
		{"unknown", "other", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := periodSeconds(quota.WindowName(tt.win))
			if got != tt.want {
				t.Errorf("periodSeconds(%q) = %d, want %d", tt.win, got, tt.want)
			}
		})
	}
}
