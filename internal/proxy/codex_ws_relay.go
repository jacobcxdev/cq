package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// ExplicitWebSocketExecutor dials one already-selected credential attempt.
type ExplicitWebSocketExecutor interface {
	Dial(context.Context, RouteChoice, CandidateAttempt, string, http.Header) (*websocket.Conn, *http.Response, []byte, error)
}

// CodexWebSocketAttemptExecutor resolves secrets only for one upstream dial.
type CodexWebSocketAttemptExecutor struct {
	Secrets codex.SecretResolver
	Dialer  websocket.Dialer
}

func NewCodexWebSocketAttemptExecutor(secrets codex.SecretResolver) *CodexWebSocketAttemptExecutor {
	return &CodexWebSocketAttemptExecutor{
		Secrets: secrets,
		Dialer: websocket.Dialer{
			Proxy:             http.ProxyFromEnvironment,
			HandshakeTimeout:  30 * time.Second,
			EnableCompression: true,
		},
	}
}

// Dial performs one explicit upstream handshake without selection or retry.
func (e *CodexWebSocketAttemptExecutor) Dial(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, upstreamURL string, incoming http.Header) (*websocket.Conn, *http.Response, []byte, error) {
	if e == nil || e.Secrets == nil {
		return nil, nil, nil, fmt.Errorf("Codex WebSocket executor unavailable")
	}
	if attempt.AccountKey == "" || attempt.Candidate.AccountKey != attempt.AccountKey || choice.AccountKey != attempt.AccountKey {
		return nil, nil, nil, fmt.Errorf("Codex WebSocket attempt identity mismatch")
	}
	material, err := e.Secrets.Resolve(ctx, attempt.Candidate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve Codex WebSocket credential candidate: %w", err)
	}
	if material.AccessToken == "" {
		return nil, nil, nil, fmt.Errorf("resolved Codex WebSocket credential has no access token")
	}
	headers := cloneCodexAppServerHeaders(incoming)
	headers.Set("Authorization", "Bearer "+material.AccessToken)
	headers.Del("x-api-key")
	if material.AccountID != "" {
		headers.Set("ChatGPT-Account-ID", material.AccountID)
	} else {
		headers.Del("ChatGPT-Account-ID")
	}
	dialer := e.Dialer
	conn, response, err := dialer.DialContext(ctx, upstreamURL, headers)
	if err == nil {
		return conn, response, nil, nil
	}
	var body []byte
	if response != nil && response.Body != nil {
		body, _ = ioReadBounded(response.Body, codexAttemptResponseLimit)
		response.Body.Close()
	}
	return nil, response, body, err
}

type websocketRelayConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	Close() error
}

type websocketRelayGeneration struct {
	value atomic.Uint64
}

// relayWebSocketPair supervises both pumps, fences late writes, and joins exit.
func relayWebSocketPair(ctx context.Context, left, right websocketRelayConn) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var generation websocketRelayGeneration
	active := generation.value.Add(1)
	errCh := make(chan error, 2)
	pump := func(src, dst websocketRelayConn) {
		defer func() {
			if recovered := recover(); recovered != nil {
				errCh <- fmt.Errorf("Codex WebSocket relay panic")
			}
		}()
		for {
			messageType, message, err := src.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if generation.value.Load() != active {
				errCh <- context.Canceled
				return
			}
			if err := dst.WriteMessage(messageType, message); err != nil {
				errCh <- err
				return
			}
		}
	}
	go pump(left, right)
	go pump(right, left)

	var first error
	select {
	case first = <-errCh:
	case <-ctx.Done():
		first = ctx.Err()
	}
	generation.value.Add(1)
	now := time.Now()
	_ = left.SetReadDeadline(now)
	_ = left.SetWriteDeadline(now)
	_ = right.SetReadDeadline(now)
	_ = right.SetWriteDeadline(now)
	_ = left.Close()
	_ = right.Close()
	second := <-errCh
	return errors.Join(first, second)
}

func ioReadBounded(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}
