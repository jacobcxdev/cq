package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// CodexNativeHTTPRequestPlanner prepares one frozen native request and commits
// its initial durable attempt before returning ownership to the HTTP session.
type CodexNativeHTTPRequestPlanner interface {
	Build(context.Context, CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error)
}

// CodexRetainedNativeHTTPRequestPlanner can prove one exact retained binding
// without selecting a prospective route or mutating durable authority.
type CodexRetainedNativeHTTPRequestPlanner interface {
	CodexNativeHTTPRequestPlanner
	ProbeRetained(context.Context, CodexHTTPRequestPlanInput) (*CodexRetainedHTTPRequestClaim, bool, error)
}

// CodexNativeHTTPRequestSession executes one prepared request without
// inspecting, transforming, or selecting from live request state again.
type CodexNativeHTTPRequestSession interface {
	Do(
		context.Context,
		*http.Request,
		CodexFrozenDispatchPlan,
		*CodexFrozenRequest,
		CodexHTTPRequestLifecycle,
	) (CodexHTTPRequestSessionResult, error)
}

// CodexNativeHTTPRoutingHandler may claim one native HTTP request. A false
// result promises that request bytes and durable/shadow authority are untouched.
// This lets a later retained-fence handler decline unseen off/observe traffic.
type CodexNativeHTTPRoutingHandler interface {
	TryServe(http.ResponseWriter, *http.Request, bool) (handled bool, model string)
}

// CodexNativeHTTPHandler is the authoritative native Responses HTTP vertical.
// A Server uses it only when startup readiness selected HTTP enforcement.
type CodexNativeHTTPHandler struct {
	planner              CodexNativeHTTPRequestPlanner
	session              CodexNativeHTTPRequestSession
	upstream             url.URL
	requests             *codexNativeHTTPRequestGate
	planningContext      context.Context
	cancelPlanning       context.CancelFunc
	installedProbe       atomic.Pointer[codexInstalledHTTPGateProbe]
	reportPlanFailure    func(CodexHTTPRequestPlanFailure)
	reportSessionFailure func(codexNativeHTTPSessionFailure)
}

func (handler *CodexNativeHTTPHandler) installCodexInstalledHTTPGateProbe(probe *codexInstalledHTTPGateProbe) (func(), error) {
	if handler == nil || probe == nil || !handler.installedProbe.CompareAndSwap(nil, probe) {
		return nil, errCodexInstalledListenerAcceptance
	}
	var once sync.Once
	return func() {
		once.Do(func() { handler.installedProbe.CompareAndSwap(probe, nil) })
	}, nil
}

// NewCodexNativeHTTPHandler validates every dependency before the listener can
// expose the authoritative native HTTP path.
func NewCodexNativeHTTPHandler(planner CodexNativeHTTPRequestPlanner, session CodexNativeHTTPRequestSession, upstream string) (*CodexNativeHTTPHandler, error) {
	if planner == nil || session == nil {
		return nil, errors.New("Codex native HTTP handler unavailable")
	}
	parsed, err := url.Parse(strings.TrimSpace(upstream))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Codex native HTTP upstream is invalid")
	}
	planningContext, cancelPlanning := context.WithCancel(context.Background())
	return &CodexNativeHTTPHandler{
		planner: planner, session: session, upstream: *parsed,
		requests:             newCodexNativeHTTPRequestGate(),
		planningContext:      planningContext,
		cancelPlanning:       cancelPlanning,
		reportPlanFailure:    reportCodexNativeHTTPPlanFailure,
		reportSessionFailure: reportCodexNativeHTTPSessionFailure,
	}, nil
}

func reportCodexNativeHTTPPlanFailure(failure CodexHTTPRequestPlanFailure) {
	fmt.Fprintf(os.Stderr, "cq: Codex route trace transport=http event=plan_failed stage=%s reason=%s\n", failure.Stage, failure.Reason)
}

type codexNativeHTTPSessionFailure struct {
	stage     string
	reason    string
	roundTrip *codexHTTPRoundTripError
}

func classifyCodexNativeHTTPSessionFailure(err error) codexNativeHTTPSessionFailure {
	var roundTrip *codexHTTPRoundTripError
	if errors.As(err, &roundTrip) {
		return codexNativeHTTPSessionFailure{stage: "round_trip", reason: string(roundTrip.reason), roundTrip: roundTrip}
	}
	return codexNativeHTTPSessionFailure{stage: "session", reason: string(codexRequestFailureReason(err))}
}

