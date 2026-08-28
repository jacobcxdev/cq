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

type codexWebSocketFailureStage string

const (
	codexWebSocketFailureStageUnknown        codexWebSocketFailureStage = "unknown"
	codexWebSocketFailureStageDownstreamRead codexWebSocketFailureStage = "downstream_read"
	codexWebSocketFailureStageFrameDecode    codexWebSocketFailureStage = "frame_decode"
	codexWebSocketFailureStageUpstreamIdle   codexWebSocketFailureStage = "upstream_idle"
	codexWebSocketFailureStageUpstreamRead   codexWebSocketFailureStage = "upstream_read"
)

type codexWebSocketFailureReason string

const (
	codexWebSocketFailureReasonUnknown                      codexWebSocketFailureReason = "unknown"
	codexWebSocketFailureReasonUpstreamClosed               codexWebSocketFailureReason = "upstream_closed"
	codexWebSocketFailureReasonUpstreamOutcomeIndeterminate codexWebSocketFailureReason = "upstream_outcome_indeterminate"
	codexWebSocketFailureReasonInvalidFrame                 codexWebSocketFailureReason = "invalid_frame"
	codexWebSocketFailureReasonDownstreamReadFailed         codexWebSocketFailureReason = "downstream_read_failed"
	codexWebSocketFailureReasonLeaseTransition              codexWebSocketFailureReason = "lease_transition"
	codexWebSocketFailureReasonStaleGeneration              codexWebSocketFailureReason = "stale_generation"
)

type codexWebSocketFailure struct {
	Stage  codexWebSocketFailureStage
	Reason codexWebSocketFailureReason
	plan   bool
}

type codexWebSocketBrokerError struct {
	failure codexWebSocketFailure
}

func (err *codexWebSocketBrokerError) Error() string {
	return "Codex WebSocket broker failed"
}

func newCodexWebSocketBrokerError(stage codexWebSocketFailureStage, reason codexWebSocketFailureReason) error {
	return &codexWebSocketBrokerError{failure: codexWebSocketFailure{
		Stage:  safeCodexWebSocketFailureStage(stage),
		Reason: safeCodexWebSocketFailureReason(reason),
	}}
}

func classifyCodexWebSocketFailure(err error) codexWebSocketFailure {
	var planErr *CodexHTTPRequestPlanError
	if errors.As(err, &planErr) {
		return codexWebSocketFailure{
			Stage:  codexWebSocketFailureStage(safeCodexHTTPRequestPlanErrorCode(planErr.Code)),
			Reason: codexWebSocketFailureReason(safeCodexRequestFailureReason(planErr.Reason)),
			plan:   true,
		}
	}
	var brokerErr *codexWebSocketBrokerError
	if errors.As(err, &brokerErr) {
		return codexWebSocketFailure{
			Stage:  safeCodexWebSocketFailureStage(brokerErr.failure.Stage),
			Reason: safeCodexWebSocketFailureReason(brokerErr.failure.Reason),
		}
	}
	if errors.Is(err, ErrCodexWSInvalidFrame) {
		return codexWebSocketFailure{Stage: codexWebSocketFailureStageFrameDecode, Reason: codexWebSocketFailureReasonInvalidFrame}
	}
	if errors.Is(err, ErrCodexLeaseTransition) {
		return codexWebSocketFailure{Stage: codexWebSocketFailureStageUnknown, Reason: codexWebSocketFailureReasonLeaseTransition}
	}
	if errors.Is(err, ErrCodexWSStaleGeneration) {
		return codexWebSocketFailure{Stage: codexWebSocketFailureStageUnknown, Reason: codexWebSocketFailureReasonStaleGeneration}
	}
	if reason := codexRequestFailureReason(err); reason != CodexRequestFailureUnknown {
		return codexWebSocketFailure{Stage: codexWebSocketFailureStageUnknown, Reason: codexWebSocketFailureReason(reason)}
	}
	return codexWebSocketFailure{Stage: codexWebSocketFailureStageUnknown, Reason: codexWebSocketFailureReasonUnknown}
}

