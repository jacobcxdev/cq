package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexWebSocketRoutingHandler owns one accepted downstream WebSocket while
// readiness-gated routing selects and supervises account-bound upstreams.
type CodexWebSocketRoutingHandler interface {
	Serve(context.Context, *websocket.Conn, http.Header) error
}

type codexTerminatingWebSocketHandler struct {
	plans       CodexNativeHTTPRequestPlanner
	upstream    codexWSUpstreamDialer
	upstreamURL string
	generation  atomic.Uint64
}

// NewCodexTerminatingWebSocketHandler constructs readiness-gated WebSocket
// enforcement. Downstream acceptance remains local; this handler performs
// provider admission only after inspecting the first strong request frame.
func NewCodexTerminatingWebSocketHandler(plans CodexNativeHTTPRequestPlanner, executor ExplicitWebSocketExecutor, upstream string) (CodexWebSocketRoutingHandler, error) {
	if plans == nil || executor == nil {
		return nil, errors.New("Codex WebSocket routing handler unavailable")
	}
	upstreamURL, err := codexAppServerWebSocketURL(upstream)
	if err != nil {
		return nil, errors.New("Codex WebSocket upstream is invalid")
	}
	return &codexTerminatingWebSocketHandler{
		plans:       plans,
		upstream:    codexExplicitWSUpstreamDialer{executor: executor},
		upstreamURL: upstreamURL,
	}, nil
}

func (handler *codexTerminatingWebSocketHandler) Serve(ctx context.Context, downstream *websocket.Conn, header http.Header) error {
	if handler == nil || downstream == nil || handler.plans == nil || handler.upstream == nil || handler.upstreamURL == "" {
		return ErrCodexLeaseWriterUnavailable
	}
	generation := handler.generation.Add(1)
	if generation == 0 {
		return ErrCodexLeaseWriterUnavailable
	}
	broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
		Plans:                handler.plans,
		Upstream:             handler.upstream,
		UpstreamURL:          handler.upstreamURL,
		Headers:              header,
		DownstreamGeneration: generation,
	})
	if err != nil {
		return err
	}
	downstream.SetReadLimit(maxRequestBody)
	return broker.Serve(ctx, downstream)
}

type codexWSUpstreamDialer interface {
	Dial(context.Context, RouteChoice, CandidateAttempt, string, http.Header) (websocketRelayConn, *http.Response, []byte, CandidateAttempt, error)
}

type codexExplicitWSUpstreamDialer struct {
	executor ExplicitWebSocketExecutor
}

func (dialer codexExplicitWSUpstreamDialer) Dial(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, upstreamURL string, header http.Header) (websocketRelayConn, *http.Response, []byte, CandidateAttempt, error) {
	if dialer.executor == nil {
		return nil, nil, nil, attempt, ErrCodexLeaseWriterUnavailable
	}
	conn, response, body, actual, err := executeCodexWebSocketAttempt(dialer.executor, ctx, choice, attempt, upstreamURL, header, nil)
	return conn, response, body, actual, err
}

type codexTerminatingWSBrokerConfig struct {
	Plans                CodexNativeHTTPRequestPlanner
	Upstream             codexWSUpstreamDialer
	UpstreamURL          string
	Headers              http.Header
	AcceptedRevision     codex.Revision
	DownstreamGeneration uint64
}

type codexTerminatingWSBroker struct {
	config             codexTerminatingWSBrokerConfig
	upstreamGeneration uint64
}

type codexWSActiveUpstream struct {
	conn       websocketRelayConn
	account    codex.AccountKey
	generation uint64
}

type codexWSDialResult struct {
	lifecycle *codexWSLifecycle
	wrapped   CodexWrappedError
	response  *http.Response
	body      []byte
	err       error
}

func newCodexTerminatingWSBroker(config codexTerminatingWSBrokerConfig) (*codexTerminatingWSBroker, error) {
	if config.Plans == nil || config.Upstream == nil || config.UpstreamURL == "" || config.DownstreamGeneration == 0 {
		return nil, fmt.Errorf("%w: incomplete terminating WebSocket broker", ErrCodexLeaseWriterUnavailable)
	}
	config.Headers = config.Headers.Clone()
	return &codexTerminatingWSBroker{config: config}, nil
}

func (broker *codexTerminatingWSBroker) Serve(ctx context.Context, downstream websocketRelayConn) error {
	if broker == nil || downstream == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var active codexWSActiveUpstream
	defer func() {
		if active.conn != nil {
			_ = active.conn.Close()
		}
	}()
	for {
		messageType, encoded, err := readCodexWSMessage(ctx, downstream)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return fmt.Errorf("Codex downstream WebSocket read failed")
		}
		pending, err := newCodexWSPendingFrame(messageType, encoded)
		if err != nil {
			return err
		}
		err = broker.serveFrame(ctx, downstream, pending, &active)
		pending.Release()
		if err != nil {
			return err
		}
	}
}

