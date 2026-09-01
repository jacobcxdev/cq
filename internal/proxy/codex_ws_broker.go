package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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
	Stage       codexWebSocketFailureStage
	Reason      codexWebSocketFailureReason
	Origin      codexWSInvalidFrameOrigin
	FrameType   codexWSFrameType
	FrameSize   codexWSFrameSize
	FrameDetail codexWSInvalidFrameDetail
	EventType   codexWSEventType
	ErrorStatus string
	ErrorType   codexWSEventType
	ErrorCode   codexWSEventType
	plan        bool
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
	var frameErr *codexWSInvalidFrameError
	if errors.As(err, &frameErr) {
		return codexWebSocketFailure{
			Stage:       codexWebSocketFailureStageFrameDecode,
			Reason:      codexWebSocketFailureReasonInvalidFrame,
			Origin:      frameErr.Origin,
			FrameType:   frameErr.Type,
			FrameSize:   frameErr.Size,
			FrameDetail: frameErr.Detail,
			EventType:   frameErr.Event,
			ErrorStatus: frameErr.Status,
			ErrorType:   frameErr.Kind,
			ErrorCode:   frameErr.Code,
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
	plans          CodexNativeHTTPRequestPlanner
	upstream       codexWSUpstreamDialer
	refresher      codex.CredentialReferenceRefresher
	capacity       *CodexCapacityLedger
	upstreamURL    string
	prewarmTimeout time.Duration
	generation     atomic.Uint64
}

// NewCodexTerminatingWebSocketHandler constructs readiness-gated WebSocket
// enforcement. Downstream acceptance remains local; this handler performs
// provider admission only after inspecting the first strong request frame.
func NewCodexTerminatingWebSocketHandler(plans CodexNativeHTTPRequestPlanner, executor ExplicitWebSocketExecutor, refresher codex.CredentialReferenceRefresher, capacity *CodexCapacityLedger, upstream string) (CodexWebSocketRoutingHandler, error) {
	if plans == nil || executor == nil || refresher == nil || capacity == nil {
		return nil, errors.New("Codex WebSocket routing handler unavailable")
	}
	upstreamURL, err := codexAppServerWebSocketURL(upstream)
	if err != nil {
		return nil, errors.New("Codex WebSocket upstream is invalid")
	}
	return &codexTerminatingWebSocketHandler{
		plans:          plans,
		upstream:       codexExplicitWSUpstreamDialer{executor: executor},
		refresher:      refresher,
		capacity:       capacity,
		upstreamURL:    upstreamURL,
		prewarmTimeout: codexWSPrewarmResponseTimeout,
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
		Refresher:            handler.refresher,
		Capacity:             handler.capacity,
		UpstreamURL:          handler.upstreamURL,
		Headers:              header,
		DownstreamGeneration: generation,
		PrewarmTimeout:       handler.prewarmTimeout,
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
	Refresher            codex.CredentialReferenceRefresher
	Capacity             *CodexCapacityLedger
	UpstreamURL          string
	Headers              http.Header
	AcceptedRevision     codex.Revision
	DownstreamGeneration uint64
	PrewarmTimeout       time.Duration
}

type codexTerminatingWSBroker struct {
	config             codexTerminatingWSBrokerConfig
	upstreamGeneration uint64
}

const (
	codexWSIdleKeepAliveInterval  = 30 * time.Second
	codexWSControlWriteTimeout    = time.Second
	codexWSPrewarmResponseTimeout = 10 * time.Second
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
	attempt    CandidateAttempt
	generation uint64
	readCancel context.CancelFunc
	readFrames <-chan codexWSUpstreamRead
	readDone   <-chan struct{}
	idleCancel context.CancelFunc
	idleDone   <-chan error
	prewarm    CodexPrewarmReservation
}

func codexWSActiveUpstreamMatchesFirstAttempt(active *codexWSActiveUpstream, account CodexFrozenDispatchAccount) bool {
	if active == nil || active.conn == nil || active.account != account.Choice().AccountKey {
		return false
	}
	attempts := account.Attempts()
	return len(attempts) != 0 && active.attempt == attempts[0]
}

func codexWSActiveUpstreamMatchesAnchoredCandidate(active *codexWSActiveUpstream, account CodexFrozenDispatchAccount) bool {
	if active == nil || active.conn == nil || active.account != account.Choice().AccountKey || active.attempt.Candidate.CandidateID == "" {
		return false
	}
	// Response anchors belong to the established provider socket. Inventory may
	// publish a newer revision while that socket remains authenticated, so an
	// anchored successor keeps it only while credential identity is unchanged.
	for _, attempt := range account.Attempts() {
		if active.attempt.Candidate == attempt.Candidate && active.attempt.Source == attempt.Source {
			return true
		}
	}
	return false
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

type codexWSDownstreamReader struct {
	conn     websocketRelayConn
	cancel   context.CancelFunc
	frames   <-chan codexWSUpstreamRead
	terminal <-chan error
	done     <-chan struct{}
}

func newCodexTerminatingWSBroker(config codexTerminatingWSBrokerConfig) (*codexTerminatingWSBroker, error) {
	if config.Plans == nil || config.Upstream == nil || config.UpstreamURL == "" || config.DownstreamGeneration == 0 {
		return nil, fmt.Errorf("%w: incomplete terminating WebSocket broker", ErrCodexLeaseWriterUnavailable)
	}
	if config.PrewarmTimeout <= 0 {
		config.PrewarmTimeout = codexWSPrewarmResponseTimeout
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
	serveCtx, cancelServe := context.WithCancel(ctx)
	downstreamReader := startCodexWSDownstreamReader(serveCtx, cancelServe, downstream)
	defer downstreamReader.close()
	var active codexWSActiveUpstream
	defer func() {
		broker.cancelActivePrewarm(&active)
		closeCodexWSActiveUpstream(&active)
	}()
	for {
		messageType, encoded, err := downstreamReader.read(ctx, serveCtx)
		if err != nil {
			return classifyCodexWSDownstreamReadError(err)
		}
		pending, err := newCodexWSPendingFrame(messageType, encoded)
		if err != nil {
			return err
		}
		mergeRouteDiagnostics(serveCtx, pending.diagnostics)
		emitAcceptedCodexWSFrameObservation(serveCtx, pending.diagnostics)
		err = broker.serveFrame(serveCtx, downstream, pending, &active)
		pending.Release()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if serveCtx.Err() != nil && errors.Is(err, context.Canceled) {
				return classifyCodexWSDownstreamReadError(downstreamReader.terminalError())
			}
			return err
		}
	}
}

func startCodexWSDownstreamReader(ctx context.Context, cancel context.CancelFunc, conn websocketRelayConn) *codexWSDownstreamReader {
	frames := make(chan codexWSUpstreamRead, 1)
	terminal := make(chan error, 1)
	done := make(chan struct{})
	reader := &codexWSDownstreamReader{conn: conn, cancel: cancel, frames: frames, terminal: terminal, done: done}
	go func() {
		defer close(done)
		defer close(frames)
		defer func() {
			if recover() != nil {
				terminal <- errors.New("Codex downstream WebSocket read failed")
				cancel()
			}
		}()
		for {
			messageType, payload, err := readCodexWSMessage(ctx, conn)
			if err != nil {
				terminal <- err
				cancel()
				return
			}
			select {
			case frames <- codexWSUpstreamRead{messageType: messageType, payload: payload}:
			case <-ctx.Done():
				clearBytes(payload)
				return
			}
		}
	}()
	return reader
}

func (reader *codexWSDownstreamReader) read(parent, ctx context.Context) (int, []byte, error) {
	if reader == nil {
		return 0, nil, ErrCodexLeaseWriterUnavailable
	}
	select {
	case frame, ok := <-reader.frames:
		if ok {
			return frame.messageType, frame.payload, nil
		}
		return 0, nil, reader.terminalError()
	case <-ctx.Done():
		if parent != nil && parent.Err() != nil {
			return 0, nil, parent.Err()
		}
		return 0, nil, reader.terminalError()
	}
}

func (reader *codexWSDownstreamReader) terminalError() error {
	if reader == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	select {
	case err := <-reader.terminal:
		return err
	default:
		return context.Canceled
	}
}

func (reader *codexWSDownstreamReader) close() {
	if reader == nil {
		return
	}
	reader.cancel()
	_ = reader.conn.SetReadDeadline(time.Now())
	<-reader.done
	for frame := range reader.frames {
		clearBytes(frame.payload)
	}
}

func classifyCodexWSDownstreamReadError(err error) error {
	var closeErr *websocket.CloseError
	if errors.Is(err, io.EOF) || errors.As(err, &closeErr) {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrCodexWSInvalidFrame) {
		return err
	}
	return newCodexWebSocketBrokerError(codexWebSocketFailureStageDownstreamRead, codexWebSocketFailureReasonDownstreamReadFailed)
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
	for {
		select {
		case result, ok := <-active.readFrames:
			if !ok || result.err != nil {
				return newCodexWebSocketBrokerError(codexWebSocketFailureStageUpstreamIdle, codexWebSocketFailureReasonUpstreamClosed)
			}
			if !codexWSIdleMaintenanceFrame(result) {
				return ErrCodexWSInvalidFrame
			}
		default:
			return nil
		}
	}
}

func codexWSIdleMaintenanceFrame(result codexWSUpstreamRead) bool {
	if result.messageType != websocket.TextMessage {
		return false
	}
	observation := classifyCodexSSEData(result.payload)
	switch observation.Type {
	case "codex.rate_limits":
		if observation.Kind != CodexSSERateLimits {
			return false
		}
		_, err := ParseCodexRateLimitEvent(result.payload)
		return err == nil
	case "keepalive", "responsesapi.websocket_timing":
		return observation.Kind == CodexSSEIgnored
	default:
		return false
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
		if pending != nil && !pending.portable {
			return writeCodexWSAccountUnavailableClose(downstream)
		}
	}
	if idleErr := codexWSIdleUpstreamError(active); idleErr != nil {
		closeCodexWSActiveUpstream(active)
		if codexWSIdleUpstreamClosed(idleErr) && pending != nil && !pending.portable {
			return writeCodexWSAccountUnavailableClose(downstream)
		}
		if !codexWSIdleUpstreamClosed(idleErr) {
			return idleErr
		}
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
	accounts := codexWSDispatchAccounts(prepared.Dispatch)
	if len(accounts) == 0 {
		return codexWSAbandonPrepared(ctx, prepared.Lifecycle, prepared.Dispatch.TerminalError())
	}
	resetAvailable := len(prepared.Dispatch.AccountUnavailableResetCandidates()) != 0
	if idleErr := codexWSIdleUpstreamError(active); idleErr != nil {
		closeCodexWSActiveUpstream(active)
		if !codexWSIdleUpstreamClosed(idleErr) {
			return codexWSAbandonPreparedHandle(ctx, prepared.leaseHandle, idleErr)
		}
		if !pending.portable {
			if err := codexWSAbandonPreparedHandle(ctx, prepared.leaseHandle, nil); err != nil {
				return err
			}
			return writeCodexWSAccountUnavailableClose(downstream)
		}
	}
	accountIndex := 0
	for {
		account, refreshErr := broker.prepareAccount(ctx, accounts[accountIndex])
		if refreshErr != nil {
			replacementSlot := uint32(0)
			if pending.portable && accountIndex+1 < len(accounts) {
				replacementSlot, err = codexWSReplacementSlot(prepared.leaseHandle, accounts[accountIndex+1].Choice().AccountKey)
				if err != nil {
					return err
				}
			}
			next, unavailableErr := prepared.leaseHandle.RecordAccountUnavailableContext(ctx, replacementSlot)
			if next != nil {
				prepared.leaseHandle = next
			}
			if unavailableErr != nil {
				return errors.Join(refreshErr, unavailableErr)
			}
			if replacementSlot != 0 {
				accountIndex++
				continue
			}
			prepared.receipt.terminal(CodexTurnReceiptRejected)
			if !pending.portable && resetAvailable {
				closeCodexWSActiveUpstream(active)
				return writeCodexWSAccountUnavailableClose(downstream)
			}
			return refreshErr
		}
		dial := broker.connect(ctx, prepared.leaseHandle, prepared.receipt, account, active, requestFrame.request.PreviousResponseID != "")
		if dial.lifecycle == nil {
			if dial.err != nil {
				return codexWSAbandonPreparedHandle(ctx, prepared.leaseHandle, dial.err)
			}
			return codexWSAbandonPreparedHandle(ctx, prepared.leaseHandle, ErrCodexLeaseWriterUnavailable)
		}
		lifecycle := dial.lifecycle
		rotated, err := broker.serveDispatchedFrame(ctx, downstream, requestFrame, lifecycle, accounts, &accountIndex, resetAvailable, active, dial)
		if err != nil {
			return err
		}
		if !rotated {
			return nil
		}
		prepared.leaseHandle = lifecycle.handle
	}
}

func codexWSIdleUpstreamClosed(err error) bool {
	var brokerErr *codexWebSocketBrokerError
	return errors.As(err, &brokerErr) &&
		brokerErr.failure.Stage == codexWebSocketFailureStageUpstreamIdle &&
		brokerErr.failure.Reason == codexWebSocketFailureReasonUpstreamClosed
}

func codexWSDispatchAccounts(plan CodexFrozenDispatchPlan) []CodexFrozenDispatchAccount {
	return append(plan.Accounts(), plan.AccountUnavailableFallbacks()...)
}

func codexWSReplacementSlot(handle *CodexLeaseRequestHandle, account codex.AccountKey) (uint32, error) {
	if handle == nil || account == "" {
		return 0, ErrCodexLeaseWriterUnavailable
	}
	for slot, frozenAccount := range handle.slotAccounts {
		if frozenAccount != account {
			continue
		}
		candidate := uint32(slot + 1)
		used := false
		for _, attempt := range handle.record.Attempts {
			if attempt.Slot == candidate {
				used = true
				break
			}
		}
		if !used {
			return candidate, nil
		}
	}
	return 0, ErrCodexLeaseAuthorityMismatch
}

func (broker *codexTerminatingWSBroker) prepareAccount(ctx context.Context, account CodexFrozenDispatchAccount) (CodexFrozenDispatchAccount, error) {
	if len(account.Attempts()) != 0 {
		return account, nil
	}
	refreshed, err := broker.refreshAccountAttempt(ctx, account, 1)
	if err != nil {
		return account, err
	}
	account.attempts = []CandidateAttempt{refreshed}
	return account, nil
}

func (broker *codexTerminatingWSBroker) refreshAccountAttempt(ctx context.Context, account CodexFrozenDispatchAccount, ordinal int) (CandidateAttempt, error) {
	refresh, ok := account.RefreshAttempt()
	if !ok {
		return CandidateAttempt{}, errors.New("Codex WebSocket account has no refreshable credential")
	}
	if broker == nil || broker.config.Refresher == nil {
		return CandidateAttempt{}, codex.ErrRefreshUnavailable
	}
	ref, revision, err := broker.config.Refresher.RefreshReference(ctx, refresh.Candidate, refresh.Revision)
	if err != nil {
		return CandidateAttempt{}, err
	}
	refreshed, err := candidateAttemptWithRefreshedRevision(refresh, ref, revision)
	if err != nil {
		return CandidateAttempt{}, err
	}
	refreshed.Ordinal = ordinal
	return refreshed, nil
}

func writeCodexWSAccountUnavailableClose(downstream websocketRelayConn) error {
	if downstream == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if err := downstream.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseServiceRestart, "account unavailable"),
		time.Now().Add(codexWSControlWriteTimeout),
	); err != nil {
		return fmt.Errorf("Codex downstream WebSocket resynchronisation failed")
	}
	return nil
}

func (broker *codexTerminatingWSBroker) serveDispatchedFrame(ctx context.Context, downstream websocketRelayConn, requestFrame *codexWSPendingFrame, lifecycle *codexWSLifecycle, accounts []CodexFrozenDispatchAccount, accountIndex *int, resetAvailable bool, active *codexWSActiveUpstream, dial codexWSDialResult) (rotated bool, err error) {
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
		return broker.finishHandshakeFailure(ctx, downstream, requestFrame, lifecycle, accounts, accountIndex, resetAvailable, active, dial)
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
		indeterminateErr := codexWSMarkIndeterminate(ctx, lifecycle, active.generation)
		if ctx.Err() != nil {
			return false, errors.Join(ctx.Err(), indeterminateErr)
		}
		if indeterminateErr != nil {
			return false, indeterminateErr
		}
		return false, newCodexWebSocketBrokerError(codexWebSocketFailureStageUpstreamRead, codexWebSocketFailureReasonUpstreamOutcomeIndeterminate)
	}
	return broker.readUpstreamRequest(ctx, downstream, requestFrame, lifecycle, accounts, accountIndex, resetAvailable, active)
}

func codexWSMarkIndeterminate(ctx context.Context, lifecycle *codexWSLifecycle, upstreamGeneration uint64) error {
	if lifecycle == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	err := lifecycle.Indeterminate(context.WithoutCancel(ctx), upstreamGeneration)
	if errors.Is(err, ErrCodexWSInvalidFrame) {
		return nil
	}
	return err
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
	accounts := codexWSDispatchAccounts(dispatch)
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
	for accountIndex, account := range accounts {
		account, refreshErr := broker.prepareAccount(ctx, account)
		if refreshErr != nil {
			if accountIndex+1 < len(accounts) {
				continue
			}
			broker.cancelActivePrewarm(active)
			return refreshErr
		}
		dial := broker.connectPrewarm(ctx, account, active)
		if active.conn == nil {
			if dial.wrapped.HardUsageLimit {
				broker.observeHardLimit(account, dial.response)
			}
			canRotate := accountIndex+1 < len(accounts) && (dial.wrapped.HardUsageLimit || dial.wrapped.AuthFailure)
			if canRotate {
				continue
			}
			broker.cancelActivePrewarm(active)
			frame, status := canonicalCodexWSHandshakeError(dial.response, dial.wrapped, dial.body)
			if err := writeCodexWSMessage(ctx, downstream, websocket.TextMessage, frame); err != nil {
				return fmt.Errorf("Codex downstream WebSocket write failed")
			}
			reportCodexWSHandshakeFailure(ctx, dial.response, dial.wrapped)
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
		prewarmCtx, cancelPrewarm := context.WithTimeout(ctx, broker.config.PrewarmTimeout)
		rotate, err := broker.readPrewarmResponse(prewarmCtx, ctx, downstream, planner, account, accountIndex+1 < len(accounts), active)
		cancelPrewarm()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			noteCodexObservation(ctx, codexObservationFields{Decision: "broker_failed", Reason: "timeout"})
			fmt.Fprintln(os.Stderr, "cq: Codex route trace transport=websocket event=broker_failed stage=upstream_read reason=timeout request_kind=prewarm handled=true")
			frame, _ := canonicalCodexWSHandshakeError(&http.Response{StatusCode: http.StatusGatewayTimeout}, CodexWrappedError{}, nil)
			if writeErr := writeCodexWSMessage(ctx, downstream, websocket.TextMessage, frame); writeErr != nil {
				return fmt.Errorf("Codex downstream WebSocket write failed")
			}
			return nil
		}
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
	if codexWSActiveUpstreamMatchesFirstAttempt(active, account) {
		return codexWSDialResult{response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}}
	}
	if active.conn != nil {
		reservation := active.prewarm
		closeCodexWSActiveUpstream(active)
		active.prewarm = reservation
	}
	var last codexWSDialResult
	attempts := account.Attempts()
	refreshConsidered := false
	for attemptIndex := 0; attemptIndex < len(attempts); attemptIndex++ {
		attempt := attempts[attemptIndex]
		broker.upstreamGeneration++
		generation := broker.upstreamGeneration
		conn, response, body, actual, dialErr := broker.config.Upstream.Dial(ctx, choice, attempt, broker.config.UpstreamURL, broker.config.Headers, nil)
		if dialErr == nil && conn != nil {
			active.conn = conn
			active.account = choice.AccountKey
			active.attempt = actual
			active.generation = generation
			return codexWSDialResult{response: response}
		}
		wrapped, providerBody, parseErr := codexWSDialError(response, body)
		last = codexWSDialResult{wrapped: wrapped, response: response, body: providerBody, err: errors.Join(dialErr, parseErr)}
		if wrapped.AuthFailure && attemptIndex+1 == len(attempts) && !refreshConsidered {
			refreshConsidered = true
			refreshed, refreshErr := broker.refreshAccountAttempt(ctx, account, len(attempts)+1)
			if refreshErr == nil {
				attempts = append(attempts, refreshed)
			} else {
				last.err = errors.Join(last.err, refreshErr)
			}
		}
		if !wrapped.AuthFailure {
			break
		}
	}
	return last
}

func (broker *codexTerminatingWSBroker) readPrewarmResponse(ctx, activeCtx context.Context, downstream websocketRelayConn, planner codexWebSocketPrewarmPlanner, account CodexFrozenDispatchAccount, canRotate bool, active *codexWSActiveUpstream) (bool, error) {
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
			return false, newCodexWSInvalidFrameErrorWithDetail(codexWSInvalidFrameUpstreamPrewarm, messageType, frame, codexWSInvalidFrameNonText, nil)
		}
		observation := classifyCodexSSEData(frame)
		if observation.Kind == CodexSSEError && observation.Error.HardUsageLimit {
			broker.observeHardLimit(account, nil)
		}
		if observation.Kind == CodexSSEError && !relayed && canRotate && (observation.Error.HardUsageLimit || observation.Error.AuthFailure) {
			closeCodexWSActiveUpstream(active)
			return true, nil
		}
		if observation.Kind == CodexSSEMalformed || observation.Kind == CodexSSEUnknown {
			broker.cancelActivePrewarm(active)
			closeCodexWSActiveUpstream(active)
			detail := codexWSInvalidFrameMalformedEvent
			if observation.Kind == CodexSSEUnknown {
				detail = codexWSInvalidFrameUnknownEvent
			}
			return false, newCodexWSInvalidFrameErrorWithDetail(codexWSInvalidFrameUpstreamPrewarm, messageType, frame, detail, observation.ParseError)
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
			startCodexWSUpstreamReader(activeCtx, active)
			broker.startIdleUpstreamKeepalive(activeCtx, active)
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

func (broker *codexTerminatingWSBroker) connect(ctx context.Context, handle *CodexLeaseRequestHandle, receipt *codexTurnReceiptHandle, account CodexFrozenDispatchAccount, active *codexWSActiveUpstream, anchored bool) codexWSDialResult {
	choice := account.Choice()
	if codexWSActiveUpstreamMatchesFirstAttempt(active, account) || (anchored && codexWSActiveUpstreamMatchesAnchoredCandidate(active, account)) {
		marked, err := handle.MarkDispatchedContext(ctx)
		if err != nil {
			return codexWSDialResult{err: err}
		}
		receipt.attempt(choice.AccountKey)
		lifecycle, err := newCodexWSLifecycle(marked, broker.config.DownstreamGeneration, active.generation, receipt)
		return codexWSDialResult{lifecycle: lifecycle, response: &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header)}, err: err}
	}
	if active.conn != nil {
		closeCodexWSActiveUpstream(active)
	}
	var last codexWSDialResult
	current := handle
	attempts := account.Attempts()
	refreshConsidered := false
	for attemptIndex := 0; attemptIndex < len(attempts); attemptIndex++ {
		attempt := attempts[attemptIndex]
		marked, err := current.MarkDispatchedContext(ctx)
		if err != nil {
			return codexWSDialResult{err: err}
		}
		broker.upstreamGeneration++
		generation := broker.upstreamGeneration
		lifecycle, lifecycleErr := newCodexWSLifecycle(marked, broker.config.DownstreamGeneration, generation, receipt)
		if lifecycleErr != nil {
			return codexWSDialResult{err: lifecycleErr}
		}
		conn, response, body, actual, dialErr := broker.config.Upstream.Dial(ctx, choice, attempt, broker.config.UpstreamURL, broker.config.Headers, func(actual CandidateAttempt) {
			receipt.attempt(actual.AccountKey)
		})
		if dialErr == nil && conn != nil {
			*active = codexWSActiveUpstream{conn: conn, account: choice.AccountKey, attempt: actual, generation: generation}
			return codexWSDialResult{lifecycle: lifecycle, response: response}
		}
		wrapped, providerBody, parseErr := codexWSDialError(response, body)
		last = codexWSDialResult{lifecycle: lifecycle, wrapped: wrapped, response: response, body: providerBody, err: errors.Join(dialErr, parseErr)}
		if wrapped.AuthFailure && attemptIndex+1 == len(attempts) && !refreshConsidered {
			refreshConsidered = true
			refreshed, refreshErr := broker.refreshAccountAttempt(ctx, account, len(attempts)+1)
			if refreshErr == nil {
				attempts = append(attempts, refreshed)
			} else {
				last.err = errors.Join(last.err, refreshErr)
			}
		}
		if !wrapped.AuthFailure || attemptIndex+1 == len(attempts) {
			break
		}
		nextSlot, replacementErr := lifecycle.replacementSlot(choice.AccountKey)
		if replacementErr != nil {
			last.err = errors.Join(last.err, replacementErr)
			break
		}
		if replacementErr = lifecycle.RejectAndPrepare(ctx, generation, nextSlot); replacementErr != nil {
			last.err = errors.Join(last.err, replacementErr)
			break
		}
		current = lifecycle.handle
	}
	return last
}
func (broker *codexTerminatingWSBroker) readUpstreamRequest(ctx context.Context, downstream websocketRelayConn, pending *codexWSPendingFrame, lifecycle *codexWSLifecycle, accounts []CodexFrozenDispatchAccount, accountIndex *int, resetAvailable bool, active *codexWSActiveUpstream) (bool, error) {
	for {
		messageType, frame, err := readCodexWSActiveMessage(ctx, active)
		if err != nil {
			indeterminateErr := codexWSMarkIndeterminate(ctx, lifecycle, active.generation)
			if ctx.Err() != nil {
				return false, errors.Join(ctx.Err(), indeterminateErr)
			}
			if indeterminateErr != nil {
				return false, indeterminateErr
			}
			return false, newCodexWebSocketBrokerError(codexWebSocketFailureStageUpstreamRead, codexWebSocketFailureReasonUpstreamOutcomeIndeterminate)
		}
		if messageType != websocket.TextMessage {
			_ = lifecycle.Indeterminate(ctx, active.generation)
			return false, newCodexWSInvalidFrameErrorWithDetail(codexWSInvalidFrameUpstreamResponse, messageType, frame, codexWSInvalidFrameNonText, nil)
		}
		result, observeErr := lifecycle.ObserveFrame(ctx, active.generation, frame)
		if observeErr != nil {
			return false, observeErr
		}
		accountUnavailable := result.HardUsageLimit || result.AuthFailure
		if accountUnavailable {
			if result.HardUsageLimit {
				broker.observeHardLimit(accounts[*accountIndex], nil)
			}
			if result.DefinitePreAdmissionRejection && result.AuthFailure {
				retryAccount, retryAvailable := broker.applicationAuthRetryAccount(ctx, accounts[*accountIndex], active.attempt)
				if retryAvailable {
					nextSlot, retryErr := lifecycle.replacementSlot(accounts[*accountIndex].Choice().AccountKey)
					if retryErr != nil {
						return false, retryErr
					}
					generation := active.generation
					closeCodexWSActiveUpstream(active)
					if retryErr = lifecycle.RejectAndPrepare(ctx, generation, nextSlot); retryErr != nil {
						return false, retryErr
					}
					dial := broker.connect(ctx, lifecycle.handle, lifecycle.receipt, retryAccount, active, false)
					if dial.lifecycle == nil {
						cause := dial.err
						if cause == nil {
							cause = ErrCodexLeaseWriterUnavailable
						}
						abandoned, abandonErr := lifecycle.handle.AbandonBeforeDispatchContext(context.WithoutCancel(ctx))
						if abandoned != nil {
							lifecycle.handle = abandoned
						}
						return false, errors.Join(cause, abandonErr)
					}
					*lifecycle = *dial.lifecycle
					return broker.serveDispatchedFrame(ctx, downstream, pending, lifecycle, accounts, accountIndex, resetAvailable, active, dial)
				}
			}
			hasFallback := *accountIndex+1 < len(accounts)
			replacementSlot := uint32(0)
			if !lifecycle.attemptAdmitted && result.DefinitePreAdmissionRejection && pending.portable && hasFallback {
				replacementSlot, observeErr = lifecycle.replacementSlot(accounts[*accountIndex+1].Choice().AccountKey)
				if observeErr != nil {
					return false, observeErr
				}
			}
			var unavailableErr error
			if result.HardUsageLimit {
				unavailableErr = lifecycle.RecordQuotaExhausted(ctx, active.generation, replacementSlot)
			} else {
				unavailableErr = lifecycle.RecordAccountUnavailable(ctx, active.generation, replacementSlot)
			}
			if unavailableErr != nil {
				return false, unavailableErr
			}
			closeCodexWSActiveUpstream(active)
			if lifecycle.attemptAdmitted {
				if result.AuthFailure && !resetAvailable {
					if err := lifecycle.CompleteAccountUnavailableCycle(ctx, lifecycle.upstreamGeneration); err != nil {
						return false, err
					}
				}
				if err := writeCodexWSMessage(ctx, downstream, messageType, frame); err != nil {
					return false, fmt.Errorf("Codex downstream WebSocket write failed")
				}
				return false, nil
			}
			if replacementSlot != 0 {
				*accountIndex++
				return true, nil
			}
			if hasFallback || (!pending.portable && resetAvailable) {
				return false, writeCodexWSAccountUnavailableClose(downstream)
			}
			if result.AuthFailure && lifecycle.turnAdmitted && !resetAvailable {
				if err := lifecycle.CompleteAccountUnavailableCycle(ctx, lifecycle.upstreamGeneration); err != nil {
					return false, err
				}
			}
			if err := writeCodexWSMessage(ctx, downstream, messageType, frame); err != nil {
				return false, fmt.Errorf("Codex downstream WebSocket write failed")
			}
			return false, nil
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

func (broker *codexTerminatingWSBroker) applicationAuthRetryAccount(ctx context.Context, account CodexFrozenDispatchAccount, current CandidateAttempt) (CodexFrozenDispatchAccount, bool) {
	attempts := account.Attempts()
	currentIndex := -1
	for index, attempt := range attempts {
		if attempt == current {
			currentIndex = index
			break
		}
	}
	if currentIndex < 0 {
		return CodexFrozenDispatchAccount{}, false
	}
	if currentIndex+1 < len(attempts) {
		account.attempts = append([]CandidateAttempt(nil), attempts[currentIndex+1:]...)
		return account, true
	}
	if _, ok := account.RefreshAttempt(); !ok {
		return CodexFrozenDispatchAccount{}, false
	}
	refreshed, err := broker.refreshAccountAttempt(ctx, account, len(attempts)+1)
	if err != nil {
		return CodexFrozenDispatchAccount{}, false
	}
	account.attempts = []CandidateAttempt{refreshed}
	account.refreshAttempt = nil
	return account, true
}

func (broker *codexTerminatingWSBroker) finishHandshakeFailure(ctx context.Context, downstream websocketRelayConn, pending *codexWSPendingFrame, lifecycle *codexWSLifecycle, accounts []CodexFrozenDispatchAccount, accountIndex *int, resetAvailable bool, active *codexWSActiveUpstream, dial codexWSDialResult) (bool, error) {
	if lifecycle == nil || accountIndex == nil {
		return false, ErrCodexLeaseWriterUnavailable
	}
	accountUnavailable := dial.wrapped.HardUsageLimit || dial.wrapped.AuthFailure
	if accountUnavailable {
		if dial.wrapped.HardUsageLimit {
			broker.observeHardLimit(accounts[*accountIndex], dial.response)
		}
		replacementSlot := uint32(0)
		if pending.portable && *accountIndex+1 < len(accounts) {
			var err error
			replacementSlot, err = lifecycle.replacementSlot(accounts[*accountIndex+1].Choice().AccountKey)
			if err != nil {
				return false, err
			}
		}
		var unavailableErr error
		if dial.wrapped.HardUsageLimit {
			unavailableErr = lifecycle.RecordQuotaExhausted(ctx, lifecycle.upstreamGeneration, replacementSlot)
		} else {
			unavailableErr = lifecycle.RecordAccountUnavailable(ctx, lifecycle.upstreamGeneration, replacementSlot)
		}
		if unavailableErr != nil {
			return false, unavailableErr
		}
		if replacementSlot != 0 {
			*accountIndex++
			return true, nil
		}
		if !pending.portable && resetAvailable {
			return false, writeCodexWSAccountUnavailableClose(downstream)
		}
	} else if dial.wrapped.Found || dial.response != nil {
		if err := lifecycle.FinishRejected(lifecycle.upstreamGeneration); err != nil {
			return false, err
		}
	} else if err := lifecycle.Indeterminate(ctx, lifecycle.upstreamGeneration); err != nil {
		return false, err
	}
	if dial.wrapped.AuthFailure && lifecycle.turnAdmitted && !resetAvailable {
		if err := lifecycle.CompleteAccountUnavailableCycle(ctx, lifecycle.upstreamGeneration); err != nil {
			return false, err
		}
	}
	frame, status := canonicalCodexWSHandshakeError(dial.response, dial.wrapped, dial.body)
	if err := writeCodexWSMessage(ctx, downstream, websocket.TextMessage, frame); err != nil {
		return false, fmt.Errorf("Codex downstream WebSocket write failed")
	}
	reportCodexWSHandshakeFailure(ctx, dial.response, dial.wrapped)
	_ = downstream.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, http.StatusText(status)))
	if active.conn != nil {
		closeCodexWSActiveUpstream(active)
	}
	return false, nil
}

func (broker *codexTerminatingWSBroker) observeHardLimit(account CodexFrozenDispatchAccount, response *http.Response) {
	if broker == nil || broker.config.Capacity == nil {
		return
	}
	choice := account.Choice()
	model := choice.EffectiveModel
	if model == "" {
		model = choice.RequestedModel
	}
	producer := newCodexRateLimitProducer(
		broker.config.Capacity,
		broker.config.Capacity.NewObservationStream(),
		choice.AccountKey,
		broker.config.Capacity.now,
		true,
	)
	if producer == nil {
		return
	}
	resetAt := time.Time{}
	if response != nil {
		resetAt = retryAfterReset(producer.now(), response.Header.Get("Retry-After"))
	}
	producer.ObserveHardLimit(CapacityBucketForModel(model), resetAt)
}

func readCodexWSMessage(ctx context.Context, conn websocketRelayConn) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		_ = conn.SetReadDeadline(time.Now())
	})
	messageType, payload, err := conn.ReadMessage()
	if !stop() {
		<-cancelDone
	}
	if ctx.Err() != nil {
		clearBytes(payload)
		return 0, nil, ctx.Err()
	}
	return messageType, payload, err
}

