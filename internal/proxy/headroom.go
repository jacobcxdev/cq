package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// headroomScript is the Python script that bridges headroom's compress()
// function via JSON lines over stdin/stdout.
const headroomScript = `
import json, sys
from headroom import compress
from headroom.models import get_model_info

def model_limit(model):
    info = get_model_info(model)
    if info and info.context_window:
        return info.context_window
    return 200000

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    msgs = req.get("messages", [])
    if not msgs:
        print(json.dumps({"messages": [], "tokens_saved": 0, "compression_ratio": 0}), flush=True)
        continue
    model = req.get("model", "")
    r = compress(msgs, model=model, model_limit=model_limit(model))
    print(json.dumps({"messages": r.messages, "tokens_saved": r.tokens_saved, "compression_ratio": r.compression_ratio}), flush=True)
`

// HeadroomBridge manages a persistent Python subprocess that compresses
// LLM messages via the headroom-ai library.
type HeadroomBridge struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
}

// headroomRequest is the JSON line sent to the Python bridge.
type headroomRequest struct {
	Messages json.RawMessage `json:"messages"`
	Model    string          `json:"model"`
}

// headroomResponse is the JSON line received from the Python bridge.
type headroomResponse struct {
	Messages         json.RawMessage `json:"messages"`
	TokensSaved      int             `json:"tokens_saved"`
	CompressionRatio float64         `json:"compression_ratio"`
}

// findPython3 returns the path to a python3 binary, preferring Homebrew
// installations over the system Python. LaunchAgents run with a minimal
// PATH that excludes /opt/homebrew/bin, so we probe well-known paths first.
func findPython3() (string, error) {
	var candidates []string
	if runtime.GOARCH == "arm64" {
		candidates = append(candidates, "/opt/homebrew/bin/python3")
	}
	candidates = append(candidates, "/usr/local/bin/python3")
	if p, err := exec.LookPath("python3"); err == nil {
		candidates = append(candidates, p)
	}

	seen := make(map[string]bool)
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("python3 not found in PATH or well-known locations")
}

// StartHeadroomBridge spawns the Python subprocess and verifies headroom-ai
// is importable by sending a ping. Returns an error with an install hint if
// the library is missing.
func StartHeadroomBridge() (*HeadroomBridge, error) {
	pythonPath, err := findPython3()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(pythonPath, "-u", "-c", headroomScript)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("start python3: %w", err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRequestBody)

	b := &HeadroomBridge{
		cmd:    cmd,
		stdin:  stdin,
		stdout: scanner,
	}

	// Ping to verify headroom-ai is installed.
	if err := b.ping(); err != nil {
		b.Stop()
		return nil, fmt.Errorf("headroom bridge ping failed (is headroom-ai installed? pip install \"headroom-ai[all]\"): %w", err)
	}

	return b, nil
}

// ping sends an empty messages array and expects a response within 5 seconds.
func (b *HeadroomBridge) ping() error {
	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		_, _, err := b.compress([]byte(`[]`), "")
		ch <- result{err: err}
	}()

	select {
	case r := <-ch:
		return r.err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timeout waiting for bridge response")
	}
}

// Compress takes a full request body, extracts and compresses the messages,
// then splices them back in. Returns the modified body and tokens saved.
// On any error, returns the original body unchanged with 0 tokens saved.
func (b *HeadroomBridge) Compress(body []byte) ([]byte, int, error) {
	// Extract messages and model from body.
	var partial struct {
		Messages json.RawMessage `json:"messages"`
		Model    string          `json:"model"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return body, 0, fmt.Errorf("parse request body: %w", err)
	}
	if len(partial.Messages) == 0 || string(partial.Messages) == "null" {
		return body, 0, nil
	}

	compressed, saved, err := b.compress(partial.Messages, partial.Model)
	if err != nil {
		return body, 0, err
	}
	if saved <= 0 {
		return body, 0, nil
	}

	// Splice compressed messages back into the original body.
	spliced, err := spliceMessages(body, compressed)
	if err != nil {
		return body, 0, fmt.Errorf("splice compressed messages: %w", err)
	}

	return spliced, saved, nil
}

// compress sends messages to the bridge and returns compressed messages.
func (b *HeadroomBridge) compress(messages json.RawMessage, model string) (json.RawMessage, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	req := headroomRequest{
		Messages: messages,
		Model:    model,
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal bridge request: %w", err)
	}

	line = append(line, '\n')
	if _, err := b.stdin.Write(line); err != nil {
		return nil, 0, fmt.Errorf("write to bridge: %w", err)
	}

	if !b.stdout.Scan() {
		if err := b.stdout.Err(); err != nil {
			return nil, 0, fmt.Errorf("read from bridge: %w", err)
		}
		return nil, 0, fmt.Errorf("bridge process exited unexpectedly")
	}

	var resp headroomResponse
	if err := json.Unmarshal(b.stdout.Bytes(), &resp); err != nil {
		return nil, 0, fmt.Errorf("parse bridge response: %w", err)
	}

	return resp.Messages, resp.TokensSaved, nil
}

// spliceMessages replaces the "messages" field in body with compressed messages,
// preserving all other fields.
func spliceMessages(body []byte, messages json.RawMessage) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	raw["messages"] = messages
	return json.Marshal(raw)
}

// Stop shuts down the Python subprocess gracefully, falling back to SIGKILL.
func (b *HeadroomBridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.stdin.Close()

	done := make(chan struct{})
	go func() {
		b.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		b.cmd.Process.Kill()
		<-done
	}
}
