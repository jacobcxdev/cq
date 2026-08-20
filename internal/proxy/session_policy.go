package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
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
	Allowed        []providerCodex.AccountKey
	PolicyRevision uint64
	Status         PolicyDecisionStatus
}

type SessionPolicyResolver struct {
	key    [32]byte
	policy RoutingPolicyV1
}

func NewSessionPolicyResolver(key []byte, policy RoutingPolicyV1) *SessionPolicyResolver {
	resolver := &SessionPolicyResolver{policy: cloneRoutingPolicy(policy)}
	if len(key) == 32 {
		copy(resolver.key[:], key)
	}
	return resolver
}

func (r *SessionPolicyResolver) Resolve(exactSession []byte, global []providerCodex.AccountKey) SessionPolicyDecision {
	digest := keyedSessionDigest(r.key[:], exactSession)
	decision := SessionPolicyDecision{SessionDigest: digest, PolicyRevision: r.policy.RoutingGeneration, Status: PolicyDecisionUnbound}
	var poolName string
	for _, binding := range r.policy.SessionBindings {
		if hmac.Equal([]byte(binding.SessionDigest), []byte(digest)) {
			poolName = binding.Pool
			break
		}
	}
	if poolName == "" {
		return decision
	}
	decision.Pool = poolName
	poolMembers := make(map[providerCodex.AccountKey]struct{})
	for _, pool := range r.policy.Pools {
		if pool.Name == poolName {
			for _, member := range pool.Members {
				poolMembers[member] = struct{}{}
			}
			break
		}
	}
	supported := make(map[providerCodex.AccountKey]bool)
	for _, evidence := range r.policy.CapabilityEvidence {
		supported[evidence.AccountKey] = evidence.State == CapabilitySupported
	}
	seen := make(map[providerCodex.AccountKey]struct{})
	for _, account := range global {
		if _, inPool := poolMembers[account]; inPool && supported[account] {
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

func keyedSessionDigest(key, exact []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/session-policy/digest/v1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(exact)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(exact)
	return hex.EncodeToString(mac.Sum(nil))
}
