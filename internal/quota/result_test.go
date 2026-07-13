package quota

import (
	"math"
	"testing"
)

func TestResultIsUsable(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusOK, true},
		{StatusExhausted, true},
		{StatusError, false},
	}
	for _, tt := range tests {
		if got := (Result{Status: tt.status}).IsUsable(); got != tt.want {
			t.Errorf("IsUsable(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestResultMinRemainingPct(t *testing.T) {
	tests := []struct {
		name    string
		windows map[WindowName]Window
		want    int
	}{
		{"no windows", nil, -1},
		{"single", map[WindowName]Window{Window5Hour: {RemainingPct: 42}}, 42},
		{"min of two", map[WindowName]Window{
			Window5Hour: {RemainingPct: 80},
			Window7Day:  {RemainingPct: 30},
		}, 30},
		{"ignores scoped windows when shared windows exist", map[WindowName]Window{
			Window5Hour:             {RemainingPct: 60},
			Window7Day:              {RemainingPct: 80},
			WindowName("7d:sonnet"): {RemainingPct: 0},
		}, 60},
		{"falls back to scoped windows when no shared windows exist", map[WindowName]Window{
			WindowName("7d:sonnet"): {RemainingPct: 25},
			WindowName("7d:opus"):   {RemainingPct: 10},
		}, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Result{Windows: tt.windows}
			if got := r.MinRemainingPct(); got != tt.want {
				t.Errorf("MinRemainingPct() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestErrorResult(t *testing.T) {
	r := ErrorResult("api_error", "server error", 500)
	if r.Status != StatusError {
		t.Fatalf("status = %q, want %q", r.Status, StatusError)
	}
	if r.Error == nil || r.Error.Code != "api_error" || r.Error.HTTPStatus != 500 {
		t.Fatalf("error = %+v, want code=api_error http=500", r.Error)
	}
	if r.Error.Message != "server error" {
		t.Fatalf("message = %q, want %q", r.Error.Message, "server error")
	}
}

func TestStatusFromWindows(t *testing.T) {
	tests := []struct {
		name    string
		windows map[WindowName]Window
		want    Status
	}{
		{"empty map", nil, StatusOK},
		{"all windows > 0", map[WindowName]Window{
			Window5Hour: {RemainingPct: 50},
			Window7Day:  {RemainingPct: 100},
		}, StatusOK},
		{"one window at 0", map[WindowName]Window{
			Window5Hour: {RemainingPct: 0},
		}, StatusExhausted},
		{"one window negative", map[WindowName]Window{
			Window5Hour: {RemainingPct: -1},
		}, StatusExhausted},
		{"mixed one 0 one positive", map[WindowName]Window{
			Window5Hour: {RemainingPct: 0},
			Window7Day:  {RemainingPct: 80},
		}, StatusExhausted},
		{"scoped exhaustion does not exhaust whole account when shared windows remain", map[WindowName]Window{
			Window5Hour:             {RemainingPct: 60},
			Window7Day:              {RemainingPct: 80},
			WindowName("7d:sonnet"): {RemainingPct: 0},
		}, StatusOK},
		{"scoped-only windows still exhaust when depleted", map[WindowName]Window{
			WindowName("7d:sonnet"): {RemainingPct: 0},
		}, StatusExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StatusFromWindows(tt.windows)
			if got != tt.want {
				t.Errorf("StatusFromWindows() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultResetEpochNegativePeriod(t *testing.T) {
	// DefaultResetEpoch(-100, 1000) should return 1000 + (-100) = 900.
	// This documents the arithmetic behaviour with a negative periodS.
	got := DefaultResetEpoch(-100, 1000)
	want := int64(900)
	if got != want {
		t.Errorf("DefaultResetEpoch(-100, 1000) = %d, want %d", got, want)
	}
}

func TestPeriodFor(t *testing.T) {
	tests := []struct {
		name WindowName
		want int64 // seconds
	}{
		{Window5Hour, 5 * 3600},
		{Window7Day, 7 * 24 * 3600},
		{WindowPro, 24 * 3600},
		{WindowFlash, 24 * 3600},
		{WindowFlashLite, 24 * 3600},
		{"1d", 24 * 3600},
		{"90m:scoped", 90 * 60},
		{"24h", 0},
		{"60m", 0},
		{"unknown", 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.name), func(t *testing.T) {
			got := int64(PeriodFor(tt.name).Seconds())
			if got != tt.want {
				t.Errorf("PeriodFor(%q) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

func TestWindowNameForPeriod(t *testing.T) {
	tests := []struct {
		name    string
		seconds int64
		bucket  string
		want    WindowName
		wantOK  bool
	}{
		{name: "five hours", seconds: 5 * 60 * 60, want: "5h", wantOK: true},
		{name: "one day", seconds: 24 * 60 * 60, want: "1d", wantOK: true},
		{name: "seven days", seconds: 7 * 24 * 60 * 60, want: "7d", wantOK: true},
		{name: "ninety minutes", seconds: 90 * 60, want: "90m", wantOK: true},
		{name: "scoped", seconds: 7 * 24 * 60 * 60, bucket: "spark", want: "7d:spark", wantOK: true},
		{name: "seconds", seconds: 61, want: "61s", wantOK: true},
		{name: "zero", seconds: 0},
		{name: "negative", seconds: -1},
		{name: "overflow", seconds: math.MaxInt64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := WindowNameForPeriod(tt.seconds, tt.bucket)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("WindowNameForPeriod(%d, %q) = (%q, %v), want (%q, %v)", tt.seconds, tt.bucket, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestGenericDurationWindowsAreAggregable(t *testing.T) {
	for _, name := range []WindowName{"1d", "90m:scoped"} {
		if !IsAggregable(name) {
			t.Errorf("IsAggregable(%q) = false, want true", name)
		}
	}
	for _, name := range []WindowName{WindowPro, WindowFlash, WindowFlashLite, "unknown"} {
		if IsAggregable(name) {
			t.Errorf("IsAggregable(%q) = true, want false", name)
		}
	}
}

func TestOrderedWindowNamesGenericDurations(t *testing.T) {
	got := OrderedWindowNames([]WindowName{
		"7d:spark",
		"1d:spark",
		WindowPro,
		"1d",
		Window7Day,
		Window5Hour,
		"7d:weekly-only",
	})
	want := []WindowName{
		Window5Hour,
		"1d",
		Window7Day,
		"7d:weekly-only",
		"1d:spark",
		"7d:spark",
		WindowPro,
	}

	if len(got) != len(want) {
		t.Fatalf("OrderedWindowNames() length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("OrderedWindowNames()[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}
