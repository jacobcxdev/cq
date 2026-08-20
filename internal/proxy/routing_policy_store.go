package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	routingPolicyAnchorName = "routing-policy-anchor"
	routingPolicyMaxBytes   = 1 << 20
)

var (
	ErrRoutingFeatureInactive = errors.New("feature_inactive")
	poolNamePattern           = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

type CapabilityEvidenceState string

const (
	CapabilitySupported   CapabilityEvidenceState = "supported"
	CapabilityUnsupported CapabilityEvidenceState = "unsupported"
	CapabilityUnknown     CapabilityEvidenceState = "unknown"
)

type AccountPoolV1 struct {
	Name    string                     `json:"name"`
	Members []providerCodex.AccountKey `json:"members"`
}

type SessionBindingV1 struct {
	SessionDigest string `json:"session_digest"`
	Pool          string `json:"pool"`
}

type CapabilityEvidenceV1 struct {
	AccountKey providerCodex.AccountKey `json:"account_key"`
	State      CapabilityEvidenceState  `json:"state"`
}

type CallerDelegationV1 struct {
	Caller    string                     `json:"caller"`
	Accounts  []providerCodex.AccountKey `json:"accounts"`
	ExpiresAt time.Time                  `json:"expires_at,omitempty"`
}

type RoutingPolicyV1 struct {
	SchemaVersion       int                    `json:"schema_version"`
	AuthorityGeneration uint64                 `json:"authority_generation"`
	RoutingGeneration   uint64                 `json:"routing_generation"`
	EffectiveGeneration uint64                 `json:"effective_generation"`
	Pools               []AccountPoolV1        `json:"pools,omitempty"`
	SessionBindings     []SessionBindingV1     `json:"session_bindings,omitempty"`
	CapabilityEvidence  []CapabilityEvidenceV1 `json:"capability_evidence,omitempty"`
	Delegations         []CallerDelegationV1   `json:"delegations,omitempty"`
	MAC                 string                 `json:"mac,omitempty"`
}

type routingPolicyAnchorV1 struct {
	SchemaVersion int    `json:"schema_version"`
	ObjectDigest  string `json:"object_digest"`
	MAC           string `json:"mac,omitempty"`
}

// RoutingPolicyStore publishes authenticated immutable policy snapshots and
// moves one CAS-protected selector. Raw session identifiers are never accepted.
type RoutingPolicyStore struct {
	mu        sync.Mutex
	ctx       context.Context
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	publisher DurableObjectPublisher
	key       [32]byte
	current   *RoutingPolicyV1
	anchorID  *StableObjectIdentity
}

func OpenRoutingPolicyStore(ctx context.Context, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, publisher DurableObjectPublisher, key []byte) (*RoutingPolicyStore, error) {
	if ctx == nil || inspector == nil || directory == nil || publisher == nil || len(key) != 32 {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	store := &RoutingPolicyStore{ctx: ctx, inspector: inspector, directory: directory, publisher: publisher}
	copy(store.key[:], key)
	if err := store.reopen(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *RoutingPolicyStore) Publish(policy RoutingPolicyV1) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy.MAC = ""
	if err := validateRoutingPolicy(policy, s.current); err != nil {
		return err
	}
	body, err := sealRoutingPolicy(policy, s.key[:])
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	objectDigest := hex.EncodeToString(digest[:])
	objectName := "routing-policy-" + objectDigest + ".json"
	if _, err := s.publishOrAdopt(objectName, body); err != nil {
		return err
	}
	anchorBody, err := sealRoutingAnchor(routingPolicyAnchorV1{SchemaVersion: 1, ObjectDigest: objectDigest}, s.key[:])
	if err != nil {
		return err
	}
	var identity StableObjectIdentity
	if s.anchorID == nil {
		identity, err = s.publisher.PublishImmutable(s.ctx, s.directory, routingPolicyAnchorName, anchorBody, fs.FileMode(0o600))
	} else {
		identity, err = s.publisher.ReplaceSelectorExactPrior(s.ctx, s.directory, routingPolicyAnchorName, s.anchorID, anchorBody)
	}
	if err != nil {
		return err
	}
	policy.MAC = policyMAC(policy, s.key[:])
	s.current = &policy
	s.anchorID = &identity
	return nil
}

func (s *RoutingPolicyStore) PublishDelegation(delegation CallerDelegationV1) error {
	current := s.Current()
	if current.SchemaVersion != 1 {
		return ErrRoutingFeatureInactive
	}
	current.AuthorityGeneration++
	current.RoutingGeneration++
	current.Delegations = append(current.Delegations, delegation)
	return s.Publish(current)
}

func (s *RoutingPolicyStore) Current() RoutingPolicyV1 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return RoutingPolicyV1{}
	}
	return cloneRoutingPolicy(*s.current)
}

