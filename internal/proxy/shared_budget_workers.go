package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

type sharedBudgetTurnKey struct{ thread, turn, model string }
type sharedBudgetSessionKey struct{ session, model string }

type sharedBudgetTurnRoutes struct {
	mu        sync.Mutex
	byTurn    map[sharedBudgetTurnKey]sharedBudgetBoundState
	bySession map[sharedBudgetSessionKey]sharedBudgetBoundState
}

// Remember the subscription before streaming, so a reconnect or fresh worker
// can retain the same route even before the original response completes.
func (s *Server) recordSharedBudgetTurn(header http.Header, body []byte, state sharedBudgetBoundState) {
	if key := sharedBudgetAffinity(header); key != "" {
		routes := &s.sharedBudgetTurns
		routes.mu.Lock()
		if routes.bySession == nil {
			routes.bySession = make(map[sharedBudgetSessionKey]sharedBudgetBoundState)
		}
		sessionKey := sharedBudgetSessionKey{key, strings.ToLower(state.Model)}
		if _, exists := routes.bySession[sessionKey]; !exists && len(routes.bySession) >= 4096 {
			for old := range routes.bySession {
				delete(routes.bySession, old)
				break
			}
		}
		routes.bySession[sessionKey] = state
		routes.mu.Unlock()
	}
	info, err := ParseCodexTurnMetadata(body, header.Get(codexTurnMetadataKey), nil)
	if err != nil || !info.Strong || info.Metadata.ThreadID == "" || info.Metadata.TurnID == "" {
		return
	}
	key := sharedBudgetTurnKey{info.Metadata.ThreadID, info.Metadata.TurnID, strings.ToLower(state.Model)}
	routes := &s.sharedBudgetTurns
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.byTurn == nil {
		routes.byTurn = make(map[sharedBudgetTurnKey]sharedBudgetBoundState)
	}
	if _, exists := routes.byTurn[key]; !exists && len(routes.byTurn) >= 4096 {
		for old := range routes.byTurn {
			delete(routes.byTurn, old)
			break
		}
	}
	routes.byTurn[key] = state
}

func sharedBudgetAffinity(header http.Header) string {
	key, source := sessionCorrelation(header)
	if source == "session_id" || source == "x-codex-window-id" {
		return key
	}
	return ""
}

func (routes *sharedBudgetTurnRoutes) session(key, model string) (sharedBudgetBoundState, bool) {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	state, found := routes.bySession[sharedBudgetSessionKey{key, strings.ToLower(ParseModel(model))}]
	return state, found
}

func (s *Server) sharedBudgetTurnRoute(ctx context.Context, header http.Header, body []byte, model string) (sharedBudgetSelection, bool) {
	if len(s.SharedBudgetRoutes) == 0 || !s.hasSharedBudgetCatalog() {
		return sharedBudgetSelection{}, false
	}
	info, err := ParseCodexTurnMetadata(body, header.Get(codexTurnMetadataKey), nil)
	if err != nil || !info.Strong {
		return sharedBudgetSelection{}, false
	}
	model = strings.ToLower(ParseModel(model))
	routes := &s.sharedBudgetTurns
	bound, found := routes.lookup(sharedBudgetTurnKey{info.Metadata.ThreadID, info.Metadata.TurnID, model})
	if !found && header.Get("X-OpenAI-Subagent") == "collab_spawn" {
		parentThread, parentTurn := sharedBudgetWorkerParent(header, body, info.Source)
		if parentThread != "" && parentTurn != "" && parentThread != info.Metadata.ThreadID {
			bound, found = routes.lookup(sharedBudgetTurnKey{parentThread, parentTurn, model})
		}
	}
	if !found {
		return sharedBudgetSelection{}, false
	}
	route, available := s.SharedBudgetRoutes[bound.Provider]
	if !available || route.Credential == nil && route.CredentialFor == nil || (bound.Fallback && !route.AllowFallback) || !portableSharedBudgetInput(body) {
		return sharedBudgetSelection{}, true
	}
	credential, err := route.credentialFor(ctx, bound.Credential)
	if err != nil || credential.Bearer == "" || credential.binding() != bound.Credential {
		return sharedBudgetSelection{}, true
	}
	return sharedBudgetSelection{id: bound.Provider, route: route, credential: credential, model: bound.Model, fallback: bound.Fallback, bound: true}, true
}

func (routes *sharedBudgetTurnRoutes) lookup(key sharedBudgetTurnKey) (sharedBudgetBoundState, bool) {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	bound, found := routes.byTurn[key]
	return bound, found
}

// Keep delegation provenance separate from native Codex lease identity.
func sharedBudgetWorkerParent(header http.Header, body []byte, source CodexTurnMetadataSource) (string, string) {
	raw := []byte(header.Get(codexTurnMetadataKey))
	if source == CodexTurnMetadataNested {
		var request struct {
			ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
		}
		if json.Unmarshal(body, &request) != nil {
			return "", ""
		}
		raw = request.ClientMetadata[codexTurnMetadataKey]
		if len(raw) > 0 && raw[0] == '"' {
			var encoded string
			if json.Unmarshal(raw, &encoded) != nil {
				return "", ""
			}
			raw = []byte(encoded)
		}
	}
	var parent struct {
		Thread string `json:"parent_thread_id"`
		Turn   string `json:"parent_turn_id"`
	}
	if json.Unmarshal(raw, &parent) != nil ||
		len(parent.Thread) > codexTurnIDMaxBytes || len(parent.Turn) > codexTurnIDMaxBytes {
		return "", ""
	}
	if thread := header.Get("X-Codex-Parent-Thread-Id"); thread != "" && thread != parent.Thread {
		return "", ""
	}
	return parent.Thread, parent.Turn
}
