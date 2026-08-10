package codex

import (
	"context"
	"errors"
	"fmt"

	"github.com/jacobcxdev/cq/internal/auth"
)

var ErrCredentialIdentityMismatch = errors.New("resolved credential identity mismatch")

// PlannedCandidate is the complete, secret-free credential generation selected
// before dispatch.
type PlannedCandidate struct {
	Ref      CandidateRef
	Revision Revision
	Source   CredentialSource
	Identity AccountIdentity
}

// ExactSecretResolver resolves only the credential generation named by a plan.
type ExactSecretResolver interface {
	ResolveExact(context.Context, PlannedCandidate) (CredentialMaterial, error)
}

// PlanCandidate binds a candidate to its logical account's strong identity.
func PlanCandidate(logical LogicalAccount, candidate CredentialCandidate) PlannedCandidate {
	return PlannedCandidate{
		Ref: candidate.Ref, Revision: candidate.Revision, Source: candidate.Source,
		Identity: AccountIdentity{AccountID: logical.Identity.AccountID, UserID: logical.Identity.UserID},
	}
}

// ResolvePlannedCandidate resolves a planned generation and, on typed stale
// state only, relists once and retries the same strong-identity candidate.
func ResolvePlannedCandidate(ctx context.Context, inventory CredentialInventory, resolver ExactSecretResolver, planned PlannedCandidate) (CredentialMaterial, PlannedCandidate, error) {
	if err := ctx.Err(); err != nil {
		return CredentialMaterial{}, planned, err
	}
	if resolver == nil {
		return CredentialMaterial{}, planned, errors.New("credential resolver unavailable")
	}
	if !validPlannedCandidate(planned) {
		return CredentialMaterial{}, planned, errors.New("invalid credential resolution plan")
	}
	material, err := resolver.ResolveExact(ctx, planned)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CredentialMaterial{}, planned, ctxErr
	}
	if err == nil {
		if !credentialMaterialMatchesIdentity(material, planned.Identity) {
			return CredentialMaterial{}, planned, ErrCredentialIdentityMismatch
		}
		return material, planned, nil
	}
	if !errors.Is(err, ErrStaleRevision) {
		return CredentialMaterial{}, planned, err
	}
	if inventory == nil {
		return CredentialMaterial{}, planned, ErrStaleRevision
	}
	refreshed, err := inventory.List(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CredentialMaterial{}, planned, ctxErr
	}
	if err != nil {
		return CredentialMaterial{}, planned, fmt.Errorf("refresh credential inventory: %w", err)
	}
	replanned, err := replanSameCandidate(planned, refreshed)
	if err != nil {
		if inventoryHasDegradedExternalSource(refreshed) {
			return CredentialMaterial{}, planned, ErrCredentialInventoryDegraded
		}
		return CredentialMaterial{}, planned, err
	}
	material, err = resolver.ResolveExact(ctx, replanned)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CredentialMaterial{}, replanned, ctxErr
	}
	if err != nil {
		return CredentialMaterial{}, replanned, err
	}
	if !credentialMaterialMatchesIdentity(material, replanned.Identity) {
		return CredentialMaterial{}, replanned, ErrCredentialIdentityMismatch
	}
	return material, replanned, nil
}

func validPlannedCandidate(planned PlannedCandidate) bool {
	return planned.Ref.AccountKey != "" && planned.Ref.CandidateID != "" && planned.Revision != "" &&
		planned.Source >= SourceSystem && planned.Source <= SourceExternal && completeStrongIdentity(planned.Identity)
}

func replanSameCandidate(planned PlannedCandidate, inventory Inventory) (PlannedCandidate, error) {
	for _, logical := range inventory.Accounts {
		if logical.Key != planned.Ref.AccountKey || !logical.Routable || !sameStrongIdentity(logical.Identity, planned.Identity) {
			continue
		}
		for _, candidate := range logical.Candidates {
			if candidate.Ref.AccountKey != planned.Ref.AccountKey || candidate.Ref.CandidateID != planned.Ref.CandidateID {
				continue
			}
			if !candidate.Routable || candidate.DispatchBlocked || candidate.Source != planned.Source || candidate.Revision == "" || candidate.Revision == planned.Revision {
				return PlannedCandidate{}, ErrStaleRevision
			}
			return PlanCandidate(logical, candidate), nil
		}
	}
	return PlannedCandidate{}, ErrStaleRevision
}

