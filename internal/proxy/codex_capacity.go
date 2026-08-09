package proxy

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

// CapacityBucket identifies independently limited Codex capacity.
type CapacityBucket string

const (
	CapacityBucketBase        CapacityBucket = "base"
	capacityBucketModelPrefix                = "model:"
)

// CapacitySource identifies where a capacity fact originated.
type CapacitySource uint8

const (
	CapacitySourceUsageCache CapacitySource = iota + 1
	CapacitySourceHardLimit
	CapacitySourceHTTPHeaders
	CapacitySourceLiveRateLimits
)

// CapacityConfidence describes how strongly a capacity fact can gate admission.
type CapacityConfidence uint8

const (
	CapacityConfidenceAdvisory CapacityConfidence = iota + 1
	CapacityConfidenceAuthoritative
)

// CapacityFact is one bounded, ordered observation for an account and bucket.
type CapacityFact struct {
	AccountKey           codex.AccountKey
	Bucket               CapacityBucket
	RemainingPct         int
	Source               CapacitySource
	Sequence             uint64
	ConnectionGeneration uint64
	ObservedAt           time.Time
	ResetAt              time.Time
	Confidence           CapacityConfidence
}

// CapacityState is the admission state derived from current facts.
type CapacityState uint8

const (
	CapacityUnknown CapacityState = iota
	CapacityPositive
	CapacityZero
)

// CapacityView is a point-in-time admission view.
type CapacityView struct {
	State        CapacityState
	RemainingPct int
	ResetAt      time.Time
	Source       CapacitySource
	Exact        bool
}

type capacityFactKey struct {
	account codex.AccountKey
	bucket  CapacityBucket
	source  CapacitySource
}

// CodexCapacityObservationStream orders facts from one upstream response or connection.
type CodexCapacityObservationStream struct {
	generation uint64
	sequence   atomic.Uint64
}

// CodexCapacityLedger holds bounded capacity facts and active lease counts.
type CodexCapacityLedger struct {
	mu sync.RWMutex

	now    func() time.Time
	maxAge time.Duration
	facts  map[capacityFactKey]CapacityFact
	seq    uint64
	leases map[codex.AccountKey]int

	observationGeneration atomic.Uint64
}

// NewCodexCapacityLedger creates a ledger with a bounded cache horizon.
func NewCodexCapacityLedger(now func() time.Time, maxAge time.Duration) *CodexCapacityLedger {
	if now == nil {
		now = time.Now
	}
	if maxAge <= 0 {
		maxAge = quotaSnapshotMaxAge
	}
	return &CodexCapacityLedger{
		now:    now,
		maxAge: maxAge,
		facts:  make(map[capacityFactKey]CapacityFact),
		leases: make(map[codex.AccountKey]int),
	}
}

// NewObservationStream allocates one process-local generation for an upstream
// response or connection.
func (l *CodexCapacityLedger) NewObservationStream() *CodexCapacityObservationStream {
	if l == nil {
		return nil
	}
	return &CodexCapacityObservationStream{generation: l.observationGeneration.Add(1)}
}

// Stamp attaches this stream's generation and next sequence to a fact.
func (s *CodexCapacityObservationStream) Stamp(fact CapacityFact) CapacityFact {
	if s == nil {
		return fact
	}
	fact.ConnectionGeneration = s.generation
	fact.Sequence = s.sequence.Add(1)
	return fact
}

// CapacityBucketForModel maps a requested model to its exact quota bucket.
func CapacityBucketForModel(model string) CapacityBucket {
	normalised := strings.ToLower(ParseModel(model))
	if codexModelRequiresPro(normalised) {
		return CapacityBucket(capacityBucketModelPrefix + strings.ToLower(codexSparkModel))
	}
	return CapacityBucketBase
}

