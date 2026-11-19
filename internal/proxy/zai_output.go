package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const zaiMaxSSEEventBytes = 1 << 20

// Z.ai emits plaintext reasoning and collaboration arguments in shapes Codex
// cannot safely replay to other providers without normalisation.
func normaliseZAIResponse(data []byte) ([]byte, error) {
	return normaliseResponsesOutput(data, normaliseZAIResponseItem)
}

func normaliseResponsesOutput(data []byte, itemNormaliser func(json.RawMessage) (json.RawMessage, bool, error)) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		if !json.Valid(data) {
			return nil, errors.New("invalid Responses JSON")
		}
		return data, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, errors.New("invalid Responses JSON")
	}
	changed := false
	if item, ok := envelope["item"]; ok {
		updated, itemChanged, err := itemNormaliser(item)
		if err != nil {
			return nil, err
		}
		if itemChanged {
			envelope["item"], changed = updated, true
		}
	}
	for _, key := range []string{"", "response"} {
		response := envelope
		if key != "" {
			response = nil
			if err := json.Unmarshal(envelope[key], &response); err != nil || response == nil {
				continue
			}
		}
		var output []json.RawMessage
		if err := json.Unmarshal(response["output"], &output); err != nil {
			continue
		}
		outputChanged := false
		for i, item := range output {
			updated, itemChanged, err := itemNormaliser(item)
			if err != nil {
				return nil, err
			}
			if itemChanged {
				output[i], outputChanged = updated, true
			}
		}
		if !outputChanged {
			continue
		}
		var err error
		response["output"], err = json.Marshal(output)
		if err != nil {
			return nil, err
		}
		if key != "" {
			envelope[key], err = json.Marshal(response)
			if err != nil {
				return nil, err
			}
		}
		changed = true
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(envelope)
}

func normaliseZAIResponseItem(raw json.RawMessage) (json.RawMessage, bool, error) {
	var call map[string]json.RawMessage
	if err := json.Unmarshal(raw, &call); err != nil || call == nil {
		return raw, false, nil
	}
	var kind string
	if json.Unmarshal(call["type"], &kind) != nil {
		return raw, false, nil
	}
	if kind == "reasoning" {
		if encrypted, ok := call["encrypted_content"]; ok && !bytes.Equal(bytes.TrimSpace(encrypted), []byte("null")) {
			return raw, false, nil
		}
		var content []map[string]json.RawMessage
		if json.Unmarshal(call["content"], &content) != nil || len(content) == 0 {
			return raw, false, nil
		}
		var summary []json.RawMessage
		if previous, ok := call["summary"]; ok && json.Unmarshal(previous, &summary) != nil {
			return raw, false, nil
		}
		for _, part := range content {
			var partType string
			var text *string
			if len(part) != 2 || json.Unmarshal(part["type"], &partType) != nil || partType != "reasoning_text" ||
				json.Unmarshal(part["text"], &text) != nil || text == nil {
				return raw, false, nil
			}
			part["type"] = json.RawMessage(`"summary_text"`)
			encoded, err := json.Marshal(part)
			if err != nil {
				return nil, false, err
			}
			summary = append(summary, encoded)
		}
		var err error
		call["summary"], err = json.Marshal(summary)
		if err != nil {
			return nil, false, err
		}
		delete(call, "content")
		delete(call, "encrypted_content")
		updated, err := json.Marshal(call)
		return updated, err == nil, err
	}
	return markPlaintextCollaborationCall(raw, call)
}

func markPlaintextCollaborationCall(raw json.RawMessage, call map[string]json.RawMessage) (json.RawMessage, bool, error) {
	var kind, namespace, name string
	if json.Unmarshal(call["type"], &kind) != nil || kind != "function_call" ||
		json.Unmarshal(call["namespace"], &namespace) != nil || namespace != "collaboration" ||
		json.Unmarshal(call["name"], &name) != nil {
		return raw, false, nil
	}
	if !plaintextCollaborationTool(name) {
		return raw, false, nil
	}
	if marker, ok := call["encrypted_function_args"]; ok && !bytes.Equal(bytes.TrimSpace(marker), []byte("null")) {
		return raw, false, nil
	}
	call["encrypted_function_args"] = json.RawMessage("[]")
	updated, err := json.Marshal(call)
	return updated, err == nil, err
}

