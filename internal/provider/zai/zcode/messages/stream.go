package messages

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxEventBytes = 1 << 20

type streamBlock struct {
	contentBlock
	arguments   strings.Builder
	text        strings.Builder
	thinking    strings.Builder
	outputIndex int
	closed      bool
}

type streamState struct {
	codec                   *Codec
	writer                  io.Writer
	id, model, stop         string
	usage                   usage
	started, delta, stopped bool
	sequence                int
	blocks                  []*streamBlock
}

// ConvertSSE converts complete, bounded SSE events synchronously. It never
// manufactures a terminal event after a truncated stream or a read error.
func (c *Codec) ConvertSSE(dst io.Writer, src io.Reader) error {
	state := streamState{codec: c, writer: dst}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 32<<10), maxEventBytes)
	var data bytes.Buffer
	var event string
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if len(line) == 0 {
			if data.Len() != 0 {
				if err := state.event(event, data.Bytes()); err != nil {
					return err
				}
				if state.stopped {
					return nil
				}
			}
			data.Reset()
			event = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "data":
			if len(value)+data.Len()+1 > maxEventBytes {
				return protocolError("SSE event size")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		case "event":
			event = value
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read ZCode Messages stream: %w", err)
	}
	return errors.New("ZCode Messages stream ended before message_stop")
}

func (s *streamState) emit(kind string, fields map[string]any) error {
	fields["type"] = kind
	fields["sequence_number"] = s.sequence
	s.sequence++
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	event := append([]byte("event: "+kind+"\ndata: "), raw...)
	event = append(event, '\n', '\n')
	n, err := s.writer.Write(event)
	if err == nil && n != len(event) {
		return io.ErrShortWrite
	}
	return err
}

