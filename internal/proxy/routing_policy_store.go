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
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	poolIDPattern             = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
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
	SchemaVersion             int                           `json:"schema_version"`
	AuthorityGeneration       uint64                        `json:"authority_generation"`
	RoutingGeneration         uint64                        `json:"routing_generation"`
	EffectiveGeneration       uint64                        `json:"effective_generation"`
	Pools                     []AccountPoolV1               `json:"pools,omitempty"`
	SessionBindings           []SessionBindingV1            `json:"session_bindings,omitempty"`
	CapabilityEvidence        []CapabilityEvidenceV1        `json:"capability_evidence,omitempty"`
	CapabilityPool            string                        `json:"capability_pool,omitempty"`
	CapabilityPredicates      []CapabilityPredicateCoreV1   `json:"capability_predicates,omitempty"`
	CapabilityRoutingEvidence []CapabilityRoutingEvidenceV1 `json:"capability_routing_evidence,omitempty"`
	Delegations               []CallerDelegationV1          `json:"delegations,omitempty"`
	MAC                       string                        `json:"mac,omitempty"`
}

type PoolID string

type PoolValue uint32

type AccountPoolV2 struct {
	ID      PoolID                     `json:"id"`
	Name    string                     `json:"name"`
	Value   PoolValue                  `json:"value,omitempty"`
	Members []providerCodex.AccountKey `json:"members"`
}

type SessionBindingV2 struct {
	SessionDigest string `json:"session_digest"`
	PoolID        PoolID `json:"pool_id"`
}

type RoutingPolicyV2 struct {
	SchemaVersion             int                           `json:"schema_version"`
	AuthorityGeneration       uint64                        `json:"authority_generation"`
	RoutingGeneration         uint64                        `json:"routing_generation"`
	EffectiveGeneration       uint64                        `json:"effective_generation"`
	Pools                     []AccountPoolV2               `json:"pools,omitempty"`
	SessionBindings           []SessionBindingV2            `json:"session_bindings,omitempty"`
	CapabilityEvidence        []CapabilityEvidenceV1        `json:"capability_evidence,omitempty"`
	CapabilityPool            PoolID                        `json:"capability_pool,omitempty"`
	CapabilityPredicates      []CapabilityPredicateCoreV1   `json:"capability_predicates,omitempty"`
	CapabilityRoutingEvidence []CapabilityRoutingEvidenceV1 `json:"capability_routing_evidence,omitempty"`
	Delegations               []CallerDelegationV1          `json:"delegations,omitempty"`
	MAC                       string                        `json:"mac,omitempty"`
}

type AccountPoolDocument struct {
	Name    string                     `json:"name"`
	Value   PoolValue                  `json:"value,omitempty"`
	Members []providerCodex.AccountKey `json:"members"`
}

type SessionBindingDocument struct {
	SessionDigest string `json:"session_digest"`
	Pool          string `json:"pool"`
}