func reportCodexNativeHTTPSessionFailure(failure codexNativeHTTPSessionFailure) {
	if failure.roundTrip == nil {
		fmt.Fprintf(os.Stderr, "cq: Codex route trace transport=http event=session_failed stage=%s reason=%s\n", failure.stage, failure.reason)
		return
	}
	facts := failure.roundTrip.facts
	fmt.Fprintf(
		os.Stderr,
		"cq: Codex route trace transport=http event=session_failed stage=%s reason=%s dispatched=%t got_conn=%t conn_reused=%t conn_was_idle=%t idle_ms=%d wrote_request=%t write_error=%t got_first_response_byte=%t\n",
		failure.stage,
		failure.reason,
		true,
		facts.GotConn,
		facts.ConnReused,
		facts.ConnWasIdle,
		facts.IdleMS,
		facts.WroteRequest,
		facts.WriteError,
		facts.GotFirstResponseByte,
	)
}

const codexHTTPRequestPlanUnknown CodexHTTPRequestPlanErrorCode = "unknown"

func safeCodexHTTPRequestPlanErrorCode(code CodexHTTPRequestPlanErrorCode) CodexHTTPRequestPlanErrorCode {
	switch code {
	case CodexHTTPRequestPlanUnavailable,
		CodexHTTPRequestPlanInspect,
		CodexHTTPRequestPlanInventory,
		CodexHTTPRequestPlanRouteSnapshot,
		CodexHTTPRequestPlanDispatch,
		CodexHTTPRequestPlanFreeze,
		CodexHTTPRequestPlanBegin:
		return code
	default:
		return codexHTTPRequestPlanUnknown
	}
}

// CloseAndDrain permanently closes native request admission and waits until
// every request admitted before the close has fully returned from TryServe.
func (handler *CodexNativeHTTPHandler) CloseAndDrain(ctx context.Context) error {
	if handler == nil {
		return errors.New("Codex native HTTP handler unavailable")
	}
	if ctx == nil {
		return errors.New("Codex native HTTP drain context unavailable")
	}
	drained := handler.requests.closeAdmission()
	if handler.cancelPlanning != nil {
		handler.cancelPlanning()
	}
	return waitForCodexNativeHTTPDrain(ctx, drained)
}

// TryServe is the enforcement implementation. It claims every request before
// reading it, so every return path reports handled and legacy routing cannot run.
func (handler *CodexNativeHTTPHandler) TryServe(writer http.ResponseWriter, request *http.Request, compact bool) (bool, string) {
	if handler == nil || handler.planner == nil || handler.session == nil || writer == nil || request == nil {
		if writer != nil {
			writeError(writer, http.StatusServiceUnavailable, "api_error", "Codex native HTTP routing unavailable")
		}
		return true, ""
	}
	release, admitted := handler.requests.enter()
	if !admitted {
		closeCodexNativeHTTPRejectedRequestBody(request.Body)
		writeError(writer, http.StatusServiceUnavailable, "api_error", "Codex native HTTP routing unavailable")
		return true, ""
	}
	defer release()
	path := codexInstalledHTTPProbeResponses
	if compact {
		path = codexInstalledHTTPProbeCompact
	}
	trace := handler.installedProbe.Load().begin(path)
	if trace != nil {
		request = request.Clone(withCodexInstalledHTTPTrace(request.Context(), trace))
		defer trace.finish()
	}
	encoded, err := readCodexNativeHTTPRequest(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return true, ""
	}

	return handler.serveEncoded(writer, request, compact, encoded, nil)
}

