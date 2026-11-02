// Package codexusage adapts usage presentation while CQ owns request admission.
package codexusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
)

// FloorCredits keeps the native composer available while CQ owns request
// admission. Provider balances and routing state remain unchanged.
func FloorCredits(usage map[string]any) bool {
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

// FloorCreditResponse adapts a usage response, preserving original bytes when
// malformed, unsupported or unchanged.
func FloorCreditResponse(data []byte) []byte {
	value, err := decode(data)
	usage, ok := value.(map[string]any)
	if err != nil || !ok || !FloorCredits(usage) {
		return data
	}
	projected, err := json.Marshal(usage)
	if err != nil {
		return data
	}
	return projected
}

// FloorCreditEvent adapts only Electron usage snapshots on the native usage
// topic. Other topics, event types and clients keep their provider values.
func FloorCreditEvent(frame map[string]any) bool {
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
	return usage != nil && FloorCredits(usage)
}

// FloorCreditMessage adapts usage events and recovered subscription catchups,
// preserving exact JSON numbers and original bytes when no event changes.
func FloorCreditMessage(data []byte) []byte {
	value, err := decode(data)
	if err != nil {
		return data
	}
	changed := false
	switch frames := value.(type) {
	case map[string]any:
		changed = floorCreditWireFrame(frames)
	case []any:
		for _, value := range frames {
			if frame, ok := value.(map[string]any); ok && floorCreditWireFrame(frame) {
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	projected, err := json.Marshal(value)
	if err != nil {
		return data
	}
	return projected
}

func floorCreditWireFrame(frame map[string]any) bool {
	if _, typed := frame["type"]; typed {
		return FloorCreditEvent(frame)
	}
	reply, _ := frame["reply"].(map[string]any)
	if reply["type"] != "subscribe" || reply["topic_id"] != "codex-work-usage-push-v1" || reply["recovered"] != true {
		return false
	}
	catchups, _ := reply["catchups"].([]any)
	changed := false
	for _, value := range catchups {
		if event, ok := value.(map[string]any); ok && FloorCreditEvent(event) {
			changed = true
		}
	}
	return changed
}

func decode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid usage JSON")
	}
	return value, nil
}
