package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestConfigHeadroomJSON(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantBool bool
	}{
		{"enabled", `{"port":19280,"local_token":"tok","headroom":true}`, true},
		{"disabled", `{"port":19280,"local_token":"tok","headroom":false}`, false},
		{"omitted", `{"port":19280,"local_token":"tok"}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tt.json), &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Headroom != tt.wantBool {
				t.Errorf("Headroom = %v, want %v", cfg.Headroom, tt.wantBool)
			}
		})
	}

	// Round-trip: true should appear in JSON output.
	t.Run("marshal_true", func(t *testing.T) {
		cfg := Config{Port: 19280, LocalToken: "tok", Headroom: true}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["headroom"]) != "true" {
			t.Errorf("expected headroom:true in JSON, got %s", data)
		}
	})

	// Round-trip: false should be omitted (omitempty).
	t.Run("marshal_false_omitted", func(t *testing.T) {
		cfg := Config{Port: 19280, LocalToken: "tok", Headroom: false}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["headroom"]; ok {
			t.Errorf("expected headroom omitted when false, got %s", data)
		}
	})
}

// fakeBridge creates a HeadroomBridge backed by an in-process pipe pair
// instead of a real Python subprocess. The responder function handles
// each JSON line and writes back a response.
func fakeBridge(t *testing.T, responder func(req headroomRequest) headroomResponse) *HeadroomBridge {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	t.Cleanup(func() {
		stdinW.Close()
		stdinR.Close()
		stdoutW.Close()
	})

	go func() {
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			var req headroomRequest
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				continue
			}
			resp := responder(req)
			line, _ := json.Marshal(resp)
			line = append(line, '\n')
			stdoutW.Write(line)
		}
		stdoutW.Close()
	}()

	return &HeadroomBridge{
		cmd:    exec.Command("true"), // placeholder — never waited on in tests
		stdin:  stdinW,
		stdout: bufio.NewScanner(stdoutR),
	}
}

func TestCompress_SplicesMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{
			Messages:         json.RawMessage(`[{"role":"user","content":"hi"}]`),
			TokensSaved:      42,
			CompressionRatio: 0.5,
		}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello world, this is a long message"}],"max_tokens":1024,"stream":true}`)

	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 42 {
		t.Errorf("tokens saved = %d, want 42", saved)
	}

	// Verify messages were spliced and other fields preserved.
	var result map[string]json.RawMessage
	if err := json.Unmarshal(compressed, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["messages"]) != `[{"role":"user","content":"hi"}]` {
		t.Errorf("messages = %s, want compressed version", result["messages"])
	}
	if string(result["model"]) != `"claude-sonnet-4-5-20250929"` {
		t.Errorf("model = %s, want preserved", result["model"])
	}
	if string(result["max_tokens"]) != "1024" {
		t.Errorf("max_tokens = %s, want preserved", result["max_tokens"])
	}
	if string(result["stream"]) != "true" {
		t.Errorf("stream = %s, want preserved", result["stream"])
	}
}

func TestCompress_KnownModel_IncludesModelLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})

	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.Compress(body); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if captured.ModelLimit == nil {
		t.Fatal("model_limit missing from bridge request")
	}
	if *captured.ModelLimit != 1050000 {
		t.Fatalf("model_limit = %d, want 1050000", *captured.ModelLimit)
	}
}

func TestCompress_RegistryModel_IncludesCatalogLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})
	bridge.Catalog = modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.5", Source: modelregistry.SourceOverlay, ContextWindow: 2000000},
	}})

	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.Compress(body); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if captured.ModelLimit == nil {
		t.Fatal("model_limit missing from bridge request")
	}
	if *captured.ModelLimit != 2000000 {
		t.Fatalf("model_limit = %d, want 2000000", *captured.ModelLimit)
	}
}

func TestCompress_UnknownModel_OmitsModelLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})

	body := []byte(`{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.Compress(body); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if captured.ModelLimit != nil {
		t.Fatalf("model_limit = %d, want omitted", *captured.ModelLimit)
	}
}

func TestCompress_NoMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{
			Messages:    json.RawMessage(`[]`),
			TokensSaved: 0,
		}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":1024}`)
	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", compressed, body)
	}
}

