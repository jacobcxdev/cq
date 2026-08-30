package aggregate

import (
	"math"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

type ResetScheduleConfidence string
type ResetScheduleStatus string
type ResetScheduleReason string
type ResetRateSource string

const (
	ResetConfidenceHigh ResetScheduleConfidence = "high"
	ResetConfidenceLow  ResetScheduleConfidence = "low"

	ResetDueNow      ResetScheduleStatus = "due_now"
	ResetScheduled   ResetScheduleStatus = "scheduled"
	ResetDeferred    ResetScheduleStatus = "deferred"
	ResetNotUseful   ResetScheduleStatus = "not_yet_useful"
	ResetUnsupported ResetScheduleStatus = "unsupported"

	ResetReasonGapAvoidance   ResetScheduleReason = "gap_avoidance"
	ResetReasonExpiryPressure ResetScheduleReason = "expiry_pressure"
	ResetReasonNaturalReset   ResetScheduleReason = "natural_reset_interaction"
	ResetReasonWasteReduction ResetScheduleReason = "waste_reduction"
	ResetReasonRateFallback   ResetScheduleReason = "low_confidence_fallback"

	ResetRateEWMA         ResetRateSource = "ewma"
	ResetRateCycleAverage ResetRateSource = "cycle_average"
)

type ResetScheduleInput struct {
	Now      time.Time
	Accounts []ResetScheduleAccountInput
}

type ResetScheduleAccountInput struct {
	Key        string
	Email      string
	AccountID  string
	Multiplier int
	Windows    map[quota.WindowName]ResetScheduleWindowInput
	Credits    []ResetScheduleCreditInput
}

type ResetScheduleWindowInput struct {
	RemainingPct float64
	ResetAt      time.Time
	Period       time.Duration
	BurnPctPerS  float64
	RateSource   ResetRateSource
	Samples      int
}

type ResetScheduleCreditInput struct {
	ID        string
	GrantedAt time.Time
	ExpiresAt *time.Time
	Supported bool
}

type ResetScheduleObjective struct {
	UnmetDemandPctSeconds float64
	GapDurationSeconds    int64
	UsefulExpiredUnused   int
	WeightedDiscardedPct  float64
	RestoredPct           float64
}

type ResetScheduleBlocker struct {
	Code         string
	AccountEmail string
	AccountID    string
}

type ResetSchedule struct {
	GeneratedAt time.Time
	Horizon     time.Time
	Exact       bool
	Complete    bool
	Confidence  ResetScheduleConfidence
	Items       []ResetScheduleItem
	Objective   ResetScheduleObjective
	Blockers    []ResetScheduleBlocker
}

type ResetScheduleItem struct {
	AccountEmail  string
	AccountID     string
	CreditID      string
	UseAt         time.Time
	UseBy         time.Time
	Status        ResetScheduleStatus
	Confidence    ResetScheduleConfidence
	RestoredPct   map[quota.WindowName]float64
	AvoidedGapSec int64
	ReasonCodes   []ResetScheduleReason
}

type simulationWindow struct {
	remaining float64
	resetAt   time.Time
	period    time.Duration
}

type simulationAccount struct {
	key        string
	email      string
	accountID  string
	multiplier float64
	windows    map[quota.WindowName]simulationWindow
}

type simulationCredit struct {
	id         string
	accountKey string
	grantedAt  time.Time
	expiresAt  *time.Time
	supported  bool
}

type simulationState struct {
	now         time.Time
	horizon     time.Time
	accounts    map[string]simulationAccount
	credits     map[string]simulationCredit
	totalDemand map[quota.WindowName]float64
	confidence  ResetScheduleConfidence
	objective   ResetScheduleObjective
}

type simulationEventKind uint8

const (
	simulationEventNaturalReset simulationEventKind = iota + 1
	simulationEventExpiry
	simulationEventExhaustion
	simulationEventConsumption
)

type simulationEvent struct {
	at         time.Time
	kind       simulationEventKind
	accountKey string
	creditID   string
	window     quota.WindowName
}

func normaliseResetScheduleInput(input ResetScheduleInput) (simulationState, []ResetScheduleBlocker) {
	state := simulationState{
		now: input.Now.UTC(), accounts: make(map[string]simulationAccount),
		credits: make(map[string]simulationCredit), totalDemand: make(map[quota.WindowName]float64),
		confidence: ResetConfidenceHigh,
	}
	var blockers []ResetScheduleBlocker
	allCredits := make([]ResetScheduleCreditInput, 0)
	for _, inputAccount := range input.Accounts {
		blocker := func(code string) {
			blockers = append(blockers, ResetScheduleBlocker{Code: code, AccountEmail: inputAccount.Email, AccountID: inputAccount.AccountID})
		}
		if inputAccount.Key == "" {
			blocker("missing_account_key")
			continue
		}
		if _, duplicate := state.accounts[inputAccount.Key]; duplicate {
			blocker("duplicate_account_key")
			continue
		}
		if inputAccount.Multiplier <= 0 {
			blocker("invalid_multiplier")
			continue
		}

		account := simulationAccount{
			key: inputAccount.Key, email: inputAccount.Email, accountID: inputAccount.AccountID,
			multiplier: float64(inputAccount.Multiplier), windows: make(map[quota.WindowName]simulationWindow),
		}
		valid := true
		accountDemand := make(map[quota.WindowName]float64)
		usedFallback := false
		for _, name := range sharedResetWindows() {
			inputWindow, ok := inputAccount.Windows[name]
			if !ok {
				blocker("missing_shared_window")
				valid = false
				continue
			}
			if !finiteInRange(inputWindow.RemainingPct, 0, 100) || inputWindow.Period <= 0 || inputWindow.ResetAt.IsZero() {
				blocker("invalid_shared_window")
				valid = false
				continue
			}
			if inputWindow.ResetAt.Before(state.now) {
				blocker("stale_reset_epoch")
				valid = false
				continue
			}
			rate, fallbackOK := resetWindowRate(inputWindow, state.now)
			if !fallbackOK {
				blocker("invalid_burn_rate")
				valid = false
				continue
			}
			if inputWindow.RateSource != ResetRateEWMA || inputWindow.Samples < 2 {
				usedFallback = true
			}
			accountDemand[name] = rate * account.multiplier
			account.windows[name] = simulationWindow{
				remaining: inputWindow.RemainingPct,
				resetAt:   inputWindow.ResetAt.UTC(),
				period:    inputWindow.Period,
			}
		}
		if !valid {
			continue
		}
		if usedFallback {
			state.confidence = ResetConfidenceLow
		}
		for name, demand := range accountDemand {
			state.totalDemand[name] += demand
		}
		state.accounts[account.key] = account
		for _, inputCredit := range inputAccount.Credits {
			allCredits = append(allCredits, inputCredit)
			if inputCredit.ID == "" {
				blocker("missing_credit_id")
				continue
			}
			if _, duplicate := state.credits[inputCredit.ID]; duplicate {
				blocker("duplicate_credit_id")
				continue
			}
			credit := simulationCredit{
				id: inputCredit.ID, accountKey: account.key, grantedAt: inputCredit.GrantedAt.UTC(),
				supported: inputCredit.Supported,
			}
			if inputCredit.ExpiresAt != nil {
				expiresAt := inputCredit.ExpiresAt.UTC()
				credit.expiresAt = &expiresAt
			}
			state.credits[credit.id] = credit
		}
	}
	state.horizon = resetPlanningHorizon(state.now, allCredits)
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].AccountEmail != blockers[j].AccountEmail {
			return blockers[i].AccountEmail < blockers[j].AccountEmail
		}
		if blockers[i].AccountID != blockers[j].AccountID {
			return blockers[i].AccountID < blockers[j].AccountID
		}
		return blockers[i].Code < blockers[j].Code
	})
	return state, blockers
}

