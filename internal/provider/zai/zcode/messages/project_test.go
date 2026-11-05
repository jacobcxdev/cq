package messages

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectToolHistoryAndNamespaces(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","instructions":"Keep facts.","stream":true,"reasoning":{"effort":"max"},"input":[{"role":"user","content":[{"type":"input_text","text":"Read fixture."}]},{"type":"function_call","namespace":"functions","name":"exec","call_id":"call-1","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call-1","output":"/tmp"},{"type":"custom_tool_call","name":"apply_patch","call_id":"call-2","input":"*** patch"},{"type":"custom_tool_call_output","call_id":"call-2","output":"done"}],"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]},{"type":"custom","name":"apply_patch","description":"Edit files."}]}`)
	raw, codec, err := Project(body)
	if err != nil {
		t.Fatal(err)
	}
	var projected struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string                       `json:"role"`
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	if projected.Model != "glm-5.3" || !projected.Stream || projected.System[0].Text != "Keep facts." || len(projected.Tools) != 2 || len(projected.Messages) != 5 {
		t.Fatalf("projection: %s", raw)
	}
	for _, tool := range projected.Tools {
		if len(tool.Name) > 64 || strings.ContainsAny(tool.Name, ". ") {
			t.Fatalf("invalid tool name: %q", tool.Name)
		}
	}
	response := []byte(`{"id":"msg-1","type":"message","role":"assistant","model":"glm-5.3","content":[{"type":"tool_use","id":"call-3","name":"` + projected.Tools[0].Name + `","input":{"cmd":"ls"}},{"type":"tool_use","id":"call-4","name":"` + projected.Tools[1].Name + `","input":{"input":"new patch"}}],"stop_reason":"tool_use","usage":{"input_tokens":7,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":4}}`)
	out, err := codec.ConvertJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	var native struct {
		Output []struct{ Type, Namespace, Name, Arguments, Input string } `json:"output"`
		Usage  struct {
			InputTokens int `json:"input_tokens"`
			TotalTokens int `json:"total_tokens"`
			Details     struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &native); err != nil {
		t.Fatal(err)
	}
	if len(native.Output) != 2 || native.Output[0].Namespace != "functions" || native.Output[0].Name != "exec" || native.Output[0].Arguments != `{"cmd":"ls"}` || native.Output[1].Type != "custom_tool_call" || native.Output[1].Input != "new patch" {
		t.Fatalf("tool restoration: %s", out)
	}
	if native.Usage.InputTokens != 15 || native.Usage.TotalTokens != 19 || native.Usage.Details.CachedTokens != 5 {
		t.Fatalf("usage: %s", out)
	}
}

func TestProjectRejectedUnrepresentableInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"glm","previous_response_id":"opaque","input":"continue"}`,
		`{"model":"glm","input":[{"type":"reasoning","encrypted_content":"foreign"}]}`,
		`{"model":"glm","input":[{"type":"function_call","name":"exec","call_id":"c","arguments":"[]"}]}`,
		`{"model":"glm","input":[{"role":"user","content":[{"type":"unknown","text":"lost"}]}]}`,
		`{"model":"glm","input":"test","tools":[{"type":"web_search"}]}`,
		`{"model":"glm","input":"test","tools":[{"type":"function","name":"f"},{"type":"custom","name":"f"}]}`,
	} {
		if _, _, err := Project([]byte(body)); err == nil {
			t.Fatalf("accepted unsupported request: %s", body)
		}
	}
}

func TestProjectPreservedCompletedMalformedCallPairAsLiteralHistory(t *testing.T) {
	arguments := "{\"query\":\"unterminated\n<literal-record>"
	result := "Recorded result\n<literal-output>"
	body, _ := json.Marshal(map[string]any{"model": "glm-5.3", "input": []any{
		map[string]any{"role": "user", "content": "Continue the task."},
		map[string]any{"type": "function_call", "namespace": "fixture", "name": "lookup", "call_id": "broken-call", "status": "completed", "arguments": arguments},
		map[string]any{"type": "function_call_output", "call_id": "broken-call", "output": []any{map[string]string{"type": "input_text", "text": result}}},
	}})
	raw, _, err := Project(body)
	if err != nil {
		t.Fatal(err)
	}
	var projected struct {
		Messages []struct {
			Role    string                        `json:"role"`
			Content []struct{ Type, Text string } `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Messages) != 3 || projected.Messages[1].Role != "assistant" || projected.Messages[2].Role != "user" {
		t.Fatalf("historical roles changed: %s", raw)
	}
	callText := ""
	for _, message := range projected.Messages {
		for _, block := range message.Content {
			if block.Type != "text" {
				t.Fatalf("literal history became executable control: %s", raw)
			}
		}
	}
	for _, block := range projected.Messages[1].Content {
		callText += block.Text
	}
	if !strings.Contains(callText, "Historical function call") || !strings.Contains(callText, "fixture.lookup") || !strings.Contains(callText, "broken-call") || !strings.Contains(callText, arguments) {
		t.Fatal("raw malformed call history was lost")
	}
	resultFound, labelFound := false, false
	for _, block := range projected.Messages[2].Content {
		if block.Text == result {
			resultFound = true
		}
		if strings.Contains(block.Text, "Historical result") {
			labelFound = true
		}
	}
	if !resultFound || !labelFound {
		t.Fatal("raw paired result or its history label was lost")
	}
}

