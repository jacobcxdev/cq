package proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

var (
	ErrCapabilityRoutingInactive  = errors.New("capability routing inactive")
	ErrCapabilityRouteUnavailable = errors.New("capability route unavailable")
)

type CapabilityPredicateCoreV1 struct {
	SchemaVersion  int    `json:"schema_version"`
	Capability     string `json:"capability"`
	ProductSurface string `json:"product_surface"`
	AccessPath     string `json:"access_path"`
	AuthMode       string `json:"auth_mode"`
	RequestedModel string `json:"requested_model"`
	EffectiveModel string `json:"effective_model"`
}

type CapabilityFinalScopeCoreV1 struct {
	SchemaVersion                       int    `json:"schema_version"`
	RouteID                             string `json:"route_id"`
	Provider                            string `json:"provider"`
	TransportKind                       string `json:"transport_kind"`
	ProductSurface                      string `json:"product_surface"`
	AccessPath                          string `json:"access_path"`
	AuthMode                            string `json:"auth_mode"`
	RequestedModel                      string `json:"requested_model"`
	EffectiveModel                      string `json:"effective_model"`
	OutboundModel                       string `json:"outbound_model"`
	TransformationDigest                string `json:"transformation_digest"`
	EncodedRequestDigest                string `json:"encoded_request_digest"`
	NormalCredentialOriginBindingDigest string `json:"normal_credential_origin_binding_digest"`
}

type CapabilityRoutingEvidenceState string

const (
	CapabilityEvidenceEligible   CapabilityRoutingEvidenceState = "eligible"
	CapabilityEvidenceIneligible CapabilityRoutingEvidenceState = "ineligible"
	CapabilityEvidenceUnknown    CapabilityRoutingEvidenceState = "unknown"
)

type CapabilityRoutingEvidenceV1 struct {
	SchemaVersion     int                            `json:"schema_version"`
	AccountKey        providerCodex.AccountKey       `json:"account_key"`
	AccountKeyHMAC    string                         `json:"account_key_hmac"`
	Workspace         string                         `json:"workspace"`
	Capability        string                         `json:"capability"`
	ProductSurface    string                         `json:"product_surface"`
	AccessPath        string                         `json:"access_path"`
	AuthMode          string                         `json:"auth_mode"`
	RequestedModel    string                         `json:"requested_model"`
	EffectiveModel    string                         `json:"effective_model"`
	State             CapabilityRoutingEvidenceState `json:"state"`
	Source            string                         `json:"source"`
	ObservedAt        time.Time                      `json:"observed_at"`
	ExpiresAt         *time.Time                     `json:"expires_at,omitempty"`
	RoutingGeneration uint64                         `json:"routing_generation"`
	Authenticated     bool                           `json:"authenticated"`
}

type CapabilityEvidenceUseCoreV1 struct {
	SchemaVersion  int                            `json:"schema_version"`
	AccountKeyHMAC string                         `json:"account_key_hmac"`
	Capability     string                         `json:"capability"`
	ProductSurface string                         `json:"product_surface"`
	AccessPath     string                         `json:"access_path"`
	AuthMode       string                         `json:"auth_mode"`
	RequestedModel string                         `json:"requested_model"`
	EffectiveModel string                         `json:"effective_model"`
	State          CapabilityRoutingEvidenceState `json:"state"`
	Source         string                         `json:"source"`
	ObservedAt     time.Time                      `json:"observed_at"`
	ExpiresAt      *time.Time                     `json:"expires_at"`
}

type CapabilityRouteScopeCoreV1 struct {
	SchemaVersion int                           `json:"schema_version"`
	FinalScope    CapabilityFinalScopeCoreV1    `json:"final_scope"`
	Predicates    []CapabilityPredicateCoreV1   `json:"predicates"`
	EvidenceUsed  []CapabilityEvidenceUseCoreV1 `json:"evidence_used"`
}

type RoutingPolicySnapshotV1 struct {
	SchemaVersion     int                         `json:"schema_version"`
	Active            bool                        `json:"active"`
	RoutingGeneration uint64                      `json:"routing_generation"`
	Pool              AccountPoolV1               `json:"pool"`
	Predicates        []CapabilityPredicateCoreV1 `json:"predicates"`
}

type CapabilityAccountWorkspaceV1 struct {
	AccountKey providerCodex.AccountKey `json:"account_key"`
	Workspace  string                   `json:"workspace"`
}

