package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
)

const maxCodexRejectedResponseBytes = 1 << 20

// CodexRejectedResponseErrorCode classifies credential-free response ownership failures.
type CodexRejectedResponseErrorCode string

const (
	CodexRejectedResponseInvalidResponse   CodexRejectedResponseErrorCode = "invalid_response"
	CodexRejectedResponseBodyReadFailed    CodexRejectedResponseErrorCode = "body_read_failed"
	CodexRejectedResponseBodyTooLarge      CodexRejectedResponseErrorCode = "body_too_large"
	CodexRejectedResponseNoRetainedDefault CodexRejectedResponseErrorCode = "no_retained_default"
	CodexRejectedResponseDuplicateDefault  CodexRejectedResponseErrorCode = "duplicate_default"
	CodexRejectedResponseReleased          CodexRejectedResponseErrorCode = "released"
	CodexRejectedResponseCanceled          CodexRejectedResponseErrorCode = "canceled"
)

// CodexRejectedResponseError is safe to return without exposing response data.
type CodexRejectedResponseError struct {
	Code  CodexRejectedResponseErrorCode
	cause error
}

func (err *CodexRejectedResponseError) Error() string {
	if err == nil {
		return "Codex rejected response error"
	}
	switch err.Code {
	case CodexRejectedResponseInvalidResponse:
		return "invalid Codex rejected response"
	case CodexRejectedResponseBodyReadFailed:
		return "could not read Codex rejected response body"
	case CodexRejectedResponseBodyTooLarge:
		return "Codex rejected response body exceeds 1 MiB"
	case CodexRejectedResponseNoRetainedDefault:
		return "no retained Codex default response"
	case CodexRejectedResponseDuplicateDefault:
		return "Codex default response is already retained"
	case CodexRejectedResponseReleased:
		return "Codex rejected response retention released"
	case CodexRejectedResponseCanceled:
		return "Codex rejected response retention canceled"
	default:
		return "Codex rejected response error"
	}
}

func (err *CodexRejectedResponseError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// CodexRejectedResponseRetention owns at most one configured-default rejection.
// Its zero value is ready for use and its methods are safe for concurrent use.
type CodexRejectedResponseRetention struct {
	mu       sync.Mutex
	response *http.Response
	body     []byte
	pending  *codexRejectedPendingResponse
	released bool
}

type codexRejectedPendingResponse struct {
	body      io.ReadCloser
	closeOnce sync.Once
}

type codexRejectedResponseBody struct {
	readMu sync.Mutex
	mu     sync.Mutex
	body   []byte
	offset int
	tail   io.ReadCloser
	closed bool
}

// Reject disposes an ordinary response or retains one configured-default response.
func (retention *CodexRejectedResponseRetention) Reject(ctx context.Context, response *http.Response, configuredDefault bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if retention == nil {
		canceled := drainAndCloseCodexRejectedResponse(ctx, response)
		if canceled {
			return canceledCodexRejectedResponseError(ctx)
		}
		return &CodexRejectedResponseError{Code: CodexRejectedResponseReleased}
	}
	if response == nil || response.Body == nil {
		retention.Release()
		if err := ctx.Err(); err != nil {
			return canceledCodexRejectedResponseError(ctx)
		}
		return &CodexRejectedResponseError{Code: CodexRejectedResponseInvalidResponse}
	}
	if !configuredDefault {
		canceled := drainAndCloseCodexRejectedResponse(ctx, response)
		if canceled || ctx.Err() != nil {
			retention.Release()
			return canceledCodexRejectedResponseError(ctx)
		}
		retention.mu.Lock()
		released := retention.released
		retention.mu.Unlock()
		if released {
			return &CodexRejectedResponseError{Code: CodexRejectedResponseReleased}
		}
		return nil
	}

	pending := &codexRejectedPendingResponse{body: response.Body}
	retention.mu.Lock()
	if ctx.Err() != nil {
		retention.mu.Unlock()
		drainAndCloseCodexRejectedResponse(ctx, response)
		retention.Release()
		return canceledCodexRejectedResponseError(ctx)
	}
	if retention.released {
		retention.mu.Unlock()
		canceled := drainAndCloseCodexRejectedResponse(ctx, response)
		if canceled || ctx.Err() != nil {
			return canceledCodexRejectedResponseError(ctx)
		}
		return &CodexRejectedResponseError{Code: CodexRejectedResponseReleased}
	}
	if retention.response != nil || retention.pending != nil {
		retention.mu.Unlock()
		canceled := drainAndCloseCodexRejectedResponse(ctx, response)
		if canceled || ctx.Err() != nil {
			retention.Release()
			return canceledCodexRejectedResponseError(ctx)
		}
		return &CodexRejectedResponseError{Code: CodexRejectedResponseDuplicateDefault}
	}
	retention.pending = pending
	retention.mu.Unlock()

	body, err, canceled := readCodexRejectedResponse(ctx, pending)
	retention.mu.Lock()
	if retention.pending == pending {
		retention.pending = nil
	}
	if canceled || ctx.Err() != nil {
		retention.released = true
		retention.mu.Unlock()
		clearBytes(body)
		pending.close()
		return canceledCodexRejectedResponseError(ctx)
	}
	if retention.released {
		retention.mu.Unlock()
		clearBytes(body)
		pending.close()
		return &CodexRejectedResponseError{Code: CodexRejectedResponseReleased}
	}
	if err != nil {
		retention.released = true
		retention.mu.Unlock()
		clearBytes(body)
		pending.close()
		return &CodexRejectedResponseError{Code: CodexRejectedResponseBodyReadFailed}
	}
	if len(body) > maxCodexRejectedResponseBytes {
		retention.released = true
		retention.mu.Unlock()
		response.Body = &codexRejectedResponseBody{body: body, tail: pending.body}
		return &CodexRejectedResponseError{Code: CodexRejectedResponseBodyTooLarge}
	}
	retention.response = cloneCodexRejectedResponse(response)
	retention.body = body
	retention.mu.Unlock()
	pending.close()
	return nil
}

// Exhausted transfers the retained configured-default response after alternatives exhaust.
func (retention *CodexRejectedResponseRetention) Exhausted() (*http.Response, error) {
	if retention == nil {
		return nil, &CodexRejectedResponseError{Code: CodexRejectedResponseReleased}
	}
	retention.mu.Lock()
	if retention.released {
		retention.mu.Unlock()
		return nil, &CodexRejectedResponseError{Code: CodexRejectedResponseReleased}
	}
	if retention.response == nil {
		pending := retention.releaseLocked()
		retention.mu.Unlock()
		pending.close()
		return nil, &CodexRejectedResponseError{Code: CodexRejectedResponseNoRetainedDefault}
	}
	response := retention.response
	response.Body = &codexRejectedResponseBody{body: retention.body}
	retention.response = nil
	retention.body = nil
	retention.released = true
	retention.mu.Unlock()
	return response, nil
}

// PreferLater releases any retained default and passes a later terminal outcome through.
func (retention *CodexRejectedResponseRetention) PreferLater(response *http.Response, err error) (*http.Response, error) {
	retention.Release()
	return response, err
}

// Release discards all owned state. It is safe to call repeatedly or on nil.
func (retention *CodexRejectedResponseRetention) Release() {
	if retention == nil {
		return
	}
	retention.mu.Lock()
	if retention.released {
		retention.mu.Unlock()
		return
	}
	pending := retention.releaseLocked()
	retention.mu.Unlock()
	pending.close()
}

func (retention *CodexRejectedResponseRetention) releaseLocked() *codexRejectedPendingResponse {
	pending := retention.pending
	clearCodexRejectedResponse(retention.response, retention.body)
	retention.response = nil
	retention.body = nil
	retention.pending = nil
	retention.released = true
	return pending
}

func (pending *codexRejectedPendingResponse) close() {
	if pending == nil || pending.body == nil {
		return
	}
	pending.closeOnce.Do(func() { _ = closeCodexRejectedBody(pending.body) })
}

func closeCodexRejectedBody(body io.Closer) (err error) {
	if body == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = nil
		}
	}()
	return body.Close()
}

