package proxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexDiscoverer abstracts Codex account discovery for testability.
type CodexDiscoverer func() []codex.CodexAccount

// CodexSelector picks a Codex account for a request.
type CodexSelector interface {
	Select(ctx context.Context, exclude ...codex.SelectionExclusion) (*codex.CodexAccount, error)
}

// CodexRouteRequirements describes all capacity needed by one admission.
type CodexRouteRequirements struct {
	RequestedModel string
	RequiredModels []string
}

// RouteChoice binds account, effective model, and capacity buckets atomically.
type RouteChoice struct {
	AccountKey      codex.AccountKey
	RequestedModel  string
	EffectiveModel  string
	RequiredBuckets []CapacityBucket
}

// CodexRouteChooser returns an account/model/bucket decision without credentials.
type CodexRouteChooser interface {
	Choose(ctx context.Context, requirements CodexRouteRequirements, exclude ...codex.SelectionExclusion) (RouteChoice, error)
}

type codexQuotaReader interface {
	Snapshot(identifier string) (QuotaSnapshot, bool)
}

type codexCapacityProvider interface {
	CodexCapacityLedger() *CodexCapacityLedger
}

type codexRouteAccount struct {
	key         codex.AccountKey
	candidateID codex.CandidateID
	accountID   string
	email       string
	planType    string
	routable    bool
}

type codexSelector struct {
	discover          CodexDiscoverer
	discoverInventory func(context.Context) ([]codexRouteAccount, error)
	quota             codexQuotaReader
	capacity          *CodexCapacityLedger
	mu                sync.Mutex
}

// NewCodexInventorySelector creates a secret-free route chooser. Credential
// material remains behind CredentialInventory and SecretResolver boundaries.
func NewCodexInventorySelector(inventory codex.CredentialInventory, quota codexQuotaReader) CodexRouteChooser {
	selector := newCodexSelectorWithCapacity(nil, quota, codexCapacityForQuota(quota))
	selector.discoverInventory = func(ctx context.Context) ([]codexRouteAccount, error) {
		if inventory == nil {
			return nil, fmt.Errorf("Codex credential inventory unavailable")
		}
		view, err := inventory.List(ctx)
		if err != nil {
			return nil, err
		}
		accounts := make([]codexRouteAccount, 0, len(view.Accounts))
		for _, logical := range view.Accounts {
			if !logical.Routable {
				continue
			}
			var candidateID codex.CandidateID
			for _, candidate := range logical.Candidates {
				if !candidate.DispatchBlocked {
					candidateID = candidate.Ref.CandidateID
					break
				}
			}
			if candidateID == "" {
				continue
			}
			accounts = append(accounts, codexRouteAccount{
				key:         logical.Key,
				candidateID: candidateID,
				accountID:   logical.Identity.AccountID,
				email:       logical.Identity.Email,
				planType:    logical.Identity.PlanType,
				routable:    true,
			})
		}
		return accounts, nil
	}
	return selector
}

type codexModelContextKey struct{}

// NewCodexSelector creates a CodexSelector backed by the given discovery function.
func NewCodexSelector(discover CodexDiscoverer, quota codexQuotaReader) interface {
	CodexSelector
	CodexRouteChooser
} {
	return newCodexSelectorWithCapacity(discover, quota, codexCapacityForQuota(quota))
}

func codexCapacityForQuota(quota codexQuotaReader) *CodexCapacityLedger {
	if provider, ok := quota.(codexCapacityProvider); ok {
		if capacity := provider.CodexCapacityLedger(); capacity != nil {
			return capacity
		}
	}
	return NewCodexCapacityLedger(time.Now, transientQuotaMaxAge)
}

func newCodexSelectorWithCapacity(discover CodexDiscoverer, quota codexQuotaReader, capacity *CodexCapacityLedger) *codexSelector {
	return &codexSelector{discover: discover, quota: quota, capacity: capacity}
}

func (s *codexSelector) Select(ctx context.Context, exclude ...codex.SelectionExclusion) (*codex.CodexAccount, error) {
	if s.discover == nil {
		return nil, fmt.Errorf("legacy Codex account selection unavailable")
	}
	choice, err := s.Choose(ctx, CodexRouteRequirements{RequestedModel: codexRequestedModel(ctx)}, exclude...)
	if err != nil {
		return nil, err
	}
	for _, account := range s.discover() {
		if codexRoutingAccountKey(&account) == choice.AccountKey {
			result := account
			return &result, nil
		}
	}
	return nil, fmt.Errorf("selected Codex account disappeared")
}

