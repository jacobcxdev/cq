package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

type grokPinnedAccountContextKey struct{}

type grokBoundState struct {
	Account grok.AccountID
	Model   string
}
type grokStateBindings struct {
	mu             sync.Mutex
	responses      map[string]grokBoundState
	opaque         map[string]grokBoundState
	reasoning      map[string]json.RawMessage
	reasoningBytes int
}

// Bind native opaque state to the exact subscription that produced it. A
// restart forgets these ephemeral bindings and requires a fresh plaintext chain.
func (b *grokStateBindings) record(state grokBoundState, id string, output []json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.responses == nil {
		b.responses = make(map[string]grokBoundState)
		b.opaque = make(map[string]grokBoundState)
		b.reasoning = make(map[string]json.RawMessage)
	}
	if len(b.responses) >= 4096 {
		for k := range b.responses {
			delete(b.responses, k)
			break
		}
	}
	if id != "" {
		if previous, exists := b.responses[id]; exists && previous != state {
			b.responses[id] = grokBoundState{}
		} else {
			b.responses[id] = state
		}
	}
	for _, raw := range output {
		var item struct {
			Encrypted string `json:"encrypted_content"`
			Type      string `json:"type"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Encrypted != "" {
			if len(b.opaque) >= 4096 {
				for k := range b.opaque {
					delete(b.opaque, k)
					b.reasoningBytes -= len(b.reasoning[k])
					delete(b.reasoning, k)
					break
				}
			}
			sum := sha256.Sum256([]byte(item.Encrypted))
			key := hex.EncodeToString(sum[:])
			if previous, exists := b.opaque[key]; exists && previous != state {
				b.opaque[key] = grokBoundState{}
				b.reasoningBytes -= len(b.reasoning[key])
				delete(b.reasoning, key)
			} else {
				b.opaque[key] = state
				if item.Type == "reasoning" {
					b.reasoningBytes -= len(b.reasoning[key])
					delete(b.reasoning, key)
					for b.reasoningBytes+len(raw) > maxRequestBody {
						for old, value := range b.reasoning {
							b.reasoningBytes -= len(value)
							delete(b.reasoning, old)
							delete(b.opaque, old)
							break
						}
						if len(raw) > maxRequestBody {
							delete(b.opaque, key)
							break
						}
					}
					if len(raw) <= maxRequestBody {
						b.reasoning[key] = append(json.RawMessage(nil), raw...)
						b.reasoningBytes += len(raw)
					}
				}
			}
		}
	}
}

// Codex reserializes reasoning and adds private bookkeeping. Replay the
// original native container only after its opaque state has been bound to the
// selected subscription/model; never strip unknown private state by guessing.
func (b *grokStateBindings) restoreReasoning(body []byte, state grokBoundState) ([]byte, error) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return nil, errors.New("invalid Grok request")
	}
	var input []json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil {
		return body, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	restoredBytes := len(body)
	for i, raw := range input {
		var item struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "reasoning" || item.Encrypted == "" {
			continue
		}
		sum := sha256.Sum256([]byte(item.Encrypted))
		key := hex.EncodeToString(sum[:])
		original, ok := b.reasoning[key]
		if !ok || b.opaque[key] != state {
			return nil, errors.New("Grok native reasoning unavailable; start a fresh conversation")
		}
		restoredBytes += len(original) - len(raw)
		if restoredBytes > maxRequestBody {
			return nil, grokRouteError{http.StatusRequestEntityTooLarge, "Grok restored reasoning exceeds request budget"}
		}
		var supplied, native map[string]any
		if json.Unmarshal(raw, &supplied) != nil || json.Unmarshal(original, &native) != nil {
			return nil, errors.New("invalid Grok reasoning")
		}
		for name, value := range supplied {
			if name == "status" || name == "internal_chat_message_metadata_passthrough" || value == nil {
				continue
			}
			if !reflect.DeepEqual(value, native[name]) {
				return nil, errors.New("Grok native reasoning was modified")
			}
		}
		input[i] = original
		changed = true
	}
	if !changed {
		return body, nil
	}
	request["input"], _ = json.Marshal(input)
	return json.Marshal(request)
}
func (b *grokStateBindings) requestState(body []byte, model string) (grokBoundState, error) {
	var request struct {
		Previous string            `json:"previous_response_id"`
		Input    []json.RawMessage `json:"input"`
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return grokBoundState{}, errors.New("invalid Grok request")
	}
	_ = json.Unmarshal(envelope["previous_response_id"], &request.Previous)
	_ = json.Unmarshal(envelope["input"], &request.Input)
	b.mu.Lock()
	defer b.mu.Unlock()
	var bound grokBoundState
	merge := func(s grokBoundState, ok bool) error {
		if !ok || s.Model != model || bound.Account != "" && bound != s {
			return errors.New("Grok continuation unavailable; start a fresh conversation with full readable input")
		}
		bound = s
		return nil
	}
	if request.Previous != "" {
		s, ok := b.responses[request.Previous]
		if err := merge(s, ok); err != nil {
			return bound, err
		}
	}
	for _, raw := range request.Input {
		var item struct {
			Encrypted string `json:"encrypted_content"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Encrypted != "" {
			sum := sha256.Sum256([]byte(item.Encrypted))
			s, ok := b.opaque[hex.EncodeToString(sum[:])]
			if err := merge(s, ok); err != nil {
				return bound, err
			}
		}
	}
	return bound, nil
}

func (s *Server) hasGrokCatalog() bool {
	if s.Catalog == nil {
		return false
	}
	for _, e := range s.Catalog.Snapshot().Entries {
		if e.Provider == modelregistry.ProviderGrok {
			return true
		}
	}
	return false
}

func (s *Server) tryHandleGrok(w http.ResponseWriter, r *http.Request) (bool, string) {
	if r.Body == nil {
		return false, ""
	}
	body, err := readCodexNativeHTTPRequest(r)
	if err != nil {
		writeError(w, 400, "invalid_request_error", "failed to read request body")
		return true, ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return s.tryHandleGrokEncoded(w, r, body)
}

func (s *Server) tryHandleGrokEncoded(w http.ResponseWriter, r *http.Request, body []byte) (bool, string) {
	encoding, err := parseCodexContentEncoding(r.Header)
	if err != nil {
		return false, ""
	}
	decoded := body
	if encoding == "zstd" || encoding == "" && codexZstdFrame(body) {
		envelope, err := DecodeCodexRequest(body, encoding, codexHTTPZstdLimits())
		if err != nil {
			return false, ""
		}
		decoded = envelope.Decoded()
	}
	model := extractModel(decoded)
	if RouteModelWithCatalog(model, s.Catalog) != ProviderGrok {
		return false, ""
	}
	s.observeHarness(r.Header, model, decoded)
	if len(body) > maxRequestBody || len(decoded) > maxRequestBody {
		writeError(w, 413, "invalid_request_error", "Grok request body exceeds 10 MiB")
		return true, model
	}
	result, err := s.grokResponse(r.Context(), r.Header, decoded, strings.HasSuffix(r.URL.Path, "/responses/compact"))
	if err != nil {
		writeGrokError(w, err)
		return true, model
	}
	defer result.Response.Body.Close()
	if result.Compact != nil {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(decoded, &req)
		if req.Stream && !strings.HasSuffix(r.URL.Path, "/responses/compact") {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range zaiCompactionEvents(*result.Compact) {
				data, _ := json.Marshal(event)
				fmt.Fprintf(zaiFlushWriter{w}, "event: %s\ndata: %s\n\n", event["type"], data)
			}
		} else {
			_ = json.NewEncoder(w).Encode(result.Compact)
		}
		return true, model
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "X-Request-Id"} {
		if value := result.Response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if result.Response.StatusCode < 200 || result.Response.StatusCode >= 300 {
		writeError(w, result.Response.StatusCode, "api_error", "Grok subscription request failed")
		return true, model
	}
	mediaType, _, _ := mime.ParseMediaType(result.Response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		w.WriteHeader(result.Response.StatusCode)
		id, output, err := relayResponsesSSE(result.Response.Body, "Grok", result.Codec.normaliseResponse, func(kind string, data []byte) error {
			_, err := fmt.Fprintf(zaiFlushWriter{w}, "event: %s\ndata: %s\n\n", kind, data)
			return err
		})
		if err == nil {
			s.grokBindings.record(result.State, id, output)
		} else {
			// The response may already be admitted. Emit a safe terminal error;
			// never retry or retain an uncertain continuation.
			_, _ = io.WriteString(zaiFlushWriter{w}, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Grok subscription stream failed\"}}\n\n")
		}
	} else {
		data, err := httputil.ReadBody(result.Response.Body)
		if err != nil {
			writeError(w, 502, "api_error", "invalid Grok response")
			return true, model
		}
		data, err = result.Codec.normaliseResponse(data)
		if err != nil {
			writeError(w, 502, "api_error", "invalid Grok response")
			return true, model
		}
		var completed struct {
			ID     string            `json:"id"`
			Output []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(data, &completed) != nil || completed.ID == "" {
			writeError(w, 502, "api_error", "invalid Grok response")
			return true, model
		}
		s.grokBindings.record(result.State, completed.ID, completed.Output)
		w.WriteHeader(result.Response.StatusCode)
		_, _ = w.Write(data)
	}
	return true, model
}

type grokRouteError struct {
	status  int
	message string
}

func (e grokRouteError) Error() string { return e.message }
func writeGrokError(w http.ResponseWriter, err error) {
	var typed grokRouteError
	if errors.As(err, &typed) {
		writeError(w, typed.status, "invalid_request_error", typed.message)
		return
	}
	writeError(w, 502, "api_error", "Grok subscription request failed")
}

type grokScopedAuthority struct {
	grok.Authority
	account grok.Account
}

func (a grokScopedAuthority) Accounts(context.Context) ([]grok.Account, error) {
	return []grok.Account{a.account}, nil
}

func (s *Server) selectGrokAccount(ctx context.Context, body []byte, model string) (grokBoundState, error) {
	if s.GrokAuthority == nil {
		return grokBoundState{}, grokRouteError{503, "Grok subscription login unavailable; run cq grok login"}
	}
	bound, err := s.grokBindings.requestState(body, model)
	if err != nil {
		return bound, grokRouteError{400, err.Error()}
	}
	if pinned, ok := ctx.Value(grokPinnedAccountContextKey{}).(grokBoundState); ok {
		if pinned.Model != model || bound.Account != "" && bound != pinned {
			return bound, grokRouteError{400, "Grok session account changed"}
		}
		bound = pinned
	}
	accounts, err := s.GrokAuthority.Accounts(ctx)
	if err != nil {
		return bound, err
	}
	exhausted := false
	for _, account := range accounts {
		if bound.Account != "" && bound.Account != account.ID {
			continue
		}
		models, err := grok.FetchModels(ctx, s.GrokAuthority, account.ID)
		if err != nil {
			continue
		}
		eligible := false
		for _, m := range models {
			if m.Backend != "responses" || m.Hidden {
				continue
			}
			if m.ID == model {
				eligible = true
			}
			for _, alias := range m.Aliases {
				if alias == model {
					eligible = true
				}
			}
		}
		if !eligible {
			continue
		}
		results, err := grok.New(grokScopedAuthority{Authority: s.GrokAuthority, account: account}).Fetch(ctx, time.Now())
		if err != nil || len(results) != 1 || !results[0].IsUsable() {
			continue
		}
		usable := len(results[0].Windows) > 0
		for _, window := range results[0].Windows {
			remaining := float64(window.RemainingPct)
			if window.RemainingPctExact != nil {
				remaining = *window.RemainingPctExact
			}
			if remaining <= 0 {
				usable = false
				exhausted = true
			}
		}
		if usable {
			return grokBoundState{Account: account.ID, Model: model}, nil
		}
	}
	if exhausted {
		return bound, grokRouteError{429, "Grok included subscription quota exhausted"}
	}
	return bound, grokRouteError{503, "no eligible Grok subscription account for this model"}
}

type grokResponseResult struct {
	Response *http.Response
	Codec    *grokRequestCodec
	State    grokBoundState
	Compact  *zaiCompactResponse
}

func (s *Server) grokResponse(ctx context.Context, header http.Header, body []byte, standaloneCompact bool) (grokResponseResult, error) {
	var empty grokResponseResult
	if err := validateCodexJSONAuthority(body, "Grok request", func(_ []string, name string) bool {
		return name == "model" || name == "input" || name == "tools" || name == "previous_response_id" || name == "type" || name == "name" || name == "namespace" || name == "call_id" || name == "encrypted_content"
	}); err != nil {
		return empty, grokRouteError{400, "invalid Grok request"}
	}
	body, _, err := normaliseCQCompactionInput(body, s.cqCompactionToken())
	if err != nil {
		return empty, grokRouteError{400, err.Error()}
	}
	if len(body) > maxRequestBody {
		return empty, grokRouteError{413, "Grok request body exceeds 10 MiB"}
	}
	compact, err := zaiCompactionRequested(body)
	if err != nil {
		return empty, grokRouteError{400, "invalid Grok compaction trigger"}
	}
	model := extractModel(body)
	state, err := s.selectGrokAccount(ctx, body, model)
	if err != nil {
		return empty, err
	}
	body, err = s.grokBindings.restoreReasoning(body, state)
	if err != nil {
		var route grokRouteError
		if errors.As(err, &route) {
			return empty, err
		}
		return empty, grokRouteError{400, err.Error()}
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return empty, grokRouteError{400, "invalid Grok request"}
	}
	if compact || standaloneCompact {
		if len(request["previous_response_id"]) > 0 && string(request["previous_response_id"]) != `""` && string(request["previous_response_id"]) != "null" {
			return empty, grokRouteError{400, "Grok compaction requires full input"}
		}
		body, err = prepareCQCompactionRequest(body, compact)
		if err != nil {
			return empty, grokRouteError{400, err.Error()}
		}
	}
	encoded, codec, err := normaliseGrokRequest(body)
	if err != nil {
		return empty, grokRouteError{400, err.Error()}
	}
	path := "/responses"
	if len(encoded) > maxRequestBody {
		return empty, grokRouteError{413, "Grok request body exceeds 10 MiB"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grok.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return empty, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, name := range []string{"Accept", "X-Grok-Conv-Id"} {
		if v := header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	var stream bool
	var envelope struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(encoded, &envelope)
	stream = envelope.Stream
	if stream && path == "/responses" {
		req.Header.Set("Accept", "text/event-stream")
	}
	response, err := s.GrokAuthority.Do(ctx, state.Account, req)
	if err != nil {
		return empty, err
	}
	result := grokResponseResult{Response: response, Codec: codec, State: state}
	if compact || standaloneCompact {
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return empty, grokRouteError{response.StatusCode, "Grok subscription compaction unavailable"}
		}
		completed, err := readZAICompactionSummary(response.Body)
		if err != nil {
			return empty, err
		}
		sealed, err := sealCQCompactionSummary(s.cqCompactionToken(), request["model"], completed)
		if err != nil {
			return empty, err
		}
		if standaloneCompact {
			sealed.Object = "response.compaction"
		}
		out := &sealed
		result.Compact = out
		result.Response = &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
		s.grokBindings.record(state, out.ID, out.Output)
	}
	return result, nil
}

func (s *Server) serveGrokWebSocket(ctx context.Context, downstream *websocket.Conn, header http.Header, kind int, first []byte) error {
	var bound grokBoundState
	return s.serveResponsesHTTPWebSocket(ctx, downstream, header, kind, first, responsesHTTPBridge{Name: "Grok", PrewarmPrefix: "cq_grok_prewarm_", AcceptsModel: func(model string) bool { return RouteModelWithCatalog(model, s.Catalog) == ProviderGrok }, Compaction: zaiCompactionRequested, Stream: func(ctx context.Context, downstream *websocket.Conn, header http.Header, body []byte) (string, []json.RawMessage, error) {
		if bound.Account != "" {
			ctx = context.WithValue(ctx, grokPinnedAccountContextKey{}, bound)
		}
		result, err := s.grokResponse(ctx, header, body, false)
		if err != nil {
			return "", nil, err
		}
		defer result.Response.Body.Close()
		if bound.Account != "" && bound != result.State {
			return "", nil, errors.New("Grok session account changed")
		}
		bound = result.State
		if result.Compact != nil {
			for _, event := range zaiCompactionEvents(*result.Compact) {
				data, _ := json.Marshal(event)
				if err := downstream.WriteMessage(websocket.TextMessage, data); err != nil {
					return "", nil, err
				}
			}
			return result.Compact.ID, result.Compact.Output, nil
		}
		if result.Response.StatusCode < 200 || result.Response.StatusCode >= 300 {
			return "", nil, errors.New("Grok subscription stream rejected")
		}
		id, output, err := relayResponsesSSE(result.Response.Body, "Grok", result.Codec.normaliseResponse, func(_ string, data []byte) error { return downstream.WriteMessage(websocket.TextMessage, data) })
		if err == nil {
			s.grokBindings.record(result.State, id, output)
		}
		return id, output, err
	}})
}
