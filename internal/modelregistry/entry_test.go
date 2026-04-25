package modelregistry

import (
	"testing"
)

func TestEntryValidate_Valid(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
	}{
		{
			name:  "native anthropic entry",
			entry: Entry{Provider: ProviderAnthropic, ID: "claude-3-5-sonnet-20241022", Source: SourceNative},
		},
		{
			name:  "overlay codex entry",
			entry: Entry{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceOverlay},
		},
		{
			name:  "entry with all optional fields",
			entry: Entry{
				Provider:        ProviderAnthropic,
				ID:              "claude-opus-4-5",
				Source:          SourceNative,
				Aliases:         []string{"claude-opus"},
				DisplayName:     "Claude Opus 4.5",
				Description:     "Most capable model",
				ContextWindow:   200000,
				MaxOutputTokens: 32000,
				Visibility:      "public",
				Priority:        10,
				CloneFrom:       "",
				InferredFrom:    "api",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.entry.Validate(); err != nil {
				t.Errorf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func TestEntryValidate_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
	}{
		{
			name:  "empty provider",
			entry: Entry{Provider: "", ID: "some-model", Source: SourceNative},
		},
		{
			name:  "empty id",
			entry: Entry{Provider: ProviderAnthropic, ID: "", Source: SourceNative},
		},
		{
			name:  "unknown provider",
			entry: Entry{Provider: "openai", ID: "gpt-4", Source: SourceNative},
		},
		{
			name:  "unknown source",
			entry: Entry{Provider: ProviderAnthropic, ID: "claude-3", Source: "unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.entry.Validate(); err == nil {
				t.Errorf("Validate() expected error for %+v, got nil", tt.entry)
			}
		})
	}
}

func TestEntryValidate_EmptySourceAllowed(t *testing.T) {
	// An empty Source string should be treated as valid (unset, not "unknown").
	// This matches the omitempty JSON behaviour — source is optional on ingress.
	e := Entry{Provider: ProviderAnthropic, ID: "claude-3", Source: ""}
	if err := e.Validate(); err != nil {
		t.Errorf("Validate() unexpected error for empty source: %v", err)
	}
}