func TestProjectPreservedUnattributedHistoricalFunctionResult(t *testing.T) {
	result := "Unattributed output\n[ordinary literal text]"
	body, _ := json.Marshal(map[string]any{"model": "glm-5.3", "input": []any{
		map[string]any{"role": "user", "content": "Continue."},
		map[string]any{"type": "function_call_output", "namespace": "fixture", "name": "background", "output": result},
	}})
	raw, _, err := Project(body)
	if err != nil {
		t.Fatal(err)
	}
	var projected struct {
		Messages []struct {
			Role    string                        `json:"role"`
			Content []struct{ Type, Text string } `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &projected) != nil || len(projected.Messages) != 1 || projected.Messages[0].Role != "user" {
		t.Fatalf("unattributed output role changed: %s", raw)
	}
	resultFound, labelFound := false, false
	for _, block := range projected.Messages[0].Content {
		if block.Type != "text" {
			t.Fatal("unattributed output invented a tool identity")
		}
		if block.Text == result {
			resultFound = true
		}
		if strings.Contains(block.Text, "Unattributed historical function result") && strings.Contains(block.Text, "fixture.background") {
			labelFound = true
		}
	}
	if !resultFound || !labelFound {
		t.Fatal("unattributed result contents or metadata were lost")
	}
}

func TestProjectMixedValidAndMalformedParallelHistory(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","input":[
	 {"role":"user","content":"Continue."},
	 {"type":"function_call","name":"valid","call_id":"good","arguments":"{\"value\":1}"},
	 {"type":"function_call","name":"broken","call_id":"bad","status":"completed","arguments":"{\"value\":"},
	 {"type":"custom_tool_call","name":"custom","call_id":"custom","input":"literal custom input"},
	 {"type":"function_call_output","call_id":"good","output":"valid result"},
	 {"type":"function_call_output","call_id":"bad","output":"broken result preserved"},
	 {"type":"custom_tool_call_output","call_id":"custom","output":"custom result"}
	]}`)
	raw, _, err := Project(body)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		t.Fatal("invalid projected JSON")
	}
	var rows []struct {
		Role    string                       `json:"role"`
		Content []map[string]json.RawMessage `json:"content"`
	}
	if json.Unmarshal(object["messages"], &rows) != nil {
		t.Fatal("invalid projected messages")
	}
	uses, results := map[string]bool{}, map[string]bool{}
	var resultOrder, userText []string
	for _, row := range rows {
		seenNonResult := false
		for _, block := range row.Content {
			var kind, id string
			_ = json.Unmarshal(block["type"], &kind)
			switch kind {
			case "tool_use":
				_ = json.Unmarshal(block["id"], &id)
				uses[id] = true
			case "tool_result":
				if seenNonResult {
					t.Fatal("tool result followed literal user history")
				}
				_ = json.Unmarshal(block["tool_use_id"], &id)
				results[id] = true
				resultOrder = append(resultOrder, id)
			default:
				if row.Role == "user" {
					seenNonResult = true
					var text string
					_ = json.Unmarshal(block["text"], &text)
					userText = append(userText, text)
				}
			}
		}
	}
	if len(uses) != 2 || len(results) != 2 || !uses["good"] || !uses["custom"] || !results["good"] || !results["custom"] || uses["bad"] || results["bad"] {
		t.Fatalf("parallel pairs were corrupted: %s", raw)
	}
	if !strings.Contains(string(raw), "broken result preserved") || !strings.Contains(string(raw), "Historical function call") {
		t.Fatal("malformed parallel history disappeared")
	}
	if strings.Join(resultOrder, ",") != "good,custom" || len(userText) != 3 || userText[0] != "Continue." || !strings.HasPrefix(userText[1], "Historical result") || userText[2] != "broken result preserved" {
		t.Fatal("result or literal history order changed")
	}
}

