package proxy

import (
	"net/http"
	"testing"
)

func TestRouteModel(t *testing.T) {
	tests := []struct {
		model string
		want  Provider
	}{
		// Claude models
		{"claude-3-opus-20240229", ProviderClaude},
		{"claude-3-5-sonnet-20241022", ProviderClaude},
		{"claude-haiku-4-5-20251001", ProviderClaude},
		{"claude-opus-4-6", ProviderClaude},

		// GPT models
		{"gpt-5.4", ProviderCodex},
		{"gpt-5.4-mini", ProviderCodex},
		{"gpt-4o", ProviderCodex},
		{"GPT-5.4", ProviderCodex}, // case insensitive

		// O-series models
		{"o1", ProviderCodex},
		{"o1-preview", ProviderCodex},
		{"o3-mini", ProviderCodex},
		{"o4-mini", ProviderCodex},

		// Codex models
		{"codex-mini", ProviderCodex},
		{"codex-mini-latest", ProviderCodex},

		// Unknown/empty → Claude (backward compat)
		{"", ProviderClaude},
		{"some-unknown-model", ProviderClaude},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := RouteModel(tt.model)
			if got != tt.want {
				t.Errorf("RouteModel(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestParseModelEffort(t *testing.T) {
	tests := []struct {
		input      string
		wantModel  string
		wantEffort string
	}{
		{"gpt-5.4", "gpt-5.4", ""},
		{"gpt-5.4-xhigh", "gpt-5.4", "xhigh"},
		{"gpt-5.4-high", "gpt-5.4", "high"},
		{"gpt-5.4-medium", "gpt-5.4", "medium"},
		{"gpt-5.4-low", "gpt-5.4", "low"},
		{"gpt-5.4-mini-xhigh", "gpt-5.4-mini", "xhigh"},
		{"gpt-5.4-mini-low", "gpt-5.4-mini", "low"},
		{"claude-3-opus", "claude-3-opus", ""},
		{"o4-mini-high", "o4-mini", "high"},
		{"GPT-5.4-XHIGH", "GPT-5.4", "xhigh"}, // case insensitive suffix
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			model, effort := ParseModelEffort(tt.input)
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if effort != tt.wantEffort {
				t.Errorf("effort = %q, want %q", effort, tt.wantEffort)
			}
		})
	}
}

func TestRouteModel_WithEffortSuffix(t *testing.T) {
	tests := []struct {
		model string
		want  Provider
	}{
		{"gpt-5.4-xhigh", ProviderCodex},
		{"gpt-5.4-mini-low", ProviderCodex},
		{"o4-mini-high", ProviderCodex},
		{"claude-3-opus-high", ProviderClaude}, // suffix stripped → claude-3-opus → Claude
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := RouteModel(tt.model)
			if got != tt.want {
				t.Errorf("RouteModel(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestSyntheticModelCatalogRoutesViaRouteModel(t *testing.T) {
	for _, model := range SyntheticModelCatalog() {
		if got := RouteModel(model.ID); got != ProviderCodex {
			t.Fatalf("RouteModel(%q) = %d, want %d", model.ID, got, ProviderCodex)
		}
	}
}

func TestExtractModel(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"valid", `{"model":"gpt-5.4","messages":[]}`, "gpt-5.4"},
		{"empty body", ``, ""},
		{"no model field", `{"messages":[]}`, ""},
		{"invalid json", `{bad`, ""},
		{"claude model", `{"model":"claude-3-opus-20240229"}`, "claude-3-opus-20240229"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractModel([]byte(tt.body))
			if got != tt.want {
				t.Errorf("extractModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRouteRequest(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		model  string
		want   Provider
	}{
		{"count tokens always claude", http.MethodPost, countTokensPath, "gpt-5.4", ProviderClaude},
		{"messages codex model", http.MethodPost, "/v1/messages", "gpt-5.4", ProviderCodex},
		{"messages claude model", http.MethodPost, "/v1/messages", "claude-opus-4-6", ProviderClaude},
		{"non-post count tokens by model", http.MethodGet, countTokensPath, "gpt-5.4", ProviderCodex},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RouteRequest(tt.method, tt.path, tt.model)
			if got != tt.want {
				t.Errorf("RouteRequest(%q, %q, %q) = %d, want %d", tt.method, tt.path, tt.model, got, tt.want)
			}
		})
	}
}
