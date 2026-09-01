package proxy

import (
	"context"
	"sort"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexFrozenDispatchInput contains one caller-owned routing snapshot.
type CodexFrozenDispatchInput struct {
	Inventory                   codex.Inventory
	Capacity                    *CodexCapacityLedger
	Requirements                CodexRouteRequirements
	Provisional                 map[codex.AccountKey]int
	AccountValues               map[codex.AccountKey]PoolValue
	AffinityAccountKey          codex.AccountKey
	AffinityEffectiveModel      string
	DefaultAccountKey           codex.AccountKey
	BoundAccountKey             codex.AccountKey
	UnavailableAccountKeys      []codex.AccountKey
	ProbeUnavailableAccountKeys []codex.AccountKey
	ProbeUnavailableWhenAll     bool
	AcceptedRevision            codex.Revision
	Now                         time.Time
}

// CodexFrozenDispatchErrorCode classifies credential-free snapshot failures.
type CodexFrozenDispatchErrorCode string

const (
	CodexFrozenDispatchAccountDisappeared        CodexFrozenDispatchErrorCode = "account_disappeared"
	CodexFrozenDispatchAccountUnroutable         CodexFrozenDispatchErrorCode = "account_unroutable"
	CodexFrozenDispatchCandidateAccountMismatch  CodexFrozenDispatchErrorCode = "candidate_account_mismatch"
	CodexFrozenDispatchCandidateReferenceMissing CodexFrozenDispatchErrorCode = "candidate_reference_missing"
	CodexFrozenDispatchCandidateRevisionMissing  CodexFrozenDispatchErrorCode = "candidate_revision_missing"
	CodexFrozenDispatchCandidateSourceInvalid    CodexFrozenDispatchErrorCode = "candidate_source_invalid"
	CodexFrozenDispatchNoDispatchableCandidate   CodexFrozenDispatchErrorCode = "no_dispatchable_candidate"
	CodexFrozenDispatchStrongIdentityInvalid     CodexFrozenDispatchErrorCode = "strong_identity_invalid"
)

// CodexFrozenDispatchError deliberately carries no account or credential metadata.
type CodexFrozenDispatchError struct {
	Code CodexFrozenDispatchErrorCode
}

func (e *CodexFrozenDispatchError) Error() string {
	return "Codex frozen dispatch failed: " + string(e.Code)
}

// CodexFrozenDispatchPlan is a memory-only sequence of per-account dispatches.
type CodexFrozenDispatchPlan struct {
	accounts                          []CodexFrozenDispatchAccount
	accountUnavailableFallbacks       []CodexFrozenDispatchAccount
	accountUnavailableResetCandidates []codex.AccountKey
	accountUnavailablePortable        bool
	policyCandidates                  []CodexRoutePolicyCandidate
	status                            CodexRoutePlanStatus
	probe                             codexInstalledHTTPDispatchFacts
}

// CodexFrozenDispatchAccount binds one frozen route choice to exact credential attempts.
type CodexFrozenDispatchAccount struct {
	choice         RouteChoice
	value          PoolValue
	attempts       []CandidateAttempt
	refreshAttempt *CandidateAttempt
	isDefault      bool
	decision       codexRuntimeDecision
}

func BuildCodexFrozenDispatchPlan(ctx context.Context, input CodexFrozenDispatchInput) (CodexFrozenDispatchPlan, error) {
	if err := codexFrozenDispatchContextError(ctx); err != nil {
		return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, err
	}
	candidates, err := ProjectCodexRoutePolicyCandidates(
		input.Inventory,
		input.Capacity,
		input.Requirements,
		input.Provisional,
		input.Now,
	)
	if err != nil {
		if cancelErr := codexFrozenDispatchContextError(ctx); cancelErr != nil {
			return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, cancelErr
		}
		return CodexFrozenDispatchPlan{}, err
	}
	for index := range candidates {
		candidates[index].Value = input.AccountValues[candidates[index].Choice.AccountKey]
	}
	candidates, unavailableProbe := filterCodexFrozenDispatchUnavailableCandidates(
		candidates,
		input.UnavailableAccountKeys,
		input.ProbeUnavailableAccountKeys,
		input.ProbeUnavailableWhenAll && input.BoundAccountKey == "",
		input.DefaultAccountKey,
	)
	policyBoundAccountKey := input.BoundAccountKey
	if policyBoundAccountKey == "" {
		policyBoundAccountKey = unavailableProbe
	}
	policy, err := BuildCodexRoutePlan(ctx, candidates, CodexRoutePolicyHints{
		AffinityAccountKey:     input.AffinityAccountKey,
		AffinityEffectiveModel: input.AffinityEffectiveModel,
		DefaultAccountKey:      input.DefaultAccountKey,
		BoundAccountKey:        policyBoundAccountKey,
	})
	plan := CodexFrozenDispatchPlan{status: policy.Status(), policyCandidates: candidates}
	if err != nil {
		return plan, err
	}
	if input.BoundAccountKey != "" {
		if err := codexFrozenDispatchContextError(ctx); err != nil {
			return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, err
		}
		logical, ok := frozenDispatchLogicalAccount(input.Inventory, input.BoundAccountKey)
		if !ok {
			return plan, &CodexFrozenDispatchError{Code: CodexFrozenDispatchAccountDisappeared}
		}
		if logical.Identity.AccountID == "" || logical.Identity.UserID == "" {
			return plan, &CodexFrozenDispatchError{Code: CodexFrozenDispatchStrongIdentityInvalid}
		}
		if !logical.Routable || logical.Unstable {
			return plan, &CodexFrozenDispatchError{Code: CodexFrozenDispatchAccountUnroutable}
		}
		if policy.Status() != CodexRoutePlanReady {
			if policy.Status() == CodexRoutePlanBoundUnroutable {
				for _, candidate := range candidates {
					if candidate.Choice.AccountKey != input.BoundAccountKey {
						continue
					}
					_, freezeErr := freezeCodexDispatchAccount(
						logical,
						candidate.Choice,
						input.AcceptedRevision,
						input.Now,
						candidate.Choice.AccountKey == policy.DefaultAccountKey(),
					)
					if cancelErr := codexFrozenDispatchContextError(ctx); cancelErr != nil {
						return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, cancelErr
					}
					if freezeErr != nil {
						return plan, freezeErr
					}
					break
				}
			}
			return plan, policy.TerminalError()
		}
	}
	choices := policy.Choices()
	accounts := make([]CodexFrozenDispatchAccount, 0, len(choices))
	for _, choice := range choices {
		if err := codexFrozenDispatchContextError(ctx); err != nil {
			return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, err
		}
		logical, ok := frozenDispatchLogicalAccount(input.Inventory, choice.AccountKey)
		if !ok {
			return plan, &CodexFrozenDispatchError{Code: CodexFrozenDispatchAccountDisappeared}
		}
		account, err := freezeCodexDispatchAccount(
			logical,
			choice,
			input.AcceptedRevision,
			input.Now,
			choice.AccountKey == policy.DefaultAccountKey(),
		)
		if cancelErr := codexFrozenDispatchContextError(ctx); cancelErr != nil {
			return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, cancelErr
		}
		if err != nil {
			return plan, err
		}
		account.value = codexFrozenDispatchCandidateValue(candidates, choice.AccountKey)
		account.decision = codexFrozenDispatchDecision(input, candidates, account)
		accounts = append(accounts, account)
	}
	if err := codexFrozenDispatchContextError(ctx); err != nil {
		return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, err
	}
	plan.accounts = accounts
	plan.probe = codexInstalledHTTPDispatchFactsForPolicy(candidates, policy, CodexRoutePolicyHints{
		AffinityAccountKey: input.AffinityAccountKey,
		DefaultAccountKey:  input.DefaultAccountKey,
		BoundAccountKey:    input.BoundAccountKey,
	})
	return plan, nil
}

func codexFrozenDispatchDecision(input CodexFrozenDispatchInput, candidates []CodexRoutePolicyCandidate, account CodexFrozenDispatchAccount) codexRuntimeDecision {
	if input.BoundAccountKey != "" {
		return codexRuntimeDecisionNone
	}
	ordinaryEligible := codexFrozenDispatchOrdinarilyEligible(candidates, account.choice)
	if ordinaryEligible && input.AffinityAccountKey != "" && account.choice.AccountKey == input.AffinityAccountKey && (input.AffinityEffectiveModel == "" || codexRoutePolicySameModel(account.choice.EffectiveModel, input.AffinityEffectiveModel)) {
		return codexRuntimeDecisionAffinityReuse
	}
	if account.isDefault && !ordinaryEligible {
		return codexRuntimeDecisionTerminalDefault
	}
	return codexRuntimeDecisionFairnessSelect
}

func codexFrozenDispatchOrdinarilyEligible(candidates []CodexRoutePolicyCandidate, choice RouteChoice) bool {
	for _, candidate := range candidates {
		if candidate.Choice.AccountKey != choice.AccountKey || !candidate.Compatible || !candidate.Routable ||
			codexRoutePolicyCapacity(candidate).State == CapacityZero || !codexRoutePolicyCompatibleWithFrozen(candidate.Choice, choice) {
			continue
		}
		return true
	}
	return false
}

func codexFrozenDispatchCandidateValue(candidates []CodexRoutePolicyCandidate, account codex.AccountKey) PoolValue {
	for _, candidate := range candidates {
		if candidate.Choice.AccountKey == account {
			return candidate.Value
		}
	}
	return 0
}

func codexInstalledHTTPDispatchFactsForPolicy(candidates []CodexRoutePolicyCandidate, policy CodexRoutePlan, hints CodexRoutePolicyHints) codexInstalledHTTPDispatchFacts {
	choices := policy.Choices()
	facts := codexInstalledHTTPDispatchFacts{
		selection:  codexInstalledHTTPSelectionOrdinary,
		routeCount: uint32(len(choices)),
	}
	facts.terminalDefaultOrdinal = policy.terminalDefaultOrdinal
	if len(choices) != 0 {
		facts.selectedValue = codexFrozenDispatchCandidateValue(candidates, choices[0].AccountKey)
	}
	ordinaryCount := len(choices)
	if facts.terminalDefaultOrdinal != 0 {
		ordinaryCount--
	}
	if ordinaryCount > 1 {
		facts.eligibleCompetitors = uint32(ordinaryCount - 1)
	}
	if hints.BoundAccountKey != "" {
		facts.selection = codexInstalledHTTPSelectionBound
		return facts
	}
	if ordinaryCount == 0 {
		return facts
	}
	if hints.AffinityAccountKey == "" {
		facts.fairnessSelect = true
		return facts
	}
	if choices[0].AccountKey != hints.AffinityAccountKey {
		facts.fairnessSelect = true
		facts.affinityUnavailable = true
		if facts.terminalDefaultOrdinal != 1 {
			facts.selection = codexInstalledHTTPSelectionDeterministicFallback
		}
		return facts
	}
	facts.affinityReuse = true
	natural, err := BuildCodexRoutePlan(context.Background(), candidates, CodexRoutePolicyHints{
		DefaultAccountKey: hints.DefaultAccountKey,
	})
	if err == nil {
		naturalChoices := natural.Choices()
		if len(naturalChoices) > 0 && naturalChoices[0].AccountKey != hints.AffinityAccountKey {
			facts.selection = codexInstalledHTTPSelectionWarmAffinity
			facts.naturalWinnerDisplaced = true
		}
	}
	return facts
}

func codexFrozenDispatchContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return &CodexRoutePolicyError{Status: CodexRoutePlanCanceled, cause: err}
	}
	return nil
}

