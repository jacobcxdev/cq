package codex

import (
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

type WindowScopeKind uint8

const (
	WindowScopeShared WindowScopeKind = iota + 1
	WindowScopeModelFamily
)

type WindowDescriptor struct {
	RawLimitName string
	WindowName   quota.WindowName
	Period       time.Duration
	ScopeKind    WindowScopeKind
	Scope        string
	ResetAt      time.Time
	RemainingPct float64
}

type UsageObservation struct {
	Result  quota.Result
	Windows []WindowDescriptor
}