func writeCodexWSMessage(ctx context.Context, conn websocketRelayConn, messageType int, payload []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		_ = conn.Close()
	})
	err := conn.WriteMessage(messageType, payload)
	if !stop() {
		<-cancelDone
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func codexWSDialError(response *http.Response, body []byte) (CodexWrappedError, []byte, error) {
	if response == nil {
		return CodexWrappedError{}, nil, nil
	}
	decoded, complete := decodeCodexErrorResponseBody(body, response.Header, response.Uncompressed)
	if !complete {
		if response.StatusCode == http.StatusUnauthorized {
			return CodexWrappedError{Found: true, Status: http.StatusUnauthorized, AuthFailure: true}, nil, nil
		}
		return CodexWrappedError{}, nil, nil
	}
	wrapped, err := parseCodexHTTPError(decoded, response.StatusCode)
	if err != nil {
		if response.StatusCode == http.StatusUnauthorized {
			return CodexWrappedError{Found: true, Status: http.StatusUnauthorized, AuthFailure: true}, nil, nil
		}
		return CodexWrappedError{}, nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		wrapped.Found = true
		wrapped.Status = http.StatusUnauthorized
		wrapped.AuthFailure = true
	}
	if response.StatusCode == http.StatusForbidden && wrapped.ErrorType != "authentication_error" {
		wrapped.AuthFailure = false
	}
	if !wrapped.Found {
		return wrapped, nil, nil
	}
	return wrapped, append([]byte(nil), decoded...), nil
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

func reportCodexWSHandshakeFailure(ctx context.Context, response *http.Response, wrapped CodexWrappedError) {
	status := 0
	reason := string(codexWebSocketFailureReasonUpstreamOutcomeIndeterminate)
	if response != nil && response.StatusCode >= 400 && response.StatusCode <= 599 {
		status = response.StatusCode
		reason = "upstream_rejected"
	}
	noteCodexObservation(ctx, codexObservationFields{Decision: "broker_failed", Reason: reason})
	fmt.Fprintf(
		os.Stderr,
		"cq: Codex route trace transport=websocket event=broker_failed stage=upstream_handshake reason=%s response_present=%t upstream_status=%d auth_failure=%t hard_limit=%t\n",
		reason,
		response != nil,
		status,
		wrapped.AuthFailure,
		wrapped.HardUsageLimit,
	)
}

func canonicalCodexWSHandshakeError(response *http.Response, wrapped CodexWrappedError, providerBody []byte) ([]byte, int) {
	status := http.StatusBadGateway
	if response != nil && response.StatusCode >= 400 && response.StatusCode <= 599 {
		status = response.StatusCode
	}
	if frame, ok := codexWSProviderHandshakeError(providerBody, status); ok {
		return frame, status
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

func codexWSProviderHandshakeError(providerBody []byte, status int) ([]byte, bool) {
	if len(providerBody) == 0 || len(providerBody) > codexAttemptResponseLimit {
		return nil, false
	}
	if event, err := ParseCodexWrappedError(providerBody); err == nil && event.Found && event.Status == status {
		return append([]byte(nil), providerBody...), true
	}
	parsed, err := parseCodexHTTPError(providerBody, status)
	if err != nil || !parsed.Found || parsed.Status != status {
		return nil, false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(providerBody, &envelope); err != nil || len(envelope["error"]) == 0 {
		return nil, false
	}
	if len(envelope["type"]) == 0 {
		envelope["type"] = json.RawMessage(`"error"`)
	}
	if len(envelope["status"]) == 0 && len(envelope["status_code"]) == 0 {
		envelope["status"] = json.RawMessage(strconv.Itoa(status))
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, false
	}
	return encoded, true
}