func validateCodexFrozenDispatchCandidate(logical codex.LogicalAccount, candidate codex.CredentialCandidate) error {
	switch {
	case candidate.Ref.AccountKey != logical.Key:
		return &CodexFrozenDispatchError{Code: CodexFrozenDispatchCandidateAccountMismatch}
	case candidate.Ref.CandidateID == "":
		return &CodexFrozenDispatchError{Code: CodexFrozenDispatchCandidateReferenceMissing}
	case candidate.Revision == "":
		return &CodexFrozenDispatchError{Code: CodexFrozenDispatchCandidateRevisionMissing}
	case candidate.Source < codex.SourceSystem || candidate.Source > codex.SourceExternal:
		return &CodexFrozenDispatchError{Code: CodexFrozenDispatchCandidateSourceInvalid}
	case candidate.CQAuthored && candidate.Source != codex.SourceManaged:
		return &CodexFrozenDispatchError{Code: CodexFrozenDispatchCandidateSourceInvalid}
	default:
		return nil
	}
}

func freezeCodexDispatchAccount(logical codex.LogicalAccount, choice RouteChoice, accepted codex.Revision, now time.Time, isDefault bool) (CodexFrozenDispatchAccount, error) {
	if logical.Key != choice.AccountKey {
		return CodexFrozenDispatchAccount{}, &CodexFrozenDispatchError{Code: CodexFrozenDispatchAccountDisappeared}
	}
	if logical.Identity.AccountID == "" || logical.Identity.UserID == "" {
		return CodexFrozenDispatchAccount{}, &CodexFrozenDispatchError{Code: CodexFrozenDispatchStrongIdentityInvalid}
	}
	if !logical.Routable || logical.Unstable {
		return CodexFrozenDispatchAccount{}, &CodexFrozenDispatchError{Code: CodexFrozenDispatchAccountUnroutable}
	}
	account := CodexFrozenDispatchAccount{choice: cloneRoutePolicyChoice(choice), isDefault: isDefault}
	for _, candidate := range codex.ResolveCandidate(logical, accepted, now) {
		availability := codex.CandidateAvailabilityAt(candidate, now)
		if availability == codex.CandidateUnavailable {
			continue
		}
		if err := validateCodexFrozenDispatchCandidate(logical, candidate); err != nil {
			return CodexFrozenDispatchAccount{}, err
		}
		attempt := candidateAttemptFromPlan(codex.PlanCandidate(logical, candidate), len(account.attempts)+1)
		if availability == codex.CandidateRefreshRequired {
			if account.refreshAttempt == nil {
				copy := attempt
				account.refreshAttempt = &copy
			}
			continue
		}
		account.attempts = append(account.attempts, attempt)
		if account.refreshAttempt == nil && candidate.Source == codex.SourceManaged && candidate.CQAuthored && candidate.RefreshEligible {
			copy := attempt
			account.refreshAttempt = &copy
		}
	}
	if len(account.attempts) == 0 && account.refreshAttempt == nil {
		return CodexFrozenDispatchAccount{}, &CodexFrozenDispatchError{Code: CodexFrozenDispatchNoDispatchableCandidate}
	}
	return account, nil
}

