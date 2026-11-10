package proxy

import (
	"net/http"
	"regexp"
	"strings"
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
		input     string
		wantModel string
	}{
		// [1m] suffix is stripped; no effort is extracted from model name.
		{"gpt-5.4", "gpt-5.4"},
		{"gpt-5.4[1m]", "gpt-5.4"},
		{"gpt-5.4-mini", "gpt-5.4-mini"},
		{"claude-3-opus", "claude-3-opus"},
		{"o4-mini", "o4-mini"},
		// Suffix-like strings that are not [1m] are left as-is.
		{"gpt-5.4-xhigh", "gpt-5.4-xhigh"},
		{"gpt-5.4-high", "gpt-5.4-high"},
		{"gpt-5.4-medium", "gpt-5.4-medium"},
		{"gpt-5.4-low", "gpt-5.4-low"},
		{"gpt-5.4-mini-xhigh", "gpt-5.4-mini-xhigh"},
		{"o4-mini-high", "o4-mini-high"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			model := ParseModel(tt.input)
			if model != tt.wantModel {
				t.Errorf("ParseModel(%q) = %q, want %q", tt.input, model, tt.wantModel)
			}
		})
	}
}

func TestParseModelMatchesSuffixRegexp(t *testing.T) {
	previous := regexp.MustCompile(`(?i)\[1m\]$`)
	prefixes := []string{"", "gpt-5.4", "GPT-5.4-XHIGH", "模型", "\xff\xfe", "line\n", strings.Repeat("x", 4096)}
	suffixes := []string{"", "[1m]", "[1M]", "[1m][1M]", "[1m]\n", "[1m] ", "[1ｍ]", "[１m]", "[1ℳ]", "[1m", "1m]", "[1m]\xff"}
	for _, prefix := range prefixes {
		for _, suffix := range suffixes {
			input := prefix + suffix
			if got, want := ParseModel(input), previous.ReplaceAllString(input, ""); got != want {
				t.Fatalf("ParseModel(%q) = %q, want %q", input, got, want)
			}
		}
	}
	// Every byte substitution also covers invalid UTF-8 and suffix boundaries.
	for index := range len("[1m]") {
		for value := range 256 {
			suffix := []byte("[1m]")
			suffix[index] = byte(value)
			input := "gpt-5.4" + string(suffix)
			if got, want := ParseModel(input), previous.ReplaceAllString(input, ""); got != want {
				t.Fatalf("ParseModel(%q) = %q, want %q", input, got, want)
			}
		}
	}
}

func TestParseModelDoesNotAllocate(t *testing.T) {
	for _, input := range []string{"gpt-5.4", "gpt-5.4[1m]", "gpt-5.4[1M]"} {
		if allocations := testing.AllocsPerRun(100, func() { _ = ParseModel(input) }); allocations != 0 {
			t.Fatalf("ParseModel(%q) allocated %g times", input, allocations)
		}
	}
}

func TestRouteModel_WithOneMSuffix(t *testing.T) {
	tests := []struct {
		model string
		want  Provider
	}{
		{"gpt-5.4", ProviderCodex},
		{"gpt-5.4[1m]", ProviderCodex},
		{"gpt-5.4-mini", ProviderCodex},
		{"o4-mini", ProviderCodex},
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
		{"1m suffix", `{"model":"gpt-5.4[1m]","messages":[]}`, "gpt-5.4[1m]"},
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
		{"count tokens gpt routes codex", http.MethodPost, countTokensPath, "gpt-5.4", ProviderCodex},
		{"count tokens claude routes claude", http.MethodPost, countTokensPath, "claude-opus-4-6", ProviderClaude},
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
