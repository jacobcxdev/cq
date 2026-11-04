package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
)

// ZCodeStartSource projects the public ZCode catalogue for Start-only accounts.
// Quota must return current Start rows validated against the balance response's
// reference time, active plans and actual model-scoped token buckets. Exhaustion
// does not remove an otherwise current model from the picker.
type ZCodeStartSource struct {
	Catalogue *zcode.ModelCatalogue
	Quota     func(context.Context) ([]quota.Result, error)
}

// Fetch implements NativeSource without requiring a paid Coding Plan API key.
func (s *ZCodeStartSource) Fetch(ctx context.Context) (result SourceResult, err error) {
	defer func() {
		if recover() != nil {
			result = SourceResult{}
			err = errors.New("zcode start source: fetch failed")
		}
	}()
	if s == nil || s.Catalogue == nil || s.Quota == nil {
		return SourceResult{}, errors.New("zcode start source: not configured")
	}
	if err := ctx.Err(); err != nil {
		return SourceResult{}, err
	}
	rows, err := s.Quota(ctx)
	if err != nil {
		return SourceResult{}, errors.New("zcode start source: quota unavailable")
	}
	if err := ctx.Err(); err != nil {
		return SourceResult{}, err
	}
	permitted := make(map[string]bool)
	for _, row := range rows {
		if !row.IsUsable() || row.Plan != "ZCode Start" {
			continue
		}
		for name := range row.Windows {
			modelID := quota.WindowBucket(name)
			if modelID != "" && quota.IsAggregable(name) {
				permitted[strings.ToLower(modelID)] = true
			}
		}
	}
	result = SourceResult{Entries: []Entry{}, CodexRawByID: make(map[string]json.RawMessage), FetchedAt: time.Now()}
	if len(permitted) == 0 {
		return result, nil
	}
	models, err := s.Catalogue.Snapshot(ctx)
	if err != nil {
		return SourceResult{}, err
	}
	for _, model := range models {
		id := strings.ToLower(model.ID)
		if !permitted[id] {
			continue
		}
		entry, ok := zcodeStartEntry(model)
		if !ok {
			result.MalformedEntries++
			continue
		}
		result.Entries = append(result.Entries, entry)
		result.CodexRawByID[id] = entry.Raw
	}
	return result, nil
}

// zcodeStartEntry is a CQ Codex-harness projection, not a vendor Codex record.
// IDs, labels, limits, priority and reasoning levels come from public ZCode
// metadata. The shell, patch, truncation, summaries, input headroom and tool
// defaults below are CQ adapter policy. Hosted OpenAI search is unavailable.
func zcodeStartEntry(model zcode.ModelDefinition) (Entry, bool) {
	if model.ContextWindow <= 0 || model.MaxOutputTokens <= 0 {
		return Entry{}, false
	}
	input := []string{"text"} // CQ text baseline for an unspecified modality list.
	if len(model.InputModalities) != 0 {
		input = nil
		for _, supported := range []string{"text", "image"} {
			for _, advertised := range model.InputModalities {
				if advertised == supported {
					input = append(input, supported)
					break
				}
			}
		}
		if len(input) == 0 {
			return Entry{}, false
		}
	} else if model.Vision {
		input = append(input, "image")
	}
	if len(model.OutputModalities) > 0 {
		text := false
		for _, modality := range model.OutputModalities {
			text = text || modality == "text"
		}
		if !text {
			return Entry{}, false
		}
	}

	levels := make([]map[string]string, 0, len(model.ReasoningLevels))
	defaultPresent := model.DefaultReasoningLevel == ""
	// These are Codex's supported effort values, in presentation order. A new
	// advertised model needs no ID-specific rule, but unknown protocol values
	// cannot be offered as working Codex reasoning controls.
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		for _, advertised := range model.ReasoningLevels {
			if advertised == effort {
				levels = append(levels, map[string]string{"effort": effort, "description": effort})
				defaultPresent = defaultPresent || effort == model.DefaultReasoningLevel
				break
			}
		}
	}
	if !defaultPresent {
		return Entry{}, false
	}
	id := strings.ToLower(model.ID)
	label := model.Name
	if label == "" {
		label = model.ID
	}
	record := map[string]any{
		"slug": id, "display_name": label, "description": model.Description,
		"context_window": model.ContextWindow, "max_context_window": model.ContextWindow,
		"max_output_tokens": model.MaxOutputTokens, "priority": model.Priority,
		"supported_reasoning_levels":       levels,
		"effective_context_window_percent": 95,
		"shell_type":                       "shell_command", "visibility": "list", "supported_in_api": true,
		"base_instructions": "", "supports_reasoning_summaries": len(levels) > 0,
		"default_reasoning_summary": "none", "support_verbosity": false,
		"apply_patch_tool_type":        "freeform",
		"truncation_policy":            map[string]any{"mode": "bytes", "limit": codexFallbackTruncationBytesLimit},
		"supports_parallel_tool_calls": true, "experimental_supported_tools": []string{},
		"input_modalities": input, "supports_search_tool": false,
	}
	if model.DefaultReasoningLevel != "" {
		record["default_reasoning_level"] = model.DefaultReasoningLevel
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return Entry{}, false
	}
	return Entry{
		Provider: ProviderZAI, ID: id, DisplayName: label, Description: model.Description,
		ContextWindow: model.ContextWindow, MaxContextWindow: model.ContextWindow,
		MaxOutputTokens: model.MaxOutputTokens, Priority: model.Priority,
		Visibility: "list", Source: SourceNative, Raw: raw,
	}, true
}
