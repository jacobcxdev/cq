package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type blockingRelayConn struct {
	closed chan struct{}
	once   sync.Once
	panic  bool
}

func newBlockingRelayConn() *blockingRelayConn {
	return &blockingRelayConn{closed: make(chan struct{})}
}

func (c *blockingRelayConn) ReadMessage() (int, []byte, error) {
	if c.panic {
		c.panic = false
		panic("relay test")
	}
	<-c.closed
	return 0, nil, errors.New("closed")
}

func (*blockingRelayConn) WriteMessage(int, []byte) error   { return nil }
func (*blockingRelayConn) SetReadDeadline(time.Time) error  { return nil }
func (*blockingRelayConn) SetWriteDeadline(time.Time) error { return nil }
func (c *blockingRelayConn) Close() error                   { c.once.Do(func() { close(c.closed) }); return nil }

func TestRelayWebSocketPairJoinsPumpsAcrossRepeatedCancellation(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for iteration := 0; iteration < 1000; iteration++ {
		ctx, cancel := context.WithCancel(context.Background())
		left := newBlockingRelayConn()
		right := newBlockingRelayConn()
		cancel()
		if err := relayWebSocketPair(ctx, left, right); !errors.Is(err, context.Canceled) {
			t.Fatalf("relay error = %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+8 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline+8 {
		t.Fatalf("goroutines = %d, baseline %d", got, baseline)
	}
}

func TestRelayWebSocketPairRecoversPumpPanicAndJoinsPeer(t *testing.T) {
	left := newBlockingRelayConn()
	left.panic = true
	right := newBlockingRelayConn()
	if err := relayWebSocketPair(context.Background(), left, right); err == nil || !strings.Contains(err.Error(), "relay panic") {
		t.Fatalf("relay error = %v", err)
	}
}

func TestCodexWebSocketAttemptExecutorInjectsOnlySelectedCredential(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer selected" || r.Header.Get("ChatGPT-Account-ID") != "account" || r.Header.Get("x-api-key") != "" {
			t.Errorf("headers auth=%q account=%q api-key=%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"), r.Header.Get("x-api-key"))
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()
	resolver := &testSecretResolver{materials: map[codex.CandidateID]codex.CredentialMaterial{
		"candidate": {AccessToken: "selected", AccountID: "account"},
	}}
	executor := NewCodexWebSocketAttemptExecutor(resolver)
	choice := RouteChoice{AccountKey: "identity"}
	attempt := CandidateAttempt{AccountKey: "identity", Candidate: codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"}}
	headers := http.Header{"Authorization": []string{"Bearer caller"}, "x-api-key": []string{"caller-key"}}
	conn, response, _, err := executor.Dial(context.Background(), choice, attempt, "ws"+strings.TrimPrefix(server.URL, "http"), headers)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	_ = conn.Close()
}
