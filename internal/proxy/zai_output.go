package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const zaiMaxSSEEventBytes = 1 << 20

// Z.ai emits plaintext collaboration arguments without the marker Codex uses
// to distinguish them from encrypted collaboration arguments.
func normaliseZAIResponse(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		if !json.Valid(data) {
			return nil, errors.New("invalid Z.ai response JSON")
		}
		return data, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, errors.New("invalid Z.ai response JSON")
	}
	changed := false
	if item, ok := envelope["item"]; ok {
		updated, itemChanged, err := normaliseZAICollaborationCall(item)
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
			updated, itemChanged, err := normaliseZAICollaborationCall(item)
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

func normaliseZAICollaborationCall(raw json.RawMessage) (json.RawMessage, bool, error) {
	var call map[string]json.RawMessage
	if err := json.Unmarshal(raw, &call); err != nil || call == nil {
		return raw, false, nil
	}
	var kind, namespace, name string
	if json.Unmarshal(call["type"], &kind) != nil || kind != "function_call" ||
		json.Unmarshal(call["namespace"], &namespace) != nil || namespace != "collaboration" ||
		json.Unmarshal(call["name"], &name) != nil {
		return raw, false, nil
	}
	switch name {
	case "spawn_agent", "send_message", "followup_task":
	default:
		return raw, false, nil
	}
	if marker, ok := call["encrypted_function_args"]; ok && !bytes.Equal(bytes.TrimSpace(marker), []byte("null")) {
		return raw, false, nil
	}
	call["encrypted_function_args"] = json.RawMessage("[]")
	updated, err := json.Marshal(call)
	return updated, err == nil, err
}

// Buffer individual SSE events so data can be normalised without changing
// comments, event metadata, line endings, or an unfinished final event.
func copyZAIResponseSSE(dst io.Writer, src io.Reader) error {
	reader := bufio.NewReaderSize(src, 32<<10)
	var event []byte
	lineStart := 0
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > zaiMaxSSEEventBytes-len(event) {
			return errors.New("Z.ai SSE event exceeds 1 MiB")
		}
		event = append(event, fragment...)
		if readErr == bufio.ErrBufferFull {
			continue
		}
		line := event[lineStart:]
		complete := bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))
		if complete || readErr != nil {
			if len(event) != 0 {
				updated, err := normaliseZAISSEEvent(event)
				if err != nil {
					return err
				}
				if n, err := dst.Write(updated); err != nil {
					return err
				} else if n != len(updated) {
					return io.ErrShortWrite
				}
			}
			event, lineStart = event[:0], 0
		} else {
			lineStart = len(event)
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func normaliseZAISSEEvent(event []byte) ([]byte, error) {
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
	updated, err := normaliseZAIResponse(data)
	if err != nil || bytes.Equal(updated, data) {
		return event, err
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
	if output.Len() > zaiMaxSSEEventBytes {
		return nil, errors.New("Z.ai SSE event exceeds 1 MiB")
	}
	return output.Bytes(), nil
}
