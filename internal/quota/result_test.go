package quota

import "testing"

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