type CallerRequestAuthorityV1 struct {
	SchemaVersion     int                            `json:"schema_version"`
	AllowedAccounts   []providerCodex.AccountKey     `json:"allowed_accounts"`
	PreferredAccount  providerCodex.AccountKey       `json:"preferred_account,omitempty"`
	AccountWorkspaces []CapabilityAccountWorkspaceV1 `json:"account_workspaces"`
	EvaluatedAt       time.Time                      `json:"evaluated_at"`
	FinalScope        CapabilityFinalScopeCoreV1     `json:"final_scope"`
}

type FinalRouteChoiceV1 struct {
	SchemaVersion     int                           `json:"schema_version"`
	AccountKey        providerCodex.AccountKey      `json:"account_key"`
	AllowedAccounts   []providerCodex.AccountKey    `json:"allowed_accounts"`
	Pool              string                        `json:"pool"`
	RoutingGeneration uint64                        `json:"routing_generation"`
	FinalScope        CapabilityFinalScopeCoreV1    `json:"final_scope"`
	Predicates        []CapabilityPredicateCoreV1   `json:"predicates"`
	EvidenceUsed      []CapabilityEvidenceUseCoreV1 `json:"evidence_used"`
	RouteScope        CapabilityRouteScopeCoreV1    `json:"route_scope"`
	Digest            string                        `json:"digest"`
}

type CapabilityRoutingEvidenceSource interface {
	Load(context.Context) ([]CapabilityRoutingEvidenceV1, error)
}

func ResolveCapabilityRouteFromSource(ctx context.Context, policy RoutingPolicySnapshotV1, source CapabilityRoutingEvidenceSource, request CallerRequestAuthorityV1) (FinalRouteChoiceV1, error) {
	if !policy.Active {
		return FinalRouteChoiceV1{}, ErrCapabilityRoutingInactive
	}
	if source == nil {
		return FinalRouteChoiceV1{}, fmt.Errorf("%w: nil evidence source", ErrCapabilityRouteUnavailable)
	}
	evidence, err := source.Load(ctx)
	if err != nil {
		return FinalRouteChoiceV1{}, fmt.Errorf("%w: load evidence: %v", ErrCapabilityRouteUnavailable, err)
	}
	return ResolveCapabilityRoute(policy, evidence, request)
}

func ResolveCapabilityRoute(policy RoutingPolicySnapshotV1, evidence []CapabilityRoutingEvidenceV1, request CallerRequestAuthorityV1) (FinalRouteChoiceV1, error) {
	if !policy.Active {
		return FinalRouteChoiceV1{}, ErrCapabilityRoutingInactive
	}
	if err := validateCapabilityRoutingInputs(policy, request); err != nil {
		return FinalRouteChoiceV1{}, fmt.Errorf("%w: %v", ErrCapabilityRouteUnavailable, err)
	}

	callerAccounts := make(map[providerCodex.AccountKey]struct{}, len(request.AllowedAccounts))
	for _, account := range request.AllowedAccounts {
		callerAccounts[account] = struct{}{}
	}
	poolAccounts := make([]providerCodex.AccountKey, 0, len(policy.Pool.Members))
	for _, account := range policy.Pool.Members {
		if _, allowed := callerAccounts[account]; allowed {
			poolAccounts = append(poolAccounts, account)
		}
	}
	poolAccounts = sortedAccountKeys(poolAccounts)

	eligible := make([]providerCodex.AccountKey, 0, len(poolAccounts))
	usedByAccount := make(map[providerCodex.AccountKey][]CapabilityEvidenceUseCoreV1, len(poolAccounts))
	for _, account := range poolAccounts {
		used, ok := capabilityEvidenceAllowsAccount(account, policy, evidence, request)
		if ok {
			eligible = append(eligible, account)
			usedByAccount[account] = used
		}
	}
	if len(eligible) == 0 {
		return FinalRouteChoiceV1{}, ErrCapabilityRouteUnavailable
	}

	predicates := append([]CapabilityPredicateCoreV1(nil), policy.Predicates...)
	sort.Slice(predicates, func(i, j int) bool {
		return capabilityPredicateKey(predicates[i]) < capabilityPredicateKey(predicates[j])
	})
	selected := eligible[0]
	for _, account := range eligible {
		if account == request.PreferredAccount {
			selected = account
			break
		}
	}
	choice := FinalRouteChoiceV1{
		SchemaVersion: 1, AccountKey: selected, AllowedAccounts: eligible,
		Pool: policy.Pool.Name, RoutingGeneration: policy.RoutingGeneration,
		FinalScope: request.FinalScope, Predicates: predicates,
		EvidenceUsed: usedByAccount[selected],
	}
	choice.RouteScope = CapabilityRouteScopeCoreV1{SchemaVersion: 1, FinalScope: choice.FinalScope, Predicates: choice.Predicates, EvidenceUsed: choice.EvidenceUsed}
	encoded, err := CanonicalJSONV1(choice)
	if err != nil {
		return FinalRouteChoiceV1{}, err
	}
	choice.Digest, err = FramedSHA256Hex("cq/final-route-choice/v1\x00", encoded)
	if err != nil {
		return FinalRouteChoiceV1{}, err
	}
	return choice, nil
}