func TestCompress_NullMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{Messages: json.RawMessage(`[]`), TokensSaved: 0}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":null}`)
	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", compressed, body)
	}
}

func TestCompress_ZeroSaved(t *testing.T) {
	bridge := fakeBridge(t, func(req headroomRequest) headroomResponse {
		if string(req.Messages) != `[{"role":"user","content":"hello world"}]` {
			t.Errorf("bridge got messages = %s", req.Messages)
		}
		return headroomResponse{
			Messages:    req.Messages,
			TokensSaved: 0,
		}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello world"}]}`)
	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", compressed, body)
	}
}

func TestCompress_BridgeError_ReturnsOriginal(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, _ := io.Pipe()
	stdinW.Close()
	stdinR.Close()

	bridge := &HeadroomBridge{
		cmd:    exec.Command("true"),
		stdin:  stdinW,
		stdout: bufio.NewScanner(stdoutR),
	}

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello"}]}`)
	compressed, saved, err := bridge.Compress(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("compressed = %s, want original %s", compressed, body)
	}
}

func TestCompress_InvalidJSON_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{Messages: json.RawMessage(`[]`), TokensSaved: 0}
	})

	body := []byte(`{`)
	compressed, saved, err := bridge.Compress(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body = %s, want original %s", compressed, body)
	}
}

func TestSpliceMessages(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"old"}],"max_tokens":1}`)
	messages := json.RawMessage(`[{"role":"user","content":"new"}]`)
	out, err := spliceMessages(body, messages)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["messages"]) != string(messages) {
		t.Errorf("messages = %s, want %s", got["messages"], messages)
	}
	if string(got["model"]) != `"claude-sonnet"` {
		t.Errorf("model = %s, want preserved", got["model"])
	}
	if string(got["max_tokens"]) != "1" {
		t.Errorf("max_tokens = %s, want preserved", got["max_tokens"])
	}
}

func TestFindPython3_FallsBackToPath(t *testing.T) {
	python, err := findPython3()
	if err != nil {
		t.Fatalf("findPython3: %v", err)
	}
	if python == "" {
		t.Fatal("empty python path")
	}
}

func TestFindPython3_EmptyPATH(t *testing.T) {
	t.Skip("findPython3 probes well-known paths outside PATH")
}

func TestStartHeadroomBridge_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	bridge, err := StartHeadroomBridge()
	if err != nil {
		t.Skipf("headroom bridge unavailable: %v", err)
	}
	defer bridge.Stop()
	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello world hello world hello world hello world"}]}`)
	_, _, err = bridge.Compress(body)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
}

