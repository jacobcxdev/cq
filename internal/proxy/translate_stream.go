package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// StreamTranslator reads OpenAI Responses SSE events from a reader and writes
// Anthropic Messages SSE events to an http.ResponseWriter.
// It also accumulates state for assembling a non-streaming JSON response.
type StreamTranslator struct {
	model string

	// state
	started    bool
	blockIndex int
	msgID      string

	// accumulated state for non-streaming assembly
	contentBlocks []anthropicContentBlock
	currentText   strings.Builder
	currentTool   *anthropicContentBlock
	currentToolArgs strings.Builder
	stopReason    string
	usage         anthropicUsage
}

// NewStreamTranslator creates a StreamTranslator for the given model.
func NewStreamTranslator(model string) *StreamTranslator {
	return &StreamTranslator{model: model}
}

// Translate reads all SSE events from r and writes translated events to w.
func (st *StreamTranslator) Translate(w http.ResponseWriter, r io.Reader) error {
	flusher, _ := w.(http.Flusher)

	scanner := bufio.NewScanner(r)
	// Increase buffer for potentially large SSE events.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		if data == "[DONE]" {
			st.writeSSE(w, "message_stop", json.RawMessage(`{"type":"message_stop"}`))
			if flusher != nil {
				flusher.Flush()
			}
			break
		}

		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		events := st.translateEvent(event.Type, []byte(data))
		for _, ev := range events {
			st.writeSSE(w, ev.event, ev.data)
		}
		if flusher != nil && len(events) > 0 {
			flusher.Flush()
		}
	}

	return scanner.Err()
}

type sseEvent struct {
	event string
	data  json.RawMessage
}

func (st *StreamTranslator) translateEvent(eventType string, data []byte) []sseEvent {
	switch eventType {
	case "response.created":
		return st.handleResponseCreated(data)
	case "response.output_item.added":
		return st.handleOutputItemAdded(data)
	case "response.content_part.added":
		return st.handleContentPartAdded(data)
	case "response.output_text.delta":
		return st.handleTextDelta(data)
	case "response.function_call_arguments.delta":
		return st.handleFunctionCallDelta(data)
	case "response.content_part.done":
		return st.handleContentPartDone()
	case "response.output_item.done":
		return st.handleOutputItemDone(data)
	case "response.completed":
		return st.handleResponseCompleted(data)
	case "thread.token_usage.updated":
		return st.handleThreadTokenUsageUpdated(data)
	default:
		return nil
	}
}

