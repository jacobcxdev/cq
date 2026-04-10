package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ModelMetadata is the minimal capability shape used by the proxy model catalogue,
// the /v1/models response, and Claude Code's model-capabilities cache.
type ModelMetadata struct {
	ID             string `json:"id"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
}

var syntheticModelCatalog = []ModelMetadata{
	{ID: "gpt-5.4", MaxInputTokens: 1050000, MaxTokens: 128000},
	{ID: "gpt-5.4-mini", MaxInputTokens: 400000, MaxTokens: 128000},
	{ID: "gpt-4o", MaxInputTokens: 400000, MaxTokens: 128000},
	{ID: "o1", MaxInputTokens: 400000, MaxTokens: 128000},
	{ID: "o1-preview", MaxInputTokens: 400000, MaxTokens: 128000},
	{ID: "o3-mini", MaxInputTokens: 400000, MaxTokens: 128000},
	{ID: "o4-mini", MaxInputTokens: 400000, MaxTokens: 128000},
	{ID: "codex-mini", MaxInputTokens: 400000, MaxTokens: 128000},
	{ID: "codex-mini-latest", MaxInputTokens: 400000, MaxTokens: 128000},
}

func SyntheticModelCatalog() []ModelMetadata {
	out := make([]ModelMetadata, len(syntheticModelCatalog))
	copy(out, syntheticModelCatalog)
	return out
}

// ModelMaxInputTokens returns the synthetic catalogue's max input token limit
// for a model ID, or 0 when cq has no exact match and the caller should fall
// back to upstream resolution.
func ModelMaxInputTokens(model string) int {
	for _, metadata := range syntheticModelCatalog {
		if metadata.ID == model {
			return metadata.MaxInputTokens
		}
	}
	return 0
}

type claudeModelCapabilitiesCache struct {
	Models    []ModelMetadata `json:"models"`
	Timestamp string          `json:"timestamp"`
}

func WriteClaudeCodeModelCapabilitiesCache() error {
	configHome, err := claudeConfigDir()
	if err != nil {
		return err
	}
	return writeClaudeModelCapabilitiesCache(filepath.Join(configHome, "cache", "model-capabilities.json"), nil)
}

func claudeConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		if filepath.IsAbs(dir) {
			return dir, nil
		}
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

func writeClaudeModelCapabilitiesCache(path string, extra []ModelMetadata) error {
	merged := mergeModelMetadata(readClaudeModelCapabilitiesCache(path), extra, SyntheticModelCatalog())
	payload := claudeModelCapabilitiesCache{
		Models:    merged,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal model capabilities cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create model capabilities cache dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write model capabilities cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename model capabilities cache: %w", err)
	}
	return nil
}

func readClaudeModelCapabilitiesCache(path string) []ModelMetadata {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cache claudeModelCapabilitiesCache
	if err := json.Unmarshal(data, &cache); err == nil {
		return cache.Models
	}
	return nil
}

func mergeModelMetadata(groups ...[]ModelMetadata) []ModelMetadata {
	byID := make(map[string]ModelMetadata)
	for _, group := range groups {
		for _, model := range group {
			if model.ID == "" {
				continue
			}
			byID[model.ID] = model
		}
	}
	merged := make([]ModelMetadata, 0, len(byID))
	for _, model := range byID {
		merged = append(merged, model)
	}
	sort.Slice(merged, func(i, j int) bool {
		if len(merged[i].ID) != len(merged[j].ID) {
			return len(merged[i].ID) > len(merged[j].ID)
		}
		return strings.ToLower(merged[i].ID) < strings.ToLower(merged[j].ID)
	})
	return merged
}
