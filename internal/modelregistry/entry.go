package modelregistry

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
)

// Provider identifies the upstream API provider for a model.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderCodex     Provider = "codex"
	ProviderZAI       Provider = "zai"
	ProviderGrok      Provider = "grok"
)

// SharedBudgetModel is model metadata supplied by a shared-budget subscription.
// Its presence distinguishes a subscription route from a first-party catalogue entry.
type SharedBudgetModel struct {
	ProviderName     string   `json:"provider_name"`
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	// CredentialScope ties live catalogue authority to the credential that fetched it.
	CredentialScope SharedBudgetCredentialScope `json:"-"`
}

// SharedBudgetCredentialScope identifies the account and workspace that own a
// catalogue or response. OAuth access-token refresh preserves that identity.
type SharedBudgetCredentialScope [sha256.Size]byte

func FingerprintSharedBudgetCredential(bearer, account, workspace string) SharedBudgetCredentialScope {
	if account == "" && workspace == "" {
		return sha256.Sum256([]byte(bearer))
	}
	// Domain-separate scoped keys and OAuth from each other and raw API keys.
	identity := [3]string{"oauth", account, workspace}
	if account == "" {
		identity = [3]string{"key", bearer, workspace}
	}
	data, _ := json.Marshal(identity)
	return sha256.Sum256(append([]byte{0}, data...))
}

func (p Provider) isFirstParty() bool {
	switch p {
	case ProviderAnthropic, ProviderCodex, ProviderZAI, ProviderGrok:
		return true
	default:
		return false
	}
}

func (p Provider) isSharedBudgetID() bool {
	if p == "" || p[0] < 'a' || p[0] > 'z' || p.isFirstParty() {
		return false
	}
	for _, char := range p {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

// Source indicates where a registry entry originated.
type Source string

const (
	SourceNative  Source = "native"
	SourceOverlay Source = "overlay"
)

// Entry is a single model record in the registry.
type Entry struct {
	Provider         Provider           `json:"provider"`
	ID               string             `json:"id"`
	Aliases          []string           `json:"aliases,omitempty"`
	DisplayName      string             `json:"display_name,omitempty"`
	Description      string             `json:"description,omitempty"`
	ContextWindow    int                `json:"context_window,omitempty"`
	MaxContextWindow int                `json:"max_context_window,omitempty"`
	MaxOutputTokens  int                `json:"max_output_tokens,omitempty"`
	Visibility       string             `json:"visibility,omitempty"`
	Priority         int                `json:"priority,omitempty"`
	Source           Source             `json:"source"`
	CloneFrom        string             `json:"clone_from,omitempty"`
	InferredFrom     string             `json:"inferred_from,omitempty"`
	SharedBudget     *SharedBudgetModel `json:"shared_budget,omitempty"`
	// Raw holds the original provider JSON for pass-through use.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Validate returns an error if the entry is missing required fields or contains
// unrecognised enumeration values.
func (e Entry) Validate() error {
	if e.Provider == "" {
		return errors.New("modelregistry: entry missing provider")
	}
	if e.SharedBudget != nil {
		if !e.Provider.isSharedBudgetID() || e.Source != SourceNative ||
			e.ID != strings.TrimSpace(e.ID) || strings.TrimSpace(e.SharedBudget.ProviderName) == "" || e.ContextWindow <= 0 {
			return errors.New("modelregistry: invalid shared-budget entry")
		}
	} else if !e.Provider.isFirstParty() {
		return errors.New("modelregistry: unknown provider " + string(e.Provider))
	}
	if e.ID == "" {
		return errors.New("modelregistry: entry missing id")
	}
	// Source is optional (omitempty); only validate when non-empty.
	if e.Source != "" {
		switch e.Source {
		case SourceNative, SourceOverlay:
		default:
			return errors.New("modelregistry: unknown source " + string(e.Source))
		}
	}
	return nil
}
