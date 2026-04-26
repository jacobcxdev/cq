package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- Anthropic Messages types (subset for translation) ---

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMsg     `json:"messages"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
	Effort        string             `json:"effort,omitempty"` // "low","medium","high","xhigh","max"
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []contentBlock
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	ReasoningOutputTokens    *int `json:"reasoning_output_tokens,omitempty"`
	TotalTokens              *int `json:"total_tokens,omitempty"`
}

// --- OpenAI Responses API types (subset for translation) ---

type openaiResponsesRequest struct {
	Model           string           `json:"model"`
	Instructions    string           `json:"instructions,omitempty"`
	Input           json.RawMessage  `json:"input"` // string or []inputItem
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	TopP            *float64         `json:"top_p,omitempty"`
	Stream          bool             `json:"stream,omitempty"`
	Store           *bool            `json:"store,omitempty"`
	Tools           []openaiTool     `json:"tools,omitempty"`
	ToolChoice      json.RawMessage  `json:"tool_choice,omitempty"`
	Reasoning       *openaiReasoning `json:"reasoning,omitempty"`
}

type openaiReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type openaiInputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // []inputContentPart for messages

	// function_call fields
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	CallID    string `json:"call_id,omitempty"`

	// function_call_output fields
	Output *string `json:"output,omitempty"`
}

type openaiInputContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openaiTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openaiResponse struct {
	ID     string             `json:"id"`
	Status string             `json:"status"`
	Model  string             `json:"model"`
	Output []openaiOutputItem `json:"output"`
	Usage  *openaiUsage       `json:"usage,omitempty"`
}

type openaiOutputItem struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // for message type

	// function_call fields
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	CallID    string `json:"call_id,omitempty"`
}

type openaiOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openaiUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

type openaiInputTokenCountResponse struct {
	Object      string `json:"object"`
	InputTokens int    `json:"input_tokens"`
}

type anthropicCountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// --- Translation functions ---

// translateRequest converts an Anthropic Messages request to an OpenAI Responses request.
// The model name is normalised via ParseModel ([1m] stripped); reasoning effort comes
// exclusively from the request's native effort/thinking fields via translateEffort.
func translateRequest(body []byte) ([]byte, error) {
	oReq, err := translateOpenAIRequest(body, true, true)
	if err != nil {
		return nil, err
	}
	return json.Marshal(oReq)
}

func translateCountTokensRequest(body []byte) ([]byte, error) {
	oReq, err := translateOpenAIRequest(body, false, false)
	if err != nil {
		return nil, err
	}
	return json.Marshal(oReq)
}

func translateOpenAIRequest(body []byte, stream bool, includeStore bool) (openaiResponsesRequest, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return openaiResponsesRequest{}, err
	}

	oReq := openaiResponsesRequest{
		Model:       ParseModel(req.Model),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      stream,
	}
	if includeStore {
		storeFalse := false
		oReq.Store = &storeFalse
	}

	// Effort comes only from the native request fields; no model-name suffix extraction.
	oReq.Reasoning = translateEffort(req.Effort, req.Thinking)

	// System → instructions (Responses API requires this field to be non-empty).
	oReq.Instructions = translateSystem(req.System)
	if oReq.Instructions == "" {
		oReq.Instructions = "You are a helpful assistant."
	}

	input, err := translateMessages(req.Messages)
	if err != nil {
		return openaiResponsesRequest{}, err
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return openaiResponsesRequest{}, err
	}
	oReq.Input = inputJSON
	oReq.Tools = translateTools(req.Tools)
	oReq.ToolChoice = translateToolChoice(req.ToolChoice)

	return oReq, nil
}

// translateSystem extracts a string from Anthropic's system field.
// System can be a plain string or an array of content blocks.
func translateSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) == 1 {
			return parts[0]
		}
		result := ""
		for i, p := range parts {
			if i > 0 {
				result += "\n\n"
			}
			result += p
		}
		return result
	}

	return ""
}

// translateMessages converts Anthropic messages to OpenAI Responses input items.
func translateMessages(msgs []anthropicMsg) ([]openaiInputItem, error) {
	var items []openaiInputItem

	for _, msg := range msgs {
		msgItems, err := translateMessage(msg)
		if err != nil {
			return nil, err
		}
		items = append(items, msgItems...)
	}

	return items, nil
}

func translateMessage(msg anthropicMsg) ([]openaiInputItem, error) {
	// Content can be a string or array of content blocks.
	// Try string first.
	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		contentType := "input_text"
		if msg.Role == "assistant" {
			contentType = "output_text"
		}
		part := openaiInputContentPart{Type: contentType, Text: text}
		partJSON, err := json.Marshal([]openaiInputContentPart{part})
		if err != nil {
			return nil, err
		}
		return []openaiInputItem{{
			Type:    "message",
			Role:    msg.Role,
			Content: partJSON,
		}}, nil
	}

	// Array of content blocks.
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, err
	}

	var items []openaiInputItem
	var msgParts []openaiInputContentPart
	flushMsg := func() {
		if len(msgParts) == 0 {
			return
		}
		partJSON, _ := json.Marshal(msgParts)
		items = append(items, openaiInputItem{
			Type:    "message",
			Role:    msg.Role,
			Content: partJSON,
		})
		msgParts = nil
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			contentType := "input_text"
			if msg.Role == "assistant" {
				contentType = "output_text"
			}
			msgParts = append(msgParts, openaiInputContentPart{
				Type: contentType,
				Text: block.Text,
			})

		case "tool_use":
			flushMsg()
			argStr := "{}"
			if len(block.Input) > 0 {
				argStr = string(block.Input)
			}
			items = append(items, openaiInputItem{
				Type:      "function_call",
				Name:      block.Name,
				Arguments: argStr,
				CallID:    block.ID,
			})

		case "tool_result":
			flushMsg()
			output := extractToolResultText(block)
			items = append(items, openaiInputItem{
				Type:   "function_call_output",
				CallID: block.ToolUseID,
				Output: strPtr(output),
			})
		}
	}

	flushMsg()
	return items, nil
}

// extractToolResultText extracts text from a tool_result block's content.
func extractToolResultText(block anthropicContentBlock) string {
	if len(block.Content) == 0 {
		return ""
	}

	// Try string.
	var s string
	if json.Unmarshal(block.Content, &s) == nil {
		return s
	}

	// Try array of content blocks.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(block.Content, &parts) == nil {
		result := ""
		for i, p := range parts {
			if i > 0 {
				result += "\n"
			}
			result += p.Text
		}
		return result
	}

	return string(block.Content)
}

// translateTools converts Anthropic tools to OpenAI function tools.
func translateTools(tools []anthropicTool) []openaiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openaiTool, len(tools))
	for i, t := range tools {
		out[i] = openaiTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return out
}

// translateEffort maps Anthropic effort/thinking to OpenAI reasoning.
// Claude Code sends effort as a top-level string: "low","medium","high","xhigh","max".
// Fallback: thinking.budget_tokens is mapped to effort levels.
// Anthropic "max" is translated to OpenAI "xhigh"; explicit "xhigh" passes through unchanged.
func translateEffort(effort string, thinking *anthropicThinking) *openaiReasoning {
	// Prefer explicit effort field.
	if effort != "" {
		e := effort
		if e == "max" {
			e = "xhigh" // Anthropic "max" maps to OpenAI "xhigh" (extended high)
		}
		return &openaiReasoning{
			Effort:  e,
			Summary: "auto",
		}
	}

	// Fallback: map thinking budget_tokens to effort levels.
	if thinking != nil && thinking.Type == "enabled" && thinking.BudgetTokens > 0 {
		e := "medium"
		switch {
		case thinking.BudgetTokens < 4000:
			e = "low"
		case thinking.BudgetTokens < 10000:
			e = "medium"
		case thinking.BudgetTokens < 25000:
			e = "high"
		default:
			e = "xhigh"
		}
		return &openaiReasoning{
			Effort:  e,
			Summary: "auto",
		}
	}

	return nil
}

// translateToolChoice converts Anthropic tool_choice to OpenAI format.
func translateToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}

	// Anthropic tool_choice can be:
	//   {"type":"auto"} → "auto"
	//   {"type":"any"}  → "required"
	//   {"type":"tool","name":"X"} → {"type":"function","name":"X"}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}

	switch tc.Type {
	case "auto":
		b, _ := json.Marshal("auto")
		return b
	case "any":
		b, _ := json.Marshal("required")
		return b
	case "tool":
		b, _ := json.Marshal(map[string]string{
			"type": "function",
			"name": tc.Name,
		})
		return b
	default:
		return raw
	}
}

func describeOutputContentTypes(output []openaiOutputItem) string {
	var types []string
	for _, item := range output {
		if item.Type != "message" {
			types = append(types, item.Type)
			continue
		}

		var parts []openaiOutputContent
		if err := json.Unmarshal(item.Content, &parts); err != nil {
			types = append(types, item.Type+".unparseable_content")
			continue
		}
		if len(parts) == 0 {
			types = append(types, item.Type+".empty_content")
			continue
		}
		for _, part := range parts {
			partType := part.Type
			if partType == "" {
				partType = "unknown"
			}
			types = append(types, item.Type+"."+partType)
		}
	}
	if len(types) == 0 {
		return "none"
	}
	return strings.Join(types, ",")
}

// translateResponse converts an OpenAI Responses response to an Anthropic Messages response.
func translateResponse(body []byte, model string) ([]byte, error) {
	var oResp openaiResponse
	if err := json.Unmarshal(body, &oResp); err != nil {
		return nil, err
	}

	resp := anthropicResponse{
		ID:    "msg_" + oResp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	// Translate output items to content blocks.
	for _, item := range oResp.Output {
		switch item.Type {
		case "message":
			var parts []openaiOutputContent
			if json.Unmarshal(item.Content, &parts) == nil {
				for _, p := range parts {
					if p.Type == "output_text" && p.Text != "" {
						resp.Content = append(resp.Content, anthropicContentBlock{
							Type: "text",
							Text: p.Text,
						})
					}
				}
			}

		case "function_call":
			var inputJSON json.RawMessage
			if item.Arguments != "" {
				inputJSON = json.RawMessage(item.Arguments)
			} else {
				inputJSON = json.RawMessage("{}")
			}
			resp.Content = append(resp.Content, anthropicContentBlock{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: inputJSON,
			})
		}
	}

	if len(resp.Content) == 0 {
		return nil, fmt.Errorf("upstream response from %s contained no translatable content (observed output types: %s)", model, describeOutputContentTypes(oResp.Output))
	}

	// Stop reason.
	switch oResp.Status {
	case "completed":
		// Check if last content block is a tool use.
		if len(resp.Content) > 0 && resp.Content[len(resp.Content)-1].Type == "tool_use" {
			resp.StopReason = "tool_use"
		} else {
			resp.StopReason = "end_turn"
		}
	case "incomplete":
		resp.StopReason = "max_tokens"
	default:
		resp.StopReason = "end_turn"
	}

	// Usage.
	if oResp.Usage != nil {
		resp.Usage = translateUsage(oResp.Usage)
	}

	return json.Marshal(resp)
}

func translateUsage(usage *openaiUsage) anthropicUsage {
	if usage == nil {
		return anthropicUsage{}
	}
	translated := anthropicUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	}
	if usage.TotalTokens > 0 {
		translated.TotalTokens = intPtr(usage.TotalTokens)
	}
	if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
		translated.CacheReadInputTokens = intPtr(usage.InputTokensDetails.CachedTokens)
	}
	if usage.OutputTokensDetails != nil && usage.OutputTokensDetails.ReasoningTokens > 0 {
		translated.ReasoningOutputTokens = intPtr(usage.OutputTokensDetails.ReasoningTokens)
	}
	return translated
}

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }
