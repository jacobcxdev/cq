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
	CapacitySourceLiveUsage
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
	Windows              map[quota.WindowName]quota.Window
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

type capacityBucketKey struct {
	account codex.AccountKey
	bucket  CapacityBucket
}

type capacitySnapshotAggregate struct {
	windows   map[quota.WindowName]quota.Window
	remaining int
	reset     time.Time
	set       bool
}

// CodexCapacityObservationStream orders facts from one upstream response or connection.
type CodexCapacityObservationStream struct {
	generation uint64
	sequence   atomic.Uint64
}

// CodexCapacityLedger holds bounded capacity facts and active lease counts.
type CodexCapacityLedger struct {
	// OnReset is bound before serving requests.
	OnReset         func(CodexQuotaResetEvent) error
	resetWindows    map[codex.AccountKey]map[quota.WindowName]codexWindowFact
	includedWindows map[codex.AccountKey]map[quota.WindowName]codexWindowFact
	resetPending    map[codex.AccountKey]*codexQuotaResetPending
	// Reserve is bound before serving requests.
	Reserve *CodexReserve
	windows map[codex.AccountKey]map[quota.WindowName]codexWindowFact
	mu      sync.RWMutex

	now    func() time.Time
	maxAge time.Duration
	facts  map[capacityFactKey]CapacityFact
	seq    uint64
	leases map[codex.AccountKey]int

	livePositiveHighWater map[capacityBucketKey]CapacityFact
	suppressedHardFences  map[capacityFactKey]bool

	observationGeneration atomic.Uint64
}

// CodexQuotaResetEvent identifies recovered included quota for one account.
type CodexQuotaResetEvent struct {
	AccountKey codex.AccountKey
	EventID    string
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

		livePositiveHighWater: make(map[capacityBucketKey]CapacityFact),
		suppressedHardFences:  make(map[capacityFactKey]bool),
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
	if fact.ObservedAt.IsZero() {
		fact.ObservedAt = l.now()
	}

	l.mu.Lock()
	accepted := l.observeLocked(fact)
	l.mu.Unlock()
	if accepted {
		l.observeQuotaReset(fact)
	}
	return accepted
}

func (l *CodexCapacityLedger) observeLocked(fact CapacityFact) bool {
	if !validCapacityFact(fact) {
		return false
	}
	key := capacityFactKey{account: fact.AccountKey, bucket: fact.Bucket, source: fact.Source}
	if current, ok := l.facts[key]; ok && !capacityFactAdvances(current, fact) {
		return false
	}
	l.observeWindowsLocked(fact)
	l.facts[key] = fact
	l.updateHardFenceState(key, fact)
	return true
}

func (l *CodexCapacityLedger) updateHardFenceState(key capacityFactKey, fact CapacityFact) {
	bucketKey := capacityBucketKey{account: fact.AccountKey, bucket: fact.Bucket}
	switch fact.Source {
	case CapacitySourceHardLimit:
		live, ok := l.livePositiveHighWater[bucketKey]
		l.suppressedHardFences[key] = ok && liveFactLiftsHardFence(live, fact)
	case CapacitySourceLiveUsage, CapacitySourceLiveRateLimits:
		if fact.Confidence != CapacityConfidenceAuthoritative || fact.RemainingPct <= 0 {
			return
		}
		l.livePositiveHighWater[bucketKey] = fact
		hardKey := capacityFactKey{account: fact.AccountKey, bucket: fact.Bucket, source: CapacitySourceHardLimit}
		if hard, ok := l.facts[hardKey]; ok && liveFactLiftsHardFence(fact, hard) {
			l.suppressedHardFences[hardKey] = true
		}
	}
}

func capacityFactAdvances(current, next CapacityFact) bool {
	if next.Source == CapacitySourceUsageCache {
		return next.ObservedAt.After(current.ObservedAt)
	}
	return capacityCursorAfter(next, current)
}

func capacityCursorAfter(next, current CapacityFact) bool {
	if next.ConnectionGeneration != current.ConnectionGeneration {
		return next.ConnectionGeneration > current.ConnectionGeneration
	}
	return next.Sequence > current.Sequence
}

func validCapacityFact(fact CapacityFact) bool {
	if fact.AccountKey == "" || fact.RemainingPct < 0 || fact.RemainingPct > 100 || fact.Sequence == 0 {
		return false
	}
	switch fact.Source {
	case CapacitySourceUsageCache:
		return fact.Confidence == CapacityConfidenceAdvisory && fact.ConnectionGeneration == 0
	case CapacitySourceHardLimit:
		return fact.Confidence == CapacityConfidenceAuthoritative && fact.ConnectionGeneration > 0 && fact.RemainingPct == 0
	case CapacitySourceHTTPHeaders, CapacitySourceLiveUsage, CapacitySourceLiveRateLimits:
		return fact.Confidence == CapacityConfidenceAuthoritative && fact.ConnectionGeneration > 0
	default:
		return false
	}
}

