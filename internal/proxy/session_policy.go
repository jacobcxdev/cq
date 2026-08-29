package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

var (
	ErrSessionPolicyUnavailable = errors.New("session policy unavailable")
	ErrSessionPolicyContinuity  = errors.New("session policy conflicts with continuity")
)

type PolicyDecisionStatus string

const (
	PolicyDecisionSelected  PolicyDecisionStatus = "selected"
	PolicyDecisionUnbound   PolicyDecisionStatus = "unbound"
	PolicyDecisionNoAccount PolicyDecisionStatus = "no_allowed_accounts"
)

type SessionPolicyDecision struct {
	SessionDigest  string
	Pool           string
	PoolID         PoolID
	Allowed        []providerCodex.AccountKey
	AccountValues  map[providerCodex.AccountKey]PoolValue
	PolicyRevision uint64
	Status         PolicyDecisionStatus
}

type SessionPolicyResolver struct {
	mu     sync.RWMutex
	key    [32]byte
	policy RoutingPolicyV2
}

func (r *SessionPolicyResolver) capabilityPolicy(poolID PoolID, revision uint64) (RoutingPolicySnapshotV1, []CapabilityRoutingEvidenceV1, bool) {
	if r == nil {
		return RoutingPolicySnapshotV1{}, nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.policy.RoutingGeneration != revision || len(r.policy.CapabilityPredicates) == 0 || len(r.policy.CapabilityRoutingEvidence) == 0 {
		return RoutingPolicySnapshotV1{}, nil, false
	}
	if poolID != r.policy.CapabilityPool {
		return RoutingPolicySnapshotV1{}, nil, false
	}
	for _, candidate := range r.policy.Pools {
		if candidate.ID != poolID {
			continue
		}
		return RoutingPolicySnapshotV1{
			SchemaVersion: 1, Active: true, RoutingGeneration: r.policy.RoutingGeneration,
			PoolID: candidate.ID,
			Pool:   AccountPoolV1{Name: candidate.Name, Members: append([]providerCodex.AccountKey(nil), candidate.Members...)}, Predicates: append([]CapabilityPredicateCoreV1(nil), r.policy.CapabilityPredicates...),
		}, append([]CapabilityRoutingEvidenceV1(nil), r.policy.CapabilityRoutingEvidence...), true
	}
	return RoutingPolicySnapshotV1{}, nil, false
}

func NewSessionPolicyResolver(key []byte, policy RoutingPolicyV2) *SessionPolicyResolver {
	resolver := &SessionPolicyResolver{policy: cloneRoutingPolicyV2(policy)}
	if len(key) == 32 {
		copy(resolver.key[:], key)
	}
	return resolver
}

func (r *SessionPolicyResolver) Replace(policy RoutingPolicyV2) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.policy = cloneRoutingPolicyV2(policy)
	r.mu.Unlock()
}

func (r *SessionPolicyResolver) Resolve(exactSession []byte, global []providerCodex.AccountKey) SessionPolicyDecision {
	decision, _ := r.resolveWithPolicy(exactSession, global)
	return decision
}

func (r *SessionPolicyResolver) resolveWithPolicy(exactSession []byte, global []providerCodex.AccountKey) (SessionPolicyDecision, RoutingPolicyV2) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveLocked(exactSession, global), cloneRoutingPolicyV2(r.policy)
}

