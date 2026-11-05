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