func frozenDispatchLogicalAccount(inventory codex.Inventory, key codex.AccountKey) (codex.LogicalAccount, bool) {
	for _, logical := range inventory.Accounts {
		if logical.Key == key {
			return logical, true
		}
	}
	return codex.LogicalAccount{}, false
}

func (p CodexFrozenDispatchPlan) Accounts() []CodexFrozenDispatchAccount {
	return cloneCodexFrozenDispatchAccounts(p.accounts)
}

// AccountUnavailableFallbacks returns portable cross-account routes admitted
// only after the selected account becomes unavailable. They are deliberately
// absent from Accounts.
func (p CodexFrozenDispatchPlan) AccountUnavailableFallbacks() []CodexFrozenDispatchAccount {
	return cloneCodexFrozenDispatchAccounts(p.accountUnavailableFallbacks)
}

// AccountUnavailableResetCandidates returns detached account identities that
// remain eligible for a new full-create route after the selected account
// becomes unavailable. It grants no credential or replay authority.
func (p CodexFrozenDispatchPlan) AccountUnavailableResetCandidates() []codex.AccountKey {
	return append([]codex.AccountKey(nil), p.accountUnavailableResetCandidates...)
}

func (p CodexFrozenDispatchPlan) withAccountUnavailableResetCandidates(candidates CodexFrozenDispatchPlan, primary RouteChoice) CodexFrozenDispatchPlan {
	seen := map[codex.AccountKey]struct{}{primary.AccountKey: {}}
	for _, account := range candidates.accounts {
		choice := account.Choice()
		if _, duplicate := seen[choice.AccountKey]; duplicate ||
			!codexFrozenDispatchOrdinarilyEligible(candidates.policyCandidates, choice) ||
			!codexRoutePolicyCompatibleWithFrozen(choice, primary) {
			continue
		}
		seen[choice.AccountKey] = struct{}{}
		p.accountUnavailableResetCandidates = append(p.accountUnavailableResetCandidates, choice.AccountKey)
	}
	return p
}

