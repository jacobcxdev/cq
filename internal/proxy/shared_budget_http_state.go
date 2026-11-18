package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

type sharedBudgetBoundState struct {
	Provider   modelregistry.Provider
	Model      string
	Credential modelregistry.SharedBudgetCredentialScope
	Fallback   bool
}

func (c SharedBudgetCredential) binding() modelregistry.SharedBudgetCredentialScope {
	return modelregistry.FingerprintSharedBudgetCredential(c.Bearer, c.Account, c.WorkspaceID)
}

func (s sharedBudgetSelection) binding() sharedBudgetBoundState {
	return sharedBudgetBoundState{
		Provider: s.id, Model: s.model,
		Credential: s.credential.binding(), Fallback: s.fallback,
	}
}

// A previous response or encrypted item can return on a new HTTP request.
// Reuse the account that created it, even when the first-party pool recovers.
func (s *Server) serveSharedBudgetContinuation(w http.ResponseWriter, r *http.Request, body []byte, model string) bool {
	if len(s.SharedBudgetRoutes) == 0 || (!s.hasSharedBudgetCatalog() && !s.sharedBudgetBindings.hasReferences()) {
		return false
	}
	bound, found, complete := s.sharedBudgetRequestBinding(body)
	if !found && (complete || s.routeCodexModel(model) != ProviderSharedBudget) {
		return false
	}
	route, available := s.SharedBudgetRoutes[bound.Provider]
	if complete && available && route.Credential != nil && (!bound.Fallback || route.AllowFallback) &&
		strings.EqualFold(bound.Model, ParseModel(model)) {
		credential, err := route.Credential(r.Context())
		if err == nil && credential.Bearer != "" && credential.binding() == bound.Credential {
			selected := sharedBudgetSelection{id: bound.Provider, route: route, credential: credential, model: bound.Model, fallback: bound.Fallback}
			s.serveSelectedSharedBudgetHTTP(w, r, body, selected)
			return true
		}
	}
	writeError(w, http.StatusConflict, "invalid_request_error", "subscription continuation unavailable; retry with full readable input")
	return true
}

func (s *Server) sharedBudgetRequestBinding(body []byte) (bound sharedBudgetBoundState, found, complete bool) {
	var request struct {
		Previous     string          `json:"previous_response_id"`
		Input        json.RawMessage `json:"input"`
		Conversation json.RawMessage `json:"conversation"`
	}
	if json.Unmarshal(body, &request) != nil {
		return bound, false, true
	}
	var items []json.RawMessage
	_ = json.Unmarshal(request.Input, &items)
	b := &s.sharedBudgetBindings
	b.mu.Lock()
	defer b.mu.Unlock()
	complete = len(request.Conversation) == 0 || string(request.Conversation) == "null"
	merge := func(state sharedBudgetBoundState, recorded bool) {
		if !recorded {
			complete = false
			return
		}
		if state.Provider == "" || found && bound != state {
			complete = false
		}
		found, bound = true, state
	}
	if request.Previous != "" {
		anchor, recorded := b.responses[request.Previous]
		merge(anchor, recorded)
	}
	for _, raw := range items {
		var item struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type == "item_reference" {
			complete = false
		}
		if item.Encrypted == "" {
			continue
		}
		hash := sha256.Sum256([]byte(item.Encrypted))
		anchor, recorded := b.opaque[hex.EncodeToString(hash[:])]
		merge(anchor, recorded)
	}
	return bound, found, complete
}

func (s *Server) recordSharedBudgetResponse(state sharedBudgetBoundState, raw []byte) {
	var completed zaiCompactResponse
	if json.Unmarshal(raw, &completed) == nil && completed.ID != "" {
		s.sharedBudgetBindings.record(state, completed.ID, completed.Output)
	}
}

func (s *Server) knownSharedBudgetState(body []byte) bool {
	if !s.sharedBudgetBindings.hasReferences() {
		return false
	}
	_, found, _ := s.sharedBudgetRequestBinding(body)
	return found
}

func (s *Server) relaySharedBudgetHTTP(w http.ResponseWriter, response *http.Response, selected sharedBudgetSelection) {
	state := selected.binding()
	record := func(raw []byte) { s.recordSharedBudgetResponse(state, raw) }
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "application/json" {
		data, err := httputil.ReadBody(response.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "api_error", "subscription response unavailable")
			return
		}
		if selected.route.PlaintextCollaboration {
			data, err = plaintextCollaborationResponse(data)
			if err != nil {
				writeError(w, http.StatusBadGateway, "api_error", "invalid subscription response")
				return
			}
		}
		record(data)
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
		return
	}
	w.WriteHeader(response.StatusCode)
	body := io.Reader(response.Body)
	if mediaType == "text/event-stream" {
		body = io.TeeReader(body, &sharedBudgetSSEObserver{parser: NewCodexSSEParser(1 << 20), record: record})
		if selected.route.PlaintextCollaboration {
			_ = copyNormalisedResponsesSSE(zaiFlushWriter{w}, body, plaintextCollaborationResponse, true)
			return
		}
	}
	_, _ = io.Copy(zaiFlushWriter{w}, body)
}

// Inspect completion before forwarding bytes, without changing the stream.
type sharedBudgetSSEObserver struct {
	parser *CodexSSEParser
	record func([]byte)
}

func (o *sharedBudgetSSEObserver) Write(data []byte) (int, error) {
	if o.parser == nil {
		return len(data), nil
	}
	events, err := o.parser.Feed(data)
	if err != nil {
		o.parser = nil
		return len(data), nil
	}
	for _, event := range events {
		if event.Kind != CodexSSECompleted {
			continue
		}
		var envelope struct{ Response json.RawMessage }
		if json.Unmarshal(event.Data, &envelope) == nil {
			o.record(envelope.Response)
		}
		o.parser = nil
		break
	}
	return len(data), nil
}
