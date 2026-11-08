// Package cacheprofile measures canonical request prefix bytes, never tokens or
// provider cache eligibility. Content is represented only by keyed hashes.
package cacheprofile

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type Protocol string

const (
	Responses       Protocol = "responses"
	Messages        Protocol = "messages"
	ChatCompletions Protocol = "chat_completions"
	Gemini          Protocol = "gemini"
)

type Component struct {
	Kind  string `json:"kind"`
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes"`
}
type Snapshot struct {
	Components []Component `json:"components"`
	ModelHash  string      `json:"model_hash"`
	Complete   bool        `json:"complete"`
}
type Comparison struct {
	SharedComponents int    `json:"shared_components"`
	ReusableBytes    int    `json:"reusable_bytes"`
	PreviousBytes    int    `json:"previous_bytes"`
	ChangedComponent string `json:"changed_component,omitempty"`
	PrefixStable     bool   `json:"prefix_stable"`
}
type Profiler struct{ key [32]byte }

func New(key []byte) (*Profiler, error) {
	if len(key) != 32 {
		return nil, errors.New("cache profiling requires a 32-byte private key")
	}
	p := &Profiler{}
	copy(p.key[:], key)
	return p, nil
}

const maxComponents = 4096

// Capture represents inference content, not wire framing or a vendor tokenizer.
// Provider/account/cache-key partitioning must be recorded by the caller. Only
// top-level delivery, attribution and cache-policy fields are ignored. Nested
// IDs (including tool-call links), unknown settings and content remain hashed.
func (p *Profiler) Capture(protocol Protocol, body []byte) (Snapshot, error) {
	var snapshot Snapshot
	if p == nil {
		return snapshot, errors.New("cache profiler unavailable")
	}
	if protocol != Responses && protocol != Messages && protocol != ChatCompletions && protocol != Gemini {
		return snapshot, errors.New("unsupported cache profiling protocol")
	}
	request, err := decodeObject(body)
	if err != nil {
		return snapshot, err
	}
	model, ok := request["model"].(string)
	if !ok || model == "" {
		return snapshot, errors.New("cache profiling model unavailable")
	}
	snapshot.ModelHash = p.hash("model", []byte(model))
	if previous, present := request["previous_response_id"]; present && previous != nil && previous != "" {
		return snapshot, errors.New("opaque cache profiling continuation")
	}
	systemField, historyField, contentField := "instructions", "input", "content"
	switch protocol {
	case Messages:
		systemField, historyField = "system", "messages"
	case ChatCompletions:
		systemField, historyField = "", "messages"
	case Gemini:
		systemField, historyField, contentField = "systemInstruction", "contents", "parts"
	}
	config := omit(request, "model", "tools", systemField, historyField,
		"stream", "stream_options", "metadata", "client_metadata", "id", "store", "include", "background", "service_tier", "prompt_cache_key", "prompt_cache_retention", "previous_response_id")
	config["protocol"] = string(protocol)
	add := func(kind string, value any) error {
		if len(snapshot.Components) >= maxComponents {
			return errors.New("cache profiling component limit")
		}
		data, err := json.Marshal(value)
		if err != nil {
			return errors.New("invalid cache profiling component")
		}
		snapshot.Components = append(snapshot.Components, Component{Kind: kind, Hash: p.hash(kind, data), Bytes: len(data)})
		return nil
	}
	if err = add("config", config); err != nil {
		return snapshot, err
	}
	if raw, present := request["tools"]; present && raw != nil {
		tools, ok := raw.([]any)
		if !ok {
			return snapshot, errors.New("invalid cache profiling tools")
		}
		for _, tool := range tools {
			if err = captureTool(protocol, tool, add); err != nil {
				return snapshot, err
			}
		}
	}
	block := func(kind string, value any) error {
		if protocol == Messages {
			if object, ok := value.(map[string]any); ok {
				value = omit(object, "cache_control")
			}
		}
		return add(kind, value)
	}
	blocks := func(kind string, raw any) error {
		switch values := raw.(type) {
		case nil:
			return nil
		case string:
			return block(kind, values)
		case []any:
			for _, value := range values {
				if err := block(kind, value); err != nil {
					return err
				}
			}
			return nil
		default:
			return errors.New("invalid cache profiling content")
		}
	}
	if systemField != "" {
		system := request[systemField]
		if protocol == Gemini && system != nil {
			object, ok := system.(map[string]any)
			if !ok {
				return snapshot, errors.New("invalid cache profiling system")
			}
			if err = add("system_header", omit(object, "parts")); err != nil {
				return snapshot, err
			}
			system = object["parts"]
		}
		if err = blocks("system", system); err != nil {
			return snapshot, err
		}
	}
	raw, present := request[historyField]
	if !present {
		return snapshot, errors.New("cache profiling history unavailable")
	}
	if text, ok := raw.(string); ok && protocol == Responses {
		if err = add("message_header", map[string]any{"role": "user"}); err == nil {
			err = block("content", text)
		}
		if err != nil {
			return snapshot, err
		}
		snapshot.Complete = true
		return snapshot, nil
	}
	history, ok := raw.([]any)
	if !ok {
		return snapshot, errors.New("invalid cache profiling history")
	}
	for _, item := range history {
		message, ok := item.(map[string]any)
		if !ok {
			return snapshot, errors.New("invalid cache profiling message")
		}
		if message["type"] == "item_reference" {
			return snapshot, errors.New("opaque cache profiling item reference")
		}
		if content, present := message[contentField]; present {
			kind, headerKind := "content", "message_header"
			if message["role"] == "system" || message["role"] == "developer" {
				kind, headerKind = "system", "system_header"
			}
			if err = add(headerKind, omit(message, contentField)); err != nil {
				return snapshot, err
			}
			if err = blocks(kind, content); err != nil {
				return snapshot, err
			}
		} else {
			// Function calls/results, reasoning items and unknown structured items keep
			// every field, including links to their matching tool-call IDs.
			if err = add("message", message); err != nil {
				return snapshot, err
			}
		}
	}
	snapshot.Complete = true
	return snapshot, nil
}

func captureTool(protocol Protocol, value any, add func(string, any) error) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("invalid cache profiling tool")
	}
	if protocol == Messages {
		object = omit(object, "cache_control")
	}
	children := ""
	if protocol == Responses && object["type"] == "namespace" {
		children = "tools"
	}
	if protocol == Gemini {
		if _, present := object["functionDeclarations"]; present {
			children = "functionDeclarations"
		}
	}
	if children == "" {
		return add("tool", object)
	}
	nested, ok := object[children].([]any)
	if !ok {
		return errors.New("invalid cache profiling tool declarations")
	}
	if err := add("tool_header", omit(object, children)); err != nil {
		return err
	}
	for _, child := range nested {
		if err := captureTool(protocol, child, add); err != nil {
			return err
		}
	}
	return nil
}

func (p *Profiler) hash(kind string, data []byte) string {
	h := hmac.New(sha256.New, p.key[:])
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func omit(object map[string]any, names ...string) map[string]any {
	result := make(map[string]any, len(object))
	for key, value := range object {
		result[key] = value
	}
	for _, name := range names {
		delete(result, name)
	}
	return result
}

// Compare reports a canonical component-prefix overlap, not a predicted cache
// hit. Incomplete snapshots never claim a reusable or stable complete prefix.
func Compare(previous, current Snapshot) Comparison {
	var result Comparison
	for _, component := range previous.Components {
		if component.Bytes > 0 {
			result.PreviousBytes += component.Bytes
		}
	}
	if !previous.Complete || !current.Complete {
		result.ChangedComponent = "unknown"
		return result
	}
	if previous.ModelHash == "" || previous.ModelHash != current.ModelHash {
		result.ChangedComponent = "model"
		return result
	}
	for index, component := range previous.Components {
		if index >= len(current.Components) {
			result.ChangedComponent = component.Kind
			return result
		}
		candidate := current.Components[index]
		if component.Kind != candidate.Kind || component.Hash == "" || component.Hash != candidate.Hash || component.Bytes != candidate.Bytes {
			result.ChangedComponent = candidate.Kind
			return result
		}
		result.SharedComponents++
		result.ReusableBytes += component.Bytes
	}
	result.PrefixStable = true
	return result
}