func (p CodexFrozenDispatchPlan) withAccountUnavailableFallbacks(fallbacks CodexFrozenDispatchPlan, primary RouteChoice) CodexFrozenDispatchPlan {
	p.accountUnavailablePortable = true
	seen := make(map[codex.AccountKey]struct{}, len(p.accounts))
	for _, account := range p.accounts {
		seen[account.choice.AccountKey] = struct{}{}
	}
	seen[primary.AccountKey] = struct{}{}
	for index, account := range fallbacks.accounts {
		choice := account.Choice()
		if _, duplicate := seen[choice.AccountKey]; duplicate ||
			!codexFrozenDispatchOrdinarilyEligible(fallbacks.policyCandidates, choice) ||
			!codexRoutePolicyCompatibleWithFrozen(choice, primary) {
			continue
		}
		seen[choice.AccountKey] = struct{}{}
		p.accountUnavailableFallbacks = append(p.accountUnavailableFallbacks, cloneCodexFrozenDispatchAccount(account))
		if fallbacks.probe.terminalDefaultOrdinal == uint32(index+1) {
			p.probe.terminalDefaultOrdinal = uint32(len(p.accounts) + len(p.accountUnavailableFallbacks))
		}
	}
	p.probe.routeCount = uint32(len(p.accounts) + len(p.accountUnavailableFallbacks))
	return p
}