func (r *SessionPolicyResolver) resolveLocked(exactSession []byte, global []providerCodex.AccountKey) SessionPolicyDecision {
	digest := keyedSessionDigest(r.key[:], exactSession)
	decision := SessionPolicyDecision{
		SessionDigest: digest, PolicyRevision: r.policy.RoutingGeneration, Status: PolicyDecisionUnbound,
		AccountValues: r.accountValuesLocked(global),
	}
	var poolID PoolID
	for _, binding := range r.policy.SessionBindings {
		if hmac.Equal([]byte(binding.SessionDigest), []byte(digest)) {
			poolID = binding.PoolID
			break
		}
	}
	if poolID == "" {
		return decision
	}
	decision.PoolID = poolID
	poolMembers := make(map[providerCodex.AccountKey]struct{})
	for _, pool := range r.policy.Pools {
		if pool.ID == poolID {
			decision.Pool = pool.Name
			for _, member := range pool.Members {
				poolMembers[member] = struct{}{}
			}
			break
		}
	}
	seen := make(map[providerCodex.AccountKey]struct{})
	for _, account := range global {
		if _, inPool := poolMembers[account]; inPool {
			if _, duplicate := seen[account]; !duplicate {
				decision.Allowed = append(decision.Allowed, account)
				seen[account] = struct{}{}
			}
		}
	}
	decision.Allowed = sortedAccountKeys(decision.Allowed)
	if len(decision.Allowed) == 0 {
		decision.Status = PolicyDecisionNoAccount
	} else {
		decision.Status = PolicyDecisionSelected
	}
	return decision
}

func (r *SessionPolicyResolver) accountValuesLocked(global []providerCodex.AccountKey) map[providerCodex.AccountKey]PoolValue {
	available := make(map[providerCodex.AccountKey]struct{}, len(global))
	for _, account := range global {
		available[account] = struct{}{}
	}
	values := make(map[providerCodex.AccountKey]PoolValue, len(available))
	for _, pool := range r.policy.Pools {
		for _, account := range pool.Members {
			if _, ok := available[account]; ok && pool.Value > values[account] {
				values[account] = pool.Value
			}
		}
	}
	return values
}

// enforceSessionPolicy loads continuity first, then narrows global authority.
// Unbound sessions preserve existing routing parity.
func enforceSessionPolicy(resolver *SessionPolicyResolver, caller RuntimeCallerAuthorityV1, exactSession []byte, global []providerCodex.AccountKey, continuity providerCodex.AccountKey, now time.Time) (SessionPolicyDecision, error) {
	if resolver == nil {
		return SessionPolicyDecision{Allowed: sortedAccountKeys(global), AccountValues: map[providerCodex.AccountKey]PoolValue{}, Status: PolicyDecisionUnbound}, nil
	}
	decision, policy := resolver.resolveWithPolicy(exactSession, global)
	if decision.Status == PolicyDecisionUnbound {
		decision.Allowed = sortedAccountKeys(global)
		return decision, nil
	}
	if caller.Domain != NormalCallerLocal && caller.Domain != NormalCallerCodex {
		return SessionPolicyDecision{}, ErrSessionPolicyUnavailable
	}
	// Empty delegation state preserves existing authenticated Codex pooling.
	// Worker-keyed caller subjects cannot be named by persistent session policy.
	if caller.Domain == NormalCallerCodex && len(policy.Delegations) != 0 {
		allowed := make(map[providerCodex.AccountKey]struct{})
		matched := false
		for _, delegation := range policy.Delegations {
			if delegation.Caller != caller.SubjectID || !now.Before(delegation.ExpiresAt) {
				continue
			}
			matched = true
			for _, account := range delegation.Accounts {
				allowed[account] = struct{}{}
			}
		}
		if !matched {
			return SessionPolicyDecision{}, ErrSessionPolicyUnavailable
		}
		filtered := decision.Allowed[:0]
		for _, account := range decision.Allowed {
			if _, ok := allowed[account]; ok {
				filtered = append(filtered, account)
			}
		}
		decision.Allowed = filtered
		if len(decision.Allowed) == 0 {
			decision.Status = PolicyDecisionNoAccount
		}
	}
	if decision.Status != PolicyDecisionSelected {
		return SessionPolicyDecision{}, ErrSessionPolicyUnavailable
	}
	if continuity != "" {
		for _, account := range decision.Allowed {
			if account == continuity {
				return decision, nil
			}
		}
		return SessionPolicyDecision{}, ErrSessionPolicyContinuity
	}
	return decision, nil
}

func keyedSessionDigest(key, exact []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/session-policy/digest/v1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(exact)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(exact)
	return hex.EncodeToString(mac.Sum(nil))
}
