package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const maxCodexRequestEnvelopeBytes = 10 << 20

var (
	// ErrCodexRequestEnvelopeReleased indicates that replay-owned state is no longer available.
	ErrCodexRequestEnvelopeReleased = errors.New("Codex request envelope released")
	// ErrCodexRequestEnvelopeEncodedTooLarge indicates that encoded bytes exceed the independent limit.
	ErrCodexRequestEnvelopeEncodedTooLarge = errors.New("encoded Codex request exceeds 10 MiB")
	// ErrCodexRequestEnvelopeDecodedTooLarge indicates that decoded bytes exceed the independent limit.
	ErrCodexRequestEnvelopeDecodedTooLarge = errors.New("decoded Codex request exceeds 10 MiB")
)

// CodexRequestEnvelope owns the immutable request state shared by retry replays.
// Its methods are safe for concurrent use.
type CodexRequestEnvelope struct {
	mu             sync.Mutex
	encoded        []byte
	decoded        []byte
	ownedBytes     uint64
	headers        http.Header
	effectiveModel string
	released       bool
	replays        map[*codexRequestReplayState]struct{}
}

// CodexRequestReplay owns one independently releasable snapshot of an envelope.
// Its methods are safe for concurrent use.
type CodexRequestReplay struct {
	owner *CodexRequestEnvelope
	state *codexRequestReplayState
}

type codexRequestReplayState struct {
	mu             sync.RWMutex
	encoded        []byte
	decoded        []byte
	ownedBytes     uint64
	headers        http.Header
	effectiveModel string
	activeBodies   int
	releasePending bool
	released       bool
}

type codexRequestReplayBody struct {
	mu     sync.Mutex
	state  *codexRequestReplayState
	offset int
	closed bool
}

// NewCodexRequestEnvelope freezes independent copies of the encoded and decoded
// request bodies, the effective model, and an exact allowlist of semantic headers.
func NewCodexRequestEnvelope(encoded, decoded []byte, headers http.Header, effectiveModel string) (*CodexRequestEnvelope, error) {
	if len(encoded) > maxCodexRequestEnvelopeBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrCodexRequestEnvelopeEncodedTooLarge, len(encoded))
	}
	if len(decoded) > maxCodexRequestEnvelopeBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrCodexRequestEnvelopeDecodedTooLarge, len(decoded))
	}
	envelope := &CodexRequestEnvelope{
		encoded:        bytes.Clone(encoded),
		decoded:        bytes.Clone(decoded),
		headers:        codexReplayHeaders(headers),
		effectiveModel: strings.Clone(effectiveModel),
	}
	envelope.ownedBytes = codexProcessRuntimeObservability.ownReplayBytes(envelope.encoded, envelope.decoded)
	return envelope, nil
}