func (p CodexFrozenDispatchPlan) Status() CodexRoutePlanStatus {
	return p.status
}

func (p CodexFrozenDispatchPlan) TerminalError() error {
	return (CodexRoutePlan{status: p.status}).TerminalError()
}

func (a CodexFrozenDispatchAccount) Choice() RouteChoice {
	return cloneRoutePolicyChoice(a.choice)
}

func (a CodexFrozenDispatchAccount) Value() PoolValue {
	return a.value
}

func (a CodexFrozenDispatchAccount) Attempts() []CandidateAttempt {
	return append([]CandidateAttempt(nil), a.attempts...)
}

func (a CodexFrozenDispatchAccount) RefreshAttempt() (CandidateAttempt, bool) {
	if a.refreshAttempt == nil {
		return CandidateAttempt{}, false
	}
	return *a.refreshAttempt, true
}

func (a CodexFrozenDispatchAccount) IsDefault() bool {
	return a.isDefault
}

func cloneCodexFrozenDispatchAccount(account CodexFrozenDispatchAccount) CodexFrozenDispatchAccount {
	account.choice = cloneRoutePolicyChoice(account.choice)
	account.attempts = append([]CandidateAttempt(nil), account.attempts...)
	if account.refreshAttempt != nil {
		copy := *account.refreshAttempt
		account.refreshAttempt = &copy
	}
	return account
}