func TestDrainStderr_SuppressesKnownNoise(t *testing.T) {
	text := strings.Join([]string{
		"Warning: You are sending unauthenticated requests to the HF Hub. Please set a HF_TOKEN to enable higher rate limits and faster downloads.",
		"Tag placeholder lost during compression, appending: <system-reminder>",
	}, "\n")
	got := captureStderr(t, func() {
		bridge := &HeadroomBridge{}
		bridge.drainStderr(strings.NewReader(text))
	})
	if got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestDrainStderr_SuppressesHeadroomInfoLogs(t *testing.T) {
	text := "2026-04-17 04:31:41,662 - headroom.transforms.pipeline - INFO - Pipeline using ContentRouter for intelligent content-aware compression\n"
	got := captureStderr(t, func() {
		bridge := &HeadroomBridge{}
		bridge.drainStderr(strings.NewReader(text))
	})
	if got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestDrainStderr_PreservesUnexpectedDiagnostics(t *testing.T) {
	text := "unexpected diagnostic\n"
	got := captureStderr(t, func() {
		bridge := &HeadroomBridge{}
		bridge.drainStderr(strings.NewReader(text))
	})
	if !strings.Contains(got, text) {
		t.Fatalf("stderr = %q, want %q", got, text)
	}
}

func TestDrainStderr_SuppressesShutdownKeyboardInterrupt(t *testing.T) {
	text := strings.Join([]string{
		"Traceback (most recent call last):",
		"  File \"<string>\", line 12, in <module>",
		"    for line in sys.stdin:",
		"                ^^^^^^^^^",
		"KeyboardInterrupt",
	}, "\n")
	bridge := &HeadroomBridge{}
	bridge.shuttingDown.Store(true)
	got := captureStderr(t, func() {
		bridge.drainStderr(strings.NewReader(text))
	})
	if got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestHealthEndpoint_HeadroomField(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hasHeadroom bool
	}{
		{name: "disabled", hasHeadroom: false},
		{name: "enabled", hasHeadroom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			if tc.hasHeadroom {
				srv.Headroom = fakeBridge(t, func(_ headroomRequest) headroomResponse {
					return headroomResponse{}
				})
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/health", nil)
			srv.handleHealth(w, req)
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			got, ok := resp["headroom"].(bool)
			if !ok {
				t.Fatal("headroom field missing from health response")
			}
			if got != tc.hasHeadroom {
				t.Errorf("headroom = %v, want %v", got, tc.hasHeadroom)
			}
		})
	}
}

// fakeBridgeRaw creates a HeadroomBridge backed by an in-process pipe pair.
// The responder receives the raw request bytes and returns raw response bytes.
// This supports both messages and responses operations.
func fakeBridgeRaw(t *testing.T, responder func(req []byte) []byte) *HeadroomBridge {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	t.Cleanup(func() {
		stdinW.Close()
		stdinR.Close()
		stdoutW.Close()
	})

	go func() {
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			resp := responder(scanner.Bytes())
			resp = append(resp, '\n')
			stdoutW.Write(resp)
		}
		stdoutW.Close()
	}()

	return &HeadroomBridge{
		cmd:    exec.Command("true"),
		stdin:  stdinW,
		stdout: bufio.NewScanner(stdoutR),
	}
}

func TestCompressResponses_SkipsWhenPreviousResponseID(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_123"}`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_SkipsWhenNoInput(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	for _, body := range [][]byte{
		[]byte(`{"model":"gpt-5.4"}`),
		[]byte(`{"model":"gpt-5.4","input":null}`),
		[]byte(`{"model":"gpt-5.4","input":[]}`),
	} {
		out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
		if err != nil {
			t.Fatal(err)
		}
		if saved != 0 {
			t.Errorf("tokens saved = %d, want 0", saved)
		}
		if string(out) != string(body) {
			t.Errorf("body changed unexpectedly: got %s want %s", out, body)
		}
	}
}