// Choose returns one indivisible account, model rewrite, and bucket decision.
func (s *codexSelector) Choose(ctx context.Context, requirements CodexRouteRequirements, exclude ...codex.SelectionExclusion) (RouteChoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	accounts, err := s.routeAccounts(ctx)
	if err != nil {
		return RouteChoice{}, fmt.Errorf("list Codex route inventory: %w", err)
	}
	if len(accounts) == 0 {
		return RouteChoice{}, fmt.Errorf("no codex accounts available")
	}
	excludedAccounts, excludedCandidates := codexExclusionSets(exclude)
	requestedModel := requirements.RequestedModel
	if requestedModel == "" {
		requestedModel = codexRequestedModel(ctx)
	}

	type candidate struct {
		choice    RouteChoice
		state     CapacityState
		remaining int
		resetAt   time.Time
		native    bool
		leases    int
		index     int
	}
	var best *candidate
	hadZero := false
	var zeroReset time.Time
	validTokens := 0

	for i := range accounts {
		account := accounts[i]
		if excludedAccounts[account.key] || excludedCandidates[account.candidateID] || !account.routable {
			continue
		}
		validTokens++
		key := account.key
		if key == "" {
			continue
		}
		s.observeSnapshot(key, &account)

		effectiveModel := requestedModel
		native := codexPlanSupportsModel(account.planType, requestedModel)
		if !native {
			if rewritten, ok := rewriteCodexModelName(requestedModel); ok {
				effectiveModel = rewritten
			} else {
				continue
			}
		}
		buckets := routeBuckets(effectiveModel, requirements.RequiredModels, account.planType)
		state, remaining, resetAt := s.routeCapacity(key, buckets)
		if state == CapacityZero {
			hadZero = true
			if zeroReset.IsZero() || (!resetAt.IsZero() && resetAt.Before(zeroReset)) {
				zeroReset = resetAt
			}
			continue
		}
		current := candidate{
			choice: RouteChoice{
				AccountKey:      key,
				RequestedModel:  requestedModel,
				EffectiveModel:  effectiveModel,
				RequiredBuckets: buckets,
			},
			state:     state,
			remaining: remaining,
			resetAt:   resetAt,
			native:    native,
			leases:    s.capacity.ActiveLeases(key),
			index:     i,
		}
		if best == nil || betterRoute(current, *best) {
			copy := current
			best = &copy
		}
	}
	if best != nil {
		return best.choice, nil
	}
	if hadZero {
		return RouteChoice{}, &CachedUsageLimitError{
			RequestedModel:  requestedModel,
			RequiredBuckets: routeBuckets(requestedModel, requirements.RequiredModels, "pro"),
			ResetAt:         zeroReset,
		}
	}
	if validTokens == 0 {
		return RouteChoice{}, fmt.Errorf("no codex accounts with valid tokens")
	}
	return RouteChoice{}, fmt.Errorf("no codex accounts compatible with requested model")
}

func (s *codexSelector) routeAccounts(ctx context.Context) ([]codexRouteAccount, error) {
	if s.discoverInventory != nil {
		return s.discoverInventory(ctx)
	}
	if s.discover == nil {
		return nil, nil
	}
	discovered := s.discover()
	accounts := make([]codexRouteAccount, 0, len(discovered))
	for index := range discovered {
		account := &discovered[index]
		accounts = append(accounts, codexRouteAccount{
			key:         codexRoutingAccountKey(account),
			candidateID: codexRoutingCandidateID(account),
			accountID:   account.AccountID,
			email:       account.Email,
			planType:    account.PlanType,
			routable:    account.AccessToken != "",
		})
	}
	return accounts, nil
}

func betterRoute(candidate, current struct {
	choice    RouteChoice
	state     CapacityState
	remaining int
	resetAt   time.Time
	native    bool
	leases    int
	index     int
}) bool {
	if candidate.state != current.state {
		return candidate.state > current.state
	}
	if candidate.state == CapacityPositive && candidate.remaining != current.remaining {
		return candidate.remaining > current.remaining
	}
	if candidate.native != current.native {
		return candidate.native
	}
	if candidate.leases != current.leases {
		return candidate.leases < current.leases
	}
	return candidate.index < current.index
}

