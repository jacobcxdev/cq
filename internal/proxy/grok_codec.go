package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type grokToolIdentity struct {
	Name      string
	Namespace string
	Custom    bool
}
type grokRequestCodec struct {
	tools map[string]grokToolIdentity
	items map[string]grokToolIdentity
}

// The Responses dialect accepts JSON functions. A custom text tool is exposed
// as one JSON string argument; its original format remains in the description.
// Stable names retain namespace identity across later tool-result turns.
func normaliseGrokRequest(body []byte) ([]byte, *grokRequestCodec, error) {
	body, err := normaliseZAIRequest(body)
	if err != nil {
		return nil, nil, fmt.Errorf("Grok input: %s", strings.ReplaceAll(err.Error(), "Z.ai", "provider"))
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return nil, nil, errors.New("invalid Grok request")
	}
	codec := &grokRequestCodec{tools: map[string]grokToolIdentity{}, items: map[string]grokToolIdentity{}}
	var tools []json.RawMessage
	if raw, ok := request["tools"]; ok {
		if json.Unmarshal(raw, &tools) != nil {
			return nil, nil, errors.New("invalid Grok tools")
		}
		var out []json.RawMessage
		defined := make(map[string]bool)
		var visit func(json.RawMessage, string) error
		visit = func(raw json.RawMessage, namespace string) error {
			var tool map[string]json.RawMessage
			if json.Unmarshal(raw, &tool) != nil {
				return errors.New("invalid Grok tool")
			}
			var kind, name string
			_ = json.Unmarshal(tool["type"], &kind)
			_ = json.Unmarshal(tool["name"], &name)
			// Grok implements the Responses web_search built-in natively. Its
			// options and output are provider-owned, not a local function call.
			if kind == "web_search" && namespace == "" {
				if access, exists := tool["external_web_access"]; exists {
					var live bool
					if string(access) == "null" || json.Unmarshal(access, &live) != nil {
						return errors.New("invalid Grok web search access")
					}
					if !live {
						return errors.New("Grok does not support cached-only web search")
					}
					// Grok search is live and rejects this OpenAI-only option.
					delete(tool, "external_web_access")
				}
				encoded, err := json.Marshal(tool)
				if err != nil {
					return err
				}
				out = append(out, encoded)
				return nil
			}
			if kind == "namespace" {
				if namespace != "" || name == "" {
					return errors.New("invalid Grok tool namespace")
				}
				var nested []json.RawMessage
				if json.Unmarshal(tool["tools"], &nested) != nil {
					return errors.New("invalid Grok tool namespace")
				}
				for _, child := range nested {
					if err := visit(child, name); err != nil {
						return err
					}
				}
				return nil
			}
			if kind != "function" && kind != "custom" {
				return fmt.Errorf("Grok does not support Codex tool type %q through this route", kind)
			}
			if name == "" {
				return errors.New("missing Grok tool name")
			}
			var ownNamespace string
			_ = json.Unmarshal(tool["namespace"], &ownNamespace)
			if ownNamespace != "" {
				if namespace != "" && namespace != ownNamespace {
					return errors.New("conflicting Grok tool namespace")
				}
				namespace = ownNamespace
			}
			identity := grokToolIdentity{Name: name, Namespace: namespace, Custom: kind == "custom"}
			wire := codec.register(identity)
			if wire == "" || defined[wire] {
				return errors.New("ambiguous Grok tool identity")
			}
			defined[wire] = true
			tool["name"], _ = json.Marshal(wire)
			delete(tool, "namespace")
			if kind == "custom" {
				var description string
				_ = json.Unmarshal(tool["description"], &description)
				if format, ok := tool["format"]; ok {
					description += "\nThe input string must follow this tool format exactly: " + string(format)
				}
				tool["description"], _ = json.Marshal(description)
				tool["type"] = json.RawMessage(`"function"`)
				tool["parameters"] = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
				delete(tool, "format")
			}
			encoded, err := json.Marshal(tool)
			if err != nil {
				return err
			}
			out = append(out, encoded)
			return nil
		}
		for _, raw := range tools {
			if err := visit(raw, ""); err != nil {
				return nil, nil, err
			}
		}
		request["tools"], _ = json.Marshal(out)
	}
	// Grok rejects any tool_choice when no tools exist. Omitting "none" in
	// that case preserves the prohibition while keeping the request valid.
	if len(tools) == 0 && strings.TrimSpace(string(request["tool_choice"])) == `"none"` {
		delete(request, "tool_choice")
	}
	if raw, ok := request["tool_choice"]; ok && strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		var choice map[string]json.RawMessage
		if json.Unmarshal(raw, &choice) != nil {
			return nil, nil, errors.New("invalid Grok tool choice")
		}
		var kind, name, namespace string
		_ = json.Unmarshal(choice["type"], &kind)
		_ = json.Unmarshal(choice["name"], &name)
		_ = json.Unmarshal(choice["namespace"], &namespace)
		if kind != "function" && kind != "custom" {
			return nil, nil, errors.New("unsupported Grok tool choice")
		}
		identity := grokToolIdentity{Name: name, Namespace: namespace, Custom: kind == "custom"}
		wire := grokToolWireName(identity)
		if known, ok := codec.tools[wire]; !ok || known != identity {
			return nil, nil, errors.New("unknown Grok tool choice")
		}
		choice["type"] = json.RawMessage(`"function"`)
		choice["name"], _ = json.Marshal(wire)
		delete(choice, "namespace")
		request["tool_choice"], _ = json.Marshal(choice)
	}
	input, err := responsesBridgeInput(request["input"], "Grok")
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			return nil, nil, errors.New("invalid Grok input item")
		}
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		switch kind {
		case "reasoning":
			// Grok Build replays native reasoning verbatim except for this
			// output-only field. Ciphertext and readable content stay intact.
			delete(item, "status")
			if string(item["content"]) == "null" {
				delete(item, "content")
			}
			if metadata, exists := item["internal_chat_message_metadata_passthrough"]; exists {
				if string(metadata) != "null" {
					return nil, nil, errors.New("Grok cannot replay private ChatGPT reasoning metadata")
				}
				delete(item, "internal_chat_message_metadata_passthrough")
			}
		case "function_call", "custom_tool_call":
			var name, namespace string
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["namespace"], &namespace)
			if name == "" {
				return nil, nil, errors.New("missing Grok tool call name")
			}
			wire := codec.register(grokToolIdentity{Name: name, Namespace: namespace, Custom: kind == "custom_tool_call"})
			if wire == "" {
				return nil, nil, errors.New("ambiguous Grok tool identity")
			}
			item["name"], _ = json.Marshal(wire)
			delete(item, "namespace")
			if kind == "custom_tool_call" {
				var value string
				if json.Unmarshal(item["input"], &value) != nil {
					return nil, nil, errors.New("invalid Grok custom tool input")
				}
				args, _ := json.Marshal(map[string]string{"input": value})
				item["arguments"], _ = json.Marshal(string(args))
				item["type"] = json.RawMessage(`"function_call"`)
				delete(item, "input")
			}
			if encrypted := item["encrypted_function_args"]; len(encrypted) > 0 && string(encrypted) != "null" && string(encrypted) != "[]" {
				return nil, nil, errors.New("Grok cannot replay encrypted tool arguments")
			}
			if encrypted := item["encrypted_arguments"]; len(encrypted) > 0 && string(encrypted) != "null" && string(encrypted) != "[]" {
				return nil, nil, errors.New("Grok cannot replay encrypted tool arguments")
			}
			delete(item, "encrypted_function_args")
		case "custom_tool_call_output":
			item["type"] = json.RawMessage(`"function_call_output"`)
		case "agent_message":
			return nil, nil, errors.New("untranslated Grok agent message")
		}
		input[i], _ = json.Marshal(item)
	}
	request["input"], _ = json.Marshal(input)
	if raw, ok := request["context_management"]; ok && string(raw) != "null" && string(raw) != "[]" {
		return nil, nil, errors.New("Grok requires explicit compaction; automatic context_management is unsupported")
	}
	delete(request, "context_management")
	encoded, err := json.Marshal(request)
	return encoded, codec, err
}
func grokToolWireName(identity grokToolIdentity) string {
	name := identity.Name
	if identity.Namespace != "" || identity.Custom || len(name) > 64 {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%t", identity.Namespace, identity.Name, identity.Custom)))
		name = "cq_tool_" + hex.EncodeToString(sum[:12])
	}
	return name
}
func (c *grokRequestCodec) register(identity grokToolIdentity) string {
	name := grokToolWireName(identity)
	if old, ok := c.tools[name]; ok && old != identity {
		return ""
	}
	c.tools[name] = identity
	return name
}