// Replay returns a registered snapshot that remains valid until either it or
// its envelope is released.
func (envelope *CodexRequestEnvelope) Replay() (*CodexRequestReplay, error) {
	if envelope == nil {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	envelope.mu.Lock()
	defer envelope.mu.Unlock()
	if envelope.released {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	state := &codexRequestReplayState{
		encoded:        bytes.Clone(envelope.encoded),
		decoded:        bytes.Clone(envelope.decoded),
		headers:        codexReplayHeaders(envelope.headers),
		effectiveModel: strings.Clone(envelope.effectiveModel),
	}
	state.ownedBytes = codexProcessRuntimeObservability.ownReplayBytes(state.encoded, state.decoded)
	if envelope.replays == nil {
		envelope.replays = make(map[*codexRequestReplayState]struct{})
	}
	envelope.replays[state] = struct{}{}
	return &CodexRequestReplay{owner: envelope, state: state}, nil
}

// Release rejects new replay access and best-effort overwrites owned body bytes.
// Replay state with active bodies is cleared after the final body closes. It is
// safe to call repeatedly or on nil.
func (envelope *CodexRequestEnvelope) Release() {
	if envelope == nil {
		return
	}
	envelope.mu.Lock()
	defer envelope.mu.Unlock()
	if envelope.released {
		return
	}
	clearBytes(envelope.encoded)
	clearBytes(envelope.decoded)
	clear(envelope.headers)
	for replay := range envelope.replays {
		replay.release()
	}
	envelope.encoded = nil
	envelope.decoded = nil
	codexProcessRuntimeObservability.releaseReplayBytes(envelope.ownedBytes)
	envelope.ownedBytes = 0
	envelope.headers = nil
	envelope.effectiveModel = ""
	envelope.replays = nil
	envelope.released = true
}

// Body returns a new registered reader positioned at the start of the exact
// encoded request body. Release rejects new access while registered readers
// remain exact until closed.
func (replay *CodexRequestReplay) Body() (io.ReadCloser, error) {
	if replay == nil || replay.state == nil {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	return replay.state.newBody()
}

// GetBody returns another independent guarded reader over the encoded body.
func (replay *CodexRequestReplay) GetBody() (io.ReadCloser, error) {
	return replay.Body()
}

// DecodedBody returns a caller-owned copy of the frozen decoded view. Release
// cannot overwrite copies already handed to callers.
func (replay *CodexRequestReplay) DecodedBody() ([]byte, error) {
	if replay == nil || replay.state == nil {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	replay.state.mu.RLock()
	defer replay.state.mu.RUnlock()
	if replay.state.releasePending || replay.state.released {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	return bytes.Clone(replay.state.decoded), nil
}

// Header returns a caller-owned copy containing only allowlisted semantic
// headers. Release cannot clear copies already handed to callers.
func (replay *CodexRequestReplay) Header() (http.Header, error) {
	if replay == nil || replay.state == nil {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	replay.state.mu.RLock()
	defer replay.state.mu.RUnlock()
	if replay.state.releasePending || replay.state.released {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	return codexReplayHeaders(replay.state.headers), nil
}

// EffectiveModel returns the model fixed before the envelope was frozen.
func (replay *CodexRequestReplay) EffectiveModel() (string, error) {
	if replay == nil || replay.state == nil {
		return "", ErrCodexRequestEnvelopeReleased
	}
	replay.state.mu.RLock()
	defer replay.state.mu.RUnlock()
	if replay.state.releasePending || replay.state.released {
		return "", ErrCodexRequestEnvelopeReleased
	}
	return strings.Clone(replay.state.effectiveModel), nil
}

// ContentLength returns the exact frozen encoded body length for framing.
func (replay *CodexRequestReplay) ContentLength() (int64, error) {
	if replay == nil || replay.state == nil {
		return 0, ErrCodexRequestEnvelopeReleased
	}
	replay.state.mu.RLock()
	defer replay.state.mu.RUnlock()
	if replay.state.releasePending || replay.state.released {
		return 0, ErrCodexRequestEnvelopeReleased
	}
	return int64(len(replay.state.encoded)), nil
}

// Release rejects new access to this replay and best-effort overwrites its owned
// state after the final registered body closes, without affecting other replays.
// It is safe to call repeatedly or on nil.
func (replay *CodexRequestReplay) Release() {
	if replay == nil || replay.state == nil {
		return
	}
	if replay.owner != nil {
		replay.owner.releaseReplay(replay.state)
		return
	}
	replay.state.release()
}

func (envelope *CodexRequestEnvelope) releaseReplay(replay *codexRequestReplayState) {
	if envelope == nil || replay == nil {
		return
	}
	replay.release()
	envelope.mu.Lock()
	delete(envelope.replays, replay)
	envelope.mu.Unlock()
}

func (replay *codexRequestReplayState) newBody() (io.ReadCloser, error) {
	if replay == nil {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	replay.mu.Lock()
	defer replay.mu.Unlock()
	if replay.releasePending || replay.released {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	replay.activeBodies++
	return &codexRequestReplayBody{state: replay}, nil
}

func (replay *codexRequestReplayState) release() {
	if replay == nil {
		return
	}
	replay.mu.Lock()
	defer replay.mu.Unlock()
	if replay.releasePending {
		return
	}
	replay.releasePending = true
	if replay.activeBodies != 0 {
		return
	}
	replay.clearLocked()
}

func (replay *codexRequestReplayState) closeBody() {
	if replay == nil {
		return
	}
	replay.mu.Lock()
	defer replay.mu.Unlock()
	if replay.activeBodies > 0 {
		replay.activeBodies--
	}
	if replay.releasePending && replay.activeBodies == 0 && !replay.released {
		replay.clearLocked()
	}
}

func (replay *codexRequestReplayState) clearLocked() {
	clearBytes(replay.encoded)
	clearBytes(replay.decoded)
	clear(replay.headers)
	replay.encoded = nil
	replay.decoded = nil
	codexProcessRuntimeObservability.releaseReplayBytes(replay.ownedBytes)
	replay.ownedBytes = 0
	replay.headers = nil
	replay.effectiveModel = ""
	replay.released = true
}

func (body *codexRequestReplayBody) Read(destination []byte) (int, error) {
	if body == nil || body.state == nil {
		return 0, ErrCodexRequestEnvelopeReleased
	}
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	body.state.mu.RLock()
	defer body.state.mu.RUnlock()
	if body.state.released {
		return 0, ErrCodexRequestEnvelopeReleased
	}
	if body.offset >= len(body.state.encoded) {
		return 0, io.EOF
	}
	written := copy(destination, body.state.encoded[body.offset:])
	body.offset += written
	return written, nil
}

func (body *codexRequestReplayBody) Close() error {
	if body == nil {
		return nil
	}
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.closed {
		return nil
	}
	body.closed = true
	body.state.closeBody()
	return nil
}

func codexReplayHeaders(in http.Header) http.Header {
	if in == nil {
		return nil
	}
	excluded := make(map[string]struct{})
	for key, values := range in {
		canonical, ok := canonicalCodexReplayHeader(key)
		if !ok || canonical != "Connection" {
			continue
		}
		for _, value := range values {
			for _, name := range strings.Split(value, ",") {
				if canonical, ok := canonicalCodexReplayHeader(strings.Trim(name, " \t")); ok {
					excluded[canonical] = struct{}{}
				}
			}
		}
	}
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(http.Header)
	for _, key := range keys {
		canonical, ok := canonicalCodexReplayHeader(key)
		if !ok || !safeCodexReplayHeader(canonical) {
			continue
		}
		if _, found := excluded[canonical]; found {
			continue
		}
		for _, value := range in[key] {
			out[canonical] = append(out[canonical], strings.Clone(value))
		}
	}
	return out
}

func safeCodexReplayHeader(name string) bool {
	switch name {
	case "Content-Type", "Content-Encoding", "Accept", "Accept-Encoding", "User-Agent",
		"Openai-Beta", "Openai-Alpha",
		"X-Codex-Turn-Metadata", "X-Codex-Turn-State",
		"X-Codex-Installation-Id", "X-Codex-Parent-Thread-Id", "X-Codex-Window-Id",
		"X-Openai-Subagent", "X-Openai-Memgen-Request", "X-Openai-Internal-Codex-Responses-Lite",
		"X-Responsesapi-Include-Timing-Metrics":
		return true
	default:
		return false
	}
}

func canonicalCodexReplayHeader(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		if !codexHTTPTokenByte(name[i]) {
			return "", false
		}
	}
	return http.CanonicalHeaderKey(name), true
}

func codexHTTPTokenByte(value byte) bool {
	if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