func validateCapabilityRoutingInputs(policy RoutingPolicySnapshotV1, request CallerRequestAuthorityV1) error {
	if policy.SchemaVersion != 1 || policy.RoutingGeneration == 0 || !poolNamePattern.MatchString(policy.Pool.Name) || len(policy.Pool.Members) == 0 || len(policy.Predicates) == 0 {
		return errors.New("invalid routing policy")
	}
	seenAccounts := make(map[providerCodex.AccountKey]struct{}, len(policy.Pool.Members))
	for _, account := range policy.Pool.Members {
		if account == "" {
			return errors.New("empty policy account")
		}
		if _, duplicate := seenAccounts[account]; duplicate {
			return errors.New("duplicate policy account")
		}
		seenAccounts[account] = struct{}{}
	}
	seenPredicates := make(map[string]struct{}, len(policy.Predicates))
	for _, predicate := range policy.Predicates {
		if !validCapabilityPredicate(predicate) || !predicateMatchesFinalScope(predicate, request.FinalScope) {
			return errors.New("invalid capability predicate")
		}
		key := capabilityPredicateKey(predicate)
		if _, duplicate := seenPredicates[key]; duplicate {
			return errors.New("duplicate capability predicate")
		}
		seenPredicates[key] = struct{}{}
	}
	if request.SchemaVersion != 1 || request.EvaluatedAt.IsZero() || len(request.AllowedAccounts) == 0 || len(request.AccountWorkspaces) == 0 || !validFinalScope(request.FinalScope) {
		return errors.New("invalid caller request authority")
	}
	seenAllowed := make(map[providerCodex.AccountKey]struct{}, len(request.AllowedAccounts))
	for _, account := range request.AllowedAccounts {
		if account == "" {
			return errors.New("empty caller account")
		}
		if _, duplicate := seenAllowed[account]; duplicate {
			return errors.New("duplicate caller account")
		}
		seenAllowed[account] = struct{}{}
	}
	seenWorkspaces := make(map[providerCodex.AccountKey]struct{}, len(request.AccountWorkspaces))
	for _, workspace := range request.AccountWorkspaces {
		if workspace.AccountKey == "" || workspace.Workspace == "" {
			return errors.New("invalid account workspace")
		}
		if _, allowed := seenAllowed[workspace.AccountKey]; !allowed {
			return errors.New("workspace account outside caller authority")
		}
		if _, duplicate := seenWorkspaces[workspace.AccountKey]; duplicate {
			return errors.New("duplicate account workspace")
		}
		seenWorkspaces[workspace.AccountKey] = struct{}{}
	}
	return nil
}

func validCapabilityPredicate(predicate CapabilityPredicateCoreV1) bool {
	return predicate.SchemaVersion == 1 && predicate.Capability != "" && predicate.ProductSurface != "" && predicate.AccessPath != "" && predicate.AuthMode != "" && predicate.RequestedModel != "" && predicate.EffectiveModel != ""
}

func validFinalScope(scope CapabilityFinalScopeCoreV1) bool {
	if scope.SchemaVersion != 1 || scope.RouteID == "" || (scope.Provider != "claude" && scope.Provider != "codex") || (scope.TransportKind != "http" && scope.TransportKind != "websocket") {
		return false
	}
	if scope.ProductSurface == "" || scope.AccessPath == "" || scope.AuthMode == "" || scope.RequestedModel == "" || scope.EffectiveModel == "" || scope.OutboundModel != scope.EffectiveModel {
		return false
	}
	return lowerHexDigest(scope.TransformationDigest) && lowerHexDigest(scope.EncodedRequestDigest) && lowerHexDigest(scope.NormalCredentialOriginBindingDigest)
}

func capabilityEvidenceAllowsAccount(account providerCodex.AccountKey, policy RoutingPolicySnapshotV1, evidence []CapabilityRoutingEvidenceV1, request CallerRequestAuthorityV1) ([]CapabilityEvidenceUseCoreV1, bool) {
	workspace := ""
	for _, scoped := range request.AccountWorkspaces {
		if scoped.AccountKey == account {
			workspace = scoped.Workspace
			break
		}
	}
	if workspace == "" {
		return nil, false
	}
	used := make([]CapabilityEvidenceUseCoreV1, 0, len(policy.Predicates))
	for _, predicate := range policy.Predicates {
		matched := false
		for _, item := range evidence {
			if item.AccountKey != account || item.Workspace != workspace || !evidenceMatchesPredicate(item, predicate) {
				continue
			}
			if !validCapabilityEvidence(item, policy.RoutingGeneration, request.EvaluatedAt) || item.State != CapabilityEvidenceEligible {
				return nil, false
			}
			matched = true
			used = append(used, capabilityEvidenceUse(item))
		}
		if !matched {
			return nil, false
		}
	}
	sort.Slice(used, func(i, j int) bool { return capabilityEvidenceLess(used[i], used[j]) })
	return used, true
}