func resetWindowRate(window ResetScheduleWindowInput, now time.Time) (float64, bool) {
	if math.IsNaN(window.BurnPctPerS) || math.IsInf(window.BurnPctPerS, 0) || window.BurnPctPerS < 0 {
		return 0, false
	}
	if window.RateSource == ResetRateEWMA && window.Samples >= 2 {
		return window.BurnPctPerS, finiteInRange(window.BurnPctPerS, 0, math.MaxFloat64)
	}
	used := 100 - window.RemainingPct
	if used <= 0 {
		return 0, true
	}
	elapsed := window.Period - window.ResetAt.Sub(now)
	if elapsed > 0 {
		return used / elapsed.Seconds(), true
	}
	if window.Period <= 0 {
		return 0, false
	}
	return used / window.Period.Seconds(), true
}

func finiteInRange(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}

func sharedResetWindows() []quota.WindowName {
	return []quota.WindowName{quota.Window5Hour, quota.Window7Day}
}

func resetPlanningHorizon(now time.Time, credits []ResetScheduleCreditInput) time.Time {
	var latest time.Time
	finite := false
	for _, credit := range credits {
		if credit.ExpiresAt == nil {
			continue
		}
		expiresAt := credit.ExpiresAt.UTC()
		if !finite || expiresAt.After(latest) {
			latest = expiresAt
			finite = true
		}
	}
	if !finite {
		return now.UTC().Add(quota.PeriodFor(quota.Window7Day))
	}
	return latest.Add(quota.PeriodFor(quota.Window7Day))
}

