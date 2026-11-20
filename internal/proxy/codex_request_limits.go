package proxy

import (
	"errors"
	"io"
	"time"

	"github.com/gorilla/websocket"
)

const (
	codexHTTPRequestMaxBytes      = 0
	codexWebSocketMessageMaxBytes = 64 << 20
	// OpenAI's Codex Responses WebSocket request limit.
	codexWebSocketUpstreamRequestMaxBytes = 128 << 20
	codexDiagnosticLineMaxBytes           = 10 << 20
)

func codexLimitExceeded(size, limit int) bool {
	return limit > 0 && size > limit
}

type codexWSRequestConn struct {
	*websocket.Conn
	closeOnLimit bool
}

func (conn codexWSRequestConn) ReadMessage() (int, []byte, error) {
	kind, reader, err := conn.NextReader()
	if err != nil {
		return kind, nil, err
	}
	payload, err := readCodexWSRequestPayload(reader)
	if conn.closeOnLimit && errors.Is(err, websocket.ErrReadLimit) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"), time.Now().Add(time.Second))
	}
	return kind, payload, err
}

// Gorilla's read limit counts wire bytes. Bound the decoded request too.
func readCodexWSRequestPayload(reader io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, codexWebSocketUpstreamRequestMaxBytes+1))
	if codexLimitExceeded(len(payload), codexWebSocketUpstreamRequestMaxBytes) {
		err = websocket.ErrReadLimit
	}
	if err != nil {
		clearBytes(payload)
		return nil, err
	}
	return payload, nil
}
