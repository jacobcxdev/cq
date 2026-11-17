package quota

import (
	"math"
	"testing"
	"time"
)

func TestReserveReleaseAtDrainTime(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	for _, tt := range []struct {
		name                     string
		remaining, reserve, rate float64
		want                     time.Time
	}{
		{"drain plus margin", 5, 5, 0.01, reset.Add(-800 * time.Second)},
		{"only protected quota", 80, 5, 0.01, reset.Add(-800 * time.Second)},
		{"below threshold", 2, 5, 0.01, reset.Add(-500 * time.Second)},
		{"zero demand", 5, 5, 0, reset.Add(-5 * time.Minute)},
		{"invalid rate", 5, 5, math.NaN(), reset},
		{"negative rate", 5, 5, -1, reset},
		{"invalid remaining", math.Inf(1), 5, 0.01, reset},
		{"invalid threshold", 5, 100, 0.01, reset},
		{"tiny positive demand", 5, 5, math.SmallestNonzeroFloat64, now},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReserveReleaseAt(now, reset, tt.remaining, tt.reserve, tt.rate); !got.Equal(tt.want) {
				t.Fatalf("release = %s, want %s", got, tt.want)
			}
		})
	}
}