func readCodexRejectedResponse(ctx context.Context, pending *codexRejectedPendingResponse) ([]byte, error, bool) {
	canceled := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		pending.close()
		close(canceled)
	})
	body, err := io.ReadAll(io.LimitReader(pending.body, maxCodexRejectedResponseBytes+1))
	if !stopCancel() {
		<-canceled
	}
	return body, err, ctx.Err() != nil
}

func canceledCodexRejectedResponseError(ctx context.Context) error {
	cause := context.Canceled
	if ctx != nil && ctx.Err() != nil {
		cause = ctx.Err()
	}
	return &CodexRejectedResponseError{Code: CodexRejectedResponseCanceled, cause: cause}
}

func (body *codexRejectedResponseBody) Read(p []byte) (int, error) {
	if body == nil {
		return 0, http.ErrBodyReadAfterClose
	}
	body.readMu.Lock()
	defer body.readMu.Unlock()
	body.mu.Lock()
	if body.closed {
		body.mu.Unlock()
		return 0, http.ErrBodyReadAfterClose
	}
	if body.offset >= len(body.body) {
		tail := body.tail
		body.mu.Unlock()
		if tail == nil {
			return 0, io.EOF
		}
		return tail.Read(p)
	}
	n := copy(p, body.body[body.offset:])
	body.offset += n
	body.mu.Unlock()
	return n, nil
}

func (body *codexRejectedResponseBody) Close() error {
	if body == nil {
		return nil
	}
	body.mu.Lock()
	if body.closed {
		body.mu.Unlock()
		return nil
	}
	clearBytes(body.body)
	body.body = nil
	tail := body.tail
	body.tail = nil
	body.closed = true
	body.mu.Unlock()
	if tail == nil {
		return nil
	}
	return closeCodexRejectedBody(tail)
}

func cloneCodexRejectedResponse(response *http.Response) *http.Response {
	return &http.Response{
		Status:     strings.Clone(response.Status),
		StatusCode: response.StatusCode,
		Proto:      strings.Clone(response.Proto),
		ProtoMajor: response.ProtoMajor,
		ProtoMinor: response.ProtoMinor,
		Header:     response.Header.Clone(),
	}
}

func clearCodexRejectedResponse(response *http.Response, body []byte) {
	clearBytes(body)
	if response == nil {
		return
	}
	response.Status = ""
	response.StatusCode = 0
	response.Proto = ""
	response.ProtoMajor = 0
	response.ProtoMinor = 0
	clear(response.Header)
	response.Header = nil
}

func drainAndCloseCodexRejectedResponse(ctx context.Context, response *http.Response) bool {
	if response == nil || response.Body == nil {
		return ctx != nil && ctx.Err() != nil
	}
	pending := &codexRejectedPendingResponse{body: response.Body}
	if ctx == nil {
		ctx = context.Background()
	}
	canceled := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		pending.close()
		close(canceled)
	})
	_, _ = io.CopyN(io.Discard, pending.body, maxCodexRejectedResponseBytes+1)
	if !stopCancel() {
		<-canceled
	}
	pending.close()
	return ctx.Err() != nil
}
