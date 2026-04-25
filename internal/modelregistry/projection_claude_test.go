package modelregistry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// --- ClaudeCapabilitiesProjection tests ---

func makeAnthropicSnapshot(entries ...Entry) Snapshot {
	rawByID := make(map[string]json.RawMessage)
	for _, e := range entries {
		raw, _ := json.Marshal(map[string]any{
			"id":                e.ID,
			"display_name":      e.DisplayName,
			"context_window":    e.ContextWindow,
			"max_output_tokens": e.MaxOutputTokens,
		})
		rawByID[e.ID] = raw
	}
	return Snapshot{
		Entries:          entries,
		AnthropicRawByID: rawByID,
		FetchedAt:        time.Now(),
	}
}

func TestClaudeCapabilitiesProjection_EmptyAnthropicEntriesUsesEmptyArray(t *testing.T) {
	payload := ClaudeCapabilitiesProjection(Snapshot{Entries: []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceNative},
	}}, 1700000000)

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(data) != `{"timestamp":1700000000,"models":[]}` {
		t.Fatalf("payload JSON = %s, want empty models array", data)
	}
}

func TestClaudeCapabilitiesProjection_NumericTimestamp(t *testing.T) {
	snap := makeAnthropicSnapshot(Entry{
		Provider:        ProviderAnthropic,
		ID:              "claude-opus-4",
		DisplayName:     "Claude Opus 4",
		ContextWindow:   200000,
		MaxOutputTokens: 32000,
		Source:          SourceNative,
	})

	ts := int64(1700000000)
	result := ClaudeCapabilitiesProjection(snap, ts)

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Timestamp int64            `json:"timestamp"`
		Models    []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Timestamp != ts {
		t.Errorf("Timestamp = %d, want %d", decoded.Timestamp, ts)
	}
	if len(decoded.Models) == 0 {
		t.Fatal("models array is empty")
	}
}

func TestClaudeCapabilitiesProjection_LongestIDFirst(t *testing.T) {
	snap := makeAnthropicSnapshot(
		Entry{Provider: ProviderAnthropic, ID: "a", ContextWindow: 100, Source: SourceNative},
		Entry{Provider: ProviderAnthropic, ID: "claude-opus-4-20260101", ContextWindow: 200000, Source: SourceNative},
		Entry{Provider: ProviderAnthropic, ID: "claude-opus-4", ContextWindow: 200000, Source: SourceNative},
	)

	result := ClaudeCapabilitiesProjection(snap, 0)

	data, _ := json.Marshal(result)
	var decoded struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Models) < 2 {
		t.Fatalf("expected at least 2 models, got %d", len(decoded.Models))
	}

	// First model should have the longest ID.
	first := decoded.Models[0].ID
	for _, m := range decoded.Models[1:] {
		if len(m.ID) > len(first) {
			t.Errorf("model %q (len=%d) appears after %q (len=%d); want longest-first",
				m.ID, len(m.ID), first, len(first))
		}
	}
}

func TestClaudeCapabilitiesProjection_IncludesKnownLimits(t *testing.T) {
	snap := makeAnthropicSnapshot(Entry{
		Provider:        ProviderAnthropic,
		ID:              "claude-sonnet-4-5",
		DisplayName:     "Claude Sonnet 4.5",
		ContextWindow:   200000,
		MaxOutputTokens: 16000,
		Source:          SourceNative,
	})

	result := ClaudeCapabilitiesProjection(snap, 0)

	data, _ := json.Marshal(result)
	var decoded struct {
		Models []struct {
			ID             string `json:"id"`
			MaxInputTokens int    `json:"max_input_tokens"`
			MaxTokens      int    `json:"max_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var found bool
	for _, m := range decoded.Models {
		if m.ID == "claude-sonnet-4-5" {
			found = true
			if m.MaxInputTokens != 200000 {
				t.Errorf("max_input_tokens = %d, want 200000", m.MaxInputTokens)
			}
			if m.MaxTokens != 16000 {
				t.Errorf("max_tokens = %d, want 16000", m.MaxTokens)
			}
		}
	}
	if !found {
		t.Error("claude-sonnet-4-5 not found in capabilities projection")
	}
}

// --- PublishClaudeCapabilities tests ---

func TestPublishClaudeCapabilities_WritesNumericTimestamp(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude/cache/model-capabilities.json"

	snap := makeAnthropicSnapshot(Entry{
		Provider:        ProviderAnthropic,
		ID:              "claude-opus-4",
		ContextWindow:   200000,
		MaxOutputTokens: 32000,
		Source:          SourceNative,
	})

	now := time.Unix(1700000000, 0)
	if err := PublishClaudeCapabilities(fsys, path, snap, now); err != nil {
		t.Fatalf("PublishClaudeCapabilities() error = %v", err)
	}

	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Timestamp != now.Unix() {
		t.Errorf("timestamp = %d, want %d", got.Timestamp, now.Unix())
	}
}

func TestPublishClaudeCapabilities_ReplacesStringTimestamp(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude/cache/model-capabilities.json"

	// Write existing cache with old string timestamp.
	existing := `{"timestamp":"2024-01-01T00:00:00Z","models":[{"id":"old-model","max_input_tokens":1000,"max_tokens":100}]}`
	_ = fsys.WriteFile(path, []byte(existing), 0o600)

	snap := makeAnthropicSnapshot(Entry{
		Provider:        ProviderAnthropic,
		ID:              "claude-opus-4",
		ContextWindow:   200000,
		MaxOutputTokens: 32000,
		Source:          SourceNative,
	})

	now := time.Unix(1700000000, 0)
	if err := PublishClaudeCapabilities(fsys, path, snap, now); err != nil {
		t.Fatalf("PublishClaudeCapabilities() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Timestamp must be numeric (not a quoted string).
	raw := strings.TrimSpace(string(got.Timestamp))
	if strings.HasPrefix(raw, `"`) {
		t.Errorf("timestamp is still a string: %s", raw)
	}

	var decoded struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal capabilities: %v", err)
	}
	for _, model := range decoded.Models {
		if model.ID == "old-model" {
			t.Error("old-model from stale cache must not appear after full replace")
		}
	}
}

func TestPublishClaudeCapabilities_AtomicNoTempFile(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	path := filepath.Join(cacheDir, "model-capabilities.json")
	fsys := fsutil.OSFileSystem{}

	snap := makeAnthropicSnapshot(Entry{
		Provider:        ProviderAnthropic,
		ID:              "claude-opus-4",
		ContextWindow:   200000,
		MaxOutputTokens: 32000,
		Source:          SourceNative,
	})

	if err := PublishClaudeCapabilities(fsys, path, snap, time.Now()); err != nil {
		t.Fatalf("PublishClaudeCapabilities() error = %v", err)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestPublishClaudeCapabilities_OnlyAnthropicEntries(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude/cache/model-capabilities.json"

	// Snapshot contains both Anthropic and Codex entries.
	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-opus-4", ContextWindow: 200000, MaxOutputTokens: 32000, Source: SourceNative},
			{Provider: ProviderCodex, ID: "gpt-5.4", ContextWindow: 1050000, Source: SourceNative},
		},
		FetchedAt: time.Now(),
	}

	if err := PublishClaudeCapabilities(fsys, path, snap, time.Now()); err != nil {
		t.Fatalf("PublishClaudeCapabilities() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, m := range got.Models {
		if m.ID == "gpt-5.4" {
			t.Error("codex entry gpt-5.4 must not appear in Claude capabilities cache")
		}
	}
}
