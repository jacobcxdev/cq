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

	planningContext, releasePlanningContext := handler.native.requestPlanningContext(request.Context())
	defer releasePlanningContext()
	probeEncoded := bytes.Clone(buffered)
	claim, claimed, probeErr := handler.planner.ProbeRetained(planningContext, CodexHTTPRequestPlanInput{
		Encoded: probeEncoded,
		Headers: request.Header.Clone(),
	})
	clearBytes(probeEncoded)
	if !claimed {
		if claim != nil {
			claim.release()
		}
		restoreCodexRetainedRequest(request, buffered, owner, originalGetBody, true)
		return false, ""
	}
	defer claim.release()
	release, admitted := handler.native.requests.enter()
	if !admitted {
		clearBytes(buffered)
		_ = owner.Close()
		writeError(writer, http.StatusServiceUnavailable, "api_error", "Codex retained HTTP routing unavailable")
		return true, ""
	}
	defer release()
	if probeErr != nil || claim == nil {
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
	return handler.native.serveEncoded(writer, request, compact, buffered, claim)
}

func (handler *CodexRetainedNativeHTTPHandler) CloseAndDrain(ctx context.Context) error {
	if handler == nil || handler.native == nil {
		return errors.New("Codex retained HTTP handler unavailable")
	}
	return handler.native.CloseAndDrain(ctx)
}

type codexRetainedBodyOwner struct {
	body io.ReadCloser
	once sync.Once
	err  error
}

func (owner *codexRetainedBodyOwner) Read(target []byte) (int, error) {
	return readCodexHTTPResponseBody(owner.body, target)
}

func (owner *codexRetainedBodyOwner) Close() error {
	owner.once.Do(func() { owner.err = closeCodexHTTPResponseBody(owner.body) })
	return owner.err
}

type codexRetainedReplayBody struct {
	reader    io.Reader
	owner     *codexRetainedBodyOwner
	state     *codexRetainedReplayState
	root      bool
	closeOnce sync.Once
	closeErr  error
}

func (body *codexRetainedReplayBody) Read(target []byte) (int, error) {
	return body.reader.Read(target)
}

func (body *codexRetainedReplayBody) Close() error {
	body.closeOnce.Do(func() {
		if body.owner != nil {
			body.closeErr = body.owner.Close()
		}
		if body.state != nil {
			body.state.release(body.root)
		}
	})
	return body.closeErr
}

type codexRetainedReplayState struct {
	mu              sync.Mutex
	buffered        []byte
	refs            int
	rootOpen        bool
	request         *http.Request
	originalGetBody func() (io.ReadCloser, error)
}

func newCodexRetainedReplayState(buffered []byte) *codexRetainedReplayState {
	return &codexRetainedReplayState{buffered: buffered, refs: 1, rootOpen: true}
}

func (state *codexRetainedReplayState) read(offset *int, target []byte) (int, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if *offset >= len(state.buffered) {
		return 0, io.EOF
	}
	n := copy(target, state.buffered[*offset:])
	*offset += n
	return n, nil
}

func (state *codexRetainedReplayState) getBody() (io.ReadCloser, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.rootOpen || state.buffered == nil {
		return nil, errors.New("Codex retained HTTP replay unavailable")
	}
	state.refs++
	return &codexRetainedReplayBody{
		reader: &codexRetainedReplayReader{state: state},
		state:  state,
	}, nil
}

func (state *codexRetainedReplayState) release(root bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if root {
		if !state.rootOpen {
			return
		}
		state.rootOpen = false
		if state.request != nil {
			state.request.GetBody = state.originalGetBody
			state.request = nil
			state.originalGetBody = nil
		}
	}
	if state.refs > 0 {
		state.refs--
	}
	if state.refs == 0 {
		clearBytes(state.buffered)
		state.buffered = nil
	}
}

type codexRetainedReplayReader struct {
	state  *codexRetainedReplayState
	offset int
}

func (reader *codexRetainedReplayReader) Read(target []byte) (int, error) {
	return reader.state.read(&reader.offset, target)
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
	state := newCodexRetainedReplayState(buffered)
	prefix := &codexRetainedReplayReader{state: state}
	if complete {
		state.request = request
		state.originalGetBody = originalGetBody
		request.Body = &codexRetainedReplayBody{reader: prefix, owner: owner, state: state, root: true}
		request.GetBody = state.getBody
		return
	}
	request.Body = &codexRetainedReplayBody{reader: io.MultiReader(prefix, owner), owner: owner, state: state, root: true}
	request.GetBody = originalGetBody
}