func (broker *codexTerminatingWSBroker) serveFrame(ctx context.Context, downstream websocketRelayConn, pending *codexWSPendingFrame, active *codexWSActiveUpstream) error {
	prepared, err := broker.config.Plans.Build(ctx, CodexHTTPRequestPlanInput{
		Encoded:          pending.encoded,
		Headers:          broker.config.Headers,
		AcceptedRevision: broker.config.AcceptedRevision,
	})
	if err != nil {
		return err
	}
	if prepared.Frozen != nil {
		defer prepared.Frozen.Release()
	}
	requestFrame, err := codexWSPreparedPendingFrame(prepared.Frozen, pending)
	if err != nil {
		return err
	}
	if requestFrame != pending {
		defer requestFrame.Release()
	}
	if prepared.leaseHandle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) == 0 {
		return prepared.Dispatch.TerminalError()
	}
	accountIndex := 0
	for {
		dial := broker.connect(ctx, prepared.leaseHandle, accounts[accountIndex], active)
		if dial.lifecycle == nil {
			if dial.err != nil {
				return dial.err
			}
			return ErrCodexLeaseWriterUnavailable
		}
		lifecycle := dial.lifecycle
		if active.conn == nil {
			rotated, finishErr := broker.finishHandshakeFailure(ctx, downstream, requestFrame, lifecycle, accounts, &accountIndex, active, dial)
			if finishErr != nil {
				return finishErr
			}
			if rotated {
				prepared.leaseHandle = lifecycle.handle
				continue
			}
			return nil
		}
		if dial.response != nil {
			turnState, _, stateErr := ParseCodexTurnStateHeader(dial.response.Header)
			if stateErr != nil {
				_ = lifecycle.Indeterminate(ctx, active.generation)
				return fmt.Errorf("Codex upstream WebSocket authority invalid")
			}
			if err := lifecycle.ObserveUpstreamUpgrade(ctx, active.generation, turnState); err != nil {
				return err
			}
		}
		if err := writeCodexWSMessage(ctx, active.conn, requestFrame.messageType, requestFrame.encoded); err != nil {
			_ = lifecycle.Indeterminate(context.WithoutCancel(ctx), active.generation)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("Codex upstream WebSocket write failed")
		}
		rotated, err := broker.readUpstreamRequest(ctx, downstream, requestFrame, lifecycle, accounts, &accountIndex, active)
		if err != nil {
			return err
		}
		if !rotated {
			return nil
		}
		prepared.leaseHandle = lifecycle.handle
	}
}

func (broker *codexTerminatingWSBroker) connect(ctx context.Context, handle *CodexLeaseRequestHandle, account CodexFrozenDispatchAccount, active *codexWSActiveUpstream) codexWSDialResult {
	marked, err := handle.MarkDispatchedContext(ctx)
	if err != nil {
		return codexWSDialResult{err: err}
	}
	choice := account.Choice()
	if active.conn != nil && active.account == choice.AccountKey {
		lifecycle, err := newCodexWSLifecycle(marked, broker.config.DownstreamGeneration, active.generation)
		return codexWSDialResult{lifecycle: lifecycle, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}, err: err}
	}
	if active.conn != nil {
		_ = active.conn.Close()
		*active = codexWSActiveUpstream{}
	}
	var last codexWSDialResult
	for _, attempt := range account.Attempts() {
		broker.upstreamGeneration++
		generation := broker.upstreamGeneration
		lifecycle, lifecycleErr := newCodexWSLifecycle(marked, broker.config.DownstreamGeneration, generation)
		if lifecycleErr != nil {
			return codexWSDialResult{err: lifecycleErr}
		}
		conn, response, body, _, dialErr := broker.config.Upstream.Dial(ctx, choice, attempt, broker.config.UpstreamURL, broker.config.Headers)
		if dialErr == nil && conn != nil {
			*active = codexWSActiveUpstream{conn: conn, account: choice.AccountKey, generation: generation}
			return codexWSDialResult{lifecycle: lifecycle, response: response}
		}
		wrapped, parseErr := codexWSDialError(response, body)
		last = codexWSDialResult{lifecycle: lifecycle, wrapped: wrapped, response: response, body: append([]byte(nil), body...), err: errors.Join(dialErr, parseErr)}
		if !wrapped.AuthFailure {
			break
		}
	}
	return last
}

