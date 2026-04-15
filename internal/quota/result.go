package quota

type ErrorInfo struct {
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type Window struct {
	RemainingPct int   `json:"remaining_pct"`
	ResetAtUnix  int64 `json:"reset_at_unix,omitempty"`
}

type Result struct {
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
