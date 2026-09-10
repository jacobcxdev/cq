package aggregate

import (
	"math"
	"sort"
	"strings"
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

const resetScheduleBeamWidth = 512

type resetSearchConsumption struct {
	accountKey string
	creditID   string
	useBy      time.Time
	eventKind  simulationEventKind
	restored   map[quota.WindowName]float64
	avoidedGap int64
}

type resetSearchNode struct {
	state        simulationState
	consumptions []resetSearchConsumption
	useful       map[string]bool
}

type resetCreditRecord struct {
	credit     ResetScheduleCreditInput
	accountKey string
	email      string
	accountID  string
}

// RecommendResetSchedule returns a deterministic, percentage-only reset plan.
func RecommendResetSchedule(input ResetScheduleInput) ResetSchedule {
	state, blockers := normaliseResetScheduleInput(input)
	result := ResetSchedule{
		GeneratedAt: input.Now.UTC(),
		Horizon:     state.horizon,
		Exact:       len(blockers) == 0,
		Complete:    len(blockers) == 0,
		Confidence:  state.confidence,
		Blockers:    blockers,
	}
	records := resetCreditRecords(input)
	if len(blockers) != 0 {
		result.Exact = false
		result.Items = unfinishedResetScheduleItems(records, nil, nil, input.Now.UTC(), state.confidence)
		return result
	}

	node, exact := searchResetSchedule(state)
	result.Exact = exact
	result.Objective = node.state.objective
	consumed := make(map[string]resetSearchConsumption, len(node.consumptions))
	for _, consumption := range node.consumptions {
		consumed[consumption.creditID] = consumption
	}
	result.Items = projectResetScheduleItems(records, consumed, node.useful, input.Now.UTC(), state.confidence)
	return result
}

func searchResetSchedule(initial simulationState) (resetSearchNode, bool) {
	seed := resetSearchNode{state: initial, useful: make(map[string]bool)}
	nodes := []resetSearchNode{seed}
	if simulationActiveMultiplier(initial) == 0 && simulationHasDemand(initial) {
		nodes = branchImmediateResetConsumptions(seed)
	}
	finals := make([]resetSearchNode, 0)
	exact := true
	const maximumEvents = 10_000
	for iteration := 0; len(nodes) > 0 && iteration < maximumEvents; iteration++ {
		nextNodes := make([]resetSearchNode, 0, len(nodes)*2)
		for _, node := range nodes {
			if !node.state.now.Before(node.state.horizon) {
				finals = append(finals, node)
				continue
			}
			event, ok := nextSimulationEvent(node.state)
			if !ok {
				node.state = advanceSimulationTo(node.state, node.state.horizon)
				finals = append(finals, node)
				continue
			}
			nextNodes = append(nextNodes, expandResetSearchEvent(node, event)...)
		}
		if len(nextNodes) == 0 {
			break
		}
		var truncated bool
		nodes, truncated = pruneResetSearchNodes(nextNodes, resetScheduleBeamWidth)
		if truncated {
			exact = false
		}
	}
	for _, node := range nodes {
		if node.state.now.Before(node.state.horizon) {
			node.state = advanceSimulationTo(node.state, node.state.horizon)
		}
		finals = append(finals, node)
	}
	if len(finals) == 0 {
		return seed, false
	}
	sort.SliceStable(finals, func(i, j int) bool { return betterResetSearchNode(finals[i], finals[j]) })
	return finals[0], exact
}

func branchImmediateResetConsumptions(node resetSearchNode) []resetSearchNode {
	useful := usefulResetCredits(node.state)
	avoidedGap := projectedSimulationGap(node.state)
	for _, credit := range useful {
		node.useful[credit.id] = true
	}
	branches := make([]resetSearchNode, 0, len(useful))
	if len(useful) == 0 {
		branches = append(branches, cloneResetSearchNode(node))
	}
	for _, credit := range useful {
		branch := cloneResetSearchNode(node)
		state, restored, applied := applySimulationCredit(branch.state, credit.accountKey, credit.id)
		if !applied {
			continue
		}
		branch.state = state
		branch.consumptions = append(branch.consumptions, resetSearchConsumption{
			accountKey: credit.accountKey, creditID: credit.id, useBy: state.now,
			eventKind: simulationEventConsumption, restored: restored, avoidedGap: avoidedGap,
		})
		branches = append(branches, branch)
	}
	return branches
}

func expandResetSearchEvent(node resetSearchNode, event simulationEvent) []resetSearchNode {
	advanced := cloneResetSearchNode(node)
	advanced.state = advanceSimulationTo(advanced.state, event.at)
	useful := usefulResetCredits(advanced.state)
	for _, credit := range useful {
		advanced.useful[credit.id] = true
	}

	wait := cloneResetSearchNode(advanced)
	if event.kind == simulationEventExpiry {
		if credit, ok := wait.state.credits[event.creditID]; ok && credit.supported && resetCreditUseful(wait.state, credit) {
			wait.state.objective.UsefulExpiredUnused++
		}
	}
	wait.state = processSimulationEvent(wait.state, event)
	avoidedGap := int64(0)
	if simulationActiveMultiplier(wait.state) == 0 && simulationHasDemand(wait.state) {
		avoidedGap = projectedSimulationGap(wait.state)
	}
	branches := make([]resetSearchNode, 0, len(useful)+1)
	avoidableGap := event.kind == simulationEventExhaustion && len(useful) > 0 &&
		simulationActiveMultiplier(wait.state) == 0 && simulationHasDemand(wait.state)
	if !avoidableGap {
		branches = append(branches, wait)
	}
	for _, credit := range useful {
		branch := cloneResetSearchNode(advanced)
		state, restored, applied := applySimulationCredit(branch.state, credit.accountKey, credit.id)
		if !applied {
			continue
		}
		branch.state = state
		branch.consumptions = append(branch.consumptions, resetSearchConsumption{
			accountKey: credit.accountKey, creditID: credit.id, useBy: event.at,
			eventKind: event.kind, restored: restored, avoidedGap: avoidedGap,
		})
		branches = append(branches, branch)
	}
	return branches
}

func projectedSimulationGap(state simulationState) int64 {
	if simulationActiveMultiplier(state) > 0 || !simulationHasDemand(state) {
		return 0
	}
	end := state.horizon
	if event, ok := nextSimulationEvent(state); ok && event.at.Before(end) {
		end = event.at
	}
	if !end.After(state.now) {
		return 0
	}
	return int64(end.Sub(state.now) / time.Second)
}

func usefulResetCredits(state simulationState) []simulationCredit {
	credits := make([]simulationCredit, 0)
	for _, id := range sortedSimulationCreditIDs(state) {
		credit := state.credits[id]
		if resetCreditUseful(state, credit) {
			credits = append(credits, credit)
		}
	}
	return credits
}

func resetCreditUseful(state simulationState, credit simulationCredit) bool {
	if !credit.supported || credit.grantedAt.After(state.now) {
		return false
	}
	if credit.expiresAt != nil && credit.expiresAt.Before(state.now) {
		return false
	}
	account, ok := state.accounts[credit.accountKey]
	if !ok {
		return false
	}
	for _, name := range sharedResetWindows() {
		if 100-account.windows[name].remaining >= 1 {
			return true
		}
	}
	return false
}

func cloneResetSearchNode(node resetSearchNode) resetSearchNode {
	cloned := node
	cloned.state = cloneSimulationState(node.state)
	cloned.consumptions = append([]resetSearchConsumption(nil), node.consumptions...)
	cloned.useful = make(map[string]bool, len(node.useful))
	for id, useful := range node.useful {
		cloned.useful[id] = useful
	}
	return cloned
}

func pruneResetSearchNodes(nodes []resetSearchNode, width int) ([]resetSearchNode, bool) {
	sort.SliceStable(nodes, func(i, j int) bool { return betterResetSearchNode(nodes[i], nodes[j]) })
	kept := make([]resetSearchNode, 0, min(len(nodes), width))
	for _, candidate := range nodes {
		dominated := false
		for _, existing := range kept {
			if dominatesResetSearchNode(existing, candidate) {
				dominated = true
				break
			}
		}
		if !dominated {
			kept = append(kept, candidate)
		}
	}
	truncated := len(kept) > width
	if truncated {
		kept = kept[:width]
	}
	return kept, truncated
}

func dominatesResetSearchNode(left, right resetSearchNode) bool {
	if !left.state.now.Equal(right.state.now) || resetCreditFingerprint(left.state) != resetCreditFingerprint(right.state) ||
		resetUsefulFingerprint(left.useful) != resetUsefulFingerprint(right.useful) {
		return false
	}
	if betterResetObjective(right.state.objective, left.state.objective) {
		return false
	}
	for key, rightAccount := range right.state.accounts {
		leftAccount, ok := left.state.accounts[key]
		if !ok {
			return false
		}
		for _, name := range sharedResetWindows() {
			if leftAccount.windows[name].remaining < rightAccount.windows[name].remaining {
				return false
			}
		}
	}
	return true
}

func resetCreditFingerprint(state simulationState) string {
	return strings.Join(sortedSimulationCreditIDs(state), "\x00")
}

func resetUsefulFingerprint(useful map[string]bool) string {
	ids := make([]string, 0, len(useful))
	for id, value := range useful {
		if value {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

func betterResetObjective(left, right ResetScheduleObjective) bool {
	switch {
	case left.UnmetDemandPctSeconds != right.UnmetDemandPctSeconds:
		return left.UnmetDemandPctSeconds < right.UnmetDemandPctSeconds
	case left.GapDurationSeconds != right.GapDurationSeconds:
		return left.GapDurationSeconds < right.GapDurationSeconds
	case left.UsefulExpiredUnused != right.UsefulExpiredUnused:
		return left.UsefulExpiredUnused < right.UsefulExpiredUnused
	case left.WeightedDiscardedPct != right.WeightedDiscardedPct:
		return left.WeightedDiscardedPct < right.WeightedDiscardedPct
	default:
		return left.RestoredPct > right.RestoredPct
	}
}

func betterResetSearchNode(left, right resetSearchNode) bool {
	if betterResetObjective(left.state.objective, right.state.objective) {
		return true
	}
	if betterResetObjective(right.state.objective, left.state.objective) {
		return false
	}
	leftTimes := resetConsumptionTimes(left.consumptions)
	rightTimes := resetConsumptionTimes(right.consumptions)
	for index := 0; index < min(len(leftTimes), len(rightTimes)); index++ {
		if !leftTimes[index].Equal(rightTimes[index]) {
			return leftTimes[index].After(rightTimes[index])
		}
	}
	if len(leftTimes) != len(rightTimes) {
		return len(leftTimes) < len(rightTimes)
	}
	leftFingerprint := resetSearchFingerprint(left)
	rightFingerprint := resetSearchFingerprint(right)
	return leftFingerprint < rightFingerprint
}

func resetConsumptionTimes(consumptions []resetSearchConsumption) []time.Time {
	ordered := append([]resetSearchConsumption(nil), consumptions...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].accountKey != ordered[j].accountKey {
			return ordered[i].accountKey < ordered[j].accountKey
		}
		return ordered[i].creditID < ordered[j].creditID
	})
	times := make([]time.Time, len(ordered))
	for index, consumption := range ordered {
		times[index] = consumption.useBy
	}
	return times
}

func resetSearchFingerprint(node resetSearchNode) string {
	parts := []string{node.state.now.Format(time.RFC3339Nano), resetCreditFingerprint(node.state)}
	for _, consumption := range node.consumptions {
		parts = append(parts, consumption.accountKey, consumption.creditID, consumption.useBy.Format(time.RFC3339Nano))
	}
	return strings.Join(parts, "\x00")
}

func simulationHasDemand(state simulationState) bool {
	for _, demand := range state.totalDemand {
		if demand > 0 {
			return true
		}
	}
	return false
}

func resetCreditRecords(input ResetScheduleInput) map[string]resetCreditRecord {
	records := make(map[string]resetCreditRecord)
	for _, account := range input.Accounts {
		for _, credit := range account.Credits {
			if _, exists := records[credit.ID]; exists {
				continue
			}
			records[credit.ID] = resetCreditRecord{
				credit: credit, accountKey: account.Key, email: account.Email, accountID: account.AccountID,
			}
		}
	}
	return records
}

func projectResetScheduleItems(
	records map[string]resetCreditRecord,
	consumed map[string]resetSearchConsumption,
	useful map[string]bool,
	now time.Time,
	confidence ResetScheduleConfidence,
) []ResetScheduleItem {
	items := make([]ResetScheduleItem, 0, len(records))
	ids := sortedResetCreditRecordIDs(records)
	for _, id := range ids {
		record := records[id]
		if consumption, ok := consumed[id]; ok {
			useAt := consumption.useBy.Add(-5 * time.Minute)
			if useAt.Before(now) {
				useAt = now
			}
			status := ResetScheduled
			if !useAt.After(now) {
				status = ResetDueNow
			}
			item := ResetScheduleItem{
				AccountEmail: record.email, AccountID: record.accountID, CreditID: id,
				UseAt: useAt, UseBy: consumption.useBy, Status: status, Confidence: confidence,
				RestoredPct: consumption.restored, AvoidedGapSec: consumption.avoidedGap,
			}
			item.ReasonCodes = resetConsumptionReasons(record, consumption, confidence)
			items = append(items, item)
			continue
		}
		status := ResetNotUseful
		if !record.credit.Supported {
			status = ResetUnsupported
		} else if record.credit.ExpiresAt == nil || useful[id] {
			status = ResetDeferred
		}
		items = append(items, ResetScheduleItem{
			AccountEmail: record.email, AccountID: record.accountID, CreditID: id,
			Status: status, Confidence: confidence,
		})
	}
	sortResetScheduleItems(items)
	return items
}

func unfinishedResetScheduleItems(
	records map[string]resetCreditRecord,
	consumed map[string]resetSearchConsumption,
	useful map[string]bool,
	now time.Time,
	confidence ResetScheduleConfidence,
) []ResetScheduleItem {
	items := projectResetScheduleItems(records, consumed, useful, now, confidence)
	for index := range items {
		if items[index].Status != ResetUnsupported {
			items[index].Status = ResetDeferred
			items[index].UseAt = time.Time{}
			items[index].UseBy = time.Time{}
		}
	}
	return items
}

func resetConsumptionReasons(record resetCreditRecord, consumption resetSearchConsumption, confidence ResetScheduleConfidence) []ResetScheduleReason {
	reasons := make([]ResetScheduleReason, 0, 3)
	switch consumption.eventKind {
	case simulationEventExpiry:
		reasons = append(reasons, ResetReasonExpiryPressure)
	case simulationEventNaturalReset:
		reasons = append(reasons, ResetReasonNaturalReset)
	case simulationEventExhaustion, simulationEventConsumption:
		reasons = append(reasons, ResetReasonGapAvoidance)
	}
	if record.credit.ExpiresAt != nil && consumption.useBy.Equal(record.credit.ExpiresAt.UTC()) && consumption.eventKind != simulationEventExpiry {
		reasons = append(reasons, ResetReasonExpiryPressure)
	}
	reasons = append(reasons, ResetReasonWasteReduction)
	if confidence == ResetConfidenceLow {
		reasons = append(reasons, ResetReasonRateFallback)
	}
	return reasons
}

func sortResetScheduleItems(items []ResetScheduleItem) {
	sort.SliceStable(items, func(i, j int) bool {
		leftActionable := items[i].Status == ResetScheduled || items[i].Status == ResetDueNow
		rightActionable := items[j].Status == ResetScheduled || items[j].Status == ResetDueNow
		if leftActionable != rightActionable {
			return leftActionable
		}
		if leftActionable && !items[i].UseAt.Equal(items[j].UseAt) {
			return items[i].UseAt.Before(items[j].UseAt)
		}
		if items[i].AccountEmail != items[j].AccountEmail {
			return items[i].AccountEmail < items[j].AccountEmail
		}
		if items[i].AccountID != items[j].AccountID {
			return items[i].AccountID < items[j].AccountID
		}
		return items[i].CreditID < items[j].CreditID
	})
}

func sortedResetCreditRecordIDs(records map[string]resetCreditRecord) []string {
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