// ResolveExact returns material only while every planned generation field still
// matches the coordinator's current inventory.
func (c *CredentialCoordinator) ResolveExact(ctx context.Context, planned PlannedCandidate) (CredentialMaterial, error) {
	if err := ctx.Err(); err != nil {
		return CredentialMaterial{}, err
	}
	if !validPlannedCandidate(planned) {
		return CredentialMaterial{}, errors.New("invalid credential resolution plan")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	inventory, err := discoverAuthoritativeInventoryWithSources(ctx, c.Store.FS, c.ExternalSources...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CredentialMaterial{}, ctxErr
	}
	if err != nil {
		return CredentialMaterial{}, ErrCredentialAuthorityUnavailable
	}
	c.classifyExternalSourceState(&inventory, false)
	for _, logical := range inventory.Accounts {
		if logical.Key != planned.Ref.AccountKey || !logical.Routable || !sameStrongIdentity(logical.Identity, planned.Identity) {
			continue
		}
		for _, candidate := range logical.Candidates {
			if candidate.Ref.AccountKey != planned.Ref.AccountKey || candidate.Ref.CandidateID != planned.Ref.CandidateID {
				continue
			}
			if !candidate.Routable || candidate.DispatchBlocked || candidate.Source != planned.Source || candidate.Revision != planned.Revision {
				return CredentialMaterial{}, ErrStaleRevision
			}
			switch candidate.Source {
			case SourceSystem, SourceManaged:
				material := CredentialMaterial{
					AccessToken: candidate.Credential.AccessToken, RefreshToken: candidate.Credential.RefreshToken,
					IDToken: candidate.Credential.IDToken, AccountID: candidate.Credential.AccountID,
				}
				if !credentialMaterialMatchesIdentity(material, planned.Identity) {
					return CredentialMaterial{}, ErrCredentialIdentityMismatch
				}
				return material, nil
			case SourceExternal:
				if candidate.externalRef == nil {
					return CredentialMaterial{}, ErrStaleRevision
				}
				sourceNames, sourceNameCounts := snapshotExternalSourceNames(c.ExternalSources)
				if candidate.externalRef.Source == "" || sourceNameCounts[candidate.externalRef.Source] != 1 {
					return CredentialMaterial{}, ErrStaleRevision
				}
				for index, source := range c.ExternalSources {
					if source != nil && sourceNames[index] == candidate.externalRef.Source {
						material, err := source.Resolve(ctx, *candidate.externalRef)
						if err != nil {
							if ctxErr := ctx.Err(); ctxErr != nil {
								return CredentialMaterial{}, ctxErr
							}
							if !errors.Is(err, ErrStaleRevision) {
								return CredentialMaterial{}, ErrCredentialInventoryDegraded
							}
							return CredentialMaterial{}, err
						}
						if !credentialMaterialMatchesIdentity(material, planned.Identity) {
							return CredentialMaterial{}, ErrCredentialInventoryDegraded
						}
						return material, nil
					}
				}
				return CredentialMaterial{}, ErrStaleRevision
			}
		}
	}
	if planned.Source == SourceExternal {
		sourceName, known := c.externalSourceForPlan(planned)
		_, sourceNameCounts := snapshotExternalSourceNames(c.ExternalSources)
		if !known || sourceNameCounts[sourceName] != 1 {
			return CredentialMaterial{}, ErrStaleRevision
		}
		for _, status := range inventory.ExternalSources {
			if status.Name == sourceName && status.ErrorCode != "" && !status.TopologyInvalid {
				return CredentialMaterial{}, ErrCredentialInventoryDegraded
			}
		}
	}
	return CredentialMaterial{}, ErrStaleRevision
}

func sameStrongIdentity(a, b AccountIdentity) bool {
	return completeStrongIdentity(a) && completeStrongIdentity(b) &&
		a.AccountID == b.AccountID && a.UserID == b.UserID
}

func credentialMaterialMatchesIdentity(material CredentialMaterial, identity AccountIdentity) bool {
	claims := auth.DecodeCodexClaims(material.IDToken)
	return material.AccountID == identity.AccountID && claims.UserID == identity.UserID &&
		(claims.AccountID == "" || claims.AccountID == identity.AccountID)
}