func safeCodexWebSocketFailureStage(stage codexWebSocketFailureStage) codexWebSocketFailureStage {
	switch stage {
	case codexWebSocketFailureStageUnknown,
		codexWebSocketFailureStageDownstreamRead,
		codexWebSocketFailureStageFrameDecode,
		codexWebSocketFailureStageUpstreamIdle,
		codexWebSocketFailureStageUpstreamRead:
		return stage
	default:
		return codexWebSocketFailureStageUnknown
	}
}

func safeCodexWebSocketFailureReason(reason codexWebSocketFailureReason) codexWebSocketFailureReason {
	switch reason {
	case codexWebSocketFailureReasonUnknown,
		codexWebSocketFailureReasonUpstreamClosed,
		codexWebSocketFailureReasonUpstreamOutcomeIndeterminate,
		codexWebSocketFailureReasonInvalidFrame,
		codexWebSocketFailureReasonDownstreamReadFailed,
		codexWebSocketFailureReasonLeaseTransition,
		codexWebSocketFailureReasonStaleGeneration:
		return reason
	default:
		requestReason := safeCodexRequestFailureReason(CodexRequestFailureReason(reason))
		if requestReason != CodexRequestFailureUnknown {
			return codexWebSocketFailureReason(requestReason)
		}
		return codexWebSocketFailureReasonUnknown
	}
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
	downstream.SetReadLimit(codexWebSocketMessageMaxBytes)
	return broker.Serve(ctx, downstream)
}

type codexWSUpstreamDialer interface {
	Dial(context.Context, RouteChoice, CandidateAttempt, string, http.Header, func(CandidateAttempt)) (websocketRelayConn, *http.Response, []byte, CandidateAttempt, error)
}

type codexWebSocketPrewarmPlanner interface {
	planWebSocketPrewarm(context.Context, CodexHTTPRequestPlanInput) (CodexFrozenDispatchPlan, error)
	beginWebSocketPrewarm(CodexHTTPRequestPlanInput) (CodexPrewarmReservation, error)
	bindWebSocketPrewarm(CodexPrewarmReservation, codex.AccountKey, uint64, uint64) (CodexPrewarmReservation, error)
	readyWebSocketPrewarm(CodexPrewarmReservation, string, string) (CodexPrewarmReservation, error)
	cancelWebSocketPrewarm(CodexPrewarmReservation) error
	adoptWebSocketPrewarm(context.Context, CodexHTTPRequestPlanInput, CodexPrewarmReservation, CodexPrewarmAdoptionRevalidator) (CodexPreparedHTTPRequest, error)
}

type codexExplicitWSUpstreamDialer struct {
	executor ExplicitWebSocketExecutor
}