func cloneCodexFrozenDispatchAccounts(source []CodexFrozenDispatchAccount) []CodexFrozenDispatchAccount {
	accounts := make([]CodexFrozenDispatchAccount, len(source))
	for index := range source {
		accounts[index] = cloneCodexFrozenDispatchAccount(source[index])
	}
	return accounts
}

func filterCodexFrozenDispatchUnavailableCandidates(candidates []CodexRoutePolicyCandidate, unavailable, probeAccounts []codex.AccountKey, probeWhenAll bool, defaultAccountKey codex.AccountKey) ([]CodexRoutePolicyCandidate, codex.AccountKey) {
	if len(unavailable) == 0 {
		return candidates, ""
	}
	excluded := make(map[codex.AccountKey]struct{}, len(unavailable))
	for _, account := range unavailable {
		excluded[account] = struct{}{}
	}
	filtered := make([]CodexRoutePolicyCandidate, 0, len(candidates))
	ordinaryAvailable := false
	for _, candidate := range candidates {
		if _, ok := excluded[candidate.Choice.AccountKey]; !ok {
			filtered = append(filtered, candidate)
			ordinaryAvailable = ordinaryAvailable || codexFrozenDispatchPolicyCandidateEligible(candidate)
		}
	}
	if ordinaryAvailable || !probeWhenAll {
		return filtered, ""
	}
	probes := make(map[codex.AccountKey]struct{}, len(probeAccounts))
	for _, account := range probeAccounts {
		probes[account] = struct{}{}
	}
	probeCandidates := make([]CodexRoutePolicyCandidate, 0, len(unavailable))
	seen := make(map[codex.AccountKey]struct{}, len(unavailable))
	for _, candidate := range candidates {
		if _, probe := probes[candidate.Choice.AccountKey]; !probe ||
			!candidate.Compatible || !candidate.Routable || candidate.Choice.AccountKey == "" {
			continue
		}
		if _, duplicate := seen[candidate.Choice.AccountKey]; duplicate {
			continue
		}
		seen[candidate.Choice.AccountKey] = struct{}{}
		probeCandidates = append(probeCandidates, candidate)
	}
	if len(probeCandidates) == 0 {
		return filtered, ""
	}
	sort.SliceStable(probeCandidates, func(i, j int) bool {
		leftEligible := codexFrozenDispatchPolicyCandidateEligible(probeCandidates[i])
		rightEligible := codexFrozenDispatchPolicyCandidateEligible(probeCandidates[j])
		if leftEligible != rightEligible {
			return leftEligible
		}
		leftDefault := defaultAccountKey != "" && probeCandidates[i].Choice.AccountKey == defaultAccountKey
		rightDefault := defaultAccountKey != "" && probeCandidates[j].Choice.AccountKey == defaultAccountKey
		if leftDefault != rightDefault {
			return leftDefault
		}
		return codexRoutePolicyCandidateLess(probeCandidates[i], probeCandidates[j], "", "")
	})
	probeKey := probeCandidates[0].Choice.AccountKey
	selected := make([]CodexRoutePolicyCandidate, 0, 1)
	for _, candidate := range candidates {
		if candidate.Choice.AccountKey == probeKey {
			selected = append(selected, candidate)
		}
	}
	return selected, probeKey
}

func codexFrozenDispatchPolicyCandidateEligible(candidate CodexRoutePolicyCandidate) bool {
	return candidate.Choice.AccountKey != "" && candidate.Compatible && candidate.Routable &&
		codexRoutePolicyCapacity(candidate).State != CapacityZero
}
