package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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
    if op == "probe":
        result = {
            "operation": "probe",
            "request_id": req.get("request_id"),
            "protocol": 1,
            "ok": True,
            "cache_mode": _HAS_RESPONSES_CONVERTER,
        }
    elif op == "compress_responses":
        result = handle_compress_responses(req)
    else:
        result = handle_compress_messages(req)
    print(json.dumps(result), flush=True)
`

// HeadroomBridge manages a persistent Python subprocess that compresses
// LLM messages via the headroom-ai library.
type headroomProcess interface {
	Wait() error
	Kill() error
}

type execHeadroomProcess struct {
	cmd *exec.Cmd
}

func (p *execHeadroomProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execHeadroomProcess) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

type HeadroomBridge struct {
	process       headroomProcess
	stdin         io.WriteCloser
	stdoutPipe    io.ReadCloser
	stdout        *bufio.Scanner
	stderrPipe    io.ReadCloser
	initOnce      sync.Once
	operationGate chan struct{}
	stopping      chan struct{}
	processDone   chan struct{}
	stderrDone    chan struct{}
	shutdownOnce  sync.Once
	stopOnce      sync.Once
	stopDone      chan struct{}
	Catalog       *modelregistry.Catalog
	shuttingDown  atomic.Bool
	probeSequence atomic.Uint64
}

var (
	errHeadroomProbeUnavailable = errors.New("headroom bridge unavailable")
	errHeadroomProbeInvalid     = errors.New("headroom bridge probe invalid response")
	errHeadroomProbeUnbounded   = errors.New("headroom bridge probe requires a context deadline")
)

type headroomProbeRequest struct {
	Operation string `json:"operation"`
	RequestID string `json:"request_id"`
}

type headroomProbeResponse struct {
	Operation string `json:"operation"`
	RequestID string `json:"request_id"`
	Protocol  int    `json:"protocol"`
	OK        bool   `json:"ok"`
	CacheMode bool   `json:"cache_mode"`
}

func newHeadroomBridge(
	process headroomProcess,
	stdin io.WriteCloser,
	stdoutPipe io.ReadCloser,
	stderrPipe io.ReadCloser,
) *HeadroomBridge {
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRequestBody)

	bridge := &HeadroomBridge{
		process:    process,
		stdin:      stdin,
		stdoutPipe: stdoutPipe,
		stdout:     scanner,
		stderrPipe: stderrPipe,
	}
	bridge.ensureLifecycle()
	return bridge
}

func (b *HeadroomBridge) ensureLifecycle() {
	if b == nil {
		return
	}
	b.initOnce.Do(func() {
		b.operationGate = make(chan struct{}, 1)
		b.operationGate <- struct{}{}
		b.stopping = make(chan struct{})
		b.stopDone = make(chan struct{})
		b.processDone = make(chan struct{})
		b.stderrDone = make(chan struct{})

		if b.stderrPipe == nil {
			close(b.stderrDone)
		} else {
			go func() {
				defer close(b.stderrDone)
				defer func() { _ = recover() }()
				b.drainStderr(b.stderrPipe)
			}()
		}
		if b.process == nil {
			close(b.processDone)
		}
	})
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

	b := newHeadroomBridge(&execHeadroomProcess{cmd: cmd}, stdin, stdoutPipe, stderrPipe)

	// Ping to verify headroom-ai is installed.
	if err := b.ping(); err != nil {
		b.Stop()
		return nil, fmt.Errorf("headroom bridge ping failed (is headroom-ai installed? pip install \"headroom-ai[all]\"): %w", err)
	}

	return b, nil
}

// ping verifies core message compression within the startup deadline. Cache
// capability is checked separately by Probe when cache-mode serving is needed.
func (b *HeadroomBridge) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	line, err := json.Marshal(headroomRequest{
		Messages:   json.RawMessage(`[]`),
		ModelLimit: ModelMaxInputTokensWithCatalog("", b.Catalog),
	})
	if err != nil {
		return fmt.Errorf("marshal bridge ping: %w", err)
	}
	response, err := b.exchange(ctx, line, nil)
	if err != nil {
		return err
	}
	var parsed headroomResponse
	if err := json.Unmarshal(response, &parsed); err != nil {
		return fmt.Errorf("parse bridge ping: %w", err)
	}
	return nil
}

// Probe verifies that the live bridge supports the cache-mode protocol.
func (b *HeadroomBridge) Probe(ctx context.Context) error {
	if b == nil {
		return errHeadroomProbeUnavailable
	}
	if ctx == nil {
		return errHeadroomProbeUnbounded
	}
	if _, ok := ctx.Deadline(); !ok {
		return errHeadroomProbeUnbounded
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	requestID := strconv.FormatUint(b.probeSequence.Add(1), 10)
	line, err := json.Marshal(headroomProbeRequest{Operation: "probe", RequestID: requestID})
	if err != nil {
		return errHeadroomProbeUnavailable
	}
	_, err = b.exchange(ctx, line, func(responseBytes []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(responseBytes))
		decoder.DisallowUnknownFields()
		var response headroomProbeResponse
		if err := decoder.Decode(&response); err != nil {
			return errHeadroomProbeInvalid
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return errHeadroomProbeInvalid
		}
		if response.Operation != "probe" || response.RequestID != requestID || response.Protocol != 1 || !response.OK || !response.CacheMode {
			return errHeadroomProbeInvalid
		}
		return nil
	})
	if err == nil || errors.Is(err, errHeadroomProbeInvalid) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errHeadroomProbeUnavailable
}

func (b *HeadroomBridge) acquireOperation(ctx context.Context) error {
	if b == nil || b.stdin == nil || b.stdout == nil {
		return errHeadroomProbeUnavailable
	}
	b.ensureLifecycle()
	select {
	case <-b.stopping:
		return errHeadroomProbeUnavailable
	default:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.stopping:
		return errHeadroomProbeUnavailable
	case <-b.operationGate:
	}

	select {
	case <-ctx.Done():
		b.operationGate <- struct{}{}
		return ctx.Err()
	case <-b.stopping:
		b.operationGate <- struct{}{}
		return errHeadroomProbeUnavailable
	default:
		return nil
	}
}

func (b *HeadroomBridge) exchange(ctx context.Context, request []byte, validate func([]byte) error) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := b.acquireOperation(ctx); err != nil {
		return nil, err
	}
	defer func() { b.operationGate <- struct{}{} }()

	cancelComplete := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		b.shutdown()
		close(cancelComplete)
	})
	finishCancellation := func() error {
		if stopCancellation() {
			if err := ctx.Err(); err != nil {
				b.shutdown()
				return err
			}
			return nil
		}
		<-cancelComplete
		return ctx.Err()
	}

	line := append(bytes.Clone(request), '\n')
	var operationErr error
	if written, err := b.stdin.Write(line); err != nil {
		operationErr = fmt.Errorf("write to bridge: %w", err)
	} else if written != len(line) {
		operationErr = io.ErrShortWrite
	}

	var response []byte
	if operationErr == nil {
		if !b.stdout.Scan() {
			if err := b.stdout.Err(); err != nil {
				operationErr = fmt.Errorf("read from bridge: %w", err)
			} else {
				operationErr = errors.New("bridge process exited unexpectedly")
			}
		} else {
			response = bytes.Clone(b.stdout.Bytes())
		}
	}

	var validationErr error
	if operationErr == nil && !b.shuttingDown.Load() && validate != nil {
		validationErr = validate(response)
		if validationErr != nil {
			b.shutdown()
		}
	}
	if err := finishCancellation(); err != nil {
		return nil, err
	}
	if operationErr != nil {
		b.shutdown()
		return nil, operationErr
	}
	if validationErr != nil {
		return nil, validationErr
	}
	if b.shuttingDown.Load() {
		return nil, errHeadroomProbeUnavailable
	}
	return response, nil
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
	req := headroomRequest{
		Messages:   messages,
		Model:      model,
		ModelLimit: ModelMaxInputTokensWithCatalog(model, b.Catalog),
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal bridge request: %w", err)
	}

	response, err := b.exchange(context.Background(), line, nil)
	if err != nil {
		return nil, 0, err
	}

	var resp headroomResponse
	if err := json.Unmarshal(response, &resp); err != nil {
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

	response, err := b.exchange(context.Background(), line, nil)
	if err != nil {
		return nil, nil, false, 0, err
	}

	var resp headroomResponsesResponse
	if err := json.Unmarshal(response, &resp); err != nil {
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

func (b *HeadroomBridge) shutdown() {
	if b == nil {
		return
	}
	b.shutdownOnce.Do(func() {
		b.shuttingDown.Store(true)
		if b.stopping != nil {
			close(b.stopping)
		}
		if b.stdin != nil {
			_ = b.stdin.Close()
		}
		if b.stdoutPipe != nil {
			_ = b.stdoutPipe.Close()
		}
		if b.stderrPipe != nil {
			_ = b.stderrPipe.Close()
		}
		if b.process != nil {
			_ = b.process.Kill()
			go func() {
				defer close(b.processDone)
				defer func() { _ = recover() }()
				<-b.operationGate
				b.operationGate <- struct{}{}
				<-b.stderrDone
				_ = b.process.Wait()
			}()
		}
	})
}

// Stop shuts down the Python subprocess and synchronises with any in-flight
// bridge operation without waiting on its serialisation gate before closing
// the pipes that unblock writes and scans.
func (b *HeadroomBridge) Stop() {
	if b == nil {
		return
	}
	b.ensureLifecycle()
	b.shutdown()

	b.stopOnce.Do(func() {
		defer close(b.stopDone)

		<-b.operationGate
		b.operationGate <- struct{}{}
		<-b.processDone
		<-b.stderrDone
	})
	<-b.stopDone
}
