package dots

import (
	"encoding/json"
	"math"
	"strconv"
)

// floorNativeUsageCredits keeps the native composer usable while CQ owns
// request admission. Provider balances and routing state remain unchanged.
func floorNativeUsageCredits(usage map[string]any) bool {
	credits, ok := usage["credits"].(map[string]any)
	if usage["credits"] == nil {
		credits = make(map[string]any)
	} else if !ok {
		return false
	}
	var balance float64
	var err error
	var minimum any = "1"
	switch value := credits["balance"].(type) {
	case nil:
	case string:
		balance, err = strconv.ParseFloat(value, 64)
	case json.Number:
		balance, err = value.Float64()
		minimum = json.Number("1")
	default:
		return false
	}
	if err != nil || math.IsNaN(balance) || math.IsInf(balance, 0) {
		return false
	}
	changed := balance < 1
	if changed {
		credits["balance"] = minimum
	}
	if credits["has_credits"] != true {
		credits["has_credits"] = true
		changed = true
	}
	if changed {
		usage["credits"] = credits
	}
	return changed
}

func floorNativeUsageCreditResponse(data []byte) []byte {
	value, err := celsiusDecode(data)
	usage, ok := value.(map[string]any)
	if err != nil || !ok || !floorNativeUsageCredits(usage) {
		return data
	}
	projected, err := json.Marshal(usage)
	if err != nil {
		return data
	}
	return projected
}

func floorNativeUsageCreditEvent(frame map[string]any) bool {
	if frame["type"] != "message" || frame["topic_id"] != "codex-work-usage-push-v1" {
		return false
	}
	event, _ := frame["payload"].(map[string]any)
	if event["type"] != "usage.snapshot" {
		return false
	}
	payload, _ := event["payload"].(map[string]any)
	clients, _ := payload["usage_by_client"].(map[string]any)
	usage, _ := clients["electron"].(map[string]any)
	return usage != nil && floorNativeUsageCredits(usage)
}