func (broker *codexTerminatingWSBroker) readUpstreamRequest(ctx context.Context, downstream websocketRelayConn, pending *codexWSPendingFrame, lifecycle *codexWSLifecycle, accounts []CodexFrozenDispatchAccount, accountIndex *int, active *codexWSActiveUpstream) (bool, error) {
	for {
		messageType, frame, err := readCodexWSMessage(ctx, active.conn)
		if err != nil {
			_ = lifecycle.Indeterminate(context.WithoutCancel(ctx), active.generation)
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, fmt.Errorf("Codex upstream WebSocket outcome indeterminate")
		}
		if messageType != websocket.TextMessage {
			_ = lifecycle.Indeterminate(ctx, active.generation)
			return false, ErrCodexWSInvalidFrame
		}
		result, observeErr := lifecycle.ObserveFrame(ctx, active.generation, frame)
		if observeErr != nil {
			return false, observeErr
		}
		if result.DefinitePreAdmissionRejection && result.HardUsageLimit && pending.portable && *accountIndex+1 < len(accounts) {
			if err := lifecycle.RejectAndPrepare(ctx, active.generation, uint32(*accountIndex+2)); err != nil {
				return false, err
			}
			_ = active.conn.Close()
			*active = codexWSActiveUpstream{}
			*accountIndex++
			return true, nil
		}
		if result.DefinitePreAdmissionRejection {
			if err := lifecycle.FinishRejected(active.generation); err != nil {
				return false, err
			}
			if err := writeCodexWSMessage(ctx, downstream, messageType, frame); err != nil {
				return false, fmt.Errorf("Codex downstream WebSocket write failed")
			}
			_ = active.conn.Close()
			*active = codexWSActiveUpstream{}
			return false, nil
		}
		if err := writeCodexWSMessage(ctx, downstream, messageType, frame); err != nil {
			return false, fmt.Errorf("Codex downstream WebSocket write failed")
		}
		if result.Terminal {
			if err := lifecycle.Drain(); err != nil {
				return false, err
			}
			return false, nil
		}
	}
}

func (broker *codexTerminatingWSBroker) finishHandshakeFailure(ctx context.Context, downstream websocketRelayConn, pending *codexWSPendingFrame, lifecycle *codexWSLifecycle, accounts []CodexFrozenDispatchAccount, accountIndex *int, active *codexWSActiveUpstream, dial codexWSDialResult) (bool, error) {
	if lifecycle == nil || accountIndex == nil {
		return false, ErrCodexLeaseWriterUnavailable
	}
	canRotate := pending.portable && *accountIndex+1 < len(accounts) && (dial.wrapped.HardUsageLimit || dial.wrapped.AuthFailure)
	if canRotate {
		if err := lifecycle.RejectAndPrepare(ctx, lifecycle.upstreamGeneration, uint32(*accountIndex+2)); err != nil {
			return false, err
		}
		*accountIndex++
		return true, nil
	}
	if dial.wrapped.Found || dial.response != nil {
		if err := lifecycle.FinishRejected(lifecycle.upstreamGeneration); err != nil {
			return false, err
		}
	} else if err := lifecycle.Indeterminate(ctx, lifecycle.upstreamGeneration); err != nil {
		return false, err
	}
	frame, status := canonicalCodexWSHandshakeError(dial.response, dial.wrapped)
	if err := writeCodexWSMessage(ctx, downstream, websocket.TextMessage, frame); err != nil {
		return false, fmt.Errorf("Codex downstream WebSocket write failed")
	}
	_ = downstream.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, http.StatusText(status)))
	if active.conn != nil {
		_ = active.conn.Close()
		*active = codexWSActiveUpstream{}
	}
	return false, nil
}

func readCodexWSMessage(ctx context.Context, conn websocketRelayConn) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetReadDeadline(time.Now())
	})
	messageType, payload, err := conn.ReadMessage()
	stop()
	if ctx.Err() != nil {
		return 0, nil, ctx.Err()
	}
	return messageType, payload, err
}

func writeCodexWSMessage(ctx context.Context, conn websocketRelayConn, messageType int, payload []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetWriteDeadline(time.Now())
	})
	err := conn.WriteMessage(messageType, payload)
	stop()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func codexWSDialError(response *http.Response, body []byte) (CodexWrappedError, error) {
	if response == nil {
		return CodexWrappedError{}, nil
	}
	return parseCodexHTTPError(body, response.StatusCode)
}

func codexWSPreparedPendingFrame(frozen *CodexFrozenRequest, original *codexWSPendingFrame) (*codexWSPendingFrame, error) {
	if frozen == nil {
		return original, nil
	}
	replay, err := frozen.Replay()
	if err != nil {
		return nil, err
	}
	defer replay.Release()
	body, err := replay.Body()
	if err != nil {
		return nil, err
	}
	encoded, readErr := ioReadBounded(body, maxRequestBody)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	return newCodexWSPendingFrame(websocket.TextMessage, encoded)
}

func canonicalCodexWSHandshakeError(response *http.Response, wrapped CodexWrappedError) ([]byte, int) {
	status := http.StatusBadGateway
	if response != nil && response.StatusCode >= 400 && response.StatusCode <= 599 {
		status = response.StatusCode
	}
	errorType := "api_error"
	if wrapped.AuthFailure {
		errorType = "authentication_error"
	} else if wrapped.HardUsageLimit {
		errorType = "usage_limit_reached"
	}
	envelope := struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type string `json:"type"`
		} `json:"error"`
	}{Type: "error", Status: status}
	envelope.Error.Type = errorType
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return []byte(`{"type":"error","status":502,"error":{"type":"api_error"}}`), http.StatusBadGateway
	}
	return encoded, status
}
