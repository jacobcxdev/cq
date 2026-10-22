package proxy

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

func TestCodexWSHTTPFallbackScopesAndBoundsSessions(t *testing.T) {
	var fallback codexWSHTTPFallback
	key := codexWSHTTPFallbackKey(http.Header{"Session_id": {"session"}, "X-Codex-Window-Id": {"window"}})
	other := codexWSHTTPFallbackKey(http.Header{"Session_id": {"other"}, "X-Codex-Window-Id": {"window"}})
	window := codexWSHTTPFallbackKey(http.Header{"X-Codex-Window-Id": {"window"}})
	if key == "" || key == other || key == window || window == "" {
		t.Fatal("fallback must prefer session identity over shared window")
	}
	fallback.remember("")
	if fallback.required("") || codexWSHTTPFallbackKey(http.Header{"User-Agent": {"shared-client"}}) != "" {
		t.Fatal("anonymous requests must not disable other clients")
	}
	var group sync.WaitGroup
	for index := 0; index < 16; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			fallback.remember(key)
			if !fallback.required(key) || fallback.required(other) {
				t.Error("fallback lost session isolation")
			}
		}()
	}
	group.Wait()
	if fallback.next != 1 {
		t.Fatalf("duplicate sessions consumed %d slots, want 1", fallback.next)
	}
	for index := range fallback.sessions {
		fallback.remember(fmt.Sprint(index))
	}
	if fallback.required(key) || !fallback.required("1023") {
		t.Fatal("fallback must evict oldest session when bounded ring fills")
	}
}

func TestCodexWSReadLimitRequiresHTTPFallback(t *testing.T) {
	failure := classifyCodexWebSocketFailure(classifyCodexWSDownstreamReadError(websocket.ErrReadLimit))
	if failure.Reason != codexWebSocketFailureReasonMessageTooBig {
		t.Fatalf("read-limit failure = %+v, want message_too_big", failure)
	}
}
