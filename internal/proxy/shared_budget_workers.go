package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

type sharedBudgetTurnKey struct{ thread, turn, model string }

type sharedBudgetTurnRoutes struct {
	mu     sync.Mutex
	byTurn map[sharedBudgetTurnKey]sharedBudgetBoundState
}

// A fresh worker has its own Responses connection. Remember the selected
// subscription before streaming so a tool call can spawn a worker immediately.
func (s *Server) recordSharedBudgetTurn(header http.Header, body []byte, state sharedBudgetBoundState) {
	info, err := ParseCodexTurnMetadata(body, header.Get(codexTurnMetadataKey), nil)
	if err != nil || !info.Strong || info.Metadata.TurnID == "" {
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

func (s *Server) sharedBudgetWorkerRoute(ctx context.Context, header http.Header, body []byte, model string) (sharedBudgetSelection, bool) {
	if header.Get("X-OpenAI-Subagent") != "collab_spawn" || !s.hasSharedBudgetCatalog() {
		return sharedBudgetSelection{}, false
	}
	info, err := ParseCodexTurnMetadata(body, header.Get(codexTurnMetadataKey), nil)
	if err != nil || !info.Strong {
		return sharedBudgetSelection{}, false
	}
	parentThread, parentTurn := sharedBudgetWorkerParent(header, body, info.Source)
	if parentThread == "" || parentTurn == "" || parentThread == info.Metadata.ThreadID {
		return sharedBudgetSelection{}, false
	}
	key := sharedBudgetTurnKey{parentThread, parentTurn, strings.ToLower(ParseModel(model))}
	routes := &s.sharedBudgetTurns
	routes.mu.Lock()
	bound, found := routes.byTurn[key]
	routes.mu.Unlock()
	if !found {
		return sharedBudgetSelection{}, false
	}
	route, available := s.SharedBudgetRoutes[bound.Provider]
	if !available || route.Credential == nil || (bound.Fallback && !route.AllowFallback) || !portableSharedBudgetInput(body) {
		return sharedBudgetSelection{}, true
	}
	credential, err := route.Credential(ctx)
	if err != nil || credential.Bearer == "" || credential.binding() != bound.Credential {
		return sharedBudgetSelection{}, true
	}
	return sharedBudgetSelection{id: bound.Provider, route: route, credential: credential, model: bound.Model, fallback: bound.Fallback}, true
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