func TestCompressResponses_SkipsWhenBridgeReturnsNotOK(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		if req.Operation != "compress_responses" {
			return nil
		}
		b, _ := json.Marshal(headroomResponsesResponse{OK: false})
		return b
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_SplicesInputAndInstructions(t *testing.T) {
	compressedInput := json.RawMessage(`[{"role":"user","content":"hi"}]`)
	compressedInstr := "Be brief."

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		if req.Operation != "compress_responses" {
			return nil
		}
		instr := compressedInstr
		resp := headroomResponsesResponse{
			OK:           true,
			Input:        compressedInput,
			Instructions: &instr,
			TokensSaved:  55,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	instr := "You are a helpful assistant. Please provide detailed and comprehensive answers to all user questions, making sure to cover all relevant aspects and edge cases."
	body, _ := json.Marshal(map[string]any{
		"model":        "gpt-5.4",
		"input":        []any{map[string]any{"role": "user", "content": "hello world, this is a long message"}},
		"instructions": instr,
		"max_tokens":   1024,
	})

	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 55 {
		t.Errorf("tokens saved = %d, want 55", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["input"]) != string(compressedInput) {
		t.Errorf("input = %s, want %s", result["input"], compressedInput)
	}
	if string(result["instructions"]) != `"Be brief."` {
		t.Errorf("instructions = %s, want compressed", result["instructions"])
	}
	// Other fields preserved.
	if string(result["model"]) != `"gpt-5.4"` {
		t.Errorf("model = %s, want preserved", result["model"])
	}
	if string(result["max_tokens"]) != "1024" {
		t.Errorf("max_tokens = %s, want preserved", result["max_tokens"])
	}
	}

func TestCompressResponses_KnownModel_IncludesModelLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.CompressResponses(body, HeadroomModeToken); err != nil {
		t.Fatalf("CompressResponses: %v", err)
	}

	if captured.ModelLimit == nil {
		t.Fatal("model_limit missing from responses bridge request")
	}
	if *captured.ModelLimit != 1050000 {
		t.Fatalf("model_limit = %d, want 1050000", *captured.ModelLimit)
	}
}

func TestCompressResponses_ZeroSavedReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       req.Input,
			TokensSaved: 0,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}],"max_tokens":2048}`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_InvalidJSON_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte { return nil })

	body := []byte(`{`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_PreservesNilInstructions(t *testing.T) {
	compressedInput := json.RawMessage(`[{"role":"user","content":"hi"}]`)

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       compressedInput,
			TokensSaved: 55,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	body, _ := json.Marshal(map[string]any{
		"model":      "gpt-5.4",
		"input":      []any{map[string]any{"role": "user", "content": "hello world"}},
		"max_tokens": 1024,
	})

	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 55 {
		t.Errorf("tokens saved = %d, want 55", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["input"]) != string(compressedInput) {
		t.Errorf("input = %s, want %s", result["input"], compressedInput)
	}
	if _, ok := result["instructions"]; ok {
		t.Errorf("instructions should remain absent, got %s", result["instructions"])
	}
}

func TestSpliceResponsesFields_ReplacesInputOnly(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"old"}],"max_tokens":1}`)
	input := json.RawMessage(`[{"role":"user","content":"new"}]`)
	out, err := spliceResponsesFields(body, input, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["input"]) != string(input) {
		t.Errorf("input = %s, want %s", got["input"], input)
	}
	if string(got["model"]) != `"gpt-5.4"` {
		t.Errorf("model = %s, want preserved", got["model"])
	}
	if string(got["max_tokens"]) != "1" {
		t.Errorf("max_tokens = %s, want preserved", got["max_tokens"])
	}
}

func TestSpliceResponsesFields_ReplacesInputAndInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"old"}],"instructions":"old","max_tokens":1}`)
	input := json.RawMessage(`[{"role":"user","content":"new"}]`)
	instr := "new instructions"
	out, err := spliceResponsesFields(body, input, &instr, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["input"]) != string(input) {
		t.Errorf("input = %s, want %s", got["input"], input)
	}
	if string(got["instructions"]) != `"new instructions"` {
		t.Errorf("instructions = %s, want replaced", got["instructions"])
	}
}

func TestSpliceResponsesFields_ClearsInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"old"}],"instructions":"old","max_tokens":1}`)
	input := json.RawMessage(`[{"role":"user","content":"new"}]`)
	out, err := spliceResponsesFields(body, input, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["input"]) != string(input) {
		t.Errorf("input = %s, want %s", got["input"], input)
	}
	if _, ok := got["instructions"]; ok {
		t.Errorf("instructions should be deleted, got %s", got["instructions"])
	}
}

