package dots

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type sidebarOrder struct {
	Version int      `json:"version"`
	Handles []string `json:"handles"`
}

//go:embed native_sidebar_order.js
var nativeSidebarOrderJS string

func (c Controller) setSidebarOrder(ctx context.Context, s *Settings, input string) (map[string]any, error) {
	var references []string
	if len(input) > 64<<10 || json.Unmarshal([]byte(input), &references) != nil || len(references) == 0 || len(references) > 256 || len(references) > len(s.Dots) {
		return nil, errors.New("Choose one or more configured Dots in the desired order")
	}
	var labels map[string]string
	order := sidebarOrder{Version: 1, Handles: make([]string, 0, len(references))}
	seen := map[string]bool{}
	for _, reference := range references {
		handle := reference
		if _, ok := s.Dots[handle]; !ok {
			if labels == nil {
				labels, _ = NewAwareness(s, c.client(s), c.evaluator(s)).Names(ctx, true)
			}
			handle = ""
			for h, name := range labels {
				if name == reference {
					if handle != "" {
						return nil, errors.New("Unknown or ambiguous Dot reference; use its stable handle")
					}
					handle = h
				}
			}
			if handle == "" {
				return nil, errors.New("Unknown or ambiguous Dot reference; use its stable handle")
			}
		}
		if seen[handle] {
			return nil, errors.New("Each Dot may appear only once in sidebar order")
		}
		seen[handle] = true
		order.Handles = append(order.Handles, handle)
	}
	if err := SaveJSON(filepath.Join(c.Root, "sidebar-order.json"), order); err != nil {
		return nil, err
	}
	result := map[string]any{"saved": true, "order": order.Handles, "sidebar_refreshed": false, "sidebar_order_applied": false}
	if s.IntegrationMode == "native" {
		profiles := make([]string, len(order.Handles))
		for i, handle := range order.Handles {
			profiles[i] = s.Dots[handle].ProfileID
		}
		expression := strings.Replace(nativeSidebarOrderJS, "ORDERED_PROFILE_IDS", nativeJSON(profiles), 1)
		values, err := c.evaluator(s).EvaluateAll(ctx, nativeAsset(expression, s))
		if err == nil {
			for _, raw := range values {
				if row, ok := raw.(map[string]any); ok && row["refreshed"] == true {
					result["sidebar_refreshed"] = true
					result["sidebar_order_applied"] = row["order_applied"] == true
				}
			}
		}
	}
	return result, nil
}

// Ordering affects presentation only. Authentication/resource identities and the
// shared catalogue fixture remain unchanged, including in concurrent requests.
func orderedSidebarProfiles(s *Settings, profiles []map[string]any) ([]map[string]any, error) {
	path := filepath.Join(filepath.Dir(s.RuntimeRoot), "sidebar-order.json")
	var order sidebarOrder
	if err := coordinationRead(path, &order); err != nil {
		if os.IsNotExist(err) {
			return profiles, nil
		}
		return nil, err
	}
	if order.Version != 1 || len(order.Handles) > 256 {
		return nil, errors.New("Invalid private sidebar order")
	}
	seen := map[string]bool{}
	for _, h := range order.Handles {
		if !validText(h, 128) || seen[h] {
			return nil, errors.New("Invalid private sidebar order")
		}
		seen[h] = true
	}
	ranks := make(map[string]int, len(order.Handles))
	for index, handle := range order.Handles {
		if dot, exists := s.Dots[handle]; exists {
			ranks[dot.ProfileID] = index
		}
	}
	ordered := append([]map[string]any(nil), profiles...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, first := ranks[probeString(ordered[i]["id"])]
		b, second := ranks[probeString(ordered[j]["id"])]
		if first != second {
			return first
		}
		return first && a < b
	})
	return ordered, nil
}
