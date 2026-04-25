package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

// HeadroomMode controls the compression strategy used by the bridge.
type HeadroomMode int

const (
	// HeadroomModeToken uses token-saving compression.
	HeadroomModeToken HeadroomMode = iota
	// HeadroomModeCache uses cache-aware compression (frozen-prefix semantics).
	HeadroomModeCache
)

// String returns the lowercase string name of the mode ("token" or "cache").
func (m HeadroomMode) String() string {
	switch m {
	case HeadroomModeCache:
		return "cache"
	default:
		return "token"
	}
}

// headroomScript is the Python script that bridges headroom's compress()
// function via JSON lines over stdin/stdout. It dispatches on the "operation"
// field: "compress_messages" (default) or "compress_responses".
const headroomScript = `
import json, sys
from headroom import compress
from headroom.models import get_model_info

# Probe for Responses converter support once at startup.
try:
    from headroom.proxy.responses_converter import (
        responses_items_to_messages,
        messages_to_responses_items,
    )
    _HAS_RESPONSES_CONVERTER = True
except ImportError:
    _HAS_RESPONSES_CONVERTER = False

def model_limit(model, override=0):
    if override and override > 0:
        return override
    info = get_model_info(model)
    if info and info.context_window:
        return info.context_window
    return 200000

def handle_compress_messages(req):
    msgs = req.get("messages", [])
    if not msgs:
        return {"messages": [], "tokens_saved": 0, "compression_ratio": 0}
    model = req.get("model", "")
    limit_override = req.get("model_limit", 0)
    r = compress(msgs, model=model, model_limit=model_limit(model, limit_override))
    return {"messages": r.messages, "tokens_saved": r.tokens_saved, "compression_ratio": r.compression_ratio}

def handle_compress_responses(req):
    if not _HAS_RESPONSES_CONVERTER:
        return {"ok": False, "reason": "no_responses_converter", "input": None, "instructions": None, "tokens_saved": 0}
    items = req.get("input", [])
    if not items:
        return {"ok": False, "reason": "no_input", "input": None, "instructions": None, "tokens_saved": 0}
    model = req.get("model", "")
    limit_override = req.get("model_limit", 0)
    # Convert instructions to a system message and prepend if present.
    instructions = req.get("instructions")
    messages, preserved_indices = responses_items_to_messages(items)
    if not messages:
        return {"ok": False, "reason": "no_compressible_messages", "input": None, "instructions": None, "tokens_saved": 0}
    # Prepend instructions as system message for compression then strip after.
    has_instr = instructions is not None and instructions != ""
    if has_instr:
        messages = [{"role": "system", "content": instructions}] + messages
    r = compress(messages, model=model, model_limit=model_limit(model, limit_override))
    compressed_messages = r.messages
    compressed_instructions = None
    clear_instructions = False
    if has_instr:
        if compressed_messages and compressed_messages[0].get("role") == "system":
            compressed_instructions = compressed_messages[0].get("content", "")
            compressed_messages = compressed_messages[1:]
        else:
            # System message was fully absorbed by compression — signal removal.
            clear_instructions = True
    new_items = messages_to_responses_items(compressed_messages, items, preserved_indices)
    return {
        "ok": True,
        "input": new_items,
        "instructions": compressed_instructions,
        "clear_instructions": clear_instructions,
        "tokens_saved": r.tokens_saved,
        "compression_ratio": r.compression_ratio,
    }

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    op = req.get("operation", "compress_messages")
    if op == "compress_responses":
        result = handle_compress_responses(req)
    else:
        result = handle_compress_messages(req)
    print(json.dumps(result), flush=True)
`

// HeadroomBridge manages a persistent Python subprocess that compresses
// LLM messages via the headroom-ai library.
type HeadroomBridge struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        *bufio.Scanner
	mu            sync.Mutex
	Catalog       *modelregistry.Catalog
	shuttingDown  atomic.Bool
	stderrDrainWG sync.WaitGroup
}

// headroomRequest is the JSON line sent to the Python bridge for messages compression.
type headroomRequest struct {
	Operation  string          `json:"operation,omitempty"`
	Messages   json.RawMessage `json:"messages"`
	Model      string          `json:"model"`
	ModelLimit int             `json:"model_limit,omitempty"`
}