func (c *grokRequestCodec) normaliseItem(raw json.RawMessage) (json.RawMessage, error) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil, errors.New("invalid Grok output item")
	}
	var kind, name, id string
	_ = json.Unmarshal(item["type"], &kind)
	_ = json.Unmarshal(item["name"], &name)
	_ = json.Unmarshal(item["id"], &id)
	if kind != "function_call" {
		return raw, nil
	}
	identity, ok := c.tools[name]
	if !ok {
		return nil, errors.New("Grok returned an unknown tool")
	}
	if id != "" {
		c.items[id] = identity
	}
	item["name"], _ = json.Marshal(identity.Name)
	if identity.Namespace == "collaboration" {
		item["encrypted_function_args"] = json.RawMessage("[]")
	}
	if identity.Namespace != "" {
		item["namespace"], _ = json.Marshal(identity.Namespace)
	}
	if identity.Custom {
		item["type"] = json.RawMessage(`"custom_tool_call"`)
		var args string
		_ = json.Unmarshal(item["arguments"], &args)
		value := ""
		if args != "" {
			var parsed map[string]json.RawMessage
			if json.Unmarshal([]byte(args), &parsed) != nil || len(parsed) != 1 || json.Unmarshal(parsed["input"], &value) != nil {
				return nil, errors.New("invalid Grok custom tool arguments")
			}
		}
		item["input"], _ = json.Marshal(value)
		delete(item, "arguments")
	}
	return json.Marshal(item)
}
func (c *grokRequestCodec) normaliseResponse(raw []byte) ([]byte, error) {
	// Readable agent collaboration and reasoning use the same Codex markers as
	// Z.ai. This normalizer changes only those evidenced provider-neutral shapes.
	raw, err := normaliseZAIResponse(raw)
	if err != nil {
		return nil, err
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(raw, &event) != nil {
		return nil, errors.New("invalid Grok response")
	}
	var kind string
	_ = json.Unmarshal(event["type"], &kind)
	if kind == "response.function_call_arguments.delta" || kind == "response.function_call_arguments.done" {
		var id string
		_ = json.Unmarshal(event["item_id"], &id)
		if identity, ok := c.items[id]; ok && identity.Custom {
			if kind == "response.function_call_arguments.delta" {
				return nil, nil
			}
			var args string
			_ = json.Unmarshal(event["arguments"], &args)
			var parsed map[string]string
			if json.Unmarshal([]byte(args), &parsed) != nil || len(parsed) != 1 {
				return nil, errors.New("invalid Grok custom tool arguments")
			}
			input, ok := parsed["input"]
			if !ok {
				return nil, errors.New("missing Grok custom tool input")
			}
			event["type"] = json.RawMessage(`"response.custom_tool_call_input.delta"`)
			event["delta"], _ = json.Marshal(input)
			delete(event, "arguments")
			return json.Marshal(event)
		}
	}
	if item, ok := event["item"]; ok {
		normalized, err := c.normaliseItem(item)
		if err != nil {
			return nil, err
		}
		event["item"] = normalized
	}
	if response, ok := event["response"]; ok {
		normalized, err := c.normaliseResponse(response)
		if err != nil {
			return nil, err
		}
		event["response"] = normalized
	}
	if output, ok := event["output"]; ok {
		var items []json.RawMessage
		if json.Unmarshal(output, &items) != nil {
			return nil, errors.New("invalid Grok output")
		}
		for i, item := range items {
			normalized, err := c.normaliseItem(item)
			if err != nil {
				return nil, err
			}
			items[i] = normalized
		}
		event["output"], _ = json.Marshal(items)
	}
	return json.Marshal(event)
}
