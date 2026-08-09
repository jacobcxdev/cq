package proxy

import (
	"context"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexFrozenDispatchInput contains one caller-owned routing snapshot.
type CodexFrozenDispatchInput struct {
	Inventory          codex.Inventory
	Capacity           *CodexCapacityLedger
	Requirements       CodexRouteRequirements
	Provisional        map[codex.AccountKey]int
	AffinityAccountKey codex.AccountKey
	DefaultAccountKey  codex.AccountKey
	BoundAccountKey    codex.AccountKey
	AcceptedRevision   codex.Revision
	Now                time.Time
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
	accounts []CodexFrozenDispatchAccount
	status   CodexRoutePlanStatus
}

// CodexFrozenDispatchAccount binds one frozen route choice to exact credential attempts.
type CodexFrozenDispatchAccount struct {
	choice         RouteChoice
	attempts       []CandidateAttempt
	refreshAttempt *CandidateAttempt
	isDefault      bool
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
	)
	if err != nil {
		if cancelErr := codexFrozenDispatchContextError(ctx); cancelErr != nil {
			return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, cancelErr
		}
		return CodexFrozenDispatchPlan{}, err
	}
	policy, err := BuildCodexRoutePlan(ctx, candidates, CodexRoutePolicyHints{
		AffinityAccountKey: input.AffinityAccountKey,
		DefaultAccountKey:  input.DefaultAccountKey,
		BoundAccountKey:    input.BoundAccountKey,
	})
	plan := CodexFrozenDispatchPlan{status: policy.Status()}
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
		accounts = append(accounts, account)
	}
	if err := codexFrozenDispatchContextError(ctx); err != nil {
		return CodexFrozenDispatchPlan{status: CodexRoutePlanCanceled}, err
	}
	plan.accounts = accounts
	return plan, nil
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
		if !candidate.Routable {
			continue
		}
		if err := validateCodexFrozenDispatchCandidate(logical, candidate); err != nil {
			return CodexFrozenDispatchAccount{}, err
		}
		attempt := candidateAttemptFromPlan(codex.PlanCandidate(logical, candidate), len(account.attempts)+1)
		account.attempts = append(account.attempts, attempt)
		if account.refreshAttempt == nil && candidate.Source == codex.SourceManaged && candidate.CQAuthored {
			copy := attempt
			account.refreshAttempt = &copy
		}
	}
	if len(account.attempts) == 0 {
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
	accounts := make([]CodexFrozenDispatchAccount, len(p.accounts))
	for i := range p.accounts {
		accounts[i] = cloneCodexFrozenDispatchAccount(p.accounts[i])
	}
	return accounts
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
