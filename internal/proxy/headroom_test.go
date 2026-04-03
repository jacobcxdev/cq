package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os/exec"
	"testing"
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

func TestCompress_NoMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		t.Error("bridge should not be called for empty messages")
		return headroomResponse{}
	})

	body := []byte(`{"model":"claude","max_tokens":1024}`)
	out, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Error("body should be returned unchanged")
	}
}

func TestCompress_NullMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		t.Error("bridge should not be called for null messages")
		return headroomResponse{}
	})

	body := []byte(`{"model":"claude","messages":null}`)
	out, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Error("body should be returned unchanged")
	}
}

func TestCompress_ZeroSaved(t *testing.T) {
	bridge := fakeBridge(t, func(req headroomRequest) headroomResponse {
		return headroomResponse{
			Messages:    req.Messages,
			TokensSaved: 0,
		}
	})

	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	out, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Error("body should be returned unchanged when no savings")
	}
}

func TestCompress_BridgeError_ReturnsOriginal(t *testing.T) {
	// Create a bridge with a closed stdin to simulate failure.
	stdinR, stdinW := io.Pipe()
	stdoutR, _ := io.Pipe()
	stdinW.Close()
	stdinR.Close()

	bridge := &HeadroomBridge{
		cmd:    exec.Command("true"),
		stdin:  stdinW,
		stdout: bufio.NewScanner(stdoutR),
	}

	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"test"}]}`)
	out, saved, err := bridge.Compress(body)
	if err == nil {
		t.Error("expected error from broken bridge")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Error("body should be returned unchanged on error")
	}
}

func TestCompress_InvalidJSON_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		t.Error("bridge should not be called for invalid JSON")
		return headroomResponse{}
	})

	body := []byte(`{not valid json`)
	out, saved, err := bridge.Compress(body)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Error("body should be returned unchanged")
	}
}

func TestSpliceMessages(t *testing.T) {
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"original"}],"max_tokens":512}`)
	newMsgs := json.RawMessage(`[{"role":"user","content":"compressed"}]`)

	out, err := spliceMessages(body, newMsgs)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["messages"]) != `[{"role":"user","content":"compressed"}]` {
		t.Errorf("messages = %s, want compressed", result["messages"])
	}
	if string(result["model"]) != `"claude"` {
		t.Errorf("model lost in splice")
	}
	if string(result["max_tokens"]) != "512" {
		t.Errorf("max_tokens lost in splice")
	}
}

func TestStartHeadroomBridge_NoPython(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no python3 here
	_, err := StartHeadroomBridge()
	if err == nil {
		t.Error("expected error when python3 is missing")
	}
}

func TestStartHeadroomBridge_Integration(t *testing.T) {
	// Skip if python3 or headroom-ai is not available.
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command("python3", "-c", "import headroom").CombinedOutput()
	if err != nil {
		t.Skipf("headroom-ai not installed: %s", out)
	}

	bridge, err := StartHeadroomBridge()
	if err != nil {
		t.Fatalf("StartHeadroomBridge: %v", err)
	}
	defer bridge.Stop()

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"Hello, how are you doing today?"}]}`)
	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	// We don't assert exact savings since headroom's output varies,
	// but the body should be valid JSON with messages present.
	var result map[string]json.RawMessage
	if err := json.Unmarshal(compressed, &result); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if _, ok := result["messages"]; !ok {
		t.Error("compressed body missing messages field")
	}
	if _, ok := result["model"]; !ok {
		t.Error("compressed body missing model field")
	}

	_ = saved // may be 0 for short messages — that's fine
}

func TestHealthEndpoint_HeadroomField(t *testing.T) {
	for _, hasHeadroom := range []bool{true, false} {
		name := "without"
		if hasHeadroom {
			name = "with"
		}
		t.Run(name, func(t *testing.T) {
			srv := &Server{
				Config: &Config{LocalToken: "tok"},
			}
			if hasHeadroom {
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
			if got != hasHeadroom {
				t.Errorf("headroom = %v, want %v", got, hasHeadroom)
			}
		})
	}
}