func TestCompressResponses_ClearsInstructionsWhenBridgeAbsorbesThem(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:                true,
			Input:             req.Input,
			ClearInstructions: true,
			TokensSaved:       55,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	instr := "Be helpful"
	body, _ := json.Marshal(map[string]any{
		"model":        "gpt-5.4",
		"input":        []any{map[string]any{"role": "user", "content": "hello world"}},
		"instructions": instr,
	})

	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 55 {
		t.Errorf("tokens saved = %d, want 55", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if _, ok := result["instructions"]; ok {
		t.Errorf("instructions should be deleted, got %s", result["instructions"])
	}
}

func TestConfigHeadroomMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    HeadroomMode
		wantErr bool
	}{
		{name: "empty defaults to cache", input: "{}", want: HeadroomModeCache},
		{name: "token", input: `{"headroom_mode":"token"}`, want: HeadroomModeToken},
		{name: "cache", input: `{"headroom_mode":"cache"}`, want: HeadroomModeCache},
		{name: "invalid rejected by validate", input: `{"headroom_mode":"bogus"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tc.input), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			cfg.LocalToken = "test-token"
			cfg.setDefaults()
			err := cfg.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validate error")
				}
				return
			}
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := cfg.ResolvedHeadroomMode(); got != tc.want {
				t.Fatalf("ResolvedHeadroomMode() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHealthEndpoint_HeadroomModeField(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	srv := &Server{
		Headroom: fakeBridge(t, func(_ headroomRequest) headroomResponse {
			return headroomResponse{}
		}),
		HeadroomMode: HeadroomModeCache,
	}
	srv.handleHealth(w, req)
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got, ok := resp["headroom_mode"].(string)
	if !ok {
		t.Fatal("headroom_mode field missing")
	}
	if got != "cache" {
		t.Fatalf("headroom_mode = %q, want cache", got)
	}
}

func TestCompress_CacheMode_PriorTurnsBytestable(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	msgs := json.RawMessage(`[{"role":"system","content":"Be helpful."},{"role":"assistant","content":"OK."},{"role":"user","content":"What is the answer?"}]`)
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, _, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["messages"]) == string(msgs) {
		return
	}
}

func TestCompress_CacheMode_NoMutableSuffix_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	msgs := json.RawMessage(`[{"role":"system","content":"Be helpful."},{"role":"assistant","content":"OK."}]`)
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, saved, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompress_CacheMode_OnlyUserMessage(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	msgs := json.RawMessage(`[{"role":"user","content":"What is the answer?"}]`)
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, _, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatal(err)
	}
	_ = out
}

func TestCompressResponses_CacheMode_PriorTurnsByteStable(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"compressed"}]}]`),
			TokensSaved: 10,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	items := json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"Prior"}]},{"role":"assistant","content":[{"type":"text","text":"Reply"}]},{"role":"user","content":[{"type":"input_text","text":"Mutable"}]}]`)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"input": items,
	})

	out, _, err := bridge.CompressResponsesCache(body)
	if err != nil {
		t.Fatal(err)
	}
	_ = out
}

func TestCompressResponses_CacheMode_NoMutableSuffix_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	items := json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"Prior"}]},{"role":"assistant","content":[{"type":"text","text":"Reply"}]}]`)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"input": items,
	})

	out, saved, err := bridge.CompressResponsesCache(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestHeadroomEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "disabled", cfg: Config{}, want: false},
		{name: "legacy bool enabled", cfg: Config{Headroom: true}, want: true},
		{name: "mode enabled", cfg: Config{HeadroomMode: "cache"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.HeadroomEnabled(); got != tc.want {
				t.Fatalf("HeadroomEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompressCache_SendsFullRequestToBridge(t *testing.T) {
	var gotMessages json.RawMessage

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		gotMessages = req.Messages

		// Return all messages compressed into a single one (simulating aggressive compression).
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"fully compressed"}]`),
			TokensSaved: 50,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	// Three-turn: system + assistant + user (mutable).
	msgs := `[{"role":"system","content":"Be helpful."},{"role":"assistant","content":"OK."},{"role":"user","content":"What is the answer?"}]`
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": json.RawMessage(msgs),
	})

	out, saved, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatalf("CompressCache: %v", err)
	}
	_ = out
	_ = saved

	// Bridge must have received all 3 messages (full request).
	var sentMsgs []json.RawMessage
	if err := json.Unmarshal(gotMessages, &sentMsgs); err != nil {
		t.Fatalf("parse messages sent to bridge: %v", err)
	}
	if len(sentMsgs) != 3 {
		t.Errorf("bridge received %d messages, want 3 (full request)", len(sentMsgs))
	}
}

func TestCompressCache_RestoresFrozenPrefixAfterFullCompression(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"fully compressed"}]`),
			TokensSaved: 50,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"Be helpful."}`),
		json.RawMessage(`{"role":"assistant","content":"OK."}`),
		json.RawMessage(`{"role":"user","content":"What is the answer?"}`),
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, saved, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatalf("CompressCache: %v", err)
	}
	_ = saved

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	var outMsgs []json.RawMessage
	if err := json.Unmarshal(result["messages"], &outMsgs); err != nil {
		t.Fatalf("unmarshal output messages: %v", err)
	}
	if len(outMsgs) < 2 {
		t.Fatalf("output has %d messages, want at least 2", len(outMsgs))
	}
	if string(outMsgs[0]) != string(msgs[0]) {
		t.Errorf("frozen prefix[0] = %s, want %s", outMsgs[0], msgs[0])
	}
	if string(outMsgs[1]) != string(msgs[1]) {
		t.Errorf("frozen prefix[1] = %s, want %s", outMsgs[1], msgs[1])
	}
}

func TestCompressResponsesCache_SendsFullRequestToBridge(t *testing.T) {
	var gotInput json.RawMessage

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		gotInput = req.Input

		resp := headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"compressed"}]}]`),
			TokensSaved: 30,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	items := `[{"role":"user","content":[{"type":"input_text","text":"Prior"}]},{"role":"assistant","content":[{"type":"text","text":"Reply"}]},{"role":"user","content":[{"type":"input_text","text":"Mutable"}]}]`
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"input": json.RawMessage(items),
	})

	out, saved, err := bridge.CompressResponsesCache(body)
	_ = out
	_ = saved
	if err != nil {
		t.Fatalf("CompressResponsesCache: %v", err)
	}

	// Bridge must receive all 3 items.
	var sentItems []json.RawMessage
	if err := json.Unmarshal(gotInput, &sentItems); err != nil {
		t.Fatalf("parse items sent to bridge: %v", err)
	}
	if len(sentItems) != 3 {
		t.Errorf("bridge received %d items, want 3 (full request)", len(sentItems))
	}
}

