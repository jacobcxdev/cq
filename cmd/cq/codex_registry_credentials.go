package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

var (
	errCodexRegistryCredentialAuthorityUnavailable = errors.New("Codex registry credential authority unavailable")
	errCodexRegistryCredentialInventoryDegraded    = errors.New("Codex registry credential inventory degraded")
	errCodexRegistryCredentialUnavailable          = errors.New("no usable Codex registry credential")
)

// codexRegistryCredentialAuthority exposes only credential operations needed
// by model-catalogue requests: read inventory, resolve exact material, and ask
// the coordinator for an eligible managed refresh. It excludes direct
// persistence and every system-activation capability.
type codexRegistryCredentialAuthority interface {
	List(context.Context) (codexprov.Inventory, error)
	ResolveExact(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error)
	RefreshManagedReference(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error)
}

type codexRegistryControlAdapter struct {
	control *codexprov.CredentialControl
}

func newCodexRegistryControlAdapter(control *codexprov.CredentialControl) codexRegistryCredentialAuthority {
	if control == nil {
		return nil
	}
	return codexRegistryControlAdapter{control: control}
}

func (a codexRegistryControlAdapter) List(ctx context.Context) (codexprov.Inventory, error) {
	inventory, err := a.control.List(ctx)
	if err != nil {
		if contextErr := codexRegistryContextError(err); contextErr != nil {
			return codexprov.Inventory{}, contextErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return codexprov.Inventory{}, ctxErr
		}
		return codexprov.Inventory{}, errCodexRegistryCredentialAuthorityUnavailable
	}
	return inventory, nil
}

func (a codexRegistryControlAdapter) ResolveExact(ctx context.Context, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
	material, err := a.control.ResolveExact(ctx, planned)
	if contextErr := codexRegistryContextError(err); contextErr != nil {
		return codexprov.CredentialMaterial{}, contextErr
	}
	if errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		return codexprov.CredentialMaterial{}, errCodexRegistryCredentialAuthorityUnavailable
	}
	if errors.Is(err, codexprov.ErrCredentialInventoryDegraded) {
		return codexprov.CredentialMaterial{}, errCodexRegistryCredentialInventoryDegraded
	}
	return material, err
}

func (a codexRegistryControlAdapter) RefreshManagedReference(ctx context.Context, ref codexprov.CandidateRef, revision codexprov.Revision) (codexprov.CandidateRef, codexprov.Revision, error) {
	refreshedRef, refreshedRevision, err := a.control.RefreshReference(ctx, ref, revision)
	if contextErr := codexRegistryContextError(err); contextErr != nil {
		return codexprov.CandidateRef{}, "", contextErr
	}
	if errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		return codexprov.CandidateRef{}, "", errCodexRegistryCredentialAuthorityUnavailable
	}
	return refreshedRef, refreshedRevision, err
}

func codexRegistryAccessToken(ctx context.Context, authority codexRegistryCredentialAuthority, now time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if authority == nil {
		return "", errCodexRegistryCredentialAuthorityUnavailable
	}
	inventory, err := authority.List(ctx)
	if err != nil {
		if errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
			return "", errCodexRegistryCredentialAuthorityUnavailable
		}
		return "", fmt.Errorf("%w", errCodexRegistryCredentialAuthorityUnavailable)
	}
	candidates := orderedCodexRegistryCandidates(inventory, now)
	inventoryDegraded := codexRegistryInventoryDegraded(inventory)
	for _, candidate := range candidates {
		if codexRegistryCandidateExpired(candidate.candidate, now) {
			continue
		}
		material, _, err := codexprov.ResolvePlannedCandidate(ctx, authority, authority, codexprov.PlanCandidate(candidate.logical, candidate.candidate))
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
			return "", errCodexRegistryCredentialAuthorityUnavailable
		}
		if errors.Is(err, errCodexRegistryCredentialInventoryDegraded) || errors.Is(err, codexprov.ErrCredentialInventoryDegraded) {
			inventoryDegraded = true
			continue
		}
		if err == nil && material.AccessToken != "" {
			return material.AccessToken, nil
		}
	}
	for _, candidate := range candidates {
		if candidate.candidate.Source != codexprov.SourceManaged || !codexRegistryCandidateExpired(candidate.candidate, now) {
			continue
		}
		ref, revision, err := authority.RefreshManagedReference(ctx, candidate.candidate.Ref, candidate.candidate.Revision)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
			return "", errCodexRegistryCredentialAuthorityUnavailable
		}
		if errors.Is(err, errCodexRegistryCredentialInventoryDegraded) || errors.Is(err, codexprov.ErrCredentialInventoryDegraded) {
			inventoryDegraded = true
			continue
		}
		if err != nil || ref != candidate.candidate.Ref || revision == "" {
			continue
		}
		planned := codexprov.PlanCandidate(candidate.logical, candidate.candidate)
		planned.Ref = ref
		planned.Revision = revision
		material, _, err := codexprov.ResolvePlannedCandidate(ctx, authority, authority, planned)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) {
			return "", errCodexRegistryCredentialAuthorityUnavailable
		}
		if errors.Is(err, errCodexRegistryCredentialInventoryDegraded) || errors.Is(err, codexprov.ErrCredentialInventoryDegraded) {
			inventoryDegraded = true
			continue
		}
		if err == nil && material.AccessToken != "" {
			return material.AccessToken, nil
		}
	}
	if inventoryDegraded {
		return "", errCodexRegistryCredentialInventoryDegraded
	}
	return "", errCodexRegistryCredentialUnavailable
}

