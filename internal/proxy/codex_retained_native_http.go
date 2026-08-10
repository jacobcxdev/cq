package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// CodexRetainedNativeHTTPHandler claims only exact admitted authoritative
// turns from a retained epoch. Declined requests retain their exact body and
// never reach a route selector or durable mutation.
type CodexRetainedNativeHTTPHandler struct {
	planner CodexRetainedNativeHTTPRequestPlanner
	native  *CodexNativeHTTPHandler
}

func NewCodexRetainedNativeHTTPHandler(planner CodexRetainedNativeHTTPRequestPlanner, native *CodexNativeHTTPHandler) (*CodexRetainedNativeHTTPHandler, error) {
	if planner == nil || native == nil || native.planner == nil || native.session == nil {
		return nil, errors.New("Codex retained HTTP handler unavailable")
	}
	return &CodexRetainedNativeHTTPHandler{planner: planner, native: native}, nil
}

func (handler *CodexRetainedNativeHTTPHandler) TryServe(writer http.ResponseWriter, request *http.Request, compact bool) (bool, string) {
	if handler == nil || handler.planner == nil || handler.native == nil || writer == nil || request == nil {
		if writer != nil {
			writeError(writer, http.StatusServiceUnavailable, "api_error", "Codex retained HTTP routing unavailable")
		}
		return true, ""
	}

	buffered, complete, owner, originalGetBody, err := bufferCodexRetainedRequest(request)
	if err != nil {
		clearBytes(buffered)
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "failed to inspect request body")
		return true, ""
	}
	if !complete {
		restoreCodexRetainedRequest(request, buffered, owner, originalGetBody, false)
		return false, ""
	}

	probeEncoded := bytes.Clone(buffered)
	expected, claimed, probeErr := handler.planner.ProbeRetained(request.Context(), CodexHTTPRequestPlanInput{
		Encoded: probeEncoded,
		Headers: request.Header.Clone(),
	})
	clearBytes(probeEncoded)
	if !claimed {
		restoreCodexRetainedRequest(request, buffered, owner, originalGetBody, true)
		return false, ""
	}
	if probeErr != nil || expected == nil {
		clearBytes(buffered)
		_ = owner.Close()
		writeError(writer, http.StatusServiceUnavailable, "api_error", "Codex retained HTTP routing unavailable")
		return true, ""
	}
	if err := owner.Close(); err != nil {
		clearBytes(buffered)
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "failed to inspect request body")
		return true, ""
	}
	return handler.native.serveEncoded(writer, request, compact, buffered, expected)
}

type codexRetainedBodyOwner struct {
	body io.ReadCloser
	once sync.Once
	err  error
}

func (owner *codexRetainedBodyOwner) Read(target []byte) (int, error) {
	return owner.body.Read(target)
}

func (owner *codexRetainedBodyOwner) Close() error {
	owner.once.Do(func() { owner.err = owner.body.Close() })
	return owner.err
}

type codexRetainedReplayBody struct {
	reader io.Reader
	owner  *codexRetainedBodyOwner
}

func (body *codexRetainedReplayBody) Read(target []byte) (int, error) {
	return body.reader.Read(target)
}

func (body *codexRetainedReplayBody) Close() error {
	return body.owner.Close()
}

func bufferCodexRetainedRequest(request *http.Request) ([]byte, bool, *codexRetainedBodyOwner, func() (io.ReadCloser, error), error) {
	body := request.Body
	if body == nil {
		body = http.NoBody
	}
	owner := &codexRetainedBodyOwner{body: body}
	cancelClosed := make(chan struct{})
	stopCancelClose := context.AfterFunc(request.Context(), func() {
		_ = owner.Close()
		close(cancelClosed)
	})
	buffered, readErr := io.ReadAll(io.LimitReader(owner, maxRequestBody+1))
	if !stopCancelClose() {
		<-cancelClosed
	}
	if readErr != nil || request.Context().Err() != nil {
		_ = owner.Close()
		return buffered, false, owner, request.GetBody, errors.New("read Codex retained HTTP request")
	}
	return buffered, len(buffered) <= maxRequestBody, owner, request.GetBody, nil
}

func restoreCodexRetainedRequest(request *http.Request, buffered []byte, owner *codexRetainedBodyOwner, originalGetBody func() (io.ReadCloser, error), complete bool) {
	if complete {
		request.Body = &codexRetainedReplayBody{reader: bytes.NewReader(buffered), owner: owner}
		request.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(buffered)), nil
		}
		return
	}
	request.Body = &codexRetainedReplayBody{reader: io.MultiReader(bytes.NewReader(buffered), owner), owner: owner}
	request.GetBody = originalGetBody
}