// RoutingPolicyDocument is the name-based public policy format. Schema v1 is
// retained independently from the internal authenticated policy schema.
type RoutingPolicyDocument struct {
	SchemaVersion             int                           `json:"schema_version"`
	AuthorityGeneration       uint64                        `json:"authority_generation"`
	RoutingGeneration         uint64                        `json:"routing_generation"`
	EffectiveGeneration       uint64                        `json:"effective_generation"`
	Pools                     []AccountPoolDocument         `json:"pools,omitempty"`
	SessionBindings           []SessionBindingDocument      `json:"session_bindings,omitempty"`
	CapabilityEvidence        []CapabilityEvidenceV1        `json:"capability_evidence,omitempty"`
	CapabilityPool            string                        `json:"capability_pool,omitempty"`
	CapabilityPredicates      []CapabilityPredicateCoreV1   `json:"capability_predicates,omitempty"`
	CapabilityRoutingEvidence []CapabilityRoutingEvidenceV1 `json:"capability_routing_evidence,omitempty"`
	Delegations               []CallerDelegationV1          `json:"delegations,omitempty"`
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
	if (len(policy.CapabilityPredicates) == 0) != (len(policy.CapabilityRoutingEvidence) == 0) {
		return errors.New("incomplete capability routing policy")
	}
	if len(policy.CapabilityPredicates) > 0 {
		if policy.CapabilityPool == "" && len(policy.Pools) == 1 {
			policy.CapabilityPool = policy.Pools[0].Name
		}
		if pools[policy.CapabilityPool] == nil {
			return errors.New("invalid capability pool")
		}
	} else if policy.CapabilityPool != "" {
		return errors.New("capability pool without routing policy")
	}
	seenPredicates := make(map[string]struct{}, len(policy.CapabilityPredicates))
	for _, predicate := range policy.CapabilityPredicates {
		if !validCapabilityPredicate(predicate) {
			return errors.New("invalid capability predicate")
		}
		key := capabilityPredicateKey(predicate)
		if _, duplicate := seenPredicates[key]; duplicate {
			return errors.New("duplicate capability predicate")
		}
		seenPredicates[key] = struct{}{}
	}
	for _, item := range policy.CapabilityRoutingEvidence {
		if !validCapabilityEvidence(item, policy.RoutingGeneration, item.ObservedAt) {
			return errors.New("invalid capability routing evidence")
		}
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

func validateRoutingPolicyV2(policy RoutingPolicyV2, prior *RoutingPolicyV2) error {
	if policy.SchemaVersion != 2 || policy.AuthorityGeneration == 0 || policy.RoutingGeneration == 0 || policy.EffectiveGeneration > policy.RoutingGeneration {
		return errors.New("invalid routing policy generations")
	}
	if prior != nil && (policy.AuthorityGeneration != prior.AuthorityGeneration+1 || policy.RoutingGeneration != prior.RoutingGeneration+1 || policy.EffectiveGeneration < prior.EffectiveGeneration) {
		return errors.New("stale routing policy generation")
	}
	pools := make(map[PoolID]map[providerCodex.AccountKey]struct{}, len(policy.Pools))
	names := make(map[string]struct{}, len(policy.Pools))
	for _, pool := range policy.Pools {
		if !validPoolID(pool.ID) || !validPoolName(pool.Name) || len(pool.Members) == 0 {
			return errors.New("invalid account pool")
		}
		if _, exists := pools[pool.ID]; exists {
			return errors.New("duplicate account pool")
		}
		folded := foldPoolName(pool.Name)
		if _, exists := names[folded]; exists {
			return errors.New("duplicate account pool name")
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
		pools[pool.ID] = members
		names[folded] = struct{}{}
	}
	bindings := make(map[string]struct{}, len(policy.SessionBindings))
	for _, binding := range policy.SessionBindings {
		if !lowerHexDigest(binding.SessionDigest) || pools[binding.PoolID] == nil {
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
	if (len(policy.CapabilityPredicates) == 0) != (len(policy.CapabilityRoutingEvidence) == 0) {
		return errors.New("incomplete capability routing policy")
	}
	if len(policy.CapabilityPredicates) > 0 {
		if pools[policy.CapabilityPool] == nil {
			return errors.New("invalid capability pool")
		}
	} else if policy.CapabilityPool != "" {
		return errors.New("capability pool without routing policy")
	}
	seenPredicates := make(map[string]struct{}, len(policy.CapabilityPredicates))
	for _, predicate := range policy.CapabilityPredicates {
		if !validCapabilityPredicate(predicate) {
			return errors.New("invalid capability predicate")
		}
		key := capabilityPredicateKey(predicate)
		if _, duplicate := seenPredicates[key]; duplicate {
			return errors.New("duplicate capability predicate")
		}
		seenPredicates[key] = struct{}{}
	}
	for _, item := range policy.CapabilityRoutingEvidence {
		if !validCapabilityEvidence(item, policy.RoutingGeneration, item.ObservedAt) {
			return errors.New("invalid capability routing evidence")
		}
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

func validPoolID(id PoolID) bool {
	return poolIDPattern.MatchString(string(id))
}

func validPoolName(name string) bool {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" {
		return false
	}
	for _, value := range name {
		if unicode.IsControl(value) {
			return false
		}
	}
	return true
}

func foldPoolName(name string) string {
	var folded strings.Builder
	for _, value := range name {
		minimum := value
		for next := unicode.SimpleFold(value); next != value; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		folded.WriteRune(minimum)
	}
	return folded.String()
}

func newPoolID(random io.Reader) (PoolID, error) {
	if random == nil {
		return "", errors.New("pool identity unavailable")
	}
	var value [16]byte
	if _, err := io.ReadFull(random, value[:]); err != nil {
		return "", errors.New("pool identity unavailable")
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	raw := hex.EncodeToString(value[:])
	return PoolID(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]), nil
}

func routingPolicyDocument(policy RoutingPolicyV2) (RoutingPolicyDocument, error) {
	names := make(map[PoolID]string, len(policy.Pools))
	document := RoutingPolicyDocument{
		SchemaVersion:             1,
		AuthorityGeneration:       policy.AuthorityGeneration,
		RoutingGeneration:         policy.RoutingGeneration,
		EffectiveGeneration:       policy.EffectiveGeneration,
		Pools:                     make([]AccountPoolDocument, len(policy.Pools)),
		SessionBindings:           make([]SessionBindingDocument, len(policy.SessionBindings)),
		CapabilityEvidence:        append([]CapabilityEvidenceV1(nil), policy.CapabilityEvidence...),
		CapabilityPredicates:      append([]CapabilityPredicateCoreV1(nil), policy.CapabilityPredicates...),
		CapabilityRoutingEvidence: cloneCapabilityRoutingEvidence(policy.CapabilityRoutingEvidence),
		Delegations:               append([]CallerDelegationV1(nil), policy.Delegations...),
	}
	for index, pool := range policy.Pools {
		if names[pool.ID] != "" {
			return RoutingPolicyDocument{}, errors.New("duplicate account pool")
		}
		names[pool.ID] = pool.Name
		document.Pools[index] = AccountPoolDocument{
			Name: pool.Name, Value: pool.Value, Members: append([]providerCodex.AccountKey(nil), pool.Members...),
		}
	}
	for index, binding := range policy.SessionBindings {
		name := names[binding.PoolID]
		if name == "" {
			return RoutingPolicyDocument{}, errors.New("invalid session binding")
		}
		document.SessionBindings[index] = SessionBindingDocument{SessionDigest: binding.SessionDigest, Pool: name}
	}
	if policy.CapabilityPool != "" {
		document.CapabilityPool = names[policy.CapabilityPool]
		if document.CapabilityPool == "" {
			return RoutingPolicyDocument{}, errors.New("invalid capability pool")
		}
	}
	return document, nil
}

func compileRoutingPolicyDocument(document RoutingPolicyDocument, prior *RoutingPolicyV2, random io.Reader) (RoutingPolicyV2, error) {
	if document.SchemaVersion != 1 {
		return RoutingPolicyV2{}, errors.New("invalid routing policy schema")
	}
	existing := make(map[string]AccountPoolV2)
	if prior != nil {
		for _, pool := range prior.Pools {
			existing[foldPoolName(pool.Name)] = pool
		}
	}
	policy := RoutingPolicyV2{
		SchemaVersion:             2,
		AuthorityGeneration:       document.AuthorityGeneration,
		RoutingGeneration:         document.RoutingGeneration,
		EffectiveGeneration:       document.EffectiveGeneration,
		Pools:                     make([]AccountPoolV2, len(document.Pools)),
		SessionBindings:           make([]SessionBindingV2, len(document.SessionBindings)),
		CapabilityEvidence:        append([]CapabilityEvidenceV1(nil), document.CapabilityEvidence...),
		CapabilityPredicates:      append([]CapabilityPredicateCoreV1(nil), document.CapabilityPredicates...),
		CapabilityRoutingEvidence: cloneCapabilityRoutingEvidence(document.CapabilityRoutingEvidence),
		Delegations:               append([]CallerDelegationV1(nil), document.Delegations...),
	}
	poolIDs := make(map[string]PoolID, len(document.Pools))
	for index, pool := range document.Pools {
		if !validPoolName(pool.Name) || len(pool.Members) == 0 {
			return RoutingPolicyV2{}, errors.New("invalid account pool")
		}
		folded := foldPoolName(pool.Name)
		if _, duplicate := poolIDs[folded]; duplicate {
			return RoutingPolicyV2{}, errors.New("duplicate account pool name")
		}
		id := PoolID("")
		name := pool.Name
		if current, found := existing[folded]; found {
			id = current.ID
			name = current.Name
		} else {
			var err error
			id, err = newPoolID(random)
			if err != nil {
				return RoutingPolicyV2{}, err
			}
		}
		poolIDs[folded] = id
		policy.Pools[index] = AccountPoolV2{ID: id, Name: name, Value: pool.Value, Members: append([]providerCodex.AccountKey(nil), pool.Members...)}
	}
	for index, binding := range document.SessionBindings {
		id := poolIDs[foldPoolName(binding.Pool)]
		if id == "" {
			return RoutingPolicyV2{}, errors.New("invalid session binding")
		}
		policy.SessionBindings[index] = SessionBindingV2{SessionDigest: binding.SessionDigest, PoolID: id}
	}
	if document.CapabilityPool != "" {
		policy.CapabilityPool = poolIDs[foldPoolName(document.CapabilityPool)]
		if policy.CapabilityPool == "" {
			return RoutingPolicyV2{}, errors.New("invalid capability pool")
		}
	}
	if err := validateRoutingPolicyV2(policy, prior); err != nil {
		return RoutingPolicyV2{}, err
	}
	return policy, nil
}

func cloneCapabilityRoutingEvidence(items []CapabilityRoutingEvidenceV1) []CapabilityRoutingEvidenceV1 {
	result := append([]CapabilityRoutingEvidenceV1(nil), items...)
	for index := range result {
		if result[index].ExpiresAt != nil {
			expires := *result[index].ExpiresAt
			result[index].ExpiresAt = &expires
		}
	}
	return result
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
	policy.CapabilityPredicates = append([]CapabilityPredicateCoreV1(nil), policy.CapabilityPredicates...)
	policy.CapabilityRoutingEvidence = cloneCapabilityRoutingEvidence(policy.CapabilityRoutingEvidence)
	policy.Delegations = append([]CallerDelegationV1(nil), policy.Delegations...)
	return policy
}

func cloneRoutingPolicyV2(policy RoutingPolicyV2) RoutingPolicyV2 {
	policy.Pools = append([]AccountPoolV2(nil), policy.Pools...)
	for index := range policy.Pools {
		policy.Pools[index].Members = append([]providerCodex.AccountKey(nil), policy.Pools[index].Members...)
	}
	policy.SessionBindings = append([]SessionBindingV2(nil), policy.SessionBindings...)
	policy.CapabilityEvidence = append([]CapabilityEvidenceV1(nil), policy.CapabilityEvidence...)
	policy.CapabilityPredicates = append([]CapabilityPredicateCoreV1(nil), policy.CapabilityPredicates...)
	policy.CapabilityRoutingEvidence = cloneCapabilityRoutingEvidence(policy.CapabilityRoutingEvidence)
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