func codexRegistryModelsRequest(ctx context.Context, authority codexRegistryCredentialAuthority, client httpClientDoer, now time.Time, req *http.Request) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if authority == nil || client == nil || req == nil {
		return nil, errCodexRegistryCredentialAuthorityUnavailable
	}
	inventory, err := authority.List(ctx)
	if err != nil {
		if contextErr := codexRegistryContextError(err); contextErr != nil {
			return nil, contextErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errCodexRegistryCredentialAuthorityUnavailable
	}
	inventoryDegraded := codexRegistryInventoryDegraded(inventory)

	var rejected *http.Response
	attempt := func(planned codexprov.PlannedCandidate) (*http.Response, codexprov.PlannedCandidate, bool, error) {
		material, resolved, err := codexprov.ResolvePlannedCandidate(ctx, authority, authority, planned)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, resolved, false, ctxErr
		}
		if err != nil {
			return nil, resolved, false, err
		}
		if material.AccessToken == "" {
			return nil, resolved, false, nil
		}

		out := req.Clone(ctx)
		out.Header = req.Header.Clone()
		if out.Header == nil {
			out.Header = make(http.Header)
		}
		out.Header.Set("Authorization", "Bearer "+material.AccessToken)
		closeCodexRegistryResponse(rejected)
		rejected = nil
		response, err := client.Do(out)
		if err != nil {
			closeCodexRegistryResponse(response)
			return nil, resolved, true, err
		}
		if response == nil {
			return nil, resolved, true, errors.New("codex registry HTTP client returned no response")
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			rejected = response
			return nil, resolved, true, nil
		}
		return response, resolved, true, nil
	}

	candidates := groupedCodexRegistryCandidates(inventory, now)
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].candidate.Ref.AccountKey == candidates[start].candidate.Ref.AccountKey {
			end++
		}

		var refreshPlan *codexprov.PlannedCandidate
		queueRefresh := func(planned codexprov.PlannedCandidate, cqAuthored bool) {
			if refreshPlan != nil || planned.Source != codexprov.SourceManaged || !cqAuthored {
				return
			}
			refreshPlan = &planned
		}

		for _, candidate := range candidates[start:end] {
			planned := codexprov.PlanCandidate(candidate.logical, candidate.candidate)
			response, resolved, attempted, err := attempt(planned)
			if ctxErr := ctx.Err(); ctxErr != nil {
				closeCodexRegistryResponse(response)
				closeCodexRegistryResponse(rejected)
				return nil, ctxErr
			}
			if contextErr := codexRegistryContextError(err); contextErr != nil {
				closeCodexRegistryResponse(response)
				closeCodexRegistryResponse(rejected)
				return nil, contextErr
			}
			if codexRegistryAuthorityUnavailable(err) {
				closeCodexRegistryResponse(response)
				closeCodexRegistryResponse(rejected)
				return nil, errCodexRegistryCredentialAuthorityUnavailable
			}
			if codexRegistryInventoryDegradedError(err) {
				inventoryDegraded = true
				continue
			}
			if err != nil {
				if attempted {
					closeCodexRegistryResponse(response)
					closeCodexRegistryResponse(rejected)
					return nil, err
				}
				continue
			}
			if response != nil {
				return response, nil
			}
			if resolved.Source == codexprov.SourceManaged && (!attempted || rejected != nil) {
				queueRefresh(resolved, candidate.candidate.CQAuthored)
			}
		}

		if refreshPlan != nil {
			planned := *refreshPlan
			ref, revision, err := authority.RefreshManagedReference(ctx, planned.Ref, planned.Revision)
			if ctxErr := ctx.Err(); ctxErr != nil {
				closeCodexRegistryResponse(rejected)
				return nil, ctxErr
			}
			if contextErr := codexRegistryContextError(err); contextErr != nil {
				closeCodexRegistryResponse(rejected)
				return nil, contextErr
			}
			if codexRegistryAuthorityUnavailable(err) {
				closeCodexRegistryResponse(rejected)
				return nil, errCodexRegistryCredentialAuthorityUnavailable
			}
			if err == nil && ref == planned.Ref && revision != "" && revision != planned.Revision {
				refreshed := planned
				refreshed.Ref = ref
				refreshed.Revision = revision
				response, _, attempted, err := attempt(refreshed)
				if ctxErr := ctx.Err(); ctxErr != nil {
					closeCodexRegistryResponse(response)
					closeCodexRegistryResponse(rejected)
					return nil, ctxErr
				}
				if contextErr := codexRegistryContextError(err); contextErr != nil {
					closeCodexRegistryResponse(response)
					closeCodexRegistryResponse(rejected)
					return nil, contextErr
				}
				if codexRegistryAuthorityUnavailable(err) {
					closeCodexRegistryResponse(response)
					closeCodexRegistryResponse(rejected)
					return nil, errCodexRegistryCredentialAuthorityUnavailable
				}
				if codexRegistryInventoryDegradedError(err) {
					inventoryDegraded = true
					continue
				}
				if err != nil && attempted {
					closeCodexRegistryResponse(response)
					closeCodexRegistryResponse(rejected)
					return nil, err
				}
				if err == nil && response != nil {
					return response, nil
				}
			}
		}

		start = end
	}
	if inventoryDegraded {
		closeCodexRegistryResponse(rejected)
		return nil, errCodexRegistryCredentialInventoryDegraded
	}
	if rejected != nil {
		return rejected, nil
	}
	return nil, errCodexRegistryCredentialUnavailable
}