// headroomResponse is the JSON line received from the Python bridge for messages compression.
type headroomResponse struct {
	Messages         json.RawMessage `json:"messages"`
	TokensSaved      int             `json:"tokens_saved"`
	CompressionRatio float64         `json:"compression_ratio"`
}

// headroomResponsesRequest is the JSON line sent to the Python bridge for Responses API compression.
type headroomResponsesRequest struct {
	Operation    string          `json:"operation"`
	Model        string          `json:"model"`
	ModelLimit   int             `json:"model_limit,omitempty"`
	Input        json.RawMessage `json:"input"`
	Instructions *string         `json:"instructions,omitempty"`
}

// headroomResponsesResponse is the JSON line received from the Python bridge for Responses API compression.
// ok=false indicates a skip condition (missing converter, no input, no compressible text).
// clear_instructions=true means the bridge actively wants instructions removed (e.g. the system
// message was fully absorbed by compression), as distinct from instructions simply being absent
// from the original request (in which case clear_instructions is false and instructions is null).
type headroomResponsesResponse struct {
	OK                bool            `json:"ok"`
	Reason            string          `json:"reason,omitempty"`
	Input             json.RawMessage `json:"input"`
	Instructions      *string         `json:"instructions"`
	ClearInstructions bool            `json:"clear_instructions"`
	TokensSaved       int             `json:"tokens_saved"`
	CompressionRatio  float64         `json:"compression_ratio"`
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

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("create stderr pipe: %w", err)
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
	b.stderrDrainWG.Add(1)
	go func() {
		defer b.stderrDrainWG.Done()
		b.drainStderr(stderrPipe)
	}()

	// Ping to verify headroom-ai is installed.
	if err := b.ping(); err != nil {
		b.Stop()
		return nil, fmt.Errorf("headroom bridge ping failed (is headroom-ai installed? pip install \"headroom-ai[all]\"): %w", err)
	}

	return b, nil
}

// ping sends an empty messages array and expects a response within 30 seconds.
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
		Messages:   messages,
		Model:      model,
		ModelLimit: ModelMaxInputTokensWithCatalog(model, b.Catalog),
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