func (dialer codexExplicitWSUpstreamDialer) Dial(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, upstreamURL string, header http.Header, onDispatch func(CandidateAttempt)) (websocketRelayConn, *http.Response, []byte, CandidateAttempt, error) {
	if dialer.executor == nil {
		return nil, nil, nil, attempt, ErrCodexLeaseWriterUnavailable
	}
	conn, response, body, actual, err := executeCodexWebSocketAttempt(dialer.executor, ctx, choice, attempt, upstreamURL, header, onDispatch)
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

const (
	codexWSIdleKeepAliveInterval = 30 * time.Second
	codexWSControlWriteTimeout   = time.Second
)

type codexWSFrameObservationSinkContextKey struct{}

type codexWSFrameObservationSink func(*routeDiagnostics)

func withCodexWSFrameObservationSink(ctx context.Context, sink codexWSFrameObservationSink) context.Context {
	return context.WithValue(ctx, codexWSFrameObservationSinkContextKey{}, sink)
}

func emitAcceptedCodexWSFrameObservation(ctx context.Context, diagnostics *routeDiagnostics) {
	if ctx == nil || diagnostics == nil {
		return
	}
	sink, _ := ctx.Value(codexWSFrameObservationSinkContextKey{}).(codexWSFrameObservationSink)
	if sink != nil {
		sink(diagnostics)
	}
}

type codexWSActiveUpstream struct {
	conn       websocketRelayConn
	account    codex.AccountKey
	generation uint64
	readCancel context.CancelFunc
	readFrames <-chan codexWSUpstreamRead
	readDone   <-chan struct{}
	idleCancel context.CancelFunc
	idleDone   <-chan error
	prewarm    CodexPrewarmReservation
}

type codexWSDialResult struct {
	lifecycle *codexWSLifecycle
	wrapped   CodexWrappedError
	response  *http.Response
	body      []byte
	err       error
}

type codexWSUpstreamRead struct {
	messageType int
	payload     []byte
	err         error
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
		broker.cancelActivePrewarm(&active)
		closeCodexWSActiveUpstream(&active)
	}()
	for {
		messageType, encoded, err := readCodexWSMessage(ctx, downstream)
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.Is(err, io.EOF) || errors.As(err, &closeErr) {
				return nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return newCodexWebSocketBrokerError(codexWebSocketFailureStageDownstreamRead, codexWebSocketFailureReasonDownstreamReadFailed)
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

func startCodexWSUpstreamReader(ctx context.Context, active *codexWSActiveUpstream) {
	if active == nil || active.conn == nil || active.readCancel != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithCancel(ctx)
	frames := make(chan codexWSUpstreamRead)
	done := make(chan struct{})
	conn := active.conn
	active.readCancel = cancel
	active.readFrames = frames
	active.readDone = done
	go func() {
		defer close(done)
		defer close(frames)
		defer func() {
			if recover() != nil {
				select {
				case frames <- codexWSUpstreamRead{err: errors.New("Codex upstream WebSocket read failed")}:
				case <-readCtx.Done():
				}
			}
		}()
		for {
			messageType, payload, err := conn.ReadMessage()
			select {
			case frames <- codexWSUpstreamRead{messageType: messageType, payload: payload, err: err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

func readCodexWSActiveMessage(ctx context.Context, active *codexWSActiveUpstream) (int, []byte, error) {
	if active == nil || active.conn == nil {
		return 0, nil, ErrCodexLeaseWriterUnavailable
	}
	if active.readFrames == nil {
		return readCodexWSMessage(ctx, active.conn)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case result, ok := <-active.readFrames:
		if !ok {
			return 0, nil, errors.New("Codex upstream WebSocket read failed")
		}
		return result.messageType, result.payload, result.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func codexWSIdleUpstreamError(active *codexWSActiveUpstream) error {
	if active == nil || active.readFrames == nil {
		return nil
	}
	select {
	case result, ok := <-active.readFrames:
		if !ok || result.err != nil {
			return newCodexWebSocketBrokerError(codexWebSocketFailureStageUpstreamIdle, codexWebSocketFailureReasonUpstreamClosed)
		}
		return ErrCodexWSInvalidFrame
	default:
		return nil
	}
}

func closeCodexWSActiveConnection(active *codexWSActiveUpstream) {
	if active == nil {
		return
	}
	if active.idleCancel != nil {
		active.idleCancel()
		if active.idleDone != nil {
			<-active.idleDone
		}
	}
	active.idleCancel = nil
	active.idleDone = nil
	if active.readCancel != nil {
		active.readCancel()
	}
	if active.conn != nil {
		_ = active.conn.Close()
	}
	if active.readDone != nil {
		<-active.readDone
	}
	active.conn = nil
	active.readCancel = nil
	active.readFrames = nil
	active.readDone = nil
}

func closeCodexWSActiveUpstream(active *codexWSActiveUpstream) {
	if active == nil {
		return
	}
	closeCodexWSActiveConnection(active)
	*active = codexWSActiveUpstream{}
}

func serveCodexWSIdleKeepalive(ctx context.Context, upstream websocketRelayConn, ticks <-chan time.Time) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if upstream == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			if err := upstream.WriteControl(websocket.PingMessage, nil, time.Now().Add(codexWSControlWriteTimeout)); err != nil {
				return fmt.Errorf("Codex upstream WebSocket idle keepalive failed")
			}
		}
	}
}

func (broker *codexTerminatingWSBroker) startIdleUpstreamKeepalive(ctx context.Context, active *codexWSActiveUpstream) {
	if broker == nil || active == nil || active.conn == nil || active.idleCancel != nil {
		return
	}
	idleCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	upstream := active.conn
	active.idleCancel = cancel
	active.idleDone = done
	go func() {
		var err error
		defer func() {
			if recover() != nil {
				err = fmt.Errorf("Codex upstream WebSocket idle keepalive failed")
			}
			done <- err
		}()
		ticker := time.NewTicker(codexWSIdleKeepAliveInterval)
		defer ticker.Stop()
		err = serveCodexWSIdleKeepalive(idleCtx, upstream, ticker.C)
	}()
}

func (broker *codexTerminatingWSBroker) stopIdleUpstreamKeepalive(active *codexWSActiveUpstream) error {
	if broker == nil || active == nil || active.idleCancel == nil || active.idleDone == nil {
		return nil
	}
	active.idleCancel()
	err := <-active.idleDone
	active.idleCancel = nil
	active.idleDone = nil
	return err
}

func (broker *codexTerminatingWSBroker) serveFrame(ctx context.Context, downstream websocketRelayConn, pending *codexWSPendingFrame, active *codexWSActiveUpstream) error {
	if err := broker.stopIdleUpstreamKeepalive(active); err != nil {
		closeCodexWSActiveUpstream(active)
		return err
	}
	if err := codexWSIdleUpstreamError(active); err != nil {
		closeCodexWSActiveUpstream(active)
		return err
	}
	if pending.prewarm {
		return broker.servePrewarm(ctx, downstream, pending, active)
	}
	input := CodexHTTPRequestPlanInput{
		Encoded:          pending.encoded,
		AcceptedRevision: broker.config.AcceptedRevision,
	}
	var prepared CodexPreparedHTTPRequest
	var err error
	if active.prewarm.State == CodexPrewarmReady {
		planner, ok := broker.config.Plans.(codexWebSocketPrewarmPlanner)
		if !ok {
			return ErrCodexLeaseWriterUnavailable
		}
		if pending.request.PreviousResponseID != active.prewarm.ResponseAnchor {
			err = planner.cancelWebSocketPrewarm(active.prewarm)
			if err == nil {
				active.prewarm = CodexPrewarmReservation{}
				prepared, err = broker.config.Plans.Build(ctx, input)
			}
		} else {
			prepared, err = planner.adoptWebSocketPrewarm(ctx, input, active.prewarm, func(ctx context.Context, account codex.AccountKey, fence CodexPrewarmAdoptionFence) error {
				if active.conn == nil || active.account != account || active.generation != fence.UpstreamSocketGeneration ||
					broker.config.DownstreamGeneration != fence.DownstreamSocketGeneration || active.prewarm.Generation != fence.ReservationGeneration {
					return ErrCodexContinuity
				}
				return ctx.Err()
			})
			if err == nil {
				active.prewarm = CodexPrewarmReservation{}
			}
		}
	} else {
		prepared, err = broker.config.Plans.Build(ctx, input)
	}
	if err != nil {
		return err
	}
	if prepared.Frozen != nil {
		defer prepared.Frozen.Release()
	}
	requestFrame, err := codexWSPreparedPendingFrame(prepared.Frozen, pending)
	if err != nil {
		return codexWSAbandonPrepared(ctx, prepared.Lifecycle, err)
	}
	if requestFrame != pending {
		defer requestFrame.Release()
	}
	if prepared.leaseHandle == nil {
		return codexWSAbandonPrepared(ctx, prepared.Lifecycle, ErrCodexLeaseWriterUnavailable)
	}
	accounts := prepared.Dispatch.Accounts()
	if len(accounts) == 0 {
		return codexWSAbandonPrepared(ctx, prepared.Lifecycle, prepared.Dispatch.TerminalError())
	}
	emitAcceptedCodexWSFrameObservation(ctx, pending.diagnostics)
	accountIndex := 0
	for {
		dial := broker.connect(ctx, prepared.leaseHandle, prepared.receipt, accounts[accountIndex], active)
		if dial.lifecycle == nil {
			if dial.err != nil {
				return codexWSAbandonPreparedHandle(ctx, prepared.leaseHandle, dial.err)
			}
			return codexWSAbandonPreparedHandle(ctx, prepared.leaseHandle, ErrCodexLeaseWriterUnavailable)
		}
		lifecycle := dial.lifecycle
		rotated, err := broker.serveDispatchedFrame(ctx, downstream, requestFrame, lifecycle, accounts, &accountIndex, active, dial)
		if err != nil {
			return err
		}
		if !rotated {
			return nil
		}
		prepared.leaseHandle = lifecycle.handle
	}
}

func (broker *codexTerminatingWSBroker) serveDispatchedFrame(ctx context.Context, downstream websocketRelayConn, requestFrame *codexWSPendingFrame, lifecycle *codexWSLifecycle, accounts []CodexFrozenDispatchAccount, accountIndex *int, active *codexWSActiveUpstream, dial codexWSDialResult) (rotated bool, err error) {
	defer func() {
		if rotated && err == nil {
			return
		}
		if err != nil {
			closeCodexWSActiveUpstream(active)
		}
		err = errors.Join(err, lifecycle.cleanupAfterBrokerExit(ctx))
	}()
	if active.conn == nil {
		return broker.finishHandshakeFailure(ctx, downstream, requestFrame, lifecycle, accounts, accountIndex, active, dial)
	}
	if dial.response != nil {
		turnState, _, stateErr := ParseCodexTurnStateHeader(dial.response.Header)
		if stateErr != nil {
			_ = lifecycle.Indeterminate(ctx, active.generation)
			return false, fmt.Errorf("Codex upstream WebSocket authority invalid")
		}
		if err := lifecycle.ObserveUpstreamUpgrade(ctx, active.generation, turnState); err != nil {
			return false, err
		}
	}
	if err := writeCodexWSMessage(ctx, active.conn, requestFrame.messageType, requestFrame.encoded); err != nil {
		_ = lifecycle.Indeterminate(context.WithoutCancel(ctx), active.generation)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("Codex upstream WebSocket write failed")
	}
	return broker.readUpstreamRequest(ctx, downstream, requestFrame, lifecycle, accounts, accountIndex, active)
}

func codexWSAbandonPrepared(ctx context.Context, lifecycle CodexHTTPRequestLifecycle, cause error) error {
	if lifecycle == nil {
		return cause
	}
	_, err := lifecycle.AbandonBeforeDispatchContext(context.WithoutCancel(ctx))
	return errors.Join(cause, err)
}

func codexWSAbandonPreparedHandle(ctx context.Context, handle *CodexLeaseRequestHandle, cause error) error {
	if handle == nil {
		return cause
	}
	_, err := handle.AbandonBeforeDispatchContext(context.WithoutCancel(ctx))
	return errors.Join(cause, err)
}

func (broker *codexTerminatingWSBroker) servePrewarm(ctx context.Context, downstream websocketRelayConn, pending *codexWSPendingFrame, active *codexWSActiveUpstream) error {
	planner, ok := broker.config.Plans.(codexWebSocketPrewarmPlanner)
	if !ok {
		return ErrCodexLeaseWriterUnavailable
	}
	dispatch, err := planner.planWebSocketPrewarm(ctx, CodexHTTPRequestPlanInput{
		Encoded:          pending.encoded,
		AcceptedRevision: broker.config.AcceptedRevision,
	})
	if err != nil {
		return err
	}
	accounts := dispatch.Accounts()
	if len(accounts) == 0 {
		return dispatch.TerminalError()
	}
	reservation, err := planner.beginWebSocketPrewarm(CodexHTTPRequestPlanInput{
		Encoded:          pending.encoded,
		AcceptedRevision: broker.config.AcceptedRevision,
	})
	if err != nil {
		return err
	}
	active.prewarm = reservation
	emitAcceptedCodexWSFrameObservation(ctx, pending.diagnostics)
	for accountIndex, account := range accounts {
		dial := broker.connectPrewarm(ctx, account, active)
		if active.conn == nil {
			canRotate := accountIndex+1 < len(accounts) && (dial.wrapped.HardUsageLimit || dial.wrapped.AuthFailure)
			if canRotate {
				continue
			}
			broker.cancelActivePrewarm(active)
			frame, status := canonicalCodexWSHandshakeError(dial.response, dial.wrapped)
			if err := writeCodexWSMessage(ctx, downstream, websocket.TextMessage, frame); err != nil {
				return fmt.Errorf("Codex downstream WebSocket write failed")
			}
			_ = downstream.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, http.StatusText(status)))
			return nil
		}
		if err := writeCodexWSMessage(ctx, active.conn, pending.messageType, pending.encoded); err != nil {
			broker.cancelActivePrewarm(active)
			closeCodexWSActiveUpstream(active)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("Codex upstream WebSocket write failed")
		}
		rotate, err := broker.readPrewarmResponse(ctx, downstream, planner, accountIndex+1 < len(accounts), active)
		if err != nil {
			return err
		}
		if !rotate {
			return nil
		}
	}
	return ErrCodexLeaseWriterUnavailable
}

func (broker *codexTerminatingWSBroker) connectPrewarm(ctx context.Context, account CodexFrozenDispatchAccount, active *codexWSActiveUpstream) codexWSDialResult {
	choice := account.Choice()
	if active.conn != nil && active.account == choice.AccountKey {
		return codexWSDialResult{response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}}
	}
	if active.conn != nil {
		closeCodexWSActiveUpstream(active)
	}
	var last codexWSDialResult
	for _, attempt := range account.Attempts() {
		broker.upstreamGeneration++
		generation := broker.upstreamGeneration
		conn, response, body, _, dialErr := broker.config.Upstream.Dial(ctx, choice, attempt, broker.config.UpstreamURL, broker.config.Headers, nil)
		if dialErr == nil && conn != nil {
			active.conn = conn
			active.account = choice.AccountKey
			active.generation = generation
			return codexWSDialResult{response: response}
		}
		wrapped, parseErr := codexWSDialError(response, body)
		last = codexWSDialResult{wrapped: wrapped, response: response, body: append([]byte(nil), body...), err: errors.Join(dialErr, parseErr)}
		if !wrapped.AuthFailure {
			break
		}
	}
	return last
}

func (broker *codexTerminatingWSBroker) readPrewarmResponse(ctx context.Context, downstream websocketRelayConn, planner codexWebSocketPrewarmPlanner, canRotate bool, active *codexWSActiveUpstream) (bool, error) {
	relayed := false
	responseAnchor := ""
	turnState := ""
	for {
		messageType, frame, err := readCodexWSActiveMessage(ctx, active)
		if err != nil {
			broker.cancelActivePrewarm(active)
			closeCodexWSActiveUpstream(active)
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, fmt.Errorf("Codex upstream WebSocket prewarm failed")
		}
		if messageType != websocket.TextMessage {
			broker.cancelActivePrewarm(active)
			closeCodexWSActiveUpstream(active)
			return false, ErrCodexWSInvalidFrame
		}
		observation := classifyCodexSSEData(frame)
		if observation.Kind == CodexSSEError && !relayed && canRotate && (observation.Error.HardUsageLimit || observation.Error.AuthFailure) {
			closeCodexWSActiveUpstream(active)
			return true, nil
		}
		if observation.Kind == CodexSSEMalformed || observation.Kind == CodexSSEUnknown {
			broker.cancelActivePrewarm(active)
			closeCodexWSActiveUpstream(active)
			return false, ErrCodexWSInvalidFrame
		}
		if observation.Kind != CodexSSEError && active.prewarm.State == CodexPrewarmCreating {
			reservation, bindErr := planner.bindWebSocketPrewarm(active.prewarm, active.account, broker.config.DownstreamGeneration, active.generation)
			if bindErr != nil {
				broker.cancelActivePrewarm(active)
				closeCodexWSActiveUpstream(active)
				return false, bindErr
			}
			active.prewarm = reservation
		}
		if observation.ResponseID != "" {
			responseAnchor = observation.ResponseID
		}
		if observation.TurnState != "" {
			turnState = observation.TurnState
		}
		if err := writeCodexWSMessage(ctx, downstream, messageType, frame); err != nil {
			return false, fmt.Errorf("Codex downstream WebSocket write failed")
		}
		relayed = true
		if observation.Kind == CodexSSECompleted {
			reservation, readyErr := planner.readyWebSocketPrewarm(active.prewarm, responseAnchor, turnState)
			if readyErr != nil {
				broker.cancelActivePrewarm(active)
				closeCodexWSActiveUpstream(active)
				return false, readyErr
			}
			active.prewarm = reservation
			return false, nil
		}
		if observation.Kind == CodexSSEError {
			broker.cancelActivePrewarm(active)
			closeCodexWSActiveUpstream(active)
			return false, nil
		}
	}
}

func (broker *codexTerminatingWSBroker) cancelActivePrewarm(active *codexWSActiveUpstream) {
	if broker == nil || active == nil || active.prewarm.Lane == (LaneKey{}) || active.prewarm.Correlation == "" || active.prewarm.State == CodexPrewarmAdopted || active.prewarm.State == CodexPrewarmCancelled {
		return
	}
	planner, ok := broker.config.Plans.(codexWebSocketPrewarmPlanner)
	if ok {
		_ = planner.cancelWebSocketPrewarm(active.prewarm)
	}
	active.prewarm = CodexPrewarmReservation{}
}

func (broker *codexTerminatingWSBroker) connect(ctx context.Context, handle *CodexLeaseRequestHandle, receipt *codexTurnReceiptHandle, account CodexFrozenDispatchAccount, active *codexWSActiveUpstream) codexWSDialResult {
	choice := account.Choice()
	marked, err := handle.MarkDispatchedContext(ctx)
	if err != nil {
		return codexWSDialResult{err: err}
	}
	if active.conn != nil && active.account == choice.AccountKey {
		receipt.attempt(choice.AccountKey)
		lifecycle, err := newCodexWSLifecycle(marked, broker.config.DownstreamGeneration, active.generation, receipt)
		return codexWSDialResult{lifecycle: lifecycle, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}, err: err}
	}
	if active.conn != nil {
		closeCodexWSActiveUpstream(active)
	}
	var last codexWSDialResult
	for _, attempt := range account.Attempts() {
		broker.upstreamGeneration++
		generation := broker.upstreamGeneration
		lifecycle, lifecycleErr := newCodexWSLifecycle(marked, broker.config.DownstreamGeneration, generation, receipt)
		if lifecycleErr != nil {
			return codexWSDialResult{err: lifecycleErr}
		}
		conn, response, body, _, dialErr := broker.config.Upstream.Dial(ctx, choice, attempt, broker.config.UpstreamURL, broker.config.Headers, func(actual CandidateAttempt) {
			receipt.attempt(actual.AccountKey)
		})
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
		messageType, frame, err := readCodexWSActiveMessage(ctx, active)
		if err != nil {
			_ = lifecycle.Indeterminate(context.WithoutCancel(ctx), active.generation)
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, newCodexWebSocketBrokerError(codexWebSocketFailureStageUpstreamRead, codexWebSocketFailureReasonUpstreamOutcomeIndeterminate)
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
			closeCodexWSActiveUpstream(active)
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
			closeCodexWSActiveUpstream(active)
			return false, nil
		}
		if err := writeCodexWSMessage(ctx, downstream, messageType, frame); err != nil {
			return false, fmt.Errorf("Codex downstream WebSocket write failed")
		}
		if result.Terminal {
			if err := lifecycle.Drain(); err != nil {
				return false, err
			}
			startCodexWSUpstreamReader(ctx, active)
			broker.startIdleUpstreamKeepalive(ctx, active)
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
		closeCodexWSActiveUpstream(active)
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
	encoded, readErr := ioReadBounded(body, codexWebSocketMessageMaxBytes)
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