func codexRegistryInventoryDegraded(inventory codexprov.Inventory) bool {
	for _, source := range inventory.ExternalSources {
		if source.ErrorCode != "" && !source.OptionalAbsent {
			return true
		}
	}
	return false
}

func codexRegistryAuthorityUnavailable(err error) bool {
	return errors.Is(err, errCodexRegistryCredentialAuthorityUnavailable) || errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable)
}

func codexRegistryInventoryDegradedError(err error) bool {
	return errors.Is(err, errCodexRegistryCredentialInventoryDegraded) || errors.Is(err, codexprov.ErrCredentialInventoryDegraded)
}

func codexRegistryContextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func groupedCodexRegistryCandidates(inventory codexprov.Inventory, now time.Time) []codexRegistryCandidate {
	ordered := orderedCodexRegistryCandidates(inventory, now)
	grouped := make([]codexRegistryCandidate, 0, len(ordered))
	seen := make(map[codexprov.AccountKey]bool, len(inventory.Accounts))
	for _, first := range ordered {
		accountKey := first.candidate.Ref.AccountKey
		if seen[accountKey] {
			continue
		}
		seen[accountKey] = true
		for _, candidate := range ordered {
			if candidate.candidate.Ref.AccountKey == accountKey {
				grouped = append(grouped, candidate)
			}
		}
	}
	return grouped
}

type httpClientDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func closeCodexRegistryResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

type codexRegistryCandidate struct {
	logical   codexprov.LogicalAccount
	candidate codexprov.CredentialCandidate
}

func orderedCodexRegistryCandidates(inventory codexprov.Inventory, now time.Time) []codexRegistryCandidate {
	var candidates []codexRegistryCandidate
	for _, logical := range inventory.Accounts {
		if !logical.Routable {
			continue
		}
		for _, candidate := range logical.Candidates {
			if !candidate.Routable || candidate.DispatchBlocked || candidate.Ref.AccountKey == "" || candidate.Ref.CandidateID == "" || candidate.Revision == "" {
				continue
			}
			if candidate.Source < codexprov.SourceSystem || candidate.Source > codexprov.SourceExternal {
				continue
			}
			candidates = append(candidates, codexRegistryCandidate{logical: logical, candidate: candidate})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].candidate, candidates[j].candidate
		classA, classB := codexRegistryExpiryClass(a.AccessExpiresAt, now), codexRegistryExpiryClass(b.AccessExpiresAt, now)
		if classA != classB {
			return classA < classB
		}
		if a.CQAuthored != b.CQAuthored {
			return a.CQAuthored
		}
		if !a.AccessExpiresAt.Equal(b.AccessExpiresAt) {
			return a.AccessExpiresAt.After(b.AccessExpiresAt)
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Ref.AccountKey != b.Ref.AccountKey {
			return a.Ref.AccountKey < b.Ref.AccountKey
		}
		if a.Ref.CandidateID != b.Ref.CandidateID {
			return a.Ref.CandidateID < b.Ref.CandidateID
		}
		return a.Revision < b.Revision
	})
	return candidates
}

func codexRegistryCandidateExpired(candidate codexprov.CredentialCandidate, now time.Time) bool {
	return !candidate.AccessExpiresAt.IsZero() && !candidate.AccessExpiresAt.After(now)
}

func codexRegistryExpiryClass(expiresAt, now time.Time) int {
	if expiresAt.IsZero() {
		return 1
	}
	if expiresAt.After(now) {
		return 0
	}
	return 2
}