// Observe accepts a fact if it advances that source's ordered stream.
func (l *CodexCapacityLedger) Observe(fact CapacityFact) bool {
	if l == nil || fact.AccountKey == "" || fact.Source == 0 {
		return false
	}
	if fact.Bucket == "" {
		fact.Bucket = CapacityBucketBase
	}
	if fact.RemainingPct < 0 {
		fact.RemainingPct = 0
	}
	if fact.RemainingPct > 100 {
		fact.RemainingPct = 100
	}
	if fact.ObservedAt.IsZero() {
		fact.ObservedAt = l.now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	key := capacityFactKey{account: fact.AccountKey, bucket: fact.Bucket, source: fact.Source}
	if current, ok := l.facts[key]; ok && !capacityFactAdvances(current, fact) {
		return false
	}
	l.facts[key] = fact
	return true
}

func capacityFactAdvances(current, next CapacityFact) bool {
	if next.ConnectionGeneration != current.ConnectionGeneration {
		return next.ConnectionGeneration > current.ConnectionGeneration
	}
	if next.Sequence != current.Sequence {
		return next.Sequence > current.Sequence
	}
	if !next.ResetAt.Equal(current.ResetAt) {
		return next.ResetAt.After(current.ResetAt)
	}
	return next.ObservedAt.After(current.ObservedAt)
}

// ObserveQuotaSnapshot imports shared and exact scoped windows from usage cache.
func (l *CodexCapacityLedger) ObserveQuotaSnapshot(account codex.AccountKey, snap QuotaSnapshot) {
	if l == nil || account == "" || len(snap.Result.Windows) == 0 {
		return
	}
	type aggregate struct {
		remaining int
		reset     time.Time
		set       bool
	}
	aggregates := make(map[CapacityBucket]aggregate)
	for name, window := range snap.Result.Windows {
		bucket := CapacityBucketBase
		if scoped := quota.WindowBucket(name); scoped != "" {
			bucket = CapacityBucket(capacityBucketModelPrefix + strings.ToLower(ParseModel(scoped)))
		}
		current := aggregates[bucket]
		reset := time.Time{}
		if window.ResetAtUnix > 0 {
			reset = time.Unix(window.ResetAtUnix, 0)
		}
		if !current.set || window.RemainingPct < current.remaining {
			current.remaining = window.RemainingPct
		}
		if current.reset.IsZero() || (!reset.IsZero() && reset.Before(current.reset)) {
			current.reset = reset
		}
		current.set = true
		aggregates[bucket] = current
	}
	for bucket, aggregate := range aggregates {
		l.mu.Lock()
		key := capacityFactKey{account: account, bucket: bucket, source: CapacitySourceUsageCache}
		if current, ok := l.facts[key]; ok && !snap.FetchedAt.After(current.ObservedAt) {
			l.mu.Unlock()
			continue
		}
		l.seq++
		fact := CapacityFact{
			AccountKey:   account,
			Bucket:       bucket,
			RemainingPct: aggregate.remaining,
			Source:       CapacitySourceUsageCache,
			Sequence:     l.seq,
			ObservedAt:   snap.FetchedAt,
			ResetAt:      aggregate.reset,
			Confidence:   CapacityConfidenceAdvisory,
		}
		l.facts[key] = fact
		l.mu.Unlock()
	}
}

// Capacity returns exact bucket state, falling scoped requests back to shared state.
func (l *CodexCapacityLedger) Capacity(account codex.AccountKey, bucket CapacityBucket) CapacityView {
	if l == nil || account == "" {
		return CapacityView{State: CapacityUnknown}
	}
	if bucket == "" {
		bucket = CapacityBucketBase
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if view, ok := l.capacityLocked(account, bucket); ok {
		view.Exact = true
		return view
	}
	if bucket != CapacityBucketBase {
		if view, ok := l.capacityLocked(account, CapacityBucketBase); ok {
			view.Exact = false
			return view
		}
	}
	return CapacityView{State: CapacityUnknown, Exact: bucket == CapacityBucketBase}
}

func (l *CodexCapacityLedger) capacityLocked(account codex.AccountKey, bucket CapacityBucket) (CapacityView, bool) {
	now := l.now()
	var selected CapacityFact
	haveSelected := false
	var hard CapacityFact
	haveHard := false
	for source := CapacitySourceUsageCache; source <= CapacitySourceLiveRateLimits; source++ {
		fact, ok := l.facts[capacityFactKey{account: account, bucket: bucket, source: source}]
		if !ok || l.factStale(fact, now) {
			continue
		}
		if source == CapacitySourceHardLimit && fact.RemainingPct == 0 {
			hard, haveHard = fact, true
		}
		if !haveSelected || fact.Source > selected.Source {
			selected, haveSelected = fact, true
		}
	}
	if !haveSelected {
		return CapacityView{}, false
	}
	if haveHard && hardLimitStillFences(hard, selected) {
		selected = hard
	}
	state := CapacityPositive
	if selected.RemainingPct <= 0 {
		state = CapacityZero
	}
	return CapacityView{
		State:        state,
		RemainingPct: selected.RemainingPct,
		ResetAt:      selected.ResetAt,
		Source:       selected.Source,
	}, true
}

func (l *CodexCapacityLedger) factStale(fact CapacityFact, now time.Time) bool {
	if !fact.ResetAt.IsZero() && !now.Before(fact.ResetAt) {
		return true
	}
	return now.Sub(fact.ObservedAt) > l.maxAge
}

func hardLimitStillFences(hard, selected CapacityFact) bool {
	if selected.Source != CapacitySourceLiveRateLimits || selected.RemainingPct <= 0 {
		return true
	}
	return !selected.ObservedAt.After(hard.ObservedAt)
}

// SetActiveLeases records current admitted lease count for tie-breaking.
func (l *CodexCapacityLedger) SetActiveLeases(account codex.AccountKey, count int) {
	if l == nil || account == "" {
		return
	}
	if count < 0 {
		count = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leases[account] = count
}

// ActiveLeases returns current admitted lease count.
func (l *CodexCapacityLedger) ActiveLeases(account codex.AccountKey) int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.leases[account]
}

// CachedUsageLimitError reports that all compatible route choices are known empty.
type CachedUsageLimitError struct {
	RequestedModel  string
	RequiredBuckets []CapacityBucket
	ResetAt         time.Time
}

func (e *CachedUsageLimitError) Error() string {
	if e == nil {
		return "codex capacity exhausted"
	}
	if e.ResetAt.IsZero() {
		return fmt.Sprintf("codex capacity exhausted for %s", e.RequestedModel)
	}
	return fmt.Sprintf("codex capacity exhausted for %s until %s", e.RequestedModel, e.ResetAt.Format(time.RFC3339))
}
