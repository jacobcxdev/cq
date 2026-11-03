package modelregistry

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestZAIProjectionCompactionLimit(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         int64
	}{
		{"current window", `"context_window":1048576,"max_context_window":1048576,"effective_context_window_percent":95`, 697302},
		{"default headroom", `"context_window":1048576`, 697302},
		{"changed provider window", `"context_window":2000000,"effective_context_window_percent":90`, 1260000},
		{"maximum fallback", `"max_context_window":1000000,"effective_context_window_percent":100`, 700000},
		{"earlier provider limit", `"context_window":1048576,"auto_compact_token_limit":600000`, 600000},
		{"later provider limit", `"context_window":1048576,"auto_compact_token_limit":900000`, 697302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(`{"slug":"glm-5.3","display_name":"GLM-5.3","future_integer":9007199254740993,` + tc.fields + `}`)
			before := map[string]json.RawMessage{}
			if err := json.Unmarshal(raw, &before); err != nil {
				t.Fatal(err)
			}
			entry := Entry{Provider: ProviderZAI, ID: "glm-5.3", Source: SourceNative, Raw: raw}
			response := CodexModelsResponse(Snapshot{Entries: []Entry{entry}})
			after := map[string]json.RawMessage{}
			if err := json.Unmarshal(response.Models[0], &after); err != nil {
				t.Fatal(err)
			}
			var limit int64
			if err := json.Unmarshal(after["auto_compact_token_limit"], &limit); err != nil || limit != tc.want {
				t.Fatalf("compaction limit = %d, want %d (error %v)", limit, tc.want, err)
			}
			delete(before, "auto_compact_token_limit")
			delete(after, "auto_compact_token_limit")
			if !reflect.DeepEqual(before, after) || string(entry.Raw) != string(raw) {
				t.Fatal("compaction policy changed provider metadata")
			}
			augmented, err := AugmentCodexModels([]byte(`{"models":[]}`), Snapshot{Entries: []Entry{entry}})
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct{ Models []json.RawMessage }
			if json.Unmarshal(augmented, &envelope) != nil || string(envelope.Models[0]) != string(response.Models[0]) {
				t.Fatal("live catalogue omitted compaction policy")
			}
		})
	}
}

func TestZAIProjectionPreservesOtherModelsAndInvalidWindows(t *testing.T) {
	for _, raw := range []string{
		`{"slug":"glm-5.3-flash","context_window":1048576}`,
		`{"slug":"glm-5.4","context_window":2097152,"auto_compact_token_limit":1800000}`,
		`{"slug":"glm-5-turbo","context_window":204800}`,
		`{"slug":"glm-5.3"}`,
		`{"slug":"glm-5.3","context_window":-1}`,
		`{"slug":"glm-5.3","context_window":1048576,"effective_context_window_percent":0}`,
		`{"slug":"glm-5.3","context_window":1048576,"effective_context_window_percent":101}`,
	} {
		var identity struct{ Slug string }
		if err := json.Unmarshal([]byte(raw), &identity); err != nil {
			t.Fatal(err)
		}
		response := CodexModelsResponse(Snapshot{Entries: []Entry{{Provider: ProviderZAI, ID: identity.Slug, Raw: json.RawMessage(raw)}}})
		if string(response.Models[0]) != raw {
			t.Fatalf("unrelated or unusable model metadata changed: %s", response.Models[0])
		}
	}
}