func TestProjectMalformedHistoryFallbackRemainedNarrow(t *testing.T) {
	call := `{"type":"function_call","name":"broken","call_id":"bad","status":"completed","arguments":"{\"value\":"}`
	result := `{"type":"function_call_output","call_id":"bad","output":"recorded result"}`
	for name, input := range map[string]string{
		"unpaired":                   call,
		"not completed":              strings.Replace(call, `"completed"`, `"in_progress"`, 1) + "," + result,
		"missing status":             strings.Replace(call, `"status":"completed",`, "", 1) + "," + result,
		"non-object":                 strings.Replace(call, `"{\"value\":"`, `"[]"`, 1) + "," + result,
		"null arguments":             strings.Replace(call, `"{\"value\":"`, `"null"`, 1) + "," + result,
		"empty arguments":            strings.Replace(call, `"{\"value\":"`, `""`, 1) + "," + result,
		"duplicate call":             call + "," + call + "," + result,
		"duplicate result":           call + "," + result + "," + result,
		"result before call":         result + "," + call,
		"encrypted call":             strings.TrimSuffix(call, "}") + `,"encrypted_arguments":"opaque"},` + result,
		"custom missing identity":    `{"type":"custom_tool_call_output","output":"literal"}`,
		"explicit null identity":     `{"type":"function_call_output","call_id":null,"output":"literal"}`,
		"explicit empty identity":    `{"type":"function_call_output","call_id":"","output":"literal"}`,
		"unsupported result content": `{"type":"function_call_output","output":[{"type":"unknown","text":"literal"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"model":"glm-5.3","input":[{"role":"user","content":"Continue."},` + input + `]}`)
			if _, _, err := Project(body); err == nil {
				t.Fatal("unrepresentable history accepted")
			}
		})
	}
}

func TestProjectOmittedHostedWebSearchWithHarnessTools(t *testing.T) {
	for _, choice := range []string{"", `,"tool_choice":"auto"`, `,"tool_choice":"none"`} {
		body := []byte(`{"model":"glm","input":"test","tools":[{"type":"web_search","search_context_size":"medium"},{"type":"function","name":"exec","parameters":{"type":"object"}},{"type":"web_search_preview"},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"apply_patch"}]}]` + choice + `}`)
		raw, codec, err := Project(body)
		if err != nil {
			t.Fatalf("automatic/disabled hosted search rejected: %v", err)
		}
		var projected struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &projected); err != nil {
			t.Fatal(err)
		}
		want := 2
		if strings.Contains(choice, `"none"`) {
			want = 0
		}
		if len(projected.Tools) != want || len(codec.tools) != 2 {
			t.Fatalf("harness tool definitions lost or hosted tools retained: %s", raw)
		}
		if strings.Contains(string(raw), "web_search") {
			t.Fatalf("hosted web tool reached Messages: %s", raw)
		}
	}
}