// ObserveQuotaSnapshot imports shared and exact scoped windows from usage cache.
func (l *CodexCapacityLedger) ObserveQuotaSnapshot(account codex.AccountKey, snap QuotaSnapshot) {
	if l == nil || account == "" || len(snap.Result.Windows) == 0 {
		return
	}
	aggregates := capacitySnapshotAggregates(snap)
	for bucket, aggregate := range aggregates {
		l.mu.Lock()
		l.seq++
		fact := CapacityFact{
			AccountKey:   account,
			Windows:      aggregate.windows,
			Bucket:       bucket,
			RemainingPct: aggregate.remaining,
			Source:       CapacitySourceUsageCache,
			Sequence:     l.seq,
			ObservedAt:   snap.FetchedAt,
			ResetAt:      aggregate.reset,
			Confidence:   CapacityConfidenceAdvisory,
		}
		l.observeLocked(fact)
		l.mu.Unlock()
	}
}

// ObserveLivePositiveQuotaSnapshot records positive capacity from a fresh
// authenticated usage response. Positive live evidence can lift an older hard
// fence after a banked reset; zero usage remains advisory.
func (l *CodexCapacityLedger) ObserveLivePositiveQuotaSnapshot(stream *CodexCapacityObservationStream, account codex.AccountKey, snap QuotaSnapshot) {
	l.observeLivePositiveQuotaSnapshot(stream, account, snap)
}

func (l *CodexCapacityLedger) observeLivePositiveQuotaSnapshot(stream *CodexCapacityObservationStream, account codex.AccountKey, snap QuotaSnapshot) bool {
	if l == nil || stream == nil || account == "" || !snap.Result.IsUsable() || snap.Result.CacheAge != 0 || len(snap.Result.Windows) == 0 {
		return false
	}
	confirmed := false
	for bucket, aggregate := range capacitySnapshotAggregates(snap) {
		fact := stream.Stamp(CapacityFact{
			AccountKey: account, Windows: aggregate.windows, Bucket: bucket,
			RemainingPct: aggregate.remaining, Source: CapacitySourceLiveUsage,
			ObservedAt: snap.FetchedAt, ResetAt: aggregate.reset, Confidence: CapacityConfidenceAuthoritative,
		})
		if aggregate.remaining <= 0 {
			// Authenticated zero establishes reset evidence without changing
			// the deliberately advisory usage-zero admission policy.
			l.observeQuotaReset(fact)
			continue
		}
		if l.Observe(fact) && bucket == CapacityBucketBase && freshPositiveSharedQuota(snap, l.now(), l.maxAge) {
			confirmed = true
		}
	}
	return confirmed
}

func freshPositiveSharedQuota(snap QuotaSnapshot, now time.Time, maxAge time.Duration) bool {
	if snap.FetchedAt.IsZero() || snap.FetchedAt.After(now) || now.Sub(snap.FetchedAt) >= maxAge {
		return false
	}
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		window, ok := snap.Result.Windows[name]
		if !ok || window.RemainingPctExact == nil || !(windowRemaining(window) > 0 && windowRemaining(window) <= 100) || window.ResetAtUnix <= now.Unix() {
			return false
		}
	}
	return true
}

func capacitySnapshotAggregates(snap QuotaSnapshot) map[CapacityBucket]capacitySnapshotAggregate {
	aggregates := make(map[CapacityBucket]capacitySnapshotAggregate)
	for name, window := range snap.Result.Windows {
		bucket := CapacityBucketBase
		if scoped := quota.WindowBucket(name); scoped != "" {
			bucket = CapacityBucket(capacityBucketModelPrefix + strings.ToLower(ParseModel(scoped)))
		}
		current := aggregates[bucket]
		if current.windows == nil {
			current.windows = make(map[quota.WindowName]quota.Window)
		}
		current.windows[name] = window
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
	return aggregates
}

// Capacity returns exact bucket state, falling scoped requests back to shared
// state without letting an authoritative shared zero gate another bucket.
func (l *CodexCapacityLedger) Capacity(account codex.AccountKey, bucket CapacityBucket) CapacityView {
	if l != nil && l.Reserve != nil {
		if blocked, reset := l.Reserve.Blocked(account); blocked {
			return CapacityView{State: CapacityZero, ResetAt: time.Unix(reset, 0), Exact: true}
		}
	}
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
			if view.State == CapacityZero {
				return CapacityView{State: CapacityUnknown, RemainingPct: -1, Exact: false}
			}
			view.Exact = false
			return view
		}
	}
	return CapacityView{State: CapacityUnknown, Exact: bucket == CapacityBucketBase}
}