func (st *StreamTranslator) handleResponseCreated(data []byte) []sseEvent {
	if st.started {
		return nil
	}
	st.started = true

	var ev struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	json.Unmarshal(data, &ev)
	st.msgID = "msg_" + ev.Response.ID

	msgStart := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    st.msgID,
			"type":  "message",
			"role":  "assistant",
			"model": st.model,
			"content": []any{},
			"usage": map[string]int{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	b, _ := json.Marshal(msgStart)
	return []sseEvent{{event: "message_start", data: b}}
}

func (st *StreamTranslator) handleOutputItemAdded(data []byte) []sseEvent {
	var ev struct {
		Item struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"item"`
	}
	json.Unmarshal(data, &ev)

	if ev.Item.Type == "function_call" {
		st.currentTool = &anthropicContentBlock{
			Type: "tool_use",
			ID:   ev.Item.ID,
			Name: ev.Item.Name,
		}
		st.currentToolArgs.Reset()

		block := map[string]any{
			"type": "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    ev.Item.ID,
				"name":  ev.Item.Name,
				"input": map[string]any{},
			},
		}
		b, _ := json.Marshal(block)
		return []sseEvent{{event: "content_block_start", data: b}}
	}

	// Message items with text content are handled in content_part.added
	return nil
}

func (st *StreamTranslator) handleContentPartAdded(data []byte) []sseEvent {
	var ev struct {
		Part struct {
			Type string `json:"type"`
		} `json:"part"`
	}
	json.Unmarshal(data, &ev)

	if ev.Part.Type == "output_text" {
		st.currentText.Reset()
		block := map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}
		b, _ := json.Marshal(block)
		return []sseEvent{{event: "content_block_start", data: b}}
	}

	return nil
}

func (st *StreamTranslator) handleTextDelta(data []byte) []sseEvent {
	var ev struct {
		Delta string `json:"delta"`
	}
	json.Unmarshal(data, &ev)
	st.currentText.WriteString(ev.Delta)

	delta := map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]string{
			"type": "text_delta",
			"text": ev.Delta,
		},
	}
	b, _ := json.Marshal(delta)
	return []sseEvent{{event: "content_block_delta", data: b}}
}

func (st *StreamTranslator) handleFunctionCallDelta(data []byte) []sseEvent {
	var ev struct {
		Delta string `json:"delta"`
	}
	json.Unmarshal(data, &ev)
	st.currentToolArgs.WriteString(ev.Delta)

	delta := map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]string{
			"type":         "input_json_delta",
			"partial_json": ev.Delta,
		},
	}
	b, _ := json.Marshal(delta)
	return []sseEvent{{event: "content_block_delta", data: b}}
}

func (st *StreamTranslator) handleContentPartDone() []sseEvent {
	// Flush accumulated text block.
	if text := st.currentText.String(); text != "" {
		st.contentBlocks = append(st.contentBlocks, anthropicContentBlock{
			Type: "text",
			Text: text,
		})
		st.currentText.Reset()
	}

	stop := map[string]any{
		"type":  "content_block_stop",
		"index": st.blockIndex,
	}
	b, _ := json.Marshal(stop)
	st.blockIndex++
	return []sseEvent{{event: "content_block_stop", data: b}}
}

func (st *StreamTranslator) handleOutputItemDone(data []byte) []sseEvent {
	var ev struct {
		Item struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	json.Unmarshal(data, &ev)

	// For function_call items, flush tool block and emit content_block_stop.
	if ev.Item.Type == "function_call" {
		if st.currentTool != nil {
			args := st.currentToolArgs.String()
			if args == "" {
				args = "{}"
			}
			st.currentTool.Input = json.RawMessage(args)
			st.contentBlocks = append(st.contentBlocks, *st.currentTool)
			st.currentTool = nil
			st.currentToolArgs.Reset()
		}

		stop := map[string]any{
			"type":  "content_block_stop",
			"index": st.blockIndex,
		}
		b, _ := json.Marshal(stop)
		st.blockIndex++
		return []sseEvent{{event: "content_block_stop", data: b}}
	}

	return nil
}

func (st *StreamTranslator) handleResponseCompleted(data []byte) []sseEvent {
	var ev struct {
		Response struct {
			Status string       `json:"status"`
			Usage  *openaiUsage `json:"usage"`
		} `json:"response"`
	}
	json.Unmarshal(data, &ev)

	stopReason := "end_turn"
	if ev.Response.Status == "incomplete" {
		stopReason = "max_tokens"
	}
	// If last block is a tool_use, set stop_reason to tool_use.
	if len(st.contentBlocks) > 0 && st.contentBlocks[len(st.contentBlocks)-1].Type == "tool_use" {
		stopReason = "tool_use"
	}
	st.stopReason = stopReason

	if ev.Response.Usage != nil {
		st.usage = translateUsage(ev.Response.Usage)
	}

	msgDelta := map[string]any{
		"type": "message_delta",
		"delta": map[string]string{
			"stop_reason": stopReason,
		},
		"usage": anthropicUsageMap(st.usage),
	}
	b, _ := json.Marshal(msgDelta)
	return []sseEvent{{event: "message_delta", data: b}}
}

func (st *StreamTranslator) handleThreadTokenUsageUpdated(data []byte) []sseEvent {
	var ev struct {
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil
	}
	if ev.Usage != nil {
		st.usage = translateUsage(ev.Usage)
	}
	return nil
}

func (st *StreamTranslator) writeSSE(w http.ResponseWriter, event string, data json.RawMessage) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(data))
}

func anthropicUsageMap(usage anthropicUsage) map[string]any {
	result := map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	}
	if usage.CacheCreationInputTokens != nil {
		result["cache_creation_input_tokens"] = *usage.CacheCreationInputTokens
	}
	if usage.CacheReadInputTokens != nil {
		result["cache_read_input_tokens"] = *usage.CacheReadInputTokens
	}
	if usage.ReasoningOutputTokens != nil {
		result["reasoning_output_tokens"] = *usage.ReasoningOutputTokens
	}
	if usage.TotalTokens != nil {
		result["total_tokens"] = *usage.TotalTokens
	}
	return result
}

// Collect reads all SSE events from r, accumulating state without writing SSE output.
// After calling Collect, use AssembleResponse to get the final Anthropic JSON response.
func (st *StreamTranslator) Collect(r io.Reader) error {
	return st.Translate(&discardResponseWriter{}, r)
}

// AssembleResponse builds a complete Anthropic Messages JSON response from accumulated stream state.
func (st *StreamTranslator) AssembleResponse(model string) ([]byte, error) {
	content := st.contentBlocks
	if len(content) == 0 {
		content = []anthropicContentBlock{{Type: "text", Text: ""}}
	}

	stopReason := st.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	resp := anthropicResponse{
		ID:         st.msgID,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    content,
		StopReason: stopReason,
		Usage:      st.usage,
	}
	return json.Marshal(resp)
}

// discardResponseWriter implements http.ResponseWriter but discards all output.
type discardResponseWriter struct {
	header http.Header
}

func (d *discardResponseWriter) Header() http.Header {
	if d.header == nil {
		d.header = make(http.Header)
	}
	return d.header
}

func (d *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponseWriter) WriteHeader(int)              {}