func TestProjectRejectedHostedSearchWhenRequiredOrUnaccompanied(t *testing.T) {
	for _, body := range []string{
		`{"model":"glm","input":"test","tools":[{"type":"web_search"},{"type":"function","name":"exec"}],"tool_choice":{"type":"web_search"}}`,
		`{"model":"glm","input":"test","tools":[{"type":"web_search_preview"},{"type":"function","name":"exec"}],"tool_choice":{"type":"web_search_preview"}}`,
		`{"model":"glm","input":"test","tools":[{"type":"web_search"},{"type":"function","name":"exec"}],"tool_choice":"required"}`,
		`{"model":"glm","input":"test","tools":[{"type":"web_search_preview"}]}`,
		`{"model":"glm","input":"test","tools":[{"type":"future_hosted_tool"},{"type":"function","name":"exec"}]}`,
		`{"model":"glm","input":"test","tools":[{"type":"web_search_preview_2025_03_11"},{"type":"function","name":"exec"}]}`,
	} {
		if _, _, err := Project([]byte(body)); err == nil {
			t.Fatalf("accepted unavailable hosted capability: %s", body)
		}
	}
}

func TestProjectPlainInputAndThinking(t *testing.T) {
	raw, codec, err := Project([]byte(`{"model":"glm","max_output_tokens":100,"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"readable"}]},{"role":"assistant","content":[{"type":"output_text","text":"Prior answer."}]},{"role":"user","content":"Continue."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"thinking":"readable"`) || !strings.Contains(string(raw), `"max_tokens":100`) {
		t.Fatalf("readable history missing: %s", raw)
	}
	out, err := codec.ConvertJSON([]byte(`{"id":"r","type":"message","role":"assistant","model":"glm","content":[{"type":"thinking","thinking":"work","signature":"real"},{"type":"text","text":"done"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"status":"incomplete"`) || !strings.Contains(string(out), `"text":"work"`) || !strings.Contains(string(out), `"signature":"real"`) {
		t.Fatalf("thinking or incomplete state lost: %s", out)
	}
}

func TestProjectEncodedNamesRemainDistinctAndStable(t *testing.T) {
	body := []byte(`{"model":"glm","input":"test","tools":[{"type":"function","namespace":"a.b","name":"exec"},{"type":"function","namespace":"a_b","name":"exec"},{"type":"function","namespace":"a","name":"b_exec"}]}`)
	first, _, err := Project(body)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Project(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("tool names changed between requests")
	}
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(first, &request); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range request.Tools {
		if seen[tool.Name] || len(tool.Name) > 64 {
			t.Fatalf("ambiguous or invalid tool name %q", tool.Name)
		}
		seen[tool.Name] = true
	}
}

func TestReplyRejectedMissingOrUnrepresentableData(t *testing.T) {
	_, codec, err := Project([]byte(`{"model":"glm","input":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[],"stop_reason":"end_turn"}`,
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[],"stop_reason":"pause_turn","usage":{}}`,
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[],"stop_reason":"end_turn","usage":{"input_tokens":"42"}}`,
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[],"stop_reason":"end_turn","usage":{"output_tokens":-1}}`,
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[],"stop_reason":"end_turn","usage":{"input_tokens":null,"output_tokens":0}}`,
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[{"type":"text","text":null}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`,
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[{"type":"redacted_thinking","data":"opaque"}],"stop_reason":"end_turn","usage":{}}`,
		`{"id":"r","type":"message","role":"assistant","model":"glm","content":[{"type":"tool_use","id":"c","name":"unknown","input":{}}],"stop_reason":"tool_use","usage":{}}`,
	} {
		if _, err := codec.ConvertJSON([]byte(body)); err == nil {
			t.Fatalf("accepted malformed reply: %s", body)
		}
	}
}