func applyBankedReset(account simulationAccount) (simulationAccount, map[quota.WindowName]float64) {
	account = cloneSimulationAccount(account)
	restored := make(map[quota.WindowName]float64)
	for _, name := range sharedResetWindows() {
		window, ok := account.windows[name]
		if !ok {
			continue
		}
		restored[name] = 100 - window.remaining
		window.remaining = 100
		account.windows[name] = window
	}
	return account, restored
}

func applyNaturalReset(account simulationAccount, name quota.WindowName) (simulationAccount, float64) {
	account = cloneSimulationAccount(account)
	window, ok := account.windows[name]
	if !ok {
		return account, 0
	}
	discarded := window.remaining
	window.remaining = 100
	window.resetAt = window.resetAt.Add(window.period)
	account.windows[name] = window
	return account, discarded
}

func cloneSimulationAccount(account simulationAccount) simulationAccount {
	cloned := account
	cloned.windows = make(map[quota.WindowName]simulationWindow, len(account.windows))
	for name, window := range account.windows {
		cloned.windows[name] = window
	}
	return cloned
}

func cloneSimulationState(state simulationState) simulationState {
	cloned := state
	cloned.accounts = make(map[string]simulationAccount, len(state.accounts))
	for key, account := range state.accounts {
		cloned.accounts[key] = cloneSimulationAccount(account)
	}
	cloned.credits = make(map[string]simulationCredit, len(state.credits))
	for id, credit := range state.credits {
		cloned.credits[id] = credit
	}
	cloned.totalDemand = make(map[quota.WindowName]float64, len(state.totalDemand))
	for name, demand := range state.totalDemand {
		cloned.totalDemand[name] = demand
	}
	return cloned
}

func simulationAccountAvailable(account simulationAccount) bool {
	for _, name := range sharedResetWindows() {
		window, ok := account.windows[name]
		if !ok || window.remaining <= 0 {
			return false
		}
	}
	return true
}

func simulationActiveMultiplier(state simulationState) float64 {
	var total float64
	for _, account := range state.accounts {
		if simulationAccountAvailable(account) {
			total += account.multiplier
		}
	}
	return total
}

