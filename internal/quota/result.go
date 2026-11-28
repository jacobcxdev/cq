package quota

type ErrorInfo struct {
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type Window struct {
	RemainingPct      int      `json:"remaining_pct"`
	RemainingPctExact *float64 `json:"remaining_pct_exact,omitempty"`
	ResetAtUnix       int64    `json:"reset_at_unix,omitempty"`
	// RecentBurnRate is report-only, in percentage points per second. Nil means
	// insufficient recent history; a non-nil zero means observed idle usage.
	RecentBurnRate *float64 `json:"-"`
}

// ExtraCredits separates local permission from provider-reported availability.
type ExtraCredits struct {
	Enabled   bool  `json:"enabled"`
	Available *bool `json:"available"`
}

// BankedResetForecast describes confirmed and planned Codex refills separately
// from upstream quota. Scheduled timestamps are ordered and conditional.
type BankedResetForecast struct {
	LastRedeemedAtUnix int64   `json:"last_redeemed_at_unix,omitempty"`
	ScheduledAtUnix    []int64 `json:"scheduled_at_unix,omitempty"`
}

// WindowBounds returns the observed allowance's start and its next replacement.
// The provider's natural reset epoch stays the anchor for recurring cycles.
func (f *BankedResetForecast) WindowBounds(name WindowName, w Window, now int64) (start, next int64) {
	start = w.ResetAtUnix - int64(PeriodFor(name).Seconds())
	next = w.ResetAtUnix
	if f == nil || (name != Window5Hour && name != Window7Day) || next <= now || start > now {
		return start, next
	}
	if f.LastRedeemedAtUnix > start && f.LastRedeemedAtUnix <= now {
		start = f.LastRedeemedAtUnix
	}
	for _, at := range f.ScheduledAtUnix {
		if at > now && at < next {
			next = at
		}
	}
	return start, next
}

type Result struct {
	AccountKey    string                `json:"account_key,omitempty"`
	ExtraCredits  *ExtraCredits         `json:"extra_credits,omitempty"`
	AccountID     string                `json:"account_id,omitempty"`
	Email         string                `json:"email,omitempty"`
	Active        bool                  `json:"active"`
	Status        Status                `json:"status"`
	Error         *ErrorInfo            `json:"error,omitempty"`
	Plan          string                `json:"plan,omitempty"`
	Tier          string                `json:"tier,omitempty"`
	RateLimitTier string                `json:"rate_limit_tier,omitempty"`
	Windows       map[WindowName]Window `json:"windows,omitempty"`
	CacheAge      int64                 `json:"cache_age_s,omitempty"` // seconds; >0 means result is from cache
	BankedResets  *BankedResetForecast  `json:"banked_resets,omitempty"`
}

func (r Result) IsUsable() bool {
	return r.Status == StatusOK || r.Status == StatusExhausted
}

func (r Result) MinRemainingPct() int {
	if len(r.Windows) == 0 {
		return -1
	}
	minPct := 101
	for name, w := range r.Windows {
		if WindowBucket(name) != "" {
			continue
		}
		if w.RemainingPct < minPct {
			minPct = w.RemainingPct
		}
	}
	if minPct <= 100 {
		return minPct
	}
	for _, w := range r.Windows {
		if w.RemainingPct < minPct {
			minPct = w.RemainingPct
		}
	}
	if minPct == 101 {
		return -1
	}
	return minPct
}

// StatusFromWindows returns StatusExhausted if any shared window has 0%
// remaining, otherwise StatusOK. If a result has only scoped windows, any
// scoped 0% still counts as exhausted.
func StatusFromWindows(windows map[WindowName]Window) Status {
	hasShared := false
	for name, w := range windows {
		if WindowBucket(name) != "" {
			continue
		}
		hasShared = true
		if w.RemainingPct <= 0 {
			return StatusExhausted
		}
	}
	if hasShared {
		return StatusOK
	}
	for _, w := range windows {
		if w.RemainingPct <= 0 {
			return StatusExhausted
		}
	}
	return StatusOK
}

func ErrorResult(code, msg string, httpStatus int) Result {
	return Result{
		Status: StatusError,
		Error: &ErrorInfo{
			Code:       code,
			Message:    msg,
			HTTPStatus: httpStatus,
		},
	}
}