// IncludedCapacity excludes authenticated depleted windows when selecting a
// redistribution target; ordinary capacity may still admit extra-credit work.
func (l *CodexCapacityLedger) IncludedCapacity(account codex.AccountKey, bucket CapacityBucket) CapacityView {
	view := l.Capacity(account, bucket)
	if l == nil || view.State != CapacityPositive {
		return view
	}
	if bucket == "" {
		bucket = CapacityBucketBase
	}
	now := l.now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	for name, entry := range l.includedWindows[account] {
		windowBucket := CapacityBucketBase
		if scoped := quota.WindowBucket(name); scoped != "" {
			windowBucket = CapacityBucket(capacityBucketModelPrefix + strings.ToLower(ParseModel(scoped)))
		}
		if windowBucket != bucket && !(windowBucket == CapacityBucketBase && !view.Exact) {
			continue
		}
		if entry.fact.ObservedAt.After(now) || now.Sub(entry.fact.ObservedAt) >= l.maxAge || entry.window.ResetAtUnix <= now.Unix() {
			continue
		}
		if windowRemaining(entry.window) == 0 {
			return CapacityView{State: CapacityZero, RemainingPct: 0, ResetAt: time.Unix(entry.window.ResetAtUnix, 0), Source: entry.fact.Source, Exact: true}
		}
	}
	return view
}

func (l *CodexCapacityLedger) capacityLocked(account codex.AccountKey, bucket CapacityBucket) (CapacityView, bool) {
	now := l.now()
	var selected CapacityFact
	haveSelected := false
	var hard CapacityFact
	haveHard := false
	for _, source := range []CapacitySource{
		CapacitySourceUsageCache,
		CapacitySourceHardLimit,
		CapacitySourceHTTPHeaders,
		CapacitySourceLiveUsage,
		CapacitySourceLiveRateLimits,
	} {
		fact, ok := l.facts[capacityFactKey{account: account, bucket: bucket, source: source}]
		if !ok || l.factStale(fact, now) {
			continue
		}
		if source == CapacitySourceHardLimit && fact.RemainingPct == 0 {
			if l.suppressedHardFences[capacityFactKey{account: account, bucket: bucket, source: source}] {
				continue
			}
			hard, haveHard = fact, true
		}
		selected, haveSelected = fact, true
	}
	if !haveSelected {
		return CapacityView{}, false
	}
	// A fresh authenticated poll after a reset can recover capacity before
	// an already-open WebSocket delivers its next rate-limit event.
	if selected.Source == CapacitySourceLiveRateLimits && selected.RemainingPct == 0 {
		live, ok := l.facts[capacityFactKey{account: account, bucket: bucket, source: CapacitySourceLiveUsage}]
		if ok && !l.factStale(live, now) && live.ObservedAt.After(selected.ObservedAt) &&
			(liveFactLiftsHardFence(live, selected) || liveFactLiftsObservedZero(live, selected)) {
			selected = live
		}
	}
	if haveHard && !liveFactLiftsHardFence(selected, hard) {
		selected = hard
	}
	state := CapacityPositive
	if selected.RemainingPct <= 0 {
		if selected.Confidence == CapacityConfidenceAuthoritative {
			state = CapacityZero
		} else {
			state = CapacityUnknown
			selected.RemainingPct = -1
		}
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
	return !now.Before(fact.ObservedAt.Add(l.maxAge))
}

func liveFactLiftsHardFence(live, hard CapacityFact) bool {
	if (live.Source != CapacitySourceLiveUsage && live.Source != CapacitySourceLiveRateLimits) || live.Confidence != CapacityConfidenceAuthoritative || live.RemainingPct <= 0 {
		return false
	}
	if !capacityCursorAfter(live, hard) {
		return false
	}
	if hard.ResetAt.IsZero() || live.ResetAt.IsZero() || !live.ResetAt.Before(hard.ResetAt) {
		return true
	}
	// Shared usage carries the earliest window reset, while a hard failure
	// can carry the later weekly reset. Compare the exact shared windows.
	if live.Bucket != CapacityBucketBase || hard.Bucket != CapacityBucketBase || !live.ObservedAt.After(hard.ObservedAt) {
		return false
	}
	matchedEpoch := false
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		window, ok := live.Windows[name]
		if !ok || window.RemainingPctExact == nil || !(windowRemaining(window) > 0 && windowRemaining(window) <= 100) || window.ResetAtUnix <= live.ObservedAt.Unix() {
			return false
		}
		if !time.Unix(window.ResetAtUnix, 0).Before(hard.ResetAt) {
			matchedEpoch = true
		}
	}
	return matchedEpoch
}

func liveFactLiftsObservedZero(live, zero CapacityFact) bool {
	if live.Source != CapacitySourceLiveUsage || live.Confidence != CapacityConfidenceAuthoritative || live.RemainingPct <= 0 || !capacityCursorAfter(live, zero) {
		return false
	}
	confirmed := false
	for name, exhausted := range zero.Windows {
		if exhausted.RemainingPctExact == nil || windowRemaining(exhausted) != 0 {
			continue
		}
		restored, ok := live.Windows[name]
		if !ok || restored.RemainingPctExact == nil || !(windowRemaining(restored) > 0 && windowRemaining(restored) <= 100) ||
			exhausted.ResetAtUnix <= 0 || restored.ResetAtUnix < exhausted.ResetAtUnix {
			return false
		}
		confirmed = true
	}
	return confirmed
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
