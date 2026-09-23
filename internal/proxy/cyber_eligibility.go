package proxy

import (
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/provider/codex"
)

const cyberDenialTTL = 15 * time.Minute

// CyberAccessState describes one account's advertised access for one model and
// programme. Catalogue absence and failed discovery are unknown, not denials.
type CyberAccessState uint8

const (
	CyberAccessUnknown CyberAccessState = iota
	CyberAccessEligible
	CyberAccessIneligible
)

// CyberEligibilityStore keeps account-scoped catalogue evidence separate from
// the durable routing policy. The policy remains authoritative for pool members.
type CyberEligibilityStore struct {
	mu       sync.RWMutex
	routing  *RoutingPolicyStore
	now      func() time.Time
	accounts map[codex.AccountKey]map[string]map[string]CyberAccessState
	denied   map[codex.AccountKey]map[string]map[string]time.Time
}

func NewCyberEligibilityStore(routing *RoutingPolicyStore) *CyberEligibilityStore {
	return &CyberEligibilityStore{
		routing:  routing,
		now:      time.Now,
		accounts: make(map[codex.AccountKey]map[string]map[string]CyberAccessState),
		denied:   make(map[codex.AccountKey]map[string]map[string]time.Time),
	}
}

// ReplaceAccount publishes a complete, successfully fetched catalogue for one
// account. Callers leave the old evidence intact when discovery fails.
func (s *CyberEligibilityStore) ReplaceAccount(account codex.AccountKey, models map[string]map[string]CyberAccessState) {
	if s == nil || account == "" {
		return
	}
	copyModels := make(map[string]map[string]CyberAccessState, len(models))
	for model, programmes := range models {
		copyProgrammes := make(map[string]CyberAccessState, len(programmes))
		for programme, state := range programmes {
			copyProgrammes[programme] = state
		}
		copyModels[model] = copyProgrammes
	}
	s.mu.Lock()
	s.accounts[account] = copyModels
	s.mu.Unlock()
}

// MarkIneligible records an exact upstream Cyber authorisation denial. It
// overrides catalogue claims briefly, then allows eligibility to recover.
func (s *CyberEligibilityStore) MarkIneligible(account codex.AccountKey, model, programme string) {
	if s == nil || account == "" || model == "" || programme == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.denied[account] == nil {
		s.denied[account] = make(map[string]map[string]time.Time)
	}
	if s.denied[account][model] == nil {
		s.denied[account][model] = make(map[string]time.Time)
	}
	s.denied[account][model][programme] = s.now().Add(cyberDenialTTL)
}

func (s *CyberEligibilityStore) Status(account codex.AccountKey, model, programme string) CyberAccessState {
	if s == nil {
		return CyberAccessUnknown
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if expires := s.denied[account][model][programme]; s.now().Before(expires) {
		return CyberAccessIneligible
	}
	if programmes := s.accounts[account][model]; programmes != nil {
		return programmes[programme]
	}
	return CyberAccessUnknown
}

// Members returns current Cyber pool members, including manual policy updates.
func (s *CyberEligibilityStore) Members() []codex.AccountKey {
	if s == nil || s.routing == nil {
		return nil
	}
	policy := s.routing.Current()
	index := poolIndexByName(&policy, "Cyber")
	if index < 0 {
		return nil
	}
	return append([]codex.AccountKey(nil), policy.Pools[index].Members...)
}
