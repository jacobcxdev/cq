// Package messages converts Codex Responses requests and Anthropic Messages
// replies without changing the harness which owns tools and conversation history.
package messages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type object map[string]json.RawMessage

type tool struct {
	Namespace string
	Name      string
	Custom    bool
}

// Codec holds the request's tool identities. Each reply conversion has its own
// stream state, so a codec may be reused for independently received replies.
type Codec struct {
	tools      map[string]tool
	identities map[string]string
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Project converts one stateless Responses request. Opaque response IDs require
// history owned by the caller and cannot be reconstructed by this codec.
func Project(body []byte) ([]byte, *Codec, error) {
	var in struct {
		Model        string            `json:"model"`
		Instructions json.RawMessage   `json:"instructions"`
		Input        json.RawMessage   `json:"input"`
		Tools        []json.RawMessage `json:"tools"`
		ToolChoice   json.RawMessage   `json:"tool_choice"`
		Parallel     *bool             `json:"parallel_tool_calls"`
		Previous     string            `json:"previous_response_id"`
		Stream       bool              `json:"stream"`
		Max          *int64            `json:"max_output_tokens"`
		Temperature  json.RawMessage   `json:"temperature"`
		TopP         json.RawMessage   `json:"top_p"`
		Text         json.RawMessage   `json:"text"`
		Reasoning    struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if json.Unmarshal(body, &in) != nil || in.Model == "" {
		return nil, nil, errors.New("invalid Responses request")
	}
	if in.Previous != "" {
		return nil, nil, errors.New("ZCode Messages requires complete continuation history")
	}
	if !absent(in.Text) {
		var text struct {
			Format struct {
				Type string `json:"type"`
			} `json:"format"`
		}
		if json.Unmarshal(in.Text, &text) != nil || text.Format.Type != "" && text.Format.Type != "text" {
			return nil, nil, errors.New("unsupported Responses text format")
		}
	}
	codec := &Codec{tools: make(map[string]tool), identities: make(map[string]string)}
	var tools []any
	var simpleChoice string
	omitHostedSearch := absent(in.ToolChoice) || json.Unmarshal(in.ToolChoice, &simpleChoice) == nil && (simpleChoice == "auto" || simpleChoice == "none")
	omittedHostedSearch := false
	for _, raw := range in.Tools {
		var hosted struct {
			Type string `json:"type"`
		}
		if omitHostedSearch && json.Unmarshal(raw, &hosted) == nil && (hosted.Type == "web_search" || hosted.Type == "web_search_preview") {
			// Hosted OpenAI search cannot execute on Messages. Automatic
			// selection can still use the harness's remaining function tools.
			omittedHostedSearch = true
			continue
		}
		if err := codec.projectTool(raw, "", &tools); err != nil {
			return nil, nil, err
		}
	}
	if omittedHostedSearch && len(tools) == 0 {
		return nil, nil, errors.New("ZCode Messages cannot execute hosted web search without harness tools")
	}
	var system []any
	if !absent(in.Instructions) {
		var text string
		if json.Unmarshal(in.Instructions, &text) != nil {
			return nil, nil, errors.New("invalid Responses instructions")
		}
		if text != "" {
			system = append(system, map[string]any{"type": "text", "text": text})
		}
	}
	var history []message
	if err := codec.projectInput(in.Input, &history, &system); err != nil {
		return nil, nil, err
	}
	if len(history) == 0 {
		return nil, nil, errors.New("Responses input contains no conversation messages")
	}
	maxTokens := int64(32768)
	if in.Max != nil {
		if *in.Max <= 0 {
			return nil, nil, errors.New("invalid Responses output limit")
		}
		maxTokens = *in.Max
	}
	out := map[string]any{"model": in.Model, "messages": history, "max_tokens": maxTokens, "stream": in.Stream}
	if len(system) > 0 {
		out["system"] = system
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	for k, v := range map[string]json.RawMessage{"temperature": in.Temperature, "top_p": in.TopP} {
		if !absent(v) {
			out[k] = v
		}
	}
	if in.Reasoning.Effort != "" {
		if in.Reasoning.Effort == "none" {
			out["thinking"] = map[string]string{"type": "disabled"}
		} else {
			out["thinking"] = map[string]string{"type": "enabled"}
			out["output_config"] = map[string]string{"effort": in.Reasoning.Effort}
		}
	}
	if !absent(in.ToolChoice) {
		choice, err := codec.projectChoice(in.ToolChoice)
		if err != nil {
			return nil, nil, err
		}
		if choice["type"] == "none" {
			delete(out, "tools")
		} else if len(tools) > 0 {
			if in.Parallel != nil {
				choice["disable_parallel_tool_use"] = !*in.Parallel
			}
			out["tool_choice"] = choice
		}
	} else if in.Parallel != nil && len(tools) > 0 {
		out["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": !*in.Parallel}
	}
	result, err := json.Marshal(out)
	return result, codec, err
}

func (c *Codec) projectTool(raw json.RawMessage, namespace string, out *[]any) error {
	var t struct {
		Type        string            `json:"type"`
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Description string            `json:"description"`
		Parameters  json.RawMessage   `json:"parameters"`
		Tools       []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return errors.New("invalid Responses tool")
	}
	if t.Type == "namespace" {
		if namespace != "" || t.Name == "" {
			return errors.New("invalid Responses tool namespace")
		}
		for _, child := range t.Tools {
			if err := c.projectTool(child, t.Name, out); err != nil {
				return err
			}
		}
		return nil
	}
	if t.Namespace != "" {
		if namespace != "" && namespace != t.Namespace {
			return errors.New("conflicting Responses tool namespace")
		}
		namespace = t.Namespace
	}
	if t.Type != "function" && t.Type != "custom" || t.Name == "" {
		return errors.New("unsupported Responses tool")
	}
	id := tool{Namespace: namespace, Name: t.Name, Custom: t.Type == "custom"}
	key, err := c.register(id, true)
	if err != nil {
		return err
	}
	schema := t.Parameters
	if id.Custom {
		schema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
	} else if absent(schema) {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var schemaObject object
	if json.Unmarshal(schema, &schemaObject) != nil || schemaObject == nil {
		return errors.New("invalid Responses tool schema")
	}
	display := t.Name
	if namespace != "" {
		display = namespace + "." + display
	}
	description := "Tool " + display + "."
	if t.Description != "" {
		description += " " + t.Description
	}
	*out = append(*out, map[string]any{"name": key, "description": description, "input_schema": schema})
	return nil
}

func (c *Codec) register(t tool, definition bool) (string, error) {
	raw, _ := json.Marshal([]string{t.Namespace, t.Name})
	identity := string(raw)
	if previous, ok := c.identities[identity]; ok {
		if c.tools[previous] != t || definition {
			return "", errors.New("ambiguous Responses tool identity")
		}
		return previous, nil
	}
	h := sha256.Sum256(raw)
	prefix := strings.Trim(t.Namespace+"_"+t.Name, "_")
	var safe strings.Builder
	for _, r := range prefix {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			safe.WriteRune(r)
		} else {
			safe.WriteByte('_')
		}
		if safe.Len() >= 39 {
			break
		}
	}
	name := safe.String() + "_" + hex.EncodeToString(h[:12])
	if previous, ok := c.tools[name]; ok && previous != t {
		return "", errors.New("Responses tool name collision")
	}
	c.tools[name] = t
	c.identities[identity] = name
	return name, nil
}

func (c *Codec) projectChoice(raw json.RawMessage) (map[string]any, error) {
	var simple string
	if json.Unmarshal(raw, &simple) == nil {
		switch simple {
		case "auto", "none":
			return map[string]any{"type": simple}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		}
	}
	var choice struct{ Type, Name, Namespace string }
	if json.Unmarshal(raw, &choice) != nil || choice.Type != "function" && choice.Type != "custom" {
		return nil, errors.New("unsupported Responses tool choice")
	}
	name, ok := c.identities[identity(choice.Namespace, choice.Name)]
	if !ok || c.tools[name].Custom != (choice.Type == "custom") {
		return nil, errors.New("unknown Responses tool choice")
	}
	return map[string]any{"type": "tool", "name": name}, nil
}

func (c *Codec) projectInput(raw json.RawMessage, history *[]message, system *[]any) error {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		appendMessage(history, "user", []any{map[string]any{"type": "text", "text": text}})
		return nil
	}
	var input []object
	if json.Unmarshal(raw, &input) != nil || input == nil {
		return errors.New("invalid Responses input")
	}
	// A malformed completed call can only be retained as literal history when
	// its result is present and unambiguous in this complete request.
	type toolPair struct {
		calls, results, resultIndex int
		resultKind                  string
	}
	pairs := make(map[string]toolPair)
	for index, item := range input {
		call, _ := stringField(item, "call_id")
		if call == "" {
			continue
		}
		kind, _ := stringField(item, "type")
		pair := pairs[call]
		switch kind {
		case "function_call", "custom_tool_call":
			pair.calls++
		case "function_call_output", "custom_tool_call_output":
			pair.results++
			pair.resultIndex = index
			pair.resultKind = kind
		}
		pairs[call] = pair
	}
	literalCalls := make(map[string]string)
	for index, item := range input {
		kind, _ := stringField(item, "type")
		switch kind {
		case "", "message":
			role, _ := stringField(item, "role")
			parts, err := projectContent(item["content"])
			if err != nil {
				return err
			}
			if role == "system" || role == "developer" {
				for _, part := range parts {
					if part.(map[string]any)["type"] != "text" {
						return errors.New("unsupported Responses system content")
					}
				}
				if len(*history) == 0 {
					*system = append(*system, parts...)
				} else if len(parts) != 0 {
					// ZCode accepts chronological system messages. Hoisting runtime
					// reminders would invalidate every cached conversation prefix.
					var content any = parts
					if len(parts) == 1 {
						content = parts[0].(map[string]any)["text"]
					}
					*history = append(*history, message{Role: "system", Content: content})
				}
				continue
			}
			if role != "user" && role != "assistant" {
				return errors.New("unsupported Responses message role")
			}
			appendMessage(history, role, parts)
		case "function_call", "custom_tool_call":
			name, _ := stringField(item, "name")
			namespace, _ := stringField(item, "namespace")
			call, _ := stringField(item, "call_id")
			if name == "" || call == "" || !absent(item["encrypted_arguments"]) || !absent(item["encrypted_function_args"]) && string(item["encrypted_function_args"]) != "[]" {
				return errors.New("invalid or encrypted Responses tool call")
			}
			t := tool{Namespace: namespace, Name: name, Custom: kind == "custom_tool_call"}
			var value any
			if t.Custom {
				input, ok := stringField(item, "input")
				if !ok {
					return errors.New("invalid Responses custom tool input")
				}
				value = map[string]string{"input": input}
			} else {
				args, ok := stringField(item, "arguments")
				if !ok {
					return errors.New("invalid Responses tool arguments")
				}
				var obj object
				parseErr := json.Unmarshal([]byte(args), &obj)
				if parseErr != nil || obj == nil {
					status, _ := stringField(item, "status")
					pair := pairs[call]
					if !json.Valid([]byte(args)) && strings.TrimSpace(args) != "" && status == "completed" && pair.calls == 1 && pair.results == 1 && pair.resultIndex > index && pair.resultKind == "function_call_output" {
						display := name
						if namespace != "" {
							display = namespace + "." + name
						}
						label := fmt.Sprintf("Historical function call %q (call ID %q) with malformed JSON arguments. Literal recorded arguments:\n", display, call)
						appendMessage(history, "assistant", []any{map[string]any{"type": "text", "text": label + args}})
						literalCalls[call] = display
						continue
					}
					return errors.New("Responses tool arguments must be a JSON object")
				}
				value = json.RawMessage(args)
			}
			encoded, err := c.register(t, false)
			if err != nil {
				return err
			}
			appendMessage(history, "assistant", []any{map[string]any{"type": "tool_use", "id": call, "name": encoded, "input": value}})
		case "function_call_output", "custom_tool_call_output":
			call, _ := stringField(item, "call_id")
			_, hasIdentity := item["call_id"]
			unattributed := kind == "function_call_output" && !hasIdentity
			if call == "" && !unattributed {
				return errors.New("missing Responses tool result identity")
			}
			parts, err := projectContent(item["output"])
			if err != nil {
				return err
			}
			label := ""
			if unattributed {
				name, _ := stringField(item, "name")
				namespace, _ := stringField(item, "namespace")
				display := name
				if namespace != "" && name != "" {
					display = namespace + "." + name
				}
				label = "Unattributed historical function result. Literal recorded output:"
				if display != "" {
					label = fmt.Sprintf("Unattributed historical function result from %q. Literal recorded output:", display)
				}
			} else if display, ok := literalCalls[call]; ok {
				label = fmt.Sprintf("Historical result for function call %q (call ID %q). Literal recorded output:", display, call)
			}
			if label != "" {
				appendMessage(history, "user", append([]any{map[string]any{"type": "text", "text": label}}, parts...))
				continue
			}
			appendMessage(history, "user", []any{map[string]any{"type": "tool_result", "tool_use_id": call, "content": parts}})
		case "reasoning":
			if !absent(item["encrypted_content"]) {
				return errors.New("ZCode Messages cannot replay encrypted reasoning")
			}
			var parts []object
			for _, key := range []string{"summary", "content"} {
				if absent(item[key]) {
					continue
				}
				var current []object
				if json.Unmarshal(item[key], &current) != nil {
					return errors.New("invalid Responses reasoning")
				}
				parts = append(parts, current...)
			}
			var blocks []any
			for _, part := range parts {
				kind, _ := stringField(part, "type")
				text, ok := stringField(part, "text")
				if !ok || kind != "summary_text" && kind != "reasoning_text" {
					return errors.New("unsupported Responses reasoning content")
				}
				block := map[string]any{"type": "thinking", "thinking": text}
				if signature, ok := stringField(item, "signature"); ok {
					block["signature"] = signature
				}
				blocks = append(blocks, block)
			}
			if len(blocks) > 0 {
				appendMessage(history, "assistant", blocks)
			}
		default:
			return errors.New("unsupported Responses input item")
		}
	}
	return nil
}

func projectContent(raw json.RawMessage) ([]any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	var parts []object
	if json.Unmarshal(raw, &parts) != nil || parts == nil {
		return nil, errors.New("unsupported Responses message content")
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		kind, _ := stringField(part, "type")
		switch kind {
		case "input_text", "output_text", "text":
			value, ok := stringField(part, "text")
			if !ok {
				return nil, errors.New("invalid Responses text content")
			}
			out = append(out, map[string]any{"type": "text", "text": value})
		case "input_image":
			url, ok := stringField(part, "image_url")
			if !ok || url == "" {
				return nil, errors.New("unsupported Responses image content")
			}
			source := map[string]any{"type": "url", "url": url}
			if strings.HasPrefix(url, "data:") {
				pieces := strings.SplitN(url[5:], ";base64,", 2)
				if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" {
					return nil, errors.New("invalid Responses image data")
				}
				source = map[string]any{"type": "base64", "media_type": pieces[0], "data": pieces[1]}
			} else if !strings.HasPrefix(url, "https://") {
				return nil, errors.New("unsupported Responses image URL")
			}
			out = append(out, map[string]any{"type": "image", "source": source})
		default:
			return nil, errors.New("unsupported Responses message content")
		}
	}
	return out, nil
}

func appendMessage(history *[]message, role string, content []any) {
	if len(content) == 0 {
		return
	}
	if len(*history) > 0 && (*history)[len(*history)-1].Role == role {
		last := &(*history)[len(*history)-1]
		last.Content = append(last.Content.([]any), content...)
	} else {
		*history = append(*history, message{Role: role, Content: content})
	}
	if role == "user" {
		last := &(*history)[len(*history)-1]
		blocks := last.Content.([]any)
		// Messages requires tool results before other user content. Preserve
		// the recorded order within the results and within literal history.
		sort.SliceStable(blocks, func(i, j int) bool {
			left := blocks[i].(map[string]any)
			right := blocks[j].(map[string]any)
			return left["type"] == "tool_result" && right["type"] != "tool_result"
		})
	}
}

func identity(namespace, name string) string {
	raw, _ := json.Marshal([]string{namespace, name})
	return string(raw)
}
func absent(raw json.RawMessage) bool {
	return len(raw) == 0 || strings.TrimSpace(string(raw)) == "null"
}
func stringField(o object, key string) (string, bool) {
	var s string
	err := json.Unmarshal(o[key], &s)
	return s, err == nil && !absent(o[key])
}
func protocolError(what string) error { return fmt.Errorf("invalid ZCode Messages %s", what) }
