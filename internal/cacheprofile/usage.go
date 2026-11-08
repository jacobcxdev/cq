package cacheprofile

import (
	"encoding/json"
	"math"
)

type Usage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	Known                 bool  `json:"known"`
}

// UsageFromJSON distinguishes an absent cache counter (Known=false) from an
// explicit zero. Its bool reports a valid, recognised usage object. Callers
// merge incremental SSE counters before invoking this function.
func UsageFromJSON(body []byte) (Usage, bool) {
	root, err := decodeObject(body)
	if err != nil {
		return Usage{}, false
	}
	var authorities []map[string]any
	gemini := false
	for _, key := range []string{"usage", "usageMetadata"} {
		if raw, present := root[key]; present && raw != nil {
			object, ok := raw.(map[string]any)
			if !ok {
				return Usage{}, false
			}
			authorities = append(authorities, object)
			gemini = key == "usageMetadata"
		}
	}
	for _, parent := range []string{"response", "message"} {
		if envelope, ok := root[parent].(map[string]any); ok {
			if raw, present := envelope["usage"]; present && raw != nil {
				object, ok := raw.(map[string]any)
				if !ok {
					return Usage{}, false
				}
				authorities = append(authorities, object)
			}
		}
	}
	if len(authorities) != 1 {
		return Usage{}, false
	}
	raw := authorities[0]
	number := func(key string) (int64, bool, bool) {
		value, present := raw[key]
		if !present {
			return 0, false, true
		}
		n, ok := value.(json.Number)
		if !ok {
			return 0, true, false
		}
		v, e := n.Int64()
		return v, true, e == nil && v >= 0
	}
	var usage Usage
	var valid bool
	totalKey := "total_tokens"
	switch {
	case gemini:
		if raw["input_tokens"] != nil || raw["output_tokens"] != nil || raw["prompt_tokens"] != nil || raw["completion_tokens"] != nil {
			return Usage{}, false
		}
		totalKey = "totalTokenCount"
		var hasInput, hasOutput bool
		usage.InputTokens, hasInput, valid = number("promptTokenCount")
		if !valid || !hasInput {
			return Usage{}, false
		}
		usage.OutputTokens, hasOutput, valid = number("candidatesTokenCount")
		if !valid || !hasOutput {
			return Usage{}, false
		}
		thoughts, _, ok := number("thoughtsTokenCount")
		if !ok || usage.OutputTokens > math.MaxInt64-thoughts {
			return Usage{}, false
		}
		usage.OutputTokens += thoughts
		usage.CachedInputTokens, usage.Known, valid = number("cachedContentTokenCount")
		if !valid {
			return Usage{}, false
		}
	case raw["input_tokens"] != nil:
		if raw["prompt_tokens"] != nil || raw["completion_tokens"] != nil {
			return Usage{}, false
		}
		usage.InputTokens, _, valid = number("input_tokens")
		if !valid {
			return Usage{}, false
		}
		var outputPresent bool
		usage.OutputTokens, outputPresent, valid = number("output_tokens")
		if !valid || !outputPresent {
			return Usage{}, false
		}
		_, hasRead := raw["cache_read_input_tokens"]
		_, hasWrite := raw["cache_creation_input_tokens"]
		if hasRead || hasWrite {
			var read, write int64
			read, hasRead, valid = number("cache_read_input_tokens")
			if !valid {
				return Usage{}, false
			}
			write, hasWrite, valid = number("cache_creation_input_tokens")
			if !valid || usage.InputTokens > math.MaxInt64-read || usage.InputTokens+read > math.MaxInt64-write {
				return Usage{}, false
			}
			if _, present := raw["input_tokens_details"]; present {
				return Usage{}, false
			}
			usage.InputTokens += read + write
			usage.CachedInputTokens = read
			usage.CacheWriteInputTokens = write
			usage.Known = hasRead && hasWrite
		} else {
			usage.CachedInputTokens, usage.Known, valid = cacheDetails(raw, "input_tokens_details")
			if !valid {
				return Usage{}, false
			}
		}
	case raw["prompt_tokens"] != nil:
		if raw["output_tokens"] != nil || raw["cache_read_input_tokens"] != nil || raw["cache_creation_input_tokens"] != nil {
			return Usage{}, false
		}
		usage.InputTokens, _, valid = number("prompt_tokens")
		if !valid {
			return Usage{}, false
		}
		var outputPresent bool
		usage.OutputTokens, outputPresent, valid = number("completion_tokens")
		if !valid || !outputPresent {
			return Usage{}, false
		}
		usage.CachedInputTokens, usage.Known, valid = cacheDetails(raw, "prompt_tokens_details")
		if !valid {
			return Usage{}, false
		}
	default:
		return Usage{}, false
	}
	if usage.CachedInputTokens > usage.InputTokens || usage.CacheWriteInputTokens > usage.InputTokens-usage.CachedInputTokens || usage.InputTokens > math.MaxInt64-usage.OutputTokens {
		return Usage{}, false
	}
	for _, key := range []string{"output_tokens_details", "completion_tokens_details"} {
		if value, present := raw[key]; present {
			details, ok := value.(map[string]any)
			if !ok {
				return Usage{}, false
			}
			if value, present := details["reasoning_tokens"]; present {
				number, ok := value.(json.Number)
				if !ok {
					return Usage{}, false
				}
				count, err := number.Int64()
				if err != nil || count < 0 || count > usage.OutputTokens {
					return Usage{}, false
				}
			}
		}
	}
	if total, present, ok := number(totalKey); !ok || present && total < usage.InputTokens+usage.OutputTokens {
		return Usage{}, false
	}
	return usage, true
}

func cacheDetails(raw map[string]any, key string) (int64, bool, bool) {
	value, present := raw[key]
	if !present {
		return 0, false, true
	}
	details, ok := value.(map[string]any)
	if !ok {
		return 0, false, false
	}
	value, present = details["cached_tokens"]
	if !present {
		return 0, false, true
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, false, false
	}
	n, err := number.Int64()
	return n, true, err == nil && n >= 0
}