func nextSimulationEvent(state simulationState) (simulationEvent, bool) {
	var next simulationEvent
	have := false
	consider := func(candidate simulationEvent) {
		if candidate.at.Before(state.now) || candidate.at.After(state.horizon) {
			return
		}
		if !have || simulationEventLess(candidate, next) {
			next = candidate
			have = true
		}
	}

	accountKeys := sortedSimulationAccountKeys(state)
	for _, key := range accountKeys {
		account := state.accounts[key]
		for _, name := range sharedResetWindows() {
			window := account.windows[name]
			consider(simulationEvent{at: window.resetAt, kind: simulationEventNaturalReset, accountKey: key, window: name})
		}
	}
	creditIDs := sortedSimulationCreditIDs(state)
	for _, id := range creditIDs {
		credit := state.credits[id]
		if credit.expiresAt != nil {
			consider(simulationEvent{at: *credit.expiresAt, kind: simulationEventExpiry, accountKey: credit.accountKey, creditID: id})
		}
	}

	activeMultiplier := simulationActiveMultiplier(state)
	if activeMultiplier > 0 {
		for _, key := range accountKeys {
			account := state.accounts[key]
			if !simulationAccountAvailable(account) {
				continue
			}
			for _, name := range sharedResetWindows() {
				rate := state.totalDemand[name] / activeMultiplier
				if rate <= 0 {
					continue
				}
				seconds := account.windows[name].remaining / rate
				if !finiteInRange(seconds, 0, state.horizon.Sub(state.now).Seconds()) {
					continue
				}
				at := state.now.Add(time.Duration(seconds * float64(time.Second)))
				consider(simulationEvent{at: at, kind: simulationEventExhaustion, accountKey: key, window: name})
			}
		}
	}
	return next, have
}

func simulationEventLess(left, right simulationEvent) bool {
	if !left.at.Equal(right.at) {
		return left.at.Before(right.at)
	}
	if left.kind != right.kind {
		return left.kind < right.kind
	}
	if left.accountKey != right.accountKey {
		return left.accountKey < right.accountKey
	}
	if left.creditID != right.creditID {
		return left.creditID < right.creditID
	}
	return left.window < right.window
}

func advanceSimulationTo(state simulationState, target time.Time) simulationState {
	if !target.After(state.now) {
		return state
	}
	state = cloneSimulationState(state)
	seconds := target.Sub(state.now).Seconds()
	activeMultiplier := simulationActiveMultiplier(state)
	if activeMultiplier == 0 {
		var totalDemand float64
		for _, demand := range state.totalDemand {
			totalDemand += demand
		}
		if totalDemand > 0 {
			state.objective.GapDurationSeconds += int64(target.Sub(state.now) / time.Second)
			state.objective.UnmetDemandPctSeconds += totalDemand * seconds
		}
		state.now = target
		return state
	}
	for key, account := range state.accounts {
		if !simulationAccountAvailable(account) {
			continue
		}
		for _, name := range sharedResetWindows() {
			window := account.windows[name]
			window.remaining = clampPercentage(window.remaining - state.totalDemand[name]/activeMultiplier*seconds)
			account.windows[name] = window
		}
		state.accounts[key] = account
	}
	state.now = target
	return state
}

func processSimulationEvent(state simulationState, event simulationEvent) simulationState {
	state = cloneSimulationState(state)
	switch event.kind {
	case simulationEventNaturalReset:
		account, ok := state.accounts[event.accountKey]
		if !ok {
			return state
		}
		var discarded float64
		account, discarded = applyNaturalReset(account, event.window)
		state.objective.WeightedDiscardedPct += discarded * account.multiplier
		state.accounts[event.accountKey] = account
	case simulationEventExpiry:
		delete(state.credits, event.creditID)
	case simulationEventExhaustion:
		account, ok := state.accounts[event.accountKey]
		if !ok {
			return state
		}
		window := account.windows[event.window]
		window.remaining = 0
		account.windows[event.window] = window
		state.accounts[event.accountKey] = account
	}
	return state
}

func applySimulationCredit(state simulationState, accountKey, creditID string) (simulationState, map[quota.WindowName]float64, bool) {
	credit, ok := state.credits[creditID]
	if !ok || !credit.supported || credit.accountKey != accountKey {
		return state, nil, false
	}
	account, ok := state.accounts[accountKey]
	if !ok {
		return state, nil, false
	}
	state = cloneSimulationState(state)
	account, restored := applyBankedReset(account)
	state.accounts[accountKey] = account
	delete(state.credits, creditID)
	for _, amount := range restored {
		state.objective.RestoredPct += amount
	}
	return state, restored, true
}

func clampPercentage(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func sortedSimulationAccountKeys(state simulationState) []string {
	keys := make([]string, 0, len(state.accounts))
	for key := range state.accounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSimulationCreditIDs(state simulationState) []string {
	ids := make([]string, 0, len(state.credits))
	for id := range state.credits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