func (handler *CodexNativeHTTPHandler) serveEncoded(writer http.ResponseWriter, request *http.Request, compact bool, encoded []byte, claim *CodexRetainedHTTPRequestClaim) (bool, string) {
	trace := codexInstalledHTTPTraceFromContext(request.Context())
	prepared, err := func() (CodexPreparedHTTPRequest, error) {
		ctx, release := handler.requestPlanningContext(request.Context())
		defer release()
		input := CodexHTTPRequestPlanInput{Encoded: encoded, Headers: request.Header}
		if claim != nil {
			expected := claim.ExpectedBound
			input.ExpectedBound = &expected
			input.retainedPlanning = claim.planning
		}
		return handler.planner.Build(ctx, input)
	}()
	clearBytes(encoded)
	if err != nil {
		status := http.StatusServiceUnavailable
		errorType := "api_error"
		message := "Codex native HTTP routing unavailable"
		failure := CodexHTTPRequestPlanFailure{Stage: codexHTTPRequestPlanUnknown, Reason: CodexRequestFailureUnknown}
		var planErr *CodexHTTPRequestPlanError
		if errors.As(err, &planErr) {
			failure.Stage = safeCodexHTTPRequestPlanErrorCode(planErr.Code)
			failure.Reason = safeCodexRequestFailureReason(planErr.Reason)
			if planErr.Code == CodexHTTPRequestPlanInspect {
				status = http.StatusBadRequest
				errorType = "invalid_request_error"
				message = "invalid Codex Responses request"
			}
		}
		noteCodexObservation(request.Context(), codexObservationFields{Decision: "plan_failed", Reason: string(failure.Reason)})
		if handler.reportPlanFailure != nil {
			handler.reportPlanFailure(failure)
		}
		writeError(writer, status, errorType, message)
		return true, ""
	}

	model := ""
	if accounts := prepared.Dispatch.Accounts(); len(accounts) > 0 {
		model = accounts[0].Choice().EffectiveModel
	}
	template := handler.requestTemplate(request, compact)
	result, err := handler.session.Do(
		request.Context(),
		template,
		prepared.Dispatch,
		prepared.Frozen,
		prepared.Lifecycle,
	)
	if err != nil {
		failure := classifyCodexNativeHTTPSessionFailure(err)
		noteCodexObservation(request.Context(), codexObservationFields{Decision: "session_failed", Reason: failure.reason})
		if handler.reportSessionFailure != nil {
			handler.reportSessionFailure(failure)
		}
		writeError(writer, http.StatusBadGateway, "api_error", "Codex upstream request failed")
		return true, model
	}
	if result.Response == nil || result.Response.Body == nil {
		failure := codexNativeHTTPSessionFailure{stage: "response_validate", reason: "response_unavailable"}
		noteCodexObservation(request.Context(), codexObservationFields{Decision: "session_failed", Reason: failure.reason})
		if handler.reportSessionFailure != nil {
			handler.reportSessionFailure(failure)
		}
		writeError(writer, http.StatusBadGateway, "api_error", "Codex upstream response unavailable")
		return true, model
	}

	if result.Response.StatusCode < http.StatusOK || result.Response.StatusCode >= http.StatusMultipleChoices {
		defer closeCodexHTTPResponseBody(result.Response.Body)
		relayErr := relayCodexHTTPResponse(writer, result.Response, false)
		trace.relayedResponse(false, true, relayErr)
		return true, model
	}

	mode := codexHTTPResponseModeSSE
	if compact {
		mode = codexHTTPResponseModeCompact
	}
	relayErr := relayCodexAcceptedHTTPResponse(request.Context(), writer, result.Response, mode, result.Lifecycle)
	trace.relayedResponse(true, false, relayErr)
	return true, model
}

func (handler *CodexNativeHTTPHandler) requestPlanningContext(requestContext context.Context) (context.Context, func()) {
	if requestContext == nil {
		requestContext = context.Background()
	}
	ctx, cancel := context.WithCancel(requestContext)
	if handler == nil || handler.planningContext == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(handler.planningContext, cancel)
	if handler.planningContext.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

func (handler *CodexNativeHTTPHandler) requestTemplate(request *http.Request, compact bool) *http.Request {
	target := handler.upstream
	target.Path = strings.TrimRight(target.Path, "/") + "/responses"
	if compact {
		target.Path += "/compact"
	}
	target.RawPath = ""
	target.RawQuery = request.URL.RawQuery
	return &http.Request{
		Method: http.MethodPost,
		URL:    &target,
		Header: make(http.Header),
		Body:   http.NoBody,
	}
}

func readCodexNativeHTTPRequest(request *http.Request) ([]byte, error) {
	if request == nil || request.Body == nil {
		return nil, nil
	}
	bodyReader := request.Body
	var closeErr error
	var closeOnce sync.Once
	closeBody := func() {
		closeOnce.Do(func() {
			defer func() {
				if recover() != nil {
					closeErr = errors.New("close Codex native HTTP request body")
				}
			}()
			closeErr = bodyReader.Close()
		})
	}
	stopCancelClose := context.AfterFunc(request.Context(), closeBody)
	body, err := io.ReadAll(bodyReader)
	stopCancelClose()
	closeBody()
	if err != nil || closeErr != nil || request.Context().Err() != nil {
		clearBytes(body)
		return nil, errors.New("read Codex native HTTP request")
	}
	return body, nil
}
