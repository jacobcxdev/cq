package proxy

import "net/http"

func (server *Server) observeHarness(header http.Header, model string, body []byte) {
	if server.HarnessObserver == nil || model == "" {
		return
	}
	metadata, err := parseCodexTurnMetadataWithNestedLimit(body, header.Get("X-Codex-Turn-Metadata"), nil, codexTurnMetadataMaxBytes)
	if err != nil {
		return
	}
	chatID := metadata.Metadata.ThreadID
	if chatID == "" {
		chatID = header.Get("Session_id")
	}
	if chatID == "" {
		chatID = header.Get("X-Session-Id")
	}
	if chatID != "" {
		server.HarnessObserver(chatID, model)
	}
}
