package proxy

import (
	"bytes"
	"encoding/json"
)

// Go's Responses translator should receive ordinary message parameters. The
// reserved collaboration namespace tells Codex where the plaintext marker applies.
func plaintextCollaborationSchemas(raw json.RawMessage, namespace string) (json.RawMessage, bool) {
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return raw, false
	}
	changed := false
	for i, rawTool := range tools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(rawTool, &tool) != nil || tool == nil {
			continue
		}
		var kind, name, ownNamespace string
		_ = json.Unmarshal(tool["type"], &kind)
		_ = json.Unmarshal(tool["name"], &name)
		_ = json.Unmarshal(tool["namespace"], &ownNamespace)
		switch {
		case namespace == "" && kind == "namespace" && name == "collaboration":
			nested, edited := plaintextCollaborationSchemas(tool["tools"], name)
			if !edited {
				continue
			}
			tool["tools"] = nested
		case kind == "function" && plaintextCollaborationTool(name) &&
			(ownNamespace == "collaboration" || (namespace == "collaboration" && ownNamespace == "")):
			var parameters, properties, message map[string]json.RawMessage
			if json.Unmarshal(tool["parameters"], &parameters) != nil ||
				json.Unmarshal(parameters["properties"], &properties) != nil ||
				json.Unmarshal(properties["message"], &message) != nil {
				continue
			}
			if _, present := message["encrypted"]; !present {
				continue
			}
			delete(message, "encrypted")
			properties["message"], _ = json.Marshal(message)
			parameters["properties"], _ = json.Marshal(properties)
			tool["parameters"], _ = json.Marshal(parameters)
		default:
			continue
		}
		tools[i], _ = json.Marshal(tool)
		changed = true
	}
	if !changed {
		return raw, false
	}
	encoded, _ := json.Marshal(tools)
	return encoded, true
}

func plaintextCollaborationResponse(data []byte) ([]byte, error) {
	return normaliseResponsesOutput(data, func(raw json.RawMessage) (json.RawMessage, bool, error) {
		var call map[string]json.RawMessage
		if json.Unmarshal(raw, &call) != nil {
			return raw, false, nil
		}
		if encrypted := bytes.TrimSpace(call["encrypted_arguments"]); len(encrypted) != 0 && !bytes.Equal(encrypted, []byte("null")) {
			return raw, false, nil
		}
		return markPlaintextCollaborationCall(raw, call)
	})
}