func plaintextCollaborationTool(name string) bool {
	return name == "spawn_agent" || name == "send_message" || name == "followup_task"
}

// Buffer individual SSE events so data can be normalised without changing
// comments, event metadata, line endings, or an unfinished final event.
func copyZAIResponseSSE(dst io.Writer, src io.Reader) error {
	return copyNormalisedResponsesSSE(dst, src, normaliseZAIResponse, false)
}

func copyNormalisedResponsesSSE(dst io.Writer, src io.Reader, normalise func([]byte) ([]byte, error), passOversized bool) error {
	reader := bufio.NewReaderSize(src, 32<<10)
	var event []byte
	var lineContinues, oversized bool
	write := func(data []byte) error {
		if len(data) == 0 {
			return nil
		}
		if n, err := dst.Write(data); err != nil {
			return err
		} else if n != len(data) {
			return io.ErrShortWrite
		}
		return nil
	}
	for {
		fragment, readErr := reader.ReadSlice('\n')
		complete := !lineContinues && (bytes.Equal(fragment, []byte("\n")) || bytes.Equal(fragment, []byte("\r\n")))
		lineContinues = readErr == bufio.ErrBufferFull
		if oversized || len(fragment) > zaiMaxSSEEventBytes-len(event) {
			if !passOversized {
				return errors.New("Z.ai SSE event exceeds 1 MiB")
			}
			// Go already accepts arbitrarily large HTTP events. Stream
			// overflow in place, then resume rewriting at the next event.
			if err := write(event); err != nil {
				return err
			}
			event = event[:0]
			if err := write(fragment); err != nil {
				return err
			}
			oversized = !complete
		} else {
			event = append(event, fragment...)
			if len(event) != 0 && (complete || readErr != nil && !lineContinues) {
				updated, err := normaliseResponsesSSEEvent(event, normalise)
				if err != nil {
					return err
				}
				if len(updated) > zaiMaxSSEEventBytes && !passOversized {
					return errors.New("Z.ai SSE event exceeds 1 MiB")
				}
				if err := write(updated); err != nil {
					return err
				}
				event = event[:0]
			}
		}
		if readErr != nil && !lineContinues {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func normaliseResponsesSSEEvent(event []byte, normalise func([]byte) ([]byte, error)) ([]byte, error) {
	type dataSpan struct{ start, end int }
	var spans []dataSpan
	var data []byte
	anchor, offset := -1, 0
	for _, line := range bytes.SplitAfter(event, []byte("\n")) {
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		start := -1
		if bytes.HasPrefix(content, []byte("data:")) {
			start = len("data:")
			if len(content) > start && content[start] == ' ' {
				start++
			}
			if anchor < 0 {
				anchor = len(spans)
			}
		} else if bytes.Equal(content, []byte("data")) {
			start = len(content)
		}
		if start >= 0 {
			if len(spans) != 0 {
				data = append(data, '\n')
			}
			data = append(data, content[start:]...)
			spans = append(spans, dataSpan{offset + start, offset + len(content)})
		}
		offset += len(line)
	}
	if anchor < 0 || !json.Valid(data) {
		return event, nil
	}
	updated, err := normalise(data)
	if err != nil || bytes.Equal(updated, data) {
		return event, err
	}
	if len(updated) == 0 {
		return nil, nil
	}
	var output bytes.Buffer
	offset = 0
	for i, span := range spans {
		output.Write(event[offset:span.start])
		if i == anchor {
			output.Write(updated)
		}
		offset = span.end
	}
	output.Write(event[offset:])
	return output.Bytes(), nil
}