func (s *RoutingPolicyStore) SessionDigest(exact []byte) string {
	return keyedSessionDigest(s.key[:], exact)
}

func (s *RoutingPolicyStore) Resolver() *SessionPolicyResolver {
	policy := s.Current()
	return NewSessionPolicyResolver(s.key[:], policy)
}

func (s *RoutingPolicyStore) reopen() error {
	anchorBody, anchorIdentity, err := s.readStable(routingPolicyAnchorName, routingPolicyMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	anchor, err := openRoutingAnchor(anchorBody, s.key[:])
	if err != nil {
		return err
	}
	objectBody, _, err := s.readStable("routing-policy-"+anchor.ObjectDigest+".json", routingPolicyMaxBytes)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(objectBody)
	if hex.EncodeToString(digest[:]) != anchor.ObjectDigest {
		return errors.New("routing policy object digest mismatch")
	}
	policy, err := openRoutingPolicy(objectBody, s.key[:])
	if err != nil {
		return err
	}
	if err := validateRoutingPolicy(policy, nil); err != nil {
		return err
	}
	s.current = &policy
	s.anchorID = &anchorIdentity
	return nil
}

func (s *RoutingPolicyStore) publishOrAdopt(name string, body []byte) (StableObjectIdentity, error) {
	identity, err := s.publisher.PublishImmutable(s.ctx, s.directory, name, body, 0o600)
	if err == nil {
		return identity, nil
	}
	existing, existingIdentity, readErr := s.readStable(name, int64(len(body))+1)
	if readErr != nil || !bytes.Equal(existing, body) {
		return StableObjectIdentity{}, err
	}
	return existingIdentity, nil
}

func (s *RoutingPolicyStore) readStable(name string, max int64) ([]byte, StableObjectIdentity, error) {
	body, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(s.inspector, s.directory, name, max)
	if err != nil {
		return nil, StableObjectIdentity{}, err
	}
	stable, err := stableAuthorityIdentityFromParts(identity, int64(len(body)), body)
	return body, stable, err
}

func validateRoutingPolicy(policy RoutingPolicyV1, prior *RoutingPolicyV1) error {
	if policy.SchemaVersion != 1 || policy.AuthorityGeneration == 0 || policy.RoutingGeneration == 0 || policy.EffectiveGeneration > policy.RoutingGeneration {
		return errors.New("invalid routing policy generations")
	}
	if prior != nil && (policy.AuthorityGeneration != prior.AuthorityGeneration+1 || policy.RoutingGeneration != prior.RoutingGeneration+1 || policy.EffectiveGeneration < prior.EffectiveGeneration) {
		return errors.New("stale routing policy generation")
	}
	pools := make(map[string]map[providerCodex.AccountKey]struct{}, len(policy.Pools))
	for _, pool := range policy.Pools {
		if !poolNamePattern.MatchString(pool.Name) || len(pool.Members) == 0 {
			return errors.New("invalid account pool")
		}
		if _, exists := pools[pool.Name]; exists {
			return errors.New("duplicate account pool")
		}
		members := make(map[providerCodex.AccountKey]struct{}, len(pool.Members))
		for _, member := range pool.Members {
			if member == "" {
				return errors.New("empty pool member")
			}
			if _, exists := members[member]; exists {
				return errors.New("duplicate pool member")
			}
			members[member] = struct{}{}
		}
		pools[pool.Name] = members
	}
	bindings := make(map[string]struct{}, len(policy.SessionBindings))
	for _, binding := range policy.SessionBindings {
		if !lowerHexDigest(binding.SessionDigest) || pools[binding.Pool] == nil {
			return errors.New("invalid session binding")
		}
		if _, exists := bindings[binding.SessionDigest]; exists {
			return errors.New("duplicate session binding")
		}
		bindings[binding.SessionDigest] = struct{}{}
	}
	evidence := make(map[providerCodex.AccountKey]struct{}, len(policy.CapabilityEvidence))
	for _, item := range policy.CapabilityEvidence {
		if item.AccountKey == "" || (item.State != CapabilitySupported && item.State != CapabilityUnsupported && item.State != CapabilityUnknown) {
			return errors.New("invalid capability evidence")
		}
		if _, exists := evidence[item.AccountKey]; exists {
			return errors.New("duplicate capability evidence")
		}
		evidence[item.AccountKey] = struct{}{}
	}
	delegations := make(map[string]struct{}, len(policy.Delegations))
	for _, delegation := range policy.Delegations {
		if delegation.Caller == "" || len(delegation.Accounts) == 0 || delegation.ExpiresAt.IsZero() || !delegation.ExpiresAt.Equal(delegation.ExpiresAt.UTC()) {
			return errors.New("invalid caller delegation")
		}
		if _, exists := delegations[delegation.Caller]; exists {
			return errors.New("duplicate caller delegation")
		}
		delegations[delegation.Caller] = struct{}{}
		seen := make(map[providerCodex.AccountKey]struct{}, len(delegation.Accounts))
		for _, account := range delegation.Accounts {
			if account == "" {
				return errors.New("invalid caller delegation account")
			}
			if _, exists := seen[account]; exists {
				return errors.New("duplicate caller delegation account")
			}
			seen[account] = struct{}{}
		}
	}
	return nil
}

func lowerHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func cloneRoutingPolicy(policy RoutingPolicyV1) RoutingPolicyV1 {
	policy.Pools = append([]AccountPoolV1(nil), policy.Pools...)
	for index := range policy.Pools {
		policy.Pools[index].Members = append([]providerCodex.AccountKey(nil), policy.Pools[index].Members...)
	}
	policy.SessionBindings = append([]SessionBindingV1(nil), policy.SessionBindings...)
	policy.CapabilityEvidence = append([]CapabilityEvidenceV1(nil), policy.CapabilityEvidence...)
	policy.Delegations = append([]CallerDelegationV1(nil), policy.Delegations...)
	return policy
}

func sealRoutingPolicy(policy RoutingPolicyV1, key []byte) ([]byte, error) {
	policy.MAC = policyMAC(policy, key)
	return json.Marshal(policy)
}

func openRoutingPolicy(body, key []byte) (RoutingPolicyV1, error) {
	var policy RoutingPolicyV1
	if err := json.Unmarshal(body, &policy); err != nil {
		return policy, err
	}
	want := policyMAC(policy, key)
	if !hmac.Equal([]byte(policy.MAC), []byte(want)) {
		return RoutingPolicyV1{}, errors.New("routing policy authentication failed")
	}
	return policy, nil
}

func policyMAC(policy RoutingPolicyV1, key []byte) string {
	policy.MAC = ""
	body, _ := json.Marshal(policy)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/routing-policy/v1\x00"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func sealRoutingAnchor(anchor routingPolicyAnchorV1, key []byte) ([]byte, error) {
	anchor.MAC = routingAnchorMAC(anchor, key)
	return json.Marshal(anchor)
}

func openRoutingAnchor(body, key []byte) (routingPolicyAnchorV1, error) {
	var anchor routingPolicyAnchorV1
	if err := json.Unmarshal(body, &anchor); err != nil {
		return anchor, err
	}
	if anchor.SchemaVersion != 1 || !lowerHexDigest(anchor.ObjectDigest) || !hmac.Equal([]byte(anchor.MAC), []byte(routingAnchorMAC(anchor, key))) {
		return routingPolicyAnchorV1{}, errors.New("routing policy anchor authentication failed")
	}
	return anchor, nil
}

func routingAnchorMAC(anchor routingPolicyAnchorV1, key []byte) string {
	anchor.MAC = ""
	body, _ := json.Marshal(anchor)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/routing-policy-anchor/v1\x00"))
	_, _ = mac.Write(body)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func sortedAccountKeys(keys []providerCodex.AccountKey) []providerCodex.AccountKey {
	result := append([]providerCodex.AccountKey(nil), keys...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