func validCapabilityEvidence(item CapabilityRoutingEvidenceV1, generation uint64, evaluatedAt time.Time) bool {
	if item.SchemaVersion != 1 || item.AccountKey == "" || !lowerHexDigest(item.AccountKeyHMAC) || item.Workspace == "" || item.Source == "" || !item.Authenticated || item.RoutingGeneration != generation || item.ObservedAt.IsZero() || item.ObservedAt.After(evaluatedAt) {
		return false
	}
	for _, field := range []string{item.Capability, item.ProductSurface, item.AccessPath, item.AuthMode, item.RequestedModel, item.EffectiveModel} {
		if field == "" || field == "*" {
			return false
		}
	}
	if item.State != CapabilityEvidenceEligible && item.State != CapabilityEvidenceIneligible && item.State != CapabilityEvidenceUnknown {
		return false
	}
	return item.ExpiresAt == nil || item.ExpiresAt.After(evaluatedAt)
}

func evidenceMatchesPredicate(item CapabilityRoutingEvidenceV1, predicate CapabilityPredicateCoreV1) bool {
	return item.Capability == predicate.Capability && wildcardEqual(predicate.ProductSurface, item.ProductSurface) && wildcardEqual(predicate.AccessPath, item.AccessPath) && wildcardEqual(predicate.AuthMode, item.AuthMode) && wildcardEqual(predicate.RequestedModel, item.RequestedModel) && wildcardEqual(predicate.EffectiveModel, item.EffectiveModel)
}

func wildcardEqual(predicate, actual string) bool { return predicate == "*" || predicate == actual }

func predicateMatchesFinalScope(predicate CapabilityPredicateCoreV1, scope CapabilityFinalScopeCoreV1) bool {
	return wildcardEqual(predicate.ProductSurface, scope.ProductSurface) && wildcardEqual(predicate.AccessPath, scope.AccessPath) && wildcardEqual(predicate.AuthMode, scope.AuthMode) && wildcardEqual(predicate.RequestedModel, scope.RequestedModel) && wildcardEqual(predicate.EffectiveModel, scope.OutboundModel)
}

func capabilityPredicateKey(predicate CapabilityPredicateCoreV1) string {
	return strings.Join([]string{predicate.Capability, predicate.ProductSurface, predicate.AccessPath, predicate.AuthMode, predicate.RequestedModel, predicate.EffectiveModel}, "\x00")
}

func capabilityEvidenceLess(left, right CapabilityEvidenceUseCoreV1) bool {
	leftFields := []string{left.AccountKeyHMAC, left.Capability, left.ProductSurface, left.AccessPath, left.AuthMode, left.RequestedModel, left.EffectiveModel, left.Source, left.ObservedAt.UTC().Format(time.RFC3339Nano)}
	rightFields := []string{right.AccountKeyHMAC, right.Capability, right.ProductSurface, right.AccessPath, right.AuthMode, right.RequestedModel, right.EffectiveModel, right.Source, right.ObservedAt.UTC().Format(time.RFC3339Nano)}
	for index := range leftFields {
		if leftFields[index] != rightFields[index] {
			return leftFields[index] < rightFields[index]
		}
	}
	if (left.ExpiresAt == nil) != (right.ExpiresAt == nil) {
		return left.ExpiresAt != nil
	}
	if left.ExpiresAt == nil {
		return false
	}
	return left.ExpiresAt.Before(*right.ExpiresAt)
}

func capabilityEvidenceUse(item CapabilityRoutingEvidenceV1) CapabilityEvidenceUseCoreV1 {
	return CapabilityEvidenceUseCoreV1{
		SchemaVersion: 1, AccountKeyHMAC: item.AccountKeyHMAC, Capability: item.Capability,
		ProductSurface: item.ProductSurface, AccessPath: item.AccessPath, AuthMode: item.AuthMode,
		RequestedModel: item.RequestedModel, EffectiveModel: item.EffectiveModel, State: item.State,
		Source: item.Source, ObservedAt: item.ObservedAt, ExpiresAt: item.ExpiresAt,
	}
}
