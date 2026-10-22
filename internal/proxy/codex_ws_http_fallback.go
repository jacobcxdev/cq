package proxy

import (
	"net/http"
	"sync"
)

// Codex switches to HTTP on a 426 handshake response, not on a 1009 close.
// Remember affected sessions across connections. The fixed ring bounds memory;
// Codex itself retains HTTP fallback for the rest of its session.
type codexWSHTTPFallback struct {
	mu       sync.Mutex
	sessions [1024]string
	next     int
}

func codexWSHTTPFallbackKey(header http.Header) string {
	if session := header.Get("Session_id"); session != "" {
		return hashPrefix("codex-session", session)
	}
	if window := header.Get("X-Codex-Window-Id"); window != "" {
		return hashPrefix("codex-window", window)
	}
	return ""
}

func (fallback *codexWSHTTPFallback) required(key string) bool {
	if key == "" {
		return false
	}
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	for _, session := range fallback.sessions {
		if session == key {
			return true
		}
	}
	return false
}

func (fallback *codexWSHTTPFallback) remember(key string) {
	if key == "" {
		return
	}
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	for _, session := range fallback.sessions {
		if session == key {
			return
		}
	}
	fallback.sessions[fallback.next] = key
	fallback.next = (fallback.next + 1) % len(fallback.sessions)
}
