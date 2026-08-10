package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// CodexNativeHTTPRequestPlanner prepares one frozen native request and commits
// its initial durable attempt before returning ownership to the HTTP session.
type CodexNativeHTTPRequestPlanner interface {
	Build(context.Context, CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error)
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
	planner  CodexNativeHTTPRequestPlanner
	session  CodexNativeHTTPRequestSession
	upstream url.URL
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
	return &CodexNativeHTTPHandler{planner: planner, session: session, upstream: *parsed}, nil
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
	encoded, err := readCodexNativeHTTPRequest(request)
	if err != nil {
		status := http.StatusBadRequest
		message := "failed to read request body"
		if errors.Is(err, errCodexNativeHTTPRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
			message = "request body exceeds 10 MiB"
		}
		writeError(writer, status, "invalid_request_error", message)
		return true, ""
	}

	prepared, err := handler.planner.Build(request.Context(), CodexHTTPRequestPlanInput{
		Encoded: encoded,
		Headers: request.Header,
	})
	clearBytes(encoded)
	if err != nil {
		status := http.StatusServiceUnavailable
		errorType := "api_error"
		message := "Codex native HTTP routing unavailable"
		var planErr *CodexHTTPRequestPlanError
		if errors.As(err, &planErr) && planErr.Code == CodexHTTPRequestPlanInspect {
			status = http.StatusBadRequest
			errorType = "invalid_request_error"
			message = "invalid Codex Responses request"
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
		writeError(writer, http.StatusBadGateway, "api_error", "Codex upstream request failed")
		return true, model
	}
	if result.Response == nil || result.Response.Body == nil {
		writeError(writer, http.StatusBadGateway, "api_error", "Codex upstream response unavailable")
		return true, model
	}

	if result.Response.StatusCode < http.StatusOK || result.Response.StatusCode >= http.StatusMultipleChoices {
		defer result.Response.Body.Close()
		_ = relayCodexHTTPResponse(writer, result.Response, false)
		return true, model
	}

	mode := codexHTTPResponseModeSSE
	if compact {
		mode = codexHTTPResponseModeCompact
	}
	_ = relayCodexAcceptedHTTPResponse(request.Context(), writer, result.Response, mode, result.Lifecycle)
	return true, model
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

var errCodexNativeHTTPRequestTooLarge = errors.New("Codex native HTTP request exceeds limit")

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
	body, err := io.ReadAll(io.LimitReader(bodyReader, maxRequestBody+1))
	stopCancelClose()
	closeBody()
	if err != nil || closeErr != nil || request.Context().Err() != nil {
		clearBytes(body)
		return nil, errors.New("read Codex native HTTP request")
	}
	if len(body) > maxRequestBody {
		clearBytes(body)
		return nil, errCodexNativeHTTPRequestTooLarge
	}
	return body, nil
}