func (s *codexSelector) routeCapacity(account codex.AccountKey, buckets []CapacityBucket) (CapacityState, int, time.Time) {
	state := CapacityPositive
	remaining := 101
	var resetAt time.Time
	for _, bucket := range buckets {
		view := s.capacity.Capacity(account, bucket)
		if view.State == CapacityZero {
			return CapacityZero, 0, view.ResetAt
		}
		if view.State == CapacityUnknown {
			state = CapacityUnknown
			continue
		}
		if view.RemainingPct < remaining {
			remaining = view.RemainingPct
		}
		if resetAt.IsZero() || (!view.ResetAt.IsZero() && view.ResetAt.Before(resetAt)) {
			resetAt = view.ResetAt
		}
	}
	if remaining == 101 {
		remaining = -1
	}
	return state, remaining, resetAt
}

func (s *codexSelector) observeSnapshot(key codex.AccountKey, account *codexRouteAccount) {
	if s.quota == nil {
		return
	}
	snap, ok := s.snapshot(account)
	if ok {
		s.capacity.ObserveQuotaSnapshot(key, snap)
	}
}

func routeBuckets(effectiveModel string, requiredModels []string, plan string) []CapacityBucket {
	models := append([]string{effectiveModel}, requiredModels...)
	seen := make(map[CapacityBucket]bool, len(models))
	buckets := make([]CapacityBucket, 0, len(models))
	for _, model := range models {
		if !codexPlanSupportsModel(plan, model) {
			if rewritten, ok := rewriteCodexModelName(model); ok {
				model = rewritten
			}
		}
		bucket := CapacityBucketForModel(model)
		if !seen[bucket] {
			seen[bucket] = true
			buckets = append(buckets, bucket)
		}
	}
	return buckets
}

func codexExclusionSets(exclude []codex.SelectionExclusion) (map[codex.AccountKey]bool, map[codex.CandidateID]bool) {
	excludedAccounts := make(map[codex.AccountKey]bool, len(exclude))
	excludedCandidates := make(map[codex.CandidateID]bool, len(exclude))
	for _, exclusion := range exclude {
		if exclusion.AccountKey != "" {
			excludedAccounts[exclusion.AccountKey] = true
		}
		if exclusion.CandidateID != "" {
			excludedCandidates[exclusion.CandidateID] = true
		}
	}
	return excludedAccounts, excludedCandidates
}

func codexRequestedModel(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexModelContextKey{}).(string)
	return model
}

func codexModelRequiresPro(model string) bool {
	normalised := strings.ToLower(ParseModel(model))
	spark := strings.ToLower(codexSparkModel)
	return normalised == spark || strings.HasPrefix(normalised, spark+"-")
}

func codexPlanSupportsModel(plan, model string) bool {
	if !codexModelRequiresPro(model) {
		return true
	}
	return strings.EqualFold(plan, "pro")
}

func (s *codexSelector) snapshot(a *codexRouteAccount) (QuotaSnapshot, bool) {
	snap, ok := s.quota.Snapshot(a.accountID)
	if !ok {
		snap, ok = s.quota.Snapshot(a.email)
	}
	return snap, ok
}

func codexAcctExcluded(a *codex.CodexAccount, excludedAccounts map[codex.AccountKey]bool, excludedCandidates map[codex.CandidateID]bool) bool {
	return excludedAccounts[codexRoutingAccountKey(a)] || excludedCandidates[codexRoutingCandidateID(a)]
}

func codexAcctExcludeKeys(a *codex.CodexAccount) []codex.SelectionExclusion {
	return []codex.SelectionExclusion{{AccountKey: codexRoutingAccountKey(a)}}
}

func codexRoutingAccountKey(a *codex.CodexAccount) codex.AccountKey {
	if a == nil {
		return ""
	}
	if a.AccountKey != "" {
		return a.AccountKey
	}
	if a.RecordKey != "" {
		return codex.AccountKey("compat-record:" + a.RecordKey)
	}
	if a.AccountID != "" {
		return codex.AccountKey("compat-account:" + a.AccountID)
	}
	if a.Email != "" {
		return codex.AccountKey("compat-email:" + strings.ToLower(a.Email))
	}
	return ""
}

func codexRoutingCandidateID(a *codex.CodexAccount) codex.CandidateID {
	if a == nil {
		return ""
	}
	if a.CandidateID != "" {
		return a.CandidateID
	}
	if key := codexRoutingAccountKey(a); key != "" {
		return codex.CandidateID("compat-candidate:" + string(key))
	}
	return ""
}

func codexAcctIdentifier(a *codex.CodexAccount) string {
	if a.AccountID != "" {
		return a.AccountID
	}
	if a.Email != "" {
		return a.Email
	}
	return ""
}

func codexAccountHint(a *codex.CodexAccount) string {
	if a == nil {
		return ""
	}
	return redactedAccountHint("codex", a.AccountID, a.Email, a.RecordKey)
}