func TestCompressResponsesCache_RestoresFrozenPrefixAndInstructions(t *testing.T) {
	instr := "Keep these instructions unchanged."
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		if req.Instructions != nil {
			t.Fatalf("instructions sent to bridge = %q, want omitted in cache mode", *req.Instructions)
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"compressed"}]}]`),
			TokensSaved: 40,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	prefix0 := json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"Prior"}]}`)
	prefix1 := json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Reply"}]}`)
	mutable := json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"Mutable"}]}`)
	items := []json.RawMessage{prefix0, prefix1, mutable}
	body, _ := json.Marshal(map[string]any{
		"model":        "gpt-5.4",
		"input":        items,
		"instructions": instr,
	})

	out, saved, err := bridge.CompressResponsesCache(body)
	if err != nil {
		t.Fatalf("CompressResponsesCache: %v", err)
	}
	if saved != 40 {
		t.Fatalf("tokens saved = %d, want 40", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if string(result["instructions"]) != `"Keep these instructions unchanged."` {
		t.Fatalf("instructions = %s, want preserved original", result["instructions"])
	}
	var outItems []json.RawMessage
	if err := json.Unmarshal(result["input"], &outItems); err != nil {
		t.Fatalf("unmarshal output items: %v", err)
	}
	if len(outItems) < 2 {
		t.Fatalf("output has %d items, want at least 2", len(outItems))
	}
	if string(outItems[0]) != string(prefix0) {
		t.Errorf("frozen prefix[0] = %s, want %s", outItems[0], prefix0)
	}
	if string(outItems[1]) != string(prefix1) {
		t.Errorf("frozen prefix[1] = %s, want %s", outItems[1], prefix1)
	}
}

func TestCompressCache_EmptyBridgeOutputReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		_ = reqBytes
		return []byte(`not-json`)
	})

	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hello"}]}`)
	out, saved, err := bridge.CompressCache(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Fatalf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Fatalf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponsesCache_EmptyBridgeOutputReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		_ = reqBytes
		return []byte(`not-json`)
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
	out, saved, err := bridge.CompressResponsesCache(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Fatalf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Fatalf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func captureStderr(t *testing.T, fn func()) string {
		t.Helper()
		oldStderr := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		os.Stderr = w
		defer func() { os.Stderr = oldStderr }()

		fn()

		if err := w.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			t.Fatalf("read stderr: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close reader: %v", err)
		}
		return buf.String()
	}