// CompressResponses compresses a Responses API request body using headroom.
//
// It extracts input items and (optionally) instructions, converts them through
// the bridge's responses_converter pipeline, and splices the compressed results
// back into the original body. Token mode uses this path; cache mode uses
// CompressResponsesCache.
//
// Fail-open: any parse error, bridge error, skip condition (previous_response_id
// present, empty input, no compressible text, missing responses_converter), or
// zero savings returns the original body unchanged (with err=nil for skips).
func (b *HeadroomBridge) CompressResponses(body []byte, _ HeadroomMode) ([]byte, int, error) {
	// Parse only the fields we need to decide whether to compress.
	var partial struct {
		Model              string          `json:"model"`
		Input              json.RawMessage `json:"input"`
		Instructions       *string         `json:"instructions"`
		PreviousResponseID *string         `json:"previous_response_id"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return body, 0, fmt.Errorf("parse responses body: %w", err)
	}

	// Skip: continuation request — previous_response_id makes input optional.
	if partial.PreviousResponseID != nil {
		return body, 0, nil
	}

	// Skip: no input or null input.
	if len(partial.Input) == 0 || string(partial.Input) == "null" || string(partial.Input) == "[]" {
		return body, 0, nil
	}

	compressed, compressedInstr, clearInstr, saved, err := b.compressResponses(
		partial.Model, partial.Input, partial.Instructions,
	)
	if err != nil {
		return body, 0, err
	}
	if saved <= 0 {
		return body, 0, nil
	}

	spliced, err := spliceResponsesFields(body, compressed, compressedInstr, clearInstr)
	if err != nil {
		return body, 0, fmt.Errorf("splice compressed responses: %w", err)
	}
	return spliced, saved, nil
}

// compressResponses sends a Responses API compression request to the bridge.
// Returns (compressedInput, compressedInstructions, clearInstructions, tokensSaved, error).
// compressedInstructions is nil when instructions were absent or not rewritten.
// clearInstructions is true when the bridge absorbed the system message entirely and
// wants the instructions field removed from the request.
func (b *HeadroomBridge) compressResponses(
	model string,
	input json.RawMessage,
	instructions *string,
) (json.RawMessage, *string, bool, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	req := headroomResponsesRequest{
		Operation:    "compress_responses",
		Model:        model,
		ModelLimit:   ModelMaxInputTokensWithCatalog(model, b.Catalog),
		Input:        input,
		Instructions: instructions,
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, nil, false, 0, fmt.Errorf("marshal responses bridge request: %w", err)
	}

	line = append(line, '\n')
	if _, err := b.stdin.Write(line); err != nil {
		return nil, nil, false, 0, fmt.Errorf("write to bridge: %w", err)
	}

	if !b.stdout.Scan() {
		if err := b.stdout.Err(); err != nil {
			return nil, nil, false, 0, fmt.Errorf("read from bridge: %w", err)
		}
		return nil, nil, false, 0, fmt.Errorf("bridge process exited unexpectedly")
	}

	var resp headroomResponsesResponse
	if err := json.Unmarshal(b.stdout.Bytes(), &resp); err != nil {
		return nil, nil, false, 0, fmt.Errorf("parse responses bridge response: %w", err)
	}

	// ok=false is a skip condition (no converter, no input, no compressible text).
	if !resp.OK {
		return input, instructions, false, 0, nil
	}

	return resp.Input, resp.Instructions, resp.ClearInstructions, resp.TokensSaved, nil
}

// spliceResponsesFields rewrites only the "input" and (conditionally) "instructions"
// fields in body, preserving all other top-level keys unchanged.
//
//   - compressedInstr non-nil → replace instructions with the compressed value.
//   - clearInstr true          → delete instructions entirely (bridge absorbed system message).
//   - both nil/false           → leave instructions as-is.
func spliceResponsesFields(body []byte, input json.RawMessage, compressedInstr *string, clearInstr bool) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	raw["input"] = input
	switch {
	case compressedInstr != nil:
		instrJSON, err := json.Marshal(*compressedInstr)
		if err != nil {
			return nil, fmt.Errorf("marshal instructions: %w", err)
		}
		raw["instructions"] = instrJSON
	case clearInstr:
		delete(raw, "instructions")
	}
	return json.Marshal(raw)
}

func (b *HeadroomBridge) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRequestBody)
	var traceback bytes.Buffer
	capturingTraceback := false

	flushTraceback := func() {
		if traceback.Len() == 0 {
			return
		}
		text := traceback.String()
		if shouldSuppressHeadroomTraceback(text, b.shuttingDown.Load()) {
			traceback.Reset()
			capturingTraceback = false
			return
		}
		fmt.Fprintf(os.Stderr, "cq: headroom stderr: %s\n", text)
		traceback.Reset()
		capturingTraceback = false
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "Traceback (most recent call last):" {
			flushTraceback()
			traceback.WriteString(line)
			capturingTraceback = true
			continue
		}
		if capturingTraceback {
			traceback.WriteByte('\n')
			traceback.WriteString(line)
			if strings.Contains(line, "KeyboardInterrupt") {
				flushTraceback()
			}
			continue
		}
		if shouldSuppressHeadroomStderrLine(line) {
			continue
		}
		fmt.Fprintf(os.Stderr, "cq: headroom stderr: %s\n", line)
	}

	flushTraceback()
	if err := scanner.Err(); err != nil && !b.shuttingDown.Load() {
		fmt.Fprintf(os.Stderr, "cq: headroom stderr: %v\n", err)
	}
}

func shouldSuppressHeadroomStderrLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return true
	}
	if line == "## Exited Plan Mode" {
		return true
	}
	if strings.HasPrefix(line, "You have exited plan mode.") {
		return true
	}
	if strings.Contains(line, "Warning: You are sending unauthenticated requests to the HF Hub.") {
		return true
	}
	if strings.Contains(line, "Tag placeholder lost during compression, appending:") {
		return true
	}
	if strings.Contains(line, " - INFO - ") {
		return true
	}
	return false
}

func shouldSuppressHeadroomTraceback(text string, shuttingDown bool) bool {
	if strings.Contains(text, "for line in sys.stdin:") && strings.Contains(text, "KeyboardInterrupt") {
		return true
	}
	return shuttingDown && strings.Contains(text, "KeyboardInterrupt")
}

// frozenPrefixCountMessages returns the number of messages at the start of the
// slice that are considered "frozen" (prior turns). Cache mode only mutates the
// final message, and only when that final message has role "user". Everything
// before that final user turn is treated as byte-stable frozen context.
// Returns -1 if the last message is not a user turn (no mutable suffix).
func frozenPrefixCountMessages(msgs []json.RawMessage) int {
	if len(msgs) == 0 {
		return -1
	}
	// Check whether the last message has role "user".
	var last struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(msgs[len(msgs)-1], &last); err != nil || last.Role != "user" {
		return -1 // no mutable suffix
	}
	// Everything before the last message is frozen.
	return len(msgs) - 1
}

// frozenPrefixCountItems returns the number of items at the start of a
// Responses API input array that are frozen. Same logic as messages.
// Returns -1 if the last item is not a user-role item (no mutable suffix).
func frozenPrefixCountItems(items []json.RawMessage) int {
	if len(items) == 0 {
		return -1
	}
	var last struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(items[len(items)-1], &last); err != nil || last.Role != "user" {
		return -1
	}
	return len(items) - 1
}

// CompressCache compresses an Anthropic messages request in cache mode.
//
// Cache-mode semantics:
//   - Compute the frozen prefix (all messages before the final user message).
//   - If the final message is not a user turn, return the original body unchanged.
//   - Send the FULL messages array to the bridge so the compressor has full context.
//   - After compression, restore the frozen prefix to its original bytes exactly,
//     so cache keys for prior turns remain stable.
//
// Savings reporting uses the bridge's reported value; frozen-prefix restoration
// does not affect the token-savings count because those turns were not mutated.
func (b *HeadroomBridge) CompressCache(body []byte) ([]byte, int, error) {
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

	var msgs []json.RawMessage
	if err := json.Unmarshal(partial.Messages, &msgs); err != nil {
		return body, 0, fmt.Errorf("parse messages array: %w", err)
	}

	frozenCount := frozenPrefixCountMessages(msgs)
	if frozenCount < 0 {
		// Final message is not a user turn — no mutable suffix. Return unchanged.
		return body, 0, nil
	}

	// Send the FULL messages array to the bridge for compression.
	compressedAll, saved, err := b.compress(partial.Messages, partial.Model)
	if err != nil {
		return body, 0, err
	}
	if saved <= 0 {
		return body, 0, nil
	}

	// Parse compressed messages.
	var compressedMsgs []json.RawMessage
	if err := json.Unmarshal(compressedAll, &compressedMsgs); err != nil {
		return body, 0, fmt.Errorf("parse compressed messages: %w", err)
	}

	// Restore frozen prefix from original bytes. The bridge may have rewritten or
	// dropped prefix messages; we overwrite them with the originals to keep
	// cache keys stable. Only the mutable suffix (final user turn) is kept
	// from the bridge's output.
	mutableCount := len(msgs) - frozenCount
	if len(compressedMsgs) < mutableCount {
		return body, 0, nil
	}
	mutableSuffix := compressedMsgs[len(compressedMsgs)-mutableCount:]
	for _, msg := range mutableSuffix {
		var partial struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(msg, &partial); err != nil || partial.Role != "user" {
			return body, 0, nil
		}
	}

	result := make([]json.RawMessage, 0, frozenCount+mutableCount)
	result = append(result, msgs[:frozenCount]...)
	result = append(result, mutableSuffix...)

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return body, 0, fmt.Errorf("marshal result messages: %w", err)
	}

	spliced, err := spliceMessages(body, resultJSON)
	if err != nil {
		return body, 0, fmt.Errorf("splice cache-compressed messages: %w", err)
	}

	return spliced, saved, nil
}

// CompressResponsesCache compresses a Responses API request in cache mode.
//
// Cache-mode semantics:
//   - Compute the frozen prefix (all items before the final user item).
//   - If the final item is not a user-role item, return the original body unchanged.
//   - Instructions are part of the frozen context once converted to messages and
//     must NOT be passed to the bridge; they are preserved from the original.
//   - Send the FULL input array to the bridge (without instructions) so the
//     compressor has full context.
//   - After compression, restore the frozen prefix to its original bytes exactly,
//     so cache keys for prior turns remain stable.
func (b *HeadroomBridge) CompressResponsesCache(body []byte) ([]byte, int, error) {
	var partial struct {
		Model              string          `json:"model"`
		Input              json.RawMessage `json:"input"`
		Instructions       *string         `json:"instructions"`
		PreviousResponseID *string         `json:"previous_response_id"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return body, 0, fmt.Errorf("parse responses body: %w", err)
	}

	// Skip: continuation request.
	if partial.PreviousResponseID != nil {
		return body, 0, nil
	}

	// Skip: no input.
	if len(partial.Input) == 0 || string(partial.Input) == "null" || string(partial.Input) == "[]" {
		return body, 0, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(partial.Input, &items); err != nil {
		return body, 0, fmt.Errorf("parse input array: %w", err)
	}

	frozenCount := frozenPrefixCountItems(items)
	if frozenCount < 0 {
		// Final item is not a user turn — no mutable suffix. Return unchanged.
		return body, 0, nil
	}

	// Send the FULL input array to the bridge WITHOUT instructions.
	// Instructions are frozen context — they must not change in cache mode, and
	// passing them would allow the bridge to compress or drop them.
	compressed, _, _, saved, err := b.compressResponses(
		partial.Model, partial.Input, nil, // nil instructions: frozen
	)
	if err != nil {
		return body, 0, err
	}
	if saved <= 0 {
		return body, 0, nil
	}

	// Parse compressed items.
	var compressedItems []json.RawMessage
	if err := json.Unmarshal(compressed, &compressedItems); err != nil {
		return body, 0, fmt.Errorf("parse compressed items: %w", err)
	}

	// Restore frozen prefix from original bytes. The bridge may have rewritten or
	// dropped prefix items; we overwrite them with the originals to keep cache
	// keys stable. Only the mutable suffix (final user item) is kept from output.
	mutableCount := len(items) - frozenCount
	if len(compressedItems) < mutableCount {
		return body, 0, nil
	}
	mutableSuffix := compressedItems[len(compressedItems)-mutableCount:]
	for _, item := range mutableSuffix {
		var partial struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(item, &partial); err != nil || partial.Role != "user" {
			return body, 0, nil
		}
	}

	result := make([]json.RawMessage, 0, frozenCount+mutableCount)
	result = append(result, items[:frozenCount]...)
	result = append(result, mutableSuffix...)

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return body, 0, fmt.Errorf("marshal result items: %w", err)
	}

	// Never modify instructions in cache mode — pass nil/false to preserve original.
	spliced, err := spliceResponsesFields(body, resultJSON, nil, false)
	if err != nil {
		return body, 0, fmt.Errorf("splice cache-compressed responses: %w", err)
	}
	return spliced, saved, nil
}

// Stop shuts down the Python subprocess gracefully, falling back to SIGKILL.
//
// stdin is closed before acquiring the mutex so that any goroutine blocked
// inside compress() on b.stdout.Scan() receives EOF, exits the scan, and
// releases the mutex. This prevents a deadlock when ping() times out and
// the spawned goroutine is still holding b.mu while blocked on Scan().
func (b *HeadroomBridge) Stop() {
	b.shuttingDown.Store(true)

	// Close stdin outside the lock so that any goroutine holding b.mu and
	// blocked on b.stdout.Scan() gets an EOF (the Python process exits when
	// its stdin closes) and can release the lock.
	b.stdin.Close()

	// Acquire the lock only to synchronise with any in-flight compress call
	// that hasn't exited yet after the stdin close.
	b.mu.Lock()
	b.mu.Unlock() //nolint:gocritic // intentional: we just want to wait for the lock to be available

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
	b.stderrDrainWG.Wait()
}