func (s *streamState) event(event string, raw []byte) error {
	obj, err := readObject(raw)
	if err != nil {
		return err
	}
	kind, ok := stringField(obj, "type")
	if !ok || event != "" && event != kind {
		return protocolError("SSE event type")
	}
	if kind == "ping" {
		n, err := io.WriteString(s.writer, ": ping\n\n")
		if err == nil && n != len(": ping\n\n") {
			return io.ErrShortWrite
		}
		return err
	}
	if kind == "error" {
		var upstream struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(obj["error"], &upstream)
		code := "upstream_error"
		status := 502
		switch upstream.Type {
		case "rate_limit_error":
			code = "rate_limit_exceeded"
			status = 429
		case "authentication_error":
			code = "invalid_api_key"
			status = 401
		case "permission_error":
			code = "permission_denied"
			status = 403
		}
		if err := s.emit("response.failed", map[string]any{"status_code": status, "response": map[string]any{"id": s.id, "status": "failed", "output": []any{}, "error": map[string]any{"code": code, "message": "ZCode model request failed"}}}); err != nil {
			return err
		}
		return errors.New("ZCode Messages upstream reported an error")
	}
	switch kind {
	case "message_start":
		if s.started {
			return protocolError("duplicate message_start")
		}
		var start reply
		if json.Unmarshal(obj["message"], &start) != nil || start.ID == "" || start.Type != "message" || start.Role != "assistant" || start.Model == "" || start.Usage == nil || len(start.Content) != 0 {
			return protocolError("message_start")
		}
		s.id = start.ID
		s.model = start.Model
		s.usage = *start.Usage
		s.started = true
		response := map[string]any{"id": s.id, "object": "response", "status": "in_progress", "model": s.model, "output": []any{}}
		if err := s.emit("response.created", map[string]any{"response": response}); err != nil {
			return err
		}
		return s.emit("response.in_progress", map[string]any{"response": response})
	case "content_block_start":
		if !s.started || s.delta {
			return protocolError("content block order")
		}
		var index int
		if absent(obj["index"]) || json.Unmarshal(obj["index"], &index) != nil || index != len(s.blocks) {
			return protocolError("content block index")
		}
		var content contentBlock
		if json.Unmarshal(obj["content_block"], &content) != nil {
			return protocolError("content block start")
		}
		b := &streamBlock{contentBlock: content, outputIndex: len(s.blocks)}
		b.text.WriteString(content.Text)
		b.thinking.WriteString(content.Thinking)
		s.blocks = append(s.blocks, b)
		item, err := s.initialItem(b)
		if err != nil {
			return err
		}
		if err := s.emit("response.output_item.added", map[string]any{"output_index": b.outputIndex, "item": item}); err != nil {
			return err
		}
		switch b.Type {
		case "text":
			if err := s.emit("response.content_part.added", map[string]any{"item_id": item["id"], "output_index": b.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
				return err
			}
			if b.Text != "" {
				return s.emitText(b, b.Text, false)
			}
		case "thinking":
			if err := s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": item["id"], "output_index": b.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}); err != nil {
				return err
			}
			if b.Thinking != "" {
				return s.emitText(b, b.Thinking, true)
			}
		}
		return nil
	case "content_block_delta":
		b, err := s.current(obj)
		if err != nil {
			return err
		}
		var delta struct {
			Type, Text, Thinking, Signature string
			PartialJSON                     string `json:"partial_json"`
		}
		if json.Unmarshal(obj["delta"], &delta) != nil {
			return protocolError("content block delta")
		}
		var deltaFields object
		if json.Unmarshal(obj["delta"], &deltaFields) != nil || deltaFields == nil {
			return protocolError("content block delta")
		}
		var required string
		switch delta.Type {
		case "text_delta":
			required = "text"
		case "thinking_delta":
			required = "thinking"
		case "signature_delta":
			required = "signature"
		case "input_json_delta":
			required = "partial_json"
		}
		if required != "" {
			if _, ok := stringField(deltaFields, required); !ok {
				return protocolError("content block delta")
			}
		}
		switch delta.Type {
		case "text_delta":
			if b.Type != "text" {
				return protocolError("text delta target")
			}
			b.text.WriteString(delta.Text)
			return s.emitText(b, delta.Text, false)
		case "thinking_delta":
			if b.Type != "thinking" {
				return protocolError("thinking delta target")
			}
			b.thinking.WriteString(delta.Thinking)
			return s.emitText(b, delta.Thinking, true)
		case "signature_delta":
			if b.Type != "thinking" {
				return protocolError("signature delta target")
			}
			b.Signature += delta.Signature
			return nil
		case "input_json_delta":
			if b.Type != "tool_use" {
				return protocolError("tool delta target")
			}
			if b.arguments.Len()+len(delta.PartialJSON) > maxEventBytes {
				return protocolError("tool arguments size")
			}
			b.arguments.WriteString(delta.PartialJSON)
			if !s.codec.tools[b.Name].Custom {
				return s.emit("response.function_call_arguments.delta", map[string]any{"item_id": s.itemID(b), "output_index": b.outputIndex, "delta": delta.PartialJSON})
			}
			return nil
		default:
			return protocolError("delta type")
		}
	case "content_block_stop":
		b, err := s.current(obj)
		if err != nil {
			return err
		}
		b.Text = b.text.String()
		b.Thinking = b.thinking.String()
		if b.Type == "tool_use" && b.arguments.Len() > 0 {
			if !absent(b.Input) && string(bytes.TrimSpace(b.Input)) != "{}" {
				return protocolError("ambiguous tool arguments")
			}
			b.Input = json.RawMessage(b.arguments.String())
		}
		if b.Type == "tool_use" && absent(b.Input) {
			b.Input = json.RawMessage("{}")
		}
		item, err := s.codec.outputItem(s.id, b.outputIndex, b.contentBlock)
		if err != nil {
			return err
		}
		if err := s.finishItem(b, item); err != nil {
			return err
		}
		b.closed = true
		return s.emit("response.output_item.done", map[string]any{"output_index": b.outputIndex, "item": item})
	case "message_delta":
		if !s.started || s.delta {
			return protocolError("message delta order")
		}
		for _, b := range s.blocks {
			if !b.closed {
				return protocolError("unclosed content block")
			}
		}
		var delta struct {
			StopReason string `json:"stop_reason"`
		}
		if json.Unmarshal(obj["delta"], &delta) != nil || delta.StopReason == "" {
			return protocolError("message stop reason")
		}
		if _, err := nativeResponse(s.id, s.model, []any{}, delta.StopReason, s.usage); err != nil {
			return err
		}
		if !absent(obj["usage"]) {
			var fields object
			if json.Unmarshal(obj["usage"], &fields) != nil || fields == nil {
				return protocolError("message usage")
			}
			for key, dst := range map[string]*int64{"input_tokens": &s.usage.Input, "output_tokens": &s.usage.Output, "cache_read_input_tokens": &s.usage.Cached, "cache_creation_input_tokens": &s.usage.Created} {
				if value, ok := fields[key]; ok {
					if absent(value) || json.Unmarshal(value, dst) != nil || *dst < 0 {
						return protocolError("message usage")
					}
				}
			}
		}
		s.stop = delta.StopReason
		s.delta = true
		return nil
	case "message_stop":
		if !s.started || !s.delta {
			return protocolError("premature message_stop")
		}
		output := make([]any, 0, len(s.blocks))
		for _, b := range s.blocks {
			if !b.closed {
				return protocolError("unclosed content block")
			}
			item, err := s.codec.outputItem(s.id, b.outputIndex, b.contentBlock)
			if err != nil {
				return err
			}
			output = append(output, item)
		}
		response, err := nativeResponse(s.id, s.model, output, s.stop, s.usage)
		if err != nil {
			return err
		}
		kind := "response.completed"
		if response["status"] == "incomplete" {
			kind = "response.incomplete"
		}
		if err := s.emit(kind, map[string]any{"response": response}); err != nil {
			return err
		}
		s.stopped = true
		return nil
	default:
		return protocolError("SSE event")
	}
}

