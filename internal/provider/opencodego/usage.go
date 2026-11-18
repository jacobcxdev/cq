package opencodego

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/quota"
)

// OpenCode's own usage client polls this endpoint for Go subscription limits.
// The response describes used percentages and reset times, with no identity
// or Extra Usage metadata.
const UsageURL = "https://opencode.ai/zen/go/v1/usage"

func Usage(ctx context.Context, client httputil.Doer, key, workspaceID string) []quota.SharedBudgetWindow {
	if key == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if workspaceID != "" {
		req.Header.Set("X-OpenCode-Org-Id", workspaceID)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil
	}
	type window struct {
		Status   string   `json:"status"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resetsAt"`
	}
	var result struct {
		Usage struct {
			Rolling window `json:"rolling"`
			Weekly  window `json:"weekly"`
			Monthly window `json:"monthly"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	var windows []quota.SharedBudgetWindow
	for _, entry := range []struct {
		name string
		window
	}{{"5h", result.Usage.Rolling}, {"7d", result.Usage.Weekly}, {"1mo", result.Usage.Monthly}} {
		var remaining float64
		switch entry.Status {
		case "ok":
			if entry.Percent == nil || *entry.Percent < 0 || *entry.Percent > 100 {
				continue
			}
			remaining = 100 - *entry.Percent
		case "rate-limited":
			// A provider-enforced limit can be reached before its usage is
			// reflected as exactly 100%.
		default:
			continue
		}
		w := quota.SharedBudgetWindow{Name: entry.name, RemainingPct: remaining}
		if reset, err := time.Parse(time.RFC3339, entry.ResetsAt); err == nil && reset.Unix() > 0 {
			w.ResetAtUnix = reset.Unix()
		}
		windows = append(windows, w)
	}
	return windows
}
