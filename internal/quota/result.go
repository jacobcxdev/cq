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
	Status        Status                `json:"status"`
	Error         *ErrorInfo            `json:"error,omitempty"`
	Plan          string                `json:"plan,omitempty"`
	Tier          string                `json:"tier,omitempty"`
	RateLimitTier string                `json:"rate_limit_tier,omitempty"`
	Windows       map[WindowName]Window `json:"windows,omitempty"`
}

func (r Result) IsUsable() bool {
	return r.Status == StatusOK || r.Status == StatusExhausted
}

func (r Result) MinRemainingPct() int {
	if len(r.Windows) == 0 {
		return -1
	}
	minPct := 100
	for _, w := range r.Windows {
		if w.RemainingPct < minPct {
			minPct = w.RemainingPct
		}
	}
	return minPct
}

// StatusFromWindows returns StatusExhausted if any window has 0% remaining,
// otherwise StatusOK.
func StatusFromWindows(windows map[WindowName]Window) Status {
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
