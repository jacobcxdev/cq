package dots

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// Read the selected thread's effective plugin flag. The feature can occur on a
// later page, and the whole bounded result must be valid and unambiguous.
func (h *cloudHarness) readPluginRuntime(threadID string) (bool, error) {
	return h.readRuntimeFeature(threadID, "plugins")
}

func (h *cloudHarness) readRuntimeFeature(threadID, feature string) (bool, error) {
	unavailable := errors.New("native " + feature + " runtime unavailable")
	if threadID == "" {
		return false, unavailable
	}
	cursor, totalBytes := "", 0
	seenCursors := make(map[string]bool)
	found, enabled := false, false
	for page := 0; page < 4; page++ {
		params := map[string]any{"threadId": threadID, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := h.call("experimentalFeature/list", params)
		if err != nil {
			return false, unavailable
		}
		totalBytes += 2
		for key, raw := range result {
			totalBytes += len(cloudHarnessRaw(key)) + len(raw) + 2
		}
		if totalBytes > 8<<20 {
			return false, unavailable
		}
		var rows *[]struct {
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
		}
		if json.Unmarshal(result["data"], &rows) != nil || rows == nil || len(*rows) > 100 {
			return false, unavailable
		}
		for _, row := range *rows {
			if row.Name == "" || len(row.Name) > 4096 || strings.TrimSpace(row.Name) != row.Name || strings.ContainsFunc(row.Name, unicode.IsControl) || row.Enabled == nil {
				return false, unavailable
			}
			if row.Name == feature {
				if found {
					return false, unavailable
				}
				found, enabled = true, *row.Enabled
			}
		}
		var next *string
		raw, present := result["nextCursor"]
		if !present || json.Unmarshal(raw, &next) != nil {
			return false, unavailable
		}
		if next == nil {
			if !found {
				return false, unavailable
			}
			return enabled, nil
		}
		if *next == "" || len(*next) > 4096 || strings.ContainsFunc(*next, unicode.IsControl) || seenCursors[*next] {
			return false, unavailable
		}
		cursor, seenCursors[*next] = *next, true
	}
	return false, unavailable
}