func (s *streamState) current(obj object) (*streamBlock, error) {
	var index int
	if !s.started || s.delta || absent(obj["index"]) || json.Unmarshal(obj["index"], &index) != nil || index < 0 || index >= len(s.blocks) || s.blocks[index].closed {
		return nil, protocolError("content block order")
	}
	return s.blocks[index], nil
}
func (s *streamState) itemID(b *streamBlock) string { return fmt.Sprintf("%s_%d", s.id, b.outputIndex) }
func (s *streamState) initialItem(b *streamBlock) (map[string]any, error) {
	switch b.Type {
	case "text":
		return map[string]any{"type": "message", "id": s.itemID(b), "role": "assistant", "status": "in_progress", "content": []any{}}, nil
	case "thinking":
		return map[string]any{"type": "reasoning", "id": s.itemID(b), "summary": []any{}}, nil
	case "tool_use":
		tool, ok := s.codec.tools[b.Name]
		if !ok || b.ID == "" {
			return nil, protocolError("tool identity")
		}
		item := map[string]any{"type": "function_call", "id": s.itemID(b), "call_id": b.ID, "name": tool.Name, "arguments": "", "status": "in_progress"}
		if tool.Namespace != "" {
			item["namespace"] = tool.Namespace
		}
		if tool.Custom {
			item["type"] = "custom_tool_call"
			delete(item, "arguments")
			item["input"] = ""
		}
		return item, nil
	default:
		return nil, protocolError("content block type")
	}
}
func (s *streamState) emitText(b *streamBlock, text string, thinking bool) error {
	kind := "response.output_text.delta"
	fields := map[string]any{"item_id": s.itemID(b), "output_index": b.outputIndex, "delta": text}
	if thinking {
		kind = "response.reasoning_summary_text.delta"
		fields["summary_index"] = 0
	} else {
		fields["content_index"] = 0
	}
	return s.emit(kind, fields)
}
func (s *streamState) finishItem(b *streamBlock, item map[string]any) error {
	fields := map[string]any{"item_id": s.itemID(b), "output_index": b.outputIndex}
	switch b.Type {
	case "text":
		fields["content_index"] = 0
		fields["text"] = b.Text
		if err := s.emit("response.output_text.done", fields); err != nil {
			return err
		}
		return s.emit("response.content_part.done", map[string]any{"item_id": s.itemID(b), "output_index": b.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": b.Text, "annotations": []any{}}})
	case "thinking":
		fields["summary_index"] = 0
		fields["text"] = b.Thinking
		if err := s.emit("response.reasoning_summary_text.done", fields); err != nil {
			return err
		}
		return s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": s.itemID(b), "output_index": b.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": b.Thinking}})
	case "tool_use":
		if s.codec.tools[b.Name].Custom {
			fields["delta"] = item["input"]
			if err := s.emit("response.custom_tool_call_input.delta", fields); err != nil {
				return err
			}
			delete(fields, "delta")
			fields["input"] = item["input"]
			return s.emit("response.custom_tool_call_input.done", fields)
		}
		fields["arguments"] = item["arguments"]
		return s.emit("response.function_call_arguments.done", fields)
	}
	return protocolError("output item type")
}
